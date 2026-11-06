package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// The single-entry tool.
//
// Everything this instance can do is reachable through one tool whose only
// required input is nothing at all: `term()` answers "where am I and what is
// here", and `term(cmd: "run a1f3:1 go build ./...")` is one command in one call
// where the specialized surface needed four. The capability layer is what makes
// that safe — the same admission decision covers this entry and the specialized
// tools, so a narrower surface cannot be a way around a wider one.
//
// Terminating the tool surface is the point: the tool list stops growing because
// growth happens in the COMMAND space (a new directive, an alias, an installed
// MCP server) rather than in `tools/list`. The command space documents itself —
// `help`, `help <directive>`, and an empty cmd — so growth needs no second file
// to keep in sync.
const termToolName = "term"

// window defaults. Two seconds is the sampling window for a pty and the upper
// bound for a pipe's exit wait. Space stays the scarce axis: a wide window
// spends context that is never recovered.
const (
	// One window for both modes. A pipe returns as soon as its process exits
	// (early), a pty waits the window out; two seconds is long enough to catch
	// a terminal's late chunks and short enough that a stuck wait is cheap.
	defaultTimeoutSeconds  = 2
	defaultBytesLimit      = 8192
	defaultLinesLimit      = 200
	defaultTotalBytesLimit = 32768

	// THE WINDOW IS TWO COSTS THE CALLER PAYS, AND NEITHER IS A WALL.
	//
	// It bounds how long one call may wait and how much one call may bring into the
	// caller's context, because both are costs paid by the model on the other side.
	// Waiting long holds a turn open; reading a lot fills a context.
	//
	//	NEITHER HAS A HARD CAP, and the reason is the same for both: the caller is the
	//	party paying, it is the only party that knows the job, and this server has no
	//	basis to overrule the one decision it cannot see the whole of. So these three
	//	numbers are SIZES TO STAY UNDER, published as such in the argument schema, and a
	//	larger value is honoured. The reply then says what happened — `Waited` and
	//	`Still-Running` for time, `Truncated` for a generated reply, `Content-Range` for
	//	a windowed one — so a value that ran into someone else's ceiling is visible as a
	//	fact rather than as a refusal.
	//
	//	There IS a real ceiling on time and it is not this server's: the MCP transport in
	//	front of the caller may give up sooner (30s on the SSE bridge, 15s on a peer
	//	call), and past that the reply is lost while the command runs on. Refusing above
	//	a number this server picked made the caller's vocabulary depend on a timeout
	//	somewhere it cannot see; the headers let it discover the real ceiling itself.
	//
	//	windowBytesMax is about eight thousand tokens: past it a reply stops being
	//	something a caller reads and becomes something it works through. windowLinesMax
	//	is the same kind of number for a log tail, already past what one call should
	//	carry and bounded by the byte window anyway.
	maxWaitSeconds = 60
	windowBytesMax = 32768
	windowLinesMax = 2000
	// minWindowBytes is the narrowest byte window that can make progress. A window
	// never ends inside a character, so a window narrower than the widest character any
	// accepted charset can produce withholds the same bytes on every call and advances
	// the cursor nowhere: 1-3 bytes answers an unsatisfied range forever, which is what
	// "no data" looks like. 4 is utf8.UTFMax; the charset-specific width is deliberately
	// not consulted, because a floor that moves with a flag is harder to hold in mind
	// than "one character".
	minWindowBytes = 4
)

