package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/transcript"
)

// pipe and pty — the two primitives, as directives.
//
// The mode is the directive, not a parameter: there is no `--mode` flag, no
// default, no `open`. The reason is that the difference is a TTY, and a TTY
// cannot be added after the fact. Choosing wrong is unfixable: a channel that
// should have been a pty and was made a pipe can never run a TUI or a password
// prompt, and one that should have been a pipe and was made a pty has echo and
// control sequences permanently mixed into its read cursor. Making the caller say
// which one it means is the only honest option.
//
// One primitive covers both cases the old surface split in two: a target that is
// an ENTRY starts a connection plus its first channel, and a target that is a
// SESSION HANDLE adds a channel to the connection that already exists. That is
// exactly what session_start and shell_open did; this does not change the
// behaviour, it changes how you ask for it.

// directiveOpen implements `pipe <target>` and `pty <target>`.
func (s *Server) directiveOpen(ctx context.Context, stmt directive.Statement, win termArgs, pty bool) []envelope.Slot {
	mode := "pipe"
	if pty {
		mode = "pty"
	}
	if stmt.Target == "internal" && s.NoInternal {
		return []envelope.Slot{{Code: 409, Headers: map[string]string{"Path": stmt.Path, "Error-Code": CodeConflict},
			Body: "the internal profile is disabled on this instance (it was started with --no-internal)"}}
	}
	capID := capability.ShellOpen
	if _, bad := s.requireSession(stmt.Target); bad != nil {
		// The target is not an existing session, so this call creates one.
		capID = capability.SessionStart
	}
	if slot, refused := s.refuseAction(ctx, stmt, capID); refused {
		return []envelope.Slot{slot}
	}

	argv, _ := stmt.Flag("argv")
	args := argsOf("mode", mode)
	if argv != "" {
		cmd, argvParts, err := directive.SplitArgv(argv)
		if err != nil {
			usage := ""
			if op, ok := directive.Lookup(stmt.Op); ok {
				usage = "\n" + op.Usage
			}
			return []envelope.Slot{badRequest(stmt, err.Error()+usage)}
		}
		if cmd == "" {
			return []envelope.Slot{badRequest(stmt, "--argv needs a command, e.g. pipe internal --argv sleep 5")}
		}
		args["command"] = cmd
		if len(argvParts) > 0 {
			list := make([]any, 0, len(argvParts))
			for _, p := range argvParts {
				list = append(list, p)
			}
			args["args"] = list
		}
	}
	if name, ok := stmt.Flag("name"); ok {
		// AN EMPTY LABEL IS NOT A LABEL, and accepting it would be the silent kind of
		// help: a caller that computed a name and got "" opens an unnamed channel and
		// believes it is labelled (issue #188). Leaving the flag off is how a caller says
		// "no label".
		if strings.TrimSpace(name) == "" {
			return []envelope.Slot{badRequest(stmt,
				"`--name` was given an empty value. A channel either has a label or it does not — "+
					"pass a name, or leave the flag off.")}
		}
		args["name"] = name
	}
	if onExit, ok := stmt.Flag("on-exit"); ok {
		args["on_exit"] = onExit
	}
	if size, ok := stmt.Flag("size"); ok {
		rows, cols := parseSize(size)
		if rows > 0 {
			args["rows"] = float64(rows)
		}
		if cols > 0 {
			args["cols"] = float64(cols)
		}
	}

	// An existing session takes a new channel on its connection; anything else
	// starts a connection. The distinction is the caller's target, not a flag.
	var res *mcpgo.CallToolResult
	var fail *toolFailure
	if capID == capability.SessionStart {
		args["ssh_config"] = stmt.Target
		res, fail = s.callTool(ctx, "session_start", args)
	} else {
		args["session_id"] = stmt.Target
		res, fail = s.callTool(ctx, "shell_open", args)
	}
	if fail != nil {
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, fail)}
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return []envelope.Slot{s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})}
	}
	headers := map[string]string{"Path": stmt.Path}
	// The handle is NEWS here — the whole reason this call exists — so it is the
	// one reply that always carries it.
	handle := shortHandle(doc)
	headers["Handle"] = handle
	// A PTY SHELL EXISTS BEFORE IT IS READY, AND TYPING INTO IT BEFORE THEN IS LOST
	// (issue #156). The handle is created as soon as the process is; the line editor
	// inside it — PSReadLine, readline, zle — is still starting up, and input that
	// arrives before it takes over the terminal is DISCARDED rather than buffered.
	// Nothing reports that: `run` answers 200 and the command simply never happens, on
	// every channel of that session, forever.
	//
	// So a pty open waits for the shell to speak before answering, and the wait is a bound
	// rather than a claim: a shell that never prints anything (an empty PS1) is handed
	// over when the budget runs out, because a silent shell is not a broken one.
	//
	// WAITING FOR THE SHELL TO GO QUIET WAS TRIED AND REJECTED. It reads like the better
	// signal — a shell that has stopped talking has finished its startup files — and it
	// is not: measured on a themed PowerShell, startup pauses for longer than any quiet
	// threshold short enough to afford (a second wait after the open returned still found
	// the shell talking, 505ms later), while every open paid 1.8s instead of 0.1s. It made
	// the terminal slower for everyone and left the same gap, so the honest state is that
	// this is a heuristic with a ceiling — see issue #162, S1, which is the part a human
	// has to decide: the only reliable readiness probe is a round trip, and a round trip
	// writes a line into the caller's transcript.
	//
	// PIPE opens do not wait: their bytes go into the process's own stdin, which the OS
	// buffers, and `--argv sleep 30` prints nothing for thirty seconds by design.
	if mode == "pty" {
		s.awaitShellReady(ctx, handle, shellReadyBudget)
	}
	return []envelope.Slot{{Code: 200, Headers: headers, Body: createdBody(doc, mode)}}
}

