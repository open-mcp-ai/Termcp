package mcp

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
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
	TimeoutSeconds  float64
	BytesLimit      int
	LinesLimit      int
	TotalBytesLimit int
	OnTimeOverflow  string
	OnBytesOverflow string
	OnLinesOverflow string
	PipeTo          string
	LearnMode       bool

	// StructuredContent also sends the reply as the JSON projection, on top of the
	// rendered text. It is off by default because the two carry the SAME bytes and
	// the text is the copy a caller usually reads, so the JSON twin is a duplicate
	// paid for on every call. See envelope.Reply.StructuredContent.
	StructuredContent bool

	// AllowEscapes permits unquoted control bytes and raw escape sequences in cmd.
	//
	// OFF by default, because a raw carriage return in a payload does not print a
	// character — the terminal treats it as "submit this line", so one call can run
	// two commands while answering 200 for one. The same is true of Ctrl-C, Ctrl-D
	// and Ctrl-Z. A caller that genuinely needs those bytes has two honest ways to
	// say so: this flag, or `vars` (which carries the value as data, never as syntax).
	AllowEscapes bool

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
	// syntax the caller did not write.
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
	// The total cap is a property of the REPLY, not of one slot: ten slots each
	// within their own byte limit still add up to a context nobody budgeted for.
	return envelope.Reply{Slots: t.applyTotalCap(t.slots), StructuredContent: win.StructuredContent}
}

// executeTerm parses cmd and runs it, accumulating the reply.
func (s *Server) executeTerm(ctx context.Context, cmd string, win termArgs) termReply {
	// Raw control bytes in the command text are refused unless the caller said
	// otherwise. A carriage return is the case that matters: the shell parses
	// `echo a\rNoSuch` as one command of four words, but the terminal treats the \r
	// as "submit this line" — so one call runs two commands and answers 200 for one.
	// The same is true of Ctrl-C, Ctrl-D, Ctrl-Z, NUL and ESC.
	//
	// The refusal names BOTH ways out, because either is a real choice: turn the
	// gate off (`allow_escapes`), or pass the data so it never becomes syntax
	// (`env`). Refusing without saying what to do instead is the half-measure that
	// leaves a caller stuck.
	//
	// THE ARGUMENTS ARE SCANNED TOO, and that is not redundant: a value reaches the
	// terminal through `args` or `vars` without appearing in cmd at all, so scanning
	// cmd alone let `args: ["a\x03b"]` through. The observed result was worse than a
	// missing check — the terminal read the byte as an ACTION, echoed `^C`, and the
	// shell dropped into its `quote>` continuation prompt, with the reply still 200.
	// Quoting does not help: a control byte inside single quotes is still a control
	// byte to the terminal, which is why the gate has to look at the values.
	if !win.AllowEscapes {
		where, b, name, found := "", byte(0), "", false
		if b, name, found = directive.DetectRawControl(cmd); found {
			where = "cmd"
		}
		if !found {
			for i, a := range win.Args {
				if b, name, found = directive.DetectRawControl(a); found {
					where = fmt.Sprintf("args[%d]", i)
					break
				}
			}
		}
		if !found {
			for k, v := range win.Vars {
				if b, name, found = directive.DetectRawControl(v); found {
					where = "vars[" + k + "]"
					break
				}
			}
		}
		if found {
			return termReply{win: win, slots: []envelope.Slot{{
				Code: 400,
				Headers: map[string]string{
					"Error-Code": "raw_control_byte",
				},
				Body: fmt.Sprintf("%s contains an unquoted %s, which the terminal acts on instead of printing.\n"+
					"Either pass the data through the `vars` argument (it is substituted as a quoted value, so it never becomes syntax:\n"+
					"  vars: {\"NAME\": \"<value>\"}, cmd: \"run <handle> echo $NAME\"),\n"+
					"or set allow_escapes: true if you really mean to send byte 0x%02x.", where, name, b),
			}}}
		}
	}
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
			return s.runStatements(ctx, []directive.Statement{stmt}, win)
		}
		if len(cycle) > 0 {
			return termReply{win: win, slots: []envelope.Slot{aliasCycleSlot(directive.Statement{Path: cmd}, cycle)}}
		}
		return termReply{win: win, slots: []envelope.Slot{s.parseErrorSlot(err)}}
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
	out := termReply{win: win, seenUsage: map[string]bool{}}
	for _, stmt := range stmts {
		expanded, cycle := aliases.Expand(stmt)
		if cycle != nil {
			// The chain came back to where it started, so there is no directive to
			// dispatch. Saying "unknown directive" here would blame the wrong thing.
			out.slots = append(out.slots, aliasCycleSlot(stmt, cycle))
			continue
		}
		slots := s.runDirective(ctx, expanded, win)
		// learn_mode is a property of the reply, so it is attached here rather
		// than in each directive: generated replies (ps, help, detect, …) carry
		// it exactly like streamed ones. withUsage de-duplicates per directive.
		if win.LearnMode && len(slots) > 0 {
			if slots[0].Headers == nil {
				slots[0].Headers = map[string]string{}
			}
			s.addUsage(&out, expanded, slots[0].Headers)
		}
		out.slots = append(out.slots, slots...)
	}
	if win.PipeTo != "" {
		out.slots = s.forwardSlotsToStdin(ctx, win.PipeTo, out.slots)
	}
	return out
}