// termArgs is the parsed window of one term call: the parameters that decide how
// the caller sees a command, never what the command does.
//
// They are named parameters on the tool rather than flags inside cmd for three
// reasons, all of which showed up in practice: they apply to every directive
// (`ps` and `help` need protection too), they must be visible in the schema with
// their defaults (so "this does not blow up my context" is a promise the caller
// can READ), and they never reach the shell — bytes sent to a shell are shaped by
// the shell, not by this protocol.
type termArgs struct {
	TimeoutSeconds float64
	// TimeoutSet says the caller NAMED a timeout, which is not the same as the value it
	// ended up with. `timeout` has a default (2s), so every reply carries a number whether
	// or not anyone asked for one — and a subsystem that must fall back to its own default
	// (a peer call waits 15s: short enough for a 30s host watchdog, long enough for an
	// ordinary CDP evaluate) cannot tell "the caller asked for 2 seconds" from "nobody
	// asked". It read the second as the first, so the 15s default was dead code and every
	// peer call through the term door was bounded by 2 seconds instead.
	TimeoutSet bool
	// BytesLimitSet says the caller NAMED a byte limit, for the same reason TimeoutSet
	// exists: `bytes_limit` has a default, so the number in this struct is never zero
	// and "the caller asked for 200 bytes" is indistinguishable from "nobody asked".
	//
	// A pipeline is where that difference decides behaviour: `read ... | write ...`
	// copies the WHOLE span by default, and only truncates to `bytes_limit` when the
	// caller actually named one. Reading the default as a request would silently cut
	// every copy to 200 bytes and report success.
	BytesLimitSet   bool
	BytesLimit      int
	LinesLimit      int
	TotalBytesLimit int
	OnBytesOverflow string
	OnLinesOverflow string
	LearnMode       bool

	// DryRun stops after parse and expansion, before dispatch (#241). See
	// dryRunSlots for what that guarantees and what it deliberately does not.
	DryRun bool

	// StructuredContent also sends the reply as the JSON projection, on top of the
	// rendered text. It is off by default because the two carry the SAME bytes and
	// the text is the copy a caller usually reads, so the JSON twin is a duplicate
	// paid for on every call. See envelope.Reply.StructuredContent.
	StructuredContent bool
	// Content suppresses the rendered text projection on the MCP wire when the
	// caller asks for structuredContent only. False is the zero-value default and
	// preserves the existing behavior; so the inverse travels with the reply.
	NoText bool

	// Vars holds values substituted into cmd at AST parse time, by variable name.
	//
	// It is the channel for data that cannot be written into a command line: a
	// newline, a quote, a semicolon, a carriage return. The value never appears in
	// cmd, so nothing in cmd can mangle it, and the substitution quotes it before it
	// reaches the shell.
	//
	// It is NAMED vars, not env, on purpose. `env` means process environment to every
	// tool that has ever had one, and this is not that: a key here is used only if
	// cmd mentions it, nothing is exported to the child, and a $NAME cmd refers to
	// that vars does not define is a 400 rather than the empty string an environment
	// lookup would give. A caller that read `env` as "set FOO=bar for the child"
	// would be wrong in all three places.
	Vars map[string]string

	// Secrets holds named values that expand exactly like Vars and are NOT written to
	// the server log.
	//
	// IT IS A RECORDING DECISION, NOT A SUBSTITUTION ONE. The two tables are merged
	// before cmd is parsed, so `$NAME` resolves the same whichever one named it, and a
	// name in both is refused. What differs is the log: Vars is written verbatim — it is
	// what makes the log a faithful record of the call — and Secrets contributes only
	// its NAMES. That is the whole mechanism, and it is deliberately not pattern
	// matching: a redactor that knows `-p` and misses `--password` reports a safety it
	// does not have, and it would silently rewrite legitimate text on the way in. The
	// caller says what must not be written down, by where it puts the value.
	//
	// It protects the SERVER's record, and nothing else. A secret expanded into a
	// command line is still echoed back by the remote shell, and that echo belongs to
	// the channel's own transcript: the terminal cannot be asked to hide what it printed.
	Secrets map[string]string

	// Args holds positional values substituted into cmd as $1, $2, … (1-based).
	//
	// IT IS THE OPPOSITE CHANNEL TO VARS, and the difference is the whole reason it
	// exists. A positional value that IS the whole payload of `run`/`type` is spliced
	// in VERBATIM, because it is a command line: args=["pwd; whoami"] on
	// `run h:1 $1` sends two commands to the terminal. Quoting it — which is what
	// vars does, correctly, for data — produced `'pwd; whoami'`, and the shell
	// answered "command not found: pwd; whoami".
	//
	// A positional reference anywhere else (beside other words, in a flag value, in a
	// target) is substituted as ONE QUOTED WORD, so a value can never turn into
	// syntax the caller did not write. In a region no shell reads — a flag value, a
	// protocol frame — nothing quotes it, because nothing would unquote it.
	//
	// WHAT COUNTS AS A REFERENCE IS THE GRAMMAR'S ANSWER, and that is the whole rule:
	// a `$` the parser reported is a reference; one it did not report the caller kept
	// from being parsed. So `run h:1 echo '$1'` sends the literal, `--data '$1'` sends
	// the literal, and `--data $1` sends the value — the same sentence, three
	// destinations, no per-region quoting rules to remember. The one region the grammar
	// never reads is a `call` payload (a JSON frame is not a shell word), so there the
	// bytes are read directly.
	//
	// $0 is refused rather than bound to the first element: the shell gives $0 a
	// meaning (the program's own name) that an argument list does not have, so
	// binding it would be a guess. ${N} is the unambiguous spelling for N ≥ 10.
	Args []string
}