// shellReadyBudget bounds how long an open waits for the shell to say something.
//
// Generous enough for a profile that draws a prompt (a themed PowerShell on a cold
// machine is the slow case observed), short enough that a caller notices nothing when the
// shell is silent.
//
// A VAR RATHER THAN A CONST, because the tests shorten it. The wait's result is one the
// caller ignores — `open` hands over the handle whether or not the shell spoke, and a
// shell too slow to answer inside the budget is still the caller's shell — so no test
// needs the real three seconds to prove anything. It was paying them once per open that
// never speaks, which came to 87s of this package's 240s runtime. The default is what
// ships; only the suite changes it.
var shellReadyBudget = 3 * time.Second

// awaitShellReady waits until the channel has produced its first byte, or the budget runs
// out. It returns whether the shell spoke, which the caller currently ignores: the handle
// is worth handing over either way, and a shell too slow to answer inside the budget is
// still the caller's shell.
func (s *Server) awaitShellReady(ctx context.Context, handle string, budget time.Duration) bool {
	src, err := transcript.Resolve(s.sessMgr, s.msgMgr, handle)
	if err != nil || src.Live == nil {
		return false
	}
	deadline := time.Now().Add(budget)
	for {
		// THE SOURCE'S LENGTH, so the wait sees the byte as soon as the RECORD does.
		// The buffer is a cache of the log and is behind it for a few microseconds per
		// chunk; polling the cache made `open` occasionally answer "the shell has not
		// spoken" about a shell whose output a `read` would have returned. And the
		// number this waits on has to be the one `Content-Range` reports, or the two
		// answers describe the same stream differently.
		if n, err := src.Len(); err == nil && n > 0 {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		// TEN MILLISECONDS is a compromise on purpose: `ReadTerminalStream` would block
		// until the bytes arrive, but it reads through a registered reader cursor, and
		// creating one here would be state that exists for a wait. Polling a counter has
		// no such cost.
		time.Sleep(10 * time.Millisecond)
	}
}

// createdBody renders the two facts a caller needs about a new channel: its
// handle and whether stdout/stderr are separable. The latter is news for a pipe
// and false for a pty, so it is stated once, here, instead of being learned by
// reading a stream that never separates them.
func createdBody(doc map[string]any, mode string) string {
	handle := shortHandle(doc)
	// THE HANDLE IS TWO ADDRESSES IN ONE WORD, and the first reply is where that has
	// to be said: `a1f3:1` looks like one opaque id, so a caller carries it into
	// every command — until a verb wants the session (`rm` deletes a session, not a
	// channel) and answers "that is a channel". Spelling the parts once, here, is
	// what makes the difference visible at the moment the word is new.
	split := ""
	if i := strings.LastIndex(handle, ":"); i > 0 {
		split = fmt.Sprintf(" (session %s, channel %s)", handle[:i], handle[i+1:])
	}
	// THE LABEL IS PRINTED NEXT TO THE ADDRESS IT LABELS, and only when the caller chose
	// one. A caller that asked for a name gets to see that the server took it, and one
	// that did not gets the reply it always got — an empty column teaches nothing. The
	// generated default (`shell-<id>`) is not a label the caller chose, so it is not
	// shown as one (issue #188).
	label := ""
	if name := getString(doc, "name", ""); name != "" && !strings.HasPrefix(name, "shell-") {
		label = " [" + name + "]"
	}
	if mode == "pty" {
		return fmt.Sprintf("%s pty%s%s\n", handle, label, split)
	}
	return fmt.Sprintf("%s pipe (stderr separable: --stream e)%s%s\n", handle, label, split)
}

// shortHandle renders `<session>:N` from a handler reply.
//
// N COMES FROM THE REPLY OR IS NOT INVENTED. The positional form is a shorthand for a
// locator the caller will hand back, so a number made up here is not cosmetic: it is a
// promise the resolver does not have to keep. An extension reply carries
// session_id + shell_id and used to carry no index, so every `open ext` advertised
// `<session>:1` — right by accident for the first channel, and a guaranteed 404 for the
// one the reconciler had rebuilt as `:2` (issue #211).
//
// A bare shell id is a locator too, and every directive resolves it, so its absence costs
// the caller a few characters and costs the reply nothing. That is the trade this makes
// in one direction only.
func shortHandle(doc map[string]any) string {
	shellID := getString(doc, "shell_id", "")
	sessionID := getString(doc, "session_id", "")
	if sessionID == "" {
		return shellID
	}
	if _, known := doc["index"]; !known {
		return shellID
	}
	index := int(getFloatAny(doc, "index", 1))
	if index <= 0 {
		return shellID
	}
	return handleOf(sessionID, index)
}

// parseSize reads the `--size 24x80` form: rows x cols, the order a terminal
// reports them.
func parseSize(raw string) (rows, cols int) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(raw)), "x", 2)
	if len(parts) != 2 {
		return 0, 0
	}
	fmt.Sscanf(parts[0], "%d", &rows)
	fmt.Sscanf(parts[1], "%d", &cols)
	return rows, cols
}