// forwardSlotsToStdin implements pipe_to: the reply's bytes go into another
// handle's stdin, and stay out of the reply body.
//
// It is the one parameter in the schema that made a promise nothing kept — the
// argument was parsed and then read by nobody, so a caller that asked for a pipe
// got its bytes back inline instead. The rule is one call, one target: whatever
// this call produced, in order, into one channel. Type-and-enter is deliberately
// not implied (the bytes are written as they are), because a pipeline stage takes
// raw input, not a shell line.
//
// The forwarded bytes leave the reply body — that is the point of asking — and a
// target that does not resolve adds its own failing slot, because silently
// returning the bytes anyway would tell the caller the pipe worked.
func (s *Server) forwardSlotsToStdin(ctx context.Context, target string, slots []envelope.Slot) []envelope.Slot {
	var body strings.Builder
	for _, slot := range slots {
		if slot.Code < 400 {
			body.WriteString(slot.Body)
		}
	}
	if body.Len() == 0 {
		return slots
	}
	if _, fail := s.callTool(ctx, "shell_input", argsOf("shell_id", target, "text", body.String())); fail != nil {
		return append(slots, failureSlot(fail))
	}
	for i := range slots {
		if slots[i].Code < 400 {
			slots[i].Body = ""
		}
	}
	return slots
}

// parseErrorSlot renders a directive-level failure as one slot, so a bad
// statement can sit beside a good one without taking the whole call down.
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
		// The failing statement followed a directive whose payload goes to a
		// terminal, so the most likely cause is a `;` the caller meant as SHELL
		// sequencing. Naming the fix beats naming the directive table: the caller
		// did not misspell a directive, they wrote a shell command line in a slot
		// that holds one directive.
		body += fmt.Sprintf("\nHINT: %q follows a `%s`, whose payload is ONE line. A `;` in cmd separates\n"+
			"termcp statements, not shell commands — so if you meant several SHELL commands, pass\n"+
			"them as args instead:\n"+
			"  cmd: \"%s <handle> $1\"   args: [\"pwd; whoami\"]\n"+
			"Or keep it in the payload with &&: %s <handle> pwd && whoami",
			derr.Op, derr.AfterPayload, derr.AfterPayload, derr.AfterPayload)
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
}

// result assembles the tool result from a reply: the text projection, the
// structured twin, and the isError flag taken from the codes.
//
// The total byte cap has already been applied by Term, because it is a property
// of the reply rather than of one slot.
//
// In the structured-only mode the text content is omitted rather than sent empty:
// a JSON client reads structuredContent and one empty string is noise, while a
// client that renders text has already asked for something the caller declined to
// pay for. The tool result stays valid either way — content is allowed to be
// empty, and every field the caller needs is in structuredContent.
func toolResult(reply envelope.Reply) *mcpgo.CallToolResult {
	result := mcpgo.NewToolResultText(reply.Text())
	result.IsError = reply.IsError()
	/* The JSON twin is built only when it was asked for. Structured() renders every
	   slot a second time, and on a large reply that render is the cost the default
	   exists to avoid — cheaper to skip than to build and drop. */
	if reply.StructuredContent {
		structured := reply.Structured()
		if len(structured) == 1 {
			result.StructuredContent = structured[0]
		} else {
			// structuredContent must be a JSON object. A batch is several slots, so
			// it is wrapped rather than sent as a bare array, which clients reject
			// against the object-shaped result schema.
			result.StructuredContent = map[string]any{"slots": structured}
		}
	}
	return result
}

// applyTotalCap truncates the tail of the reply to the caller's total budget.
//
// A slot that crosses the cap is cut to exactly what fits and reports 206 with a
// Content-Range describing what it DID return; every slot after it is reduced to
// its headers. That is the same "fold, never drop" rule the per-slot windows use:
// the bytes are still in the log, and the range says where to continue.
func (t termReply) applyTotalCap(slots []envelope.Slot) []envelope.Slot {
	total := 0
	for i, slot := range slots {
		if total >= t.win.TotalBytesLimit {
			slot.Body = ""
			if slot.Code < 400 {
				slot.Code = 206
			}
			slots[i] = slot
			continue
		}
		remaining := t.win.TotalBytesLimit - total
		if len(slot.Body) > remaining {
			// Cut, don't just truncate: Content-Range has to describe the bytes the
			// body still carries, or the caller's next read starts past the gap.
			slot = slot.Cut(remaining)
			if slot.Code < 400 {
				slot.Code = 206
			}
		}
		total += len(slot.Body)
		slots[i] = slot
	}
	return slots
}

// termSlotResult wraps one slot as a tool result.
func termSlotResult(slot envelope.Slot) *mcpgo.CallToolResult {
	return toolResult(envelope.Reply{Slots: []envelope.Slot{slot}})
}

// defaultTermArgs is the window used when a reply is built outside a call (an
// internal error path); it matches the schema's defaults so the two cannot write
// different obligations.
func defaultTermArgs() termArgs {
	return termArgs{
		TimeoutSeconds:  defaultTimeoutSeconds,
		BytesLimit:      defaultBytesLimit,
		LinesLimit:      defaultLinesLimit,
		TotalBytesLimit: defaultTotalBytesLimit,
		OnTimeOverflow:  "truncate",
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
	res, err := withLogging(tool, h, s)(ctx, req)
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
const termDescription = "Run one terminal line on this instance and read its result: a session's bytes, a file, a listing, a directive's own output. Pass no cmd to get your bearings (what is running, what is installed, the syntax). The window parameters bound how much comes back, never what the command does."