// readCmd extracts the command line, distinguishing the three states a caller can
// send.
//
// It EXISTS because the tool's one string parameter is where a silent failure was
// most visible: a `cmd` that is not a string used to be read with a loose getter
// that answered "" for anything it did not like, and "" is the GUIDE PAGE. So a
// client whose bridge mangled the argument — issue #131 P0-3, where a call returned
// the wake-up page instead of running — got a successful-looking reply that
// described the whole protocol, and nothing anywhere said a command had been lost.
//
//	absent        -> "", nil   the documented empty call: the guide, which is legal
//	null          -> "", nil   the same, since JSON null is how "no value" arrives
//	string        -> it, nil
//	anything else -> 400       the caller sent a command and it was not a string
//
// The distinction is the point: "I did not send a command yet" and "my command did
// not survive the transport" are different facts, and only one of them is a request
// for the guide.
func readCmd(args map[string]any) (cmd string, present bool, derr *directive.Error) {
	raw, ok := args["cmd"]
	if !ok || raw == nil {
		return "", false, nil
	}
	s, isString := raw.(string)
	if !isString {
		return "", false, &directive.Error{Msg: "cmd must be a string, got " + typeNameOf(raw) +
			"; send the command line as a string, or omit cmd entirely to read the guide"}
	}
	return s, true, nil
}

// handleTerm is the MCP entrance: a thin wrapper that hands the call's arguments to
// Term and re-renders the reply as a tool result.
//
// It does not read `cmd` itself. Reading it in two places is how the two entrances
// drifted apart once already (issue #131 P0-3): each call site had its own copy of
// "absent means the guide", and the loose getter they shared folded "not a string"
// into it. Term is the one reader.
func (s *Server) handleTerm(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	return toolResult(s.Term(ctx, "", request.GetArguments())), nil
}

// Term executes one term command line and returns the reply as plain data.
//
// It is the surface-independent half of the tool: the command space IS the
// protocol, so any entrance that can produce a (cmd, window) pair gets the same
// execution and the same envelope. MCP wraps the reply in a tool result; REST
// serves its structured projection (see TermHandler). Neither re-implements a
// directive, and neither re-derives admission — every directive reaches a
// handler through callTool, which runs checkAdmission, so the capability table
// and review mode cover this entrance without being told it exists.
//
// The window parameters are read from args by their tool names, so a caller
// arriving over HTTP sends the same object the tool's schema describes.
//
// `cmd` IS READ FROM args, not from the parameter, when args carries one. The
// parameter stays because executeTerm is also reached directly and because a caller
// holding a command string should not have to build a map to run it; args wins where
// both are present, because args is the value a transport actually delivered.
//
// Reading it HERE, once, is what makes both entrances agree: a `cmd` that is not a
// string is a 400 and an absent one is the guide, and neither surface gets to have
// its own opinion about which is which (issue #131 P0-3).
func (s *Server) Term(ctx context.Context, cmd string, args map[string]any) envelope.Reply {
	if fromArgs, present, derr := readCmd(args); derr != nil {
		return envelope.Reply{Slots: []envelope.Slot{badRequestNoPath(derr.Msg)}}
	} else if present {
		cmd = fromArgs
	}
	win, derr := parseTermArgs(args)
	if derr != nil {
		return envelope.Reply{Slots: []envelope.Slot{badRequestNoPath(derr.Msg)}}
	}
	t := s.executeTerm(ctx, cmd, win)
	// EVERY 2xx SLOT INHERITS THE CALLER'S WINDOW UNLESS IT SAYS IT ALREADY DID.
	//
	// `bytes_limit`/`lines_limit` are documented as an argument of the tool, not of one
	// directive — but applying them was each handler's own job, done through
	// generatedSlot for the paths that remembered. The paths that did not (a peer's
	// tools/list, `cdp protocol`, `ls task`, `help <node>`) answered with the whole
	// body however small a window the caller asked for, so the same argument meant
	// "a budget" for a listing and "not consulted" for the listing next to it.
	//
	// Enforcing it HERE, at the one place both entrances pass through, is what makes it
	// the tool's contract rather than a rule each new directive has to be told: a slot
	// built by a directive that never heard of the window is still cut, and a slot that
	// deliberately is not (a streamed read with a cursor, a frame that must stay whole)
	// says so once, at the place that decided it.
	slots := applySlotWindows(t.slots, win)
	// The total cap is a property of the REPLY, not of one slot: ten slots each
	// within their own byte limit still add up to a context nobody budgeted for.
	return envelope.Reply{Slots: t.applyTotalCap(slots), StructuredContent: win.StructuredContent, NoText: win.NoText}
}

