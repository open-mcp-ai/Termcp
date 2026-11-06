package mcp

import (
	"context"
	"fmt"
	"sync"

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
	for _, group := range partitionGroups(stmts) {
		if len(group) == 1 {
			slots := s.runStatementSequential(ctx, &out, group[0], win)
			out.slots = append(out.slots, slots...)
			continue
		}
		// PARALLEL: every statement in the group starts together, and the reply waits
		// for all of them. Slots are emitted in the ORIGINAL order, because a caller
		// reads them positionally — collecting them by completion order would make the
		// reply order depend on which command happened to finish first.
		results := make([][]envelope.Slot, len(group))
		var wg sync.WaitGroup
		for i, stmt := range group {
			wg.Add(1)
			go func(i int, stmt directive.Statement) {
				defer wg.Done()
				results[i] = s.runStatementSlots(ctx, stmt, win)
			}(i, stmt)
		}
		wg.Wait()
		for _, slots := range results {
			out.slots = append(out.slots, slots...)
		}
	}

	return out
}

// partitionGroups groups statements by concurrency: consecutive Background
// statements, plus the first non-background statement that closes the batch, form
// one group. A trailing run of background statements is a group of its own — the
// caller asked for fire-and-forget, and waiting for a non-existent follower would
// hang the reply.
func partitionGroups(stmts []directive.Statement) [][]directive.Statement {
	var groups [][]directive.Statement
	var cur []directive.Statement
	for _, st := range stmts {
		cur = append(cur, st)
		if !st.Background {
			groups = append(groups, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		groups = append(groups, cur)
	}
	return groups
}

// runStatementSlots expands, dispatches and annotates one statement. It is the
// concurrency-safe half: it writes nothing shared, which is what lets a parallel
// group call it from several goroutines.
func (s *Server) runStatementSlots(ctx context.Context, stmt directive.Statement, win termArgs) []envelope.Slot {
	expanded, cycle := aliases.Expand(stmt)
	if cycle != nil {
		return []envelope.Slot{aliasCycleSlot(stmt, cycle)}
	}
	slots := s.runDirective(ctx, expanded, win)
	if win.LearnMode && len(slots) > 0 {
		// A parallel group has no single reply to dedup against, so usage is attached
		// directly. Two identical directives in one parallel batch would repeat their
		// usage line, which is the honest outcome: they ran twice.
		if slots[0].Headers == nil {
			slots[0].Headers = map[string]string{}
		}
		slots[0].Headers["Usage"] = expanded.Op
	}
	return slots
}

// runStatementSequential runs one statement in command order, applying the
// `&&`/`||` gate and the unknown-directive hint. Only this path touches reply
// state (previousOK, seenUsage, lastRan), which is why the parallel path cannot
// reuse it.
func (s *Server) runStatementSequential(ctx context.Context, out *termReply, stmt directive.Statement, win termArgs) []envelope.Slot {
	if stmt.Cond == "&&" && !out.previousOK {
		return []envelope.Slot{skippedSlot(stmt, "&&", "the previous statement failed")}
	}
	if stmt.Cond == "||" && out.previousOK {
		return []envelope.Slot{skippedSlot(stmt, "||", "the previous statement succeeded")}
	}
	expanded, cycle := aliases.Expand(stmt)
	if cycle != nil {
		return []envelope.Slot{aliasCycleSlot(stmt, cycle)}
	}
	slots := s.runDirective(ctx, expanded, win)
	if win.LearnMode && len(slots) > 0 {
		if slots[0].Headers == nil {
			slots[0].Headers = map[string]string{}
		}
		s.addUsage(out, expanded, slots[0].Headers)
	}
	out.previousOK = slotsAllOK(slots)
	return slots
}

// slotsAllOK reports whether every slot a statement produced is 2xx.
//
// "Every" rather than "the first": a directive may answer with several slots, and a
// chain that gated on only one of them would call a partial failure a success.
func slotsAllOK(slots []envelope.Slot) bool {
	for _, sl := range slots {
		if sl.Code >= 300 {
			return false
		}
	}
	return true
}

// skippedSlot is the reply for a statement the condition short-circuited.
//
// The code is 208 rather than 204 on purpose: 204 says "done, nothing to say", which
// is also what a successful close answers, and a caller could not tell a statement it
// skipped from one that ran. 208 is outside the 2xx-a-directive-normally-returns
// range without being an error — the caller's chain worked as written, one branch
// simply was not taken.
func skippedSlot(stmt directive.Statement, op, why string) envelope.Slot {
	return envelope.Slot{
		Code: 208,
		Headers: map[string]string{
			"Path":       stmt.Path,
			"Error-Code": "short_circuited",
		},
		Body: "not run: `" + op + "` and " + why,
	}
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
