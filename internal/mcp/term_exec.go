package mcp

import (
	"context"
	"fmt"

	"github.com/open-mcp-ai/termcp/term"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// An alias is expanded before dispatch and before Path is read, so the echo a
// caller gets back is the command that actually ran.
//
// The reply accumulator is a LOCAL, threaded where it is needed. It used to also
// live on the Server (s.currentReply) so a directive could consult learn_mode's
// de-duplication without receiving it; that made two concurrent Term calls share
// mutable state, which is a data race — and the field was redundant anyway, since
// the learn_mode attach below already runs for every statement. Removing it is why
// the MCP server is now safe for concurrent Term calls.
func (s *Server) runStatements(ctx context.Context, stmts []directive.Statement, win termArgs) termReply {
	// RAW CONTROL BYTES IN A SHELL LINE ARE REFUSED, and the check reads the PARSED
	// statements rather than cmd as typed, because what decides is which layer receives
	// the byte. `get "a\rb"` names a PATH, and a path may contain anything — the
	// caller's bytes are the answer there. `run h:1 echo a\rb` hands the byte to a
	// TERMINAL, which acts on it (submits the line, interrupts, sends EOF) instead of
	// printing it: one call runs two commands and answers 200 for one of them.
	//
	// THE TWO LANGUAGES ARE WHY THIS IS NOT A FLAG ANY MORE. cmd is DSL text and a
	// shell line is a value, so a keystroke is not something you type into either — it
	// is a directive: `key <channel> ctrl-c`. There is no legitimate control byte in a
	// shell line, which is what makes a refusal safe without an override.
	//
	// A VALUE CANNOT SMUGGLE ONE EITHER: `args: ["a\rb"]` substituted into a payload
	// lands in that payload, which is exactly the region this reads. The same bytes in
	// `--data` or in a Path go to a destination no terminal reads, and pass.
	//
	// THIS IS THE FUNNEL, so the check sits here rather than at the parse: a statement
	// reached through an alias expansion never passes the caller's parse, and that is
	// the path a substituted value would take into the terminal.
	if _, name, found := controlByteInShell(stmts); found {
		ctrlSlot := envelope.Slot{
			Code: 400,
			Headers: map[string]string{
				"Error-Code": "raw_control_byte",
			},
			Body: fmt.Sprintf("the shell line contains a raw %s, which the terminal acts on instead of printing.\n"+
				"A shell line is text a terminal will read byte by byte, so there is no way to send this one as a line.\n"+
				"To press a key, name it: `key <channel> ctrl-c` (`help key` lists the names).\n"+
				"If the byte is DATA — a path, a file's contents — send it to a destination no terminal reads: "+
				"`--data`, a Path, a frame. Those arrive as written.", name),
		}
		// THE CONTROL BYTE IS A DEFECT THIS LINE HAS, SO A DRY RUN REPORTS IT AS ONE
		// (#241). It is caught before dispatch, which is exactly the boundary a
		// preview draws; answering a real refusal while withholding the two headers
		// would leave a harness unable to tell "checked and rejected" from "not
		// checked at all".
		if win.DryRun {
			ctrlSlot.Headers["Dry-Run"] = "true"
			ctrlSlot.Headers["Verdict"] = "invalid"
		}
		return termReply{win: win, slots: []envelope.Slot{ctrlSlot}}
	}
	out := termReply{win: win, seenUsage: map[string]bool{}, previousOK: true}

	// DRY RUN STOPS HERE, before the group loop below reaches any directive
	// (#241). Everything that had to happen to answer "is this valid" has
	// happened already — parse, substitution, alias expansion and the control-byte
	// funnel above — so the only thing left to skip is the dispatch itself.
	//
	// It is one branch at the funnel rather than a flag threaded into every
	// directive, because a directive that forgot to read the flag would execute: the
	// safety of a preview cannot depend on twenty call sites remembering it.
	if win.DryRun {
		out.slots = s.dryRunSlots(stmts, win)
		return out
	}

	// GROUPS ARE THE UNIT OF CONCURRENCY. `a & b` is two statements that run
	// together; `a & b; c` is a parallel pair followed by a sequential one.
	//
	// The grouping exists because `&` otherwise did NOTHING: the shell parser reports
	// `a & b` as two statements and records the parallelism in a Background flag that
	// nothing read, so the line ran sequentially while reading as parallel. A silent
	// semantic reversal is worse than a refusal, which is why this is implemented
	// rather than rejected.
	// THE SEQUENCING RULES ARE THE PROTOCOL'S, so they live in term.Run and are
	// not repeated here (#273). What this method keeps is everything that is NOT
	// sequencing: the control-byte funnel above, the dry-run branch above, and the
	// handler below, which is the capability layer — alias expansion plus the
	// directive dispatch.
	//
	// The split matters beyond tidiness: a program that wants TERM/1 without this
	// server now implements term.Handler and gets &&/||/&/208 for free, instead of
	// re-deriving rules that are easy to get subtly wrong.
	//
	// THE TERM IS BUILT HERE, per call, because both of its parts are per call: the
	// handler closes over this call's window, and LearnMode comes from the same
	// place. Construction is a struct literal, not an allocation worth avoiding.
	t := term.New(
		directive.DefaultGrammar,
		term.WithHandler(&capabilityHandler{s: s, win: win}),
		term.WithOptions(term.Options{LearnMode: win.LearnMode}),
	)
	reply := t.Run(ctx, &term.File{Statements: stmts})
	out.slots = reply.Slots
	return out
}

// controlByteInShell reports the first raw control byte sitting in a region the
// TERMINAL will receive, and names where it is.
//
// The regions are the two the parser marked as shell syntax: a statement's payload
// and the word list of `--argv`. Everything else the caller wrote — a path, a flag
// value, a frame — is bytes for something that is not a line editor, so a control
// byte there is data and arrives as written. That split is the same one the escaping
// rule uses (see directive.Subst), and it is why this reads parsed statements instead
// of scanning cmd: the question is never "does the text contain a byte" but "which
// layer reads it".
func controlByteInShell(stmts []directive.Statement) (b byte, name string, found bool) {
	for _, st := range stmts {
		regions := []string{st.Payload}
		if argv, ok := st.Flag("argv"); ok {
			regions = append(regions, argv)
		}
		for _, text := range regions {
			if b, name, found = directive.DetectRawControl(text); found {
				return b, name, true
			}
		}
	}
	return 0, "", false
}

// parseErrorSlot renders a directive-level failure as one slot, so a bad
// statement can sit beside a good one without taking the whole call down.
//
// capabilityHandler is this server as term.Handler: the capability layer.
//
// It owns exactly the two things that are about capabilities rather than
// sequencing — alias expansion (stored aliases are data this project keeps) and
// the directive dispatch (what each verb does) — and hands everything else to
// term.Execute.
type capabilityHandler struct {
	s   *Server
	win termArgs
}

func (h *capabilityHandler) Handle(ctx context.Context, st directive.Statement) []envelope.Slot {
	expanded, cycle, err := h.s.aliases.Expand(st)
	if err != nil {
		return []envelope.Slot{h.s.parseErrorSlot(err)}
	}
	if cycle != nil {
		return []envelope.Slot{aliasCycleSlot(st, cycle)}
	}
	slots := make([]envelope.Slot, 0, len(expanded))
	for _, single := range expanded {
		slots = append(slots, h.s.runDirective(ctx, single, h.win)...)
	}
	return slots
}