// applySlotWindows caps every 2xx slot that has not already been shaped to the
// caller's window, with the same policy (and the same honesty headers) a generated
// reply gets.
//
// WHY IT IS SHAPED LIKE generatedSlot: a caller that reads `ls host` and `ls task`
// before and after this guard should not be able to tell which one went through a
// helper. The `Truncated` header, the `bytes */N` content range and the 206 status
// are the reply's account of what it withheld, so the account is produced in one
// place and reads the same everywhere.
func applySlotWindows(slots []envelope.Slot, win termArgs) []envelope.Slot {
	for i, slot := range slots {
		// A FAILURE IS NOT A WINDOW'S TO CUT: a refusal that stops mid-sentence is a
		// refusal the caller cannot act on, and the bytes a diagnosis costs are the
		// reason it was worth returning. total_bytes_limit still bounds the reply.
		// ONLY A 200 IS CONTENT: a 202 receipt names a transfer, a 204/206 already
		// reports its own truncation, and a 4xx/5xx is a diagnosis — cutting any of them
		// would answer a different question than the one asked (a 202 receipt cut to 100
		// bytes is a receipt nobody can act on).
		if slot.Code != 200 || slot.Body == "" {
			continue
		}
		body, code, cut, total := capGeneratedCounted(slot.Body, win)
		// A slot that already went through a window is 206/204 with its own headers, and
		// re-running the cap on it finds nothing further to withhold (200, cut 0). Acting
		// on that would overwrite the 206 with a 200 and erase the truncation the reply
		// had already reported — so the question is only ever "is there anything to cut?"
		if code == 200 && cut == 0 {
			continue
		}
		slot.Body = body
		slot.Code = code
		if slot.Headers == nil {
			slot.Headers = map[string]string{}
		}
		noteTruncated(slot.Headers, code, cut, total, win)
		slots[i] = slot
	}
	return slots
}

// executeTerm parses cmd and runs it, accumulating the reply.
func (s *Server) executeTerm(ctx context.Context, cmd string, win termArgs) termReply {
	file, err := directive.ParseWithSubst(cmd, directive.Subst{Named: win.Vars, Pos: win.Args})
	if err != nil {
		// A directive the caller invented (an alias) is unknown to the parser by
		// definition, so it is resolved here before the error is reported — with the
		// SAME substitution table, because the alias's expansion is the caller's
		// command line and may reference the same $1/$NAME values.
		// resolved says a directive was actually produced; cycle says the chain
		// never got there. Neither means the word is simply not a directive, and that
		// case must keep falling through to the normal parse-error teaching below.
		stmt, resolved, cycle := aliases.expandErrorCycle(err)
		if resolved {
			if win.DryRun {
				return termReply{win: win, slots: s.dryRunSlots([]directive.Statement{stmt}, win)}
			}
			return s.runStatements(ctx, []directive.Statement{stmt}, win)
		}
		if len(cycle) > 0 {
			slot := aliasCycleSlot(directive.Statement{Path: cmd}, cycle)
			if win.DryRun {
				if slot.Headers == nil {
					slot.Headers = map[string]string{}
				}
				slot.Headers["Dry-Run"] = "true"
				slot.Headers["Verdict"] = "invalid"
			}
			return termReply{win: win, slots: []envelope.Slot{slot}}
		}
		// A PARSE FAILURE IN A DRY RUN IS STILL A DRY RUN: the caller asked to be told
		// whether this line is valid, and the answer is no. The existing refusal keeps
		// its body (it is the teaching text that fixes the mistake) and gains the two
		// headers that say which question it answered (#241).
		slot := s.parseErrorSlot(err)
		if win.DryRun {
			if slot.Headers == nil {
				slot.Headers = map[string]string{}
			}
			slot.Headers["Dry-Run"] = "true"
			slot.Headers["Verdict"] = "invalid"
		}
		return termReply{win: win, slots: []envelope.Slot{slot}}
	}
	// An empty cmd is the "I just woke up" call: it must be legal, cheap, and
	// information-dense, never an error.
	if file.Empty {
		return s.termGuide(ctx, win)
	}

	return s.runStatements(ctx, file.Statements, win)
}

// runStatements dispatches already-parsed statements and assembles the reply.
//
// A directive or flag error is 400 — the syntax is wrong, not the state — and it
// carries its Usage line whether or not learn_mode was asked for: an error is
// exactly the moment the caller does not know the shape, and that is what the
// line is for. Knowing the shape already costs nothing, because a successful call
// never carries one.
func (s *Server) parseErrorSlot(err error) envelope.Slot {
	derr, ok := err.(*directive.Error)
	if !ok {
		return badRequestNoPath(err.Error())
	}
	headers := map[string]string{"Error-Code": errorCodeFor(derr)}
	if derr.Usage != "" {
		headers["Usage"] = derr.Usage
	}
	if _, known := directive.Lookup(derr.Op); !known && derr.Op != "" {
		// An unknown directive is exactly the caller who needs the table, so the
		// whole listing rides along instead of a second round trip.
		//
		// EXCEPT WHEN THE CALLER WAS NOT WRITING DIRECTIVES. A `;` that ended a
		// payload leaves an unknown "directive" that is really a shell command, and
		// printing 25 directive names beside that teaches the wrong shape: the
		// caller does not need a different directive, they need a different slot.
		// The HINT below says which one, so the table would only compete with it.
		if derr.AfterPayload == "" {
			headers["Usage"] = directiveTable()
		}
	}
	// PARSING PRECEDES EVERYTHING. The whole cmd is parsed before the first
	// statement runs, so this failure means the batch did not run — not even the
	// statements that parsed. Without saying so, a 400 with no slots is
	// indistinguishable from a batch that ran and produced nothing, and the caller
	// is left reading the error to guess whether any side effect happened.
	//
	// The count is there for the caller whose cmd was long enough that "how much
	// of it was accepted" is itself information: it is the boundary between "fix
	// one word" and "the shape is wrong".
	// The note only appears when an EARLIER statement parsed: that is the case
	// where the caller has something to be wrong about ("I wrote three things and
	// one is a typo — did the other two run?"). With a single broken statement
	// there is nothing else that could have run, so saying so is noise on the most
	// common error of all.
	body := derr.Msg
	if derr.AfterPayload != "" {
		if strings.Contains(derr.AfterPayload, " --argv") {
			// The operator that separated the statements ended the `--argv` word list,
			// and the remainder became a directive. Name the exact operator (`;`, `&`,
			// `&&`) so the diagnosis is true for the line the caller actually wrote.
			opName := derr.TrailingSep
			if opName == "" {
				opName = ";"
			}
			body += fmt.Sprintf("\nHINT: `%s` ended at `%s`; `%s` is termcp syntax, not part of --argv.\n"+
				"To run a shell command line, pass it as one argument to a shell:\n"+
				"  open <session> --argv sh -c $1   args: [\"echo OUT; exit 7\"]",
				derr.AfterPayload, opName, opName)
		} else {
			// The failing statement followed a directive whose payload goes to a
			// terminal, so the most likely cause is a `;` the caller meant as SHELL
			// sequencing. Naming the fix beats naming the directive table: the caller
			// did not misspell the directive, they wrote shell text in a slot holding
			// one variable reference.
			// NO `&&` SUGGESTION. That advice is wrong here: `&&` separates DIRECTIVES,
			// and a shell line goes through args, full stop.
			body += fmt.Sprintf("\nHINT: %q follows a `%s`, whose payload is ONE VARIABLE REFERENCE. `;` and `&&` in\n"+
				"cmd are termcp syntax — `;` separates statements and `&&` gates the next one — so a\n"+
				"shell command line with its own operators belongs in args:\n"+
				"  cmd: \"%s <channel> $1\"   args: [\"pwd; whoami\"]   or   args: [\"make && echo ok\"]",
				derr.Op, derr.AfterPayload, derr.AfterPayload)
		}
	}
	if derr.Parsed > 0 {
		body += fmt.Sprintf("\nnothing was executed, including the %d statement(s) before this one: a cmd is parsed in full before any of it runs", derr.Parsed)
	}
	return envelope.Slot{Code: 400, Headers: headers, Body: body}
}

// directiveTable renders the one-line table `help` prints, for the error reply
// that has to teach the whole shape.
func directiveTable() string {
	var b strings.Builder
	for i, op := range directive.Ops() {
		if i > 0 {
			b.WriteString(" | ")
		}
		b.WriteString(op.Name)
	}
	return b.String()
}

// errorCodeFor classifies a directive error.
func errorCodeFor(derr *directive.Error) string {
	if derr.Op == "" {
		return CodeInvalidArgument
	}
	if _, known := directive.Lookup(derr.Op); !known {
		return "unknown_directive"
	}
	return CodeInvalidArgument
}

// termReply accumulates the slots of one call and applies the total byte cap.
type termReply struct {
	win       termArgs
	slots     []envelope.Slot
	seenUsage map[string]bool
	// previousOK is what `&&`/`||` gate on: whether the previous statement came back
	// 2xx. It lives on the reply rather than in a local so the per-statement function
	// can own the bookkeeping for one statement.
	previousOK bool
}

// result assembles the tool result from a reply: the text projection, the
// structured twin, and the isError flag taken from the codes.
//
// defaultTermArgs is the window used when a reply is built outside a call (an
// internal error path); it matches the schema's defaults so the two cannot write
// different obligations.
func defaultTermArgs() termArgs {
	return termArgs{
		TimeoutSeconds:  defaultTimeoutSeconds,
		BytesLimit:      defaultBytesLimit,
		LinesLimit:      defaultLinesLimit,
		TotalBytesLimit: defaultTotalBytesLimit,
		OnBytesOverflow: "truncate",
		OnLinesOverflow: "truncate",
	}
}

// withUsage adds the directive's usage line when learn_mode asked for it. It is
// a method on the accumulating reply so the "same directive once" rule has one
// home.
func (t *termReply) withUsage(stmt directive.Statement, headers map[string]string) {
	if !t.win.LearnMode || t.seenUsage[stmt.Op] {
		return
	}
	if op, ok := directive.Lookup(stmt.Op); ok && op.Usage != "" {
		headers["Usage"] = op.Usage
		t.seenUsage[stmt.Op] = true
	}
}

// atoiFlag reads an integer flag.
//
// `absent` is the value used when the flag is NOT given at all — a documented
// default, not a rescue. A flag written WITHOUT a value is a different mistake and
// is reported as one; a flag with a value that does not parse is reported as that.
// The three cases used to be one, and `--repeat` silently meant `--repeat 1`.
func atoiFlag(stmt directive.Statement, key string, absent int) (int, error) {
	switch stmt.FlagState(key) {
	case directive.FlagAbsent:
		return absent, nil
	case directive.FlagNoValue:
		return 0, fmt.Errorf("needs a value")
	}
	raw, _ := stmt.Flag(key)
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("expected an integer, got %q", raw)
	}
	return n, nil
}

// boolFlag reports whether a valueless flag was given.
//
// A flag written as `--force=yes` is given too: the caller's intent is the same and
// the value is refused elsewhere if it matters. What is NOT given is `--force` that
// never appeared.
func boolFlag(stmt directive.Statement, key string) bool {
	return stmt.FlagState(key) != directive.FlagAbsent
}

// argsOf is the compact way to hand a directive onto an existing handler: the
// handler keeps its own argument names, which is what makes the single entry a
// re-rendering of the existing surface rather than a reimplementation of it.
func argsOf(pairs ...any) map[string]any {
	if len(pairs)%2 != 0 {
		panic("argsOf needs key/value pairs")
	}
	out := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			panic("argsOf keys are strings")
		}
		out[key] = pairs[i+1]
	}
	return out
}

// callTool invokes one of the existing handlers through the same admission path
// the MCP server uses, and renders its result as one slot.
//
// This is the whole trick behind the single entry: the directives do not
// reimplement anything. They translate a command line into the arguments the
// specialized handler already takes, and the reply is re-rendered as an HTTP
// response. So `run` is not a second implementation of shell_input + shell_key +
// shell_output; it is those calls, in order, with one reply.
// callTool invokes one operation and reports BOTH kinds of failure the same way:
// a transport error and an isError result.
//
// The isError half is not a convenience. Admission refusals (namespace isolation,
// a template's capability list) come back as an isError result, not as an error,
// and a directive that only inspected the error would report the refusal as a
// 204 success — the operation never ran, and the caller was told it did. Every
// directive routes through here for exactly that reason.
func (s *Server) callTool(ctx context.Context, tool string, args map[string]any) (*mcpgo.CallToolResult, *toolFailure) {
	h := s.handlerFor(tool)
	if h == nil {
		return nil, &toolFailure{code: "unknown_directive", msg: "no handler for " + tool}
	}
	req := mcpgo.CallToolRequest{}
	req.Params.Name = tool
	req.Params.Arguments = args
	// THE PAYLOAD OF AN INNER CALL IS NOT LOGGED. It is a copy of the line the outer
	// `term` call already recorded in full, and one of those copies can hold a value the
	// caller declared secret — which is exactly how the secret got into the log the first
	// time: the outer call logged `secrets=PW (values not logged)` and the shell_input it
	// made wrote the expanded line under `text`.
	res, err := withLogging(tool, h, s)(withInnerCall(ctx), req)
	if err != nil {
		return nil, &toolFailure{code: CodeOperationFailed, msg: err.Error()}
	}
	if res != nil && res.IsError {
		return res, failureFromResult(res)
	}
	return res, nil
}

// toolFailure is a handler failure translated into envelope terms.
type toolFailure struct {
	code string
	msg  string
}

// statusFor maps a tool error code onto the HTTP status that describes it.
//
// The mapping is by FAILURE KIND, not by tool: "the session is gone" is a 404 no
// matter which directive asked, and "the whitelist says no" is a 403 no matter
// which entrance was used.
func statusFor(code string) int {
	switch code {
	case CodeForbidden:
		return 403
	case CodeSessionNotFound, CodeShellNotFound, CodeForwardNotFound,
		CodeSSHConfigNotFound, CodeRuleNotFound, CodeHistoryNotFound,
		CodeReaderNotRegistered, CodeFileNotFound:
		return 404
	case CodeConflict, CodeSessionNotRunning:
		return 409
	case CodeInvalidArgument:
		return 400
	case CodeNotConfigured:
		return 422
	}
	return 500
}

// failureSlot renders a handler failure.
func failureSlot(f *toolFailure) envelope.Slot {
	if f == nil {
		return envelope.Slot{Code: 500, Body: "the directive failed without a reason"}
	}
	return envelope.Slot{
		Code: statusFor(f.code),
		Headers: map[string]string{
			"Error-Code": f.code,
			"Idempotent": "no",
		},
		Body: f.msg,
	}
}

// jsonSlot renders a JSON-producing handler's reply as one slot's body.
//
// It is used for the directives whose existing reply is a document (a session's
// info, a forward list). The generated-text directives (ps, help, ext list) do
// not go through here: their bodies are terminal columns, because a model reads
// `a1f3 running pipe go-build 12s` faster than the same facts as nested JSON.
func jsonSlot(code int, headers map[string]string, m map[string]any) envelope.Slot {
	body, _ := jsonMarshalIndent(m)
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Content-Type"] = "application/json"
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// fmtBytes renders a byte count for a generated body.
func fmtBytes(n int) string { return fmt.Sprintf("%d B", n) }

// termDescription is the whole model-facing description of the tool.
//
// It deliberately names no directive: the semantics live in `cmd`'s syntax and in
// `help`/`learn_mode`, not in the tool's name or blurb. That is the difference
// between a tool surface that can grow and one that cannot — every directive this
// text listed would be a line to edit the day the command space gains one.
const termDescription = "Run one terminal line on this instance and read its result: a session's bytes, a file, a listing, a directive's own output. No cmd = bearings (what runs, what is installed, the syntax); ls ext = pinned capability (MCP servers, skills trees). Window parameters bound how much comes back, never what the command does."
