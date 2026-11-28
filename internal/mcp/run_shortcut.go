package mcp

// ONE SHELL LINE, NO DSL (issue #319).
//
// The `term` tool's command language is a DSL that imitates a shell but is not one: `run
// <channel> $1` with a placeholder and an `args` array, `read` flags on one verb and not
// another, a payload that is a JSON frame on one address and a command line on another.
// That shape is right for composing statements, and it is the wrong shape for the single
// most common request a model has: "run this line on that machine".
//
// The failure mode is measurable rather than aesthetic: a caller writes shell syntax into
// `cmd`, the DSL splits it, and the reply explains a grammar the caller never meant to
// use. Six classes of ambiguity come from that surface (placeholder conflicts, flag
// capture, polymorphic arguments, reserved words, implicit semantics, switch ambiguity) and
// the fix for all six is the same one: DO NOT PARSE. `run` takes one line and hands it to
// a shell verbatim — no splitting, no placeholders, no variable expansion, no flags. What
// the line contains is the shell's business.
//
// TWO FORMS, because "one line" answers two different questions:
//
//	{run: "df -h", session: "6a29"}    one-shot: a NEW pipe process, exit_code + stdout
//	{run: "cd /srv && ls", shell: "6a29:1"}  type into an EXISTING interactive shell
//
// The first can report an exit code because it owns a process that exits; the second
// cannot, because a terminal never tells the program outside it that a command inside it
// finished — that is why it is a separate spelling instead of a flag. The second exists
// for state: `cd`, `export`, a REPL, anything that only makes sense in a shell that
// remembers the previous line.
//
// NEITHER FORM REACHES THE PARSER. `cmd` is untouched by this file, so every statement
// that works today still works, and the two spellings cannot be mixed: a call carrying
// both `cmd` and `run` is refused, because two command lines are two different requests.

import (
	"context"
	"fmt"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/mcp"
	"github.com/open-mcp-ai/termcp/term/directive"
	"github.com/open-mcp-ai/termcp/term/envelope"
)

// shortcutRefusal is a 400 that names the shape, used for every way the pair can be wrong.
func shortcutRefusal(msg string) []envelope.Slot {
	return []envelope.Slot{badRequestNoPath(msg)}
}

// runShortcut executes a `run` call, or reports that this call is not one.
//
// The bool says "this call was a shortcut and the reply is final": false means the call
// carries no `run` argument and belongs to the DSL path, which is every call that existed
// before this feature.
func (s *Server) runShortcut(ctx context.Context, args map[string]any, win termArgs) ([]envelope.Slot, bool) {
	raw, present := args["run"]
	if !present {
		// A TARGET WITHOUT A LINE IS NOT A DSL CALL WITH A TYPO, it is a caller who
		// reached for the shortcut and stopped one key short. Ignoring it silently would
		// run `cmd` (or return the guide) and leave the caller reading a reply about a
		// request they did not make.
		for _, orphan := range []string{"session", "shell"} {
			if v, ok := args[orphan]; ok && v != nil && v != "" {
				return shortcutRefusal(orphan + " only means something with `run`: it names where " +
					"the line executes. Send `run` as well (`{\"run\": \"df -h\", \"" + orphan + "\": \"…\"}`), " +
					"or use cmd for the DSL, which addresses its target inside the line (`run <handle> $1`)."), true
			}
		}
		return nil, false
	}
	line, isString := raw.(string)
	if !isString {
		return shortcutRefusal("run takes ONE command line as a string, got " + typeNameOf(raw) +
			". It is handed to the shell verbatim — no placeholders, no args array, no flags."), true
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return shortcutRefusal("run is empty. Send the line to execute, or use cmd for the DSL " +
			"(an empty cmd returns the guide; an empty run would run nothing, which is not the same request)."), true
	}
	if _, hasCmd := args["cmd"]; hasCmd && args["cmd"] != nil {
		return shortcutRefusal("cmd and run are two different command lines and only one can run: " +
			"cmd is the DSL (`run <channel> $1`, `read …`), run is one shell line handed over verbatim. Send one."), true
	}

	target, shellMode, derr := shortcutTarget(args)
	if derr != "" {
		return shortcutRefusal(derr), true
	}
	if shellMode {
		return s.runInShell(ctx, target, line, win), true
	}
	return []envelope.Slot{s.runOnce(ctx, target, line, win)}, true
}

// shortcutTarget reads the pair that says where the line runs.
//
// EXACTLY ONE OF session/shell, and the refusal says which one to pick by what the caller
// wants: a pipe for a line that finishes, an interactive shell for a line that has to
// remember the previous one. That is the whole difference, so it is the sentence a wrong
// call gets.
func shortcutTarget(args map[string]any) (target string, shellMode bool, refusal string) {
	sess := strings.TrimSpace(getString(args, "session", ""))
	shell := strings.TrimSpace(getString(args, "shell", ""))
	switch {
	case sess != "" && shell != "":
		return "", false, "session and shell are alternatives: `session` runs the line as a one-shot pipe " +
			"(it exits, so you get exit_code), `shell` types it into an existing interactive shell (state is kept, " +
			"there is no exit code). Send one of them."
	case sess == "" && shell == "":
		return "", false, "run needs a target: `session` for a one-shot pipe (exit_code + stdout), " +
			"or `shell` for an existing interactive shell (keeps cd/export, no exit code). " +
			"`ps` lists the handles; `open <session> --argv <cmd>` makes one."
	}
	if shell != "" {
		return shell, true, ""
	}
	return sess, false, ""
}

// runInShell types the line into an EXISTING channel, the way `run <channel> $1` does.
//
// IT REUSES THE DIRECTIVE instead of re-implementing the write: the statement below is
// exactly what the parser would have produced for `run <handle> $1` with args=[line], so
// the cursor, the wait, the completion markers, `Still-Running` and the echo handling are
// the ones that already exist — and a fix to any of them lands on both spellings.
func (s *Server) runInShell(ctx context.Context, handle, line string, win termArgs) []envelope.Slot {
	stmt := directive.Statement{
		Path:    "run " + handle + " " + line,
		Op:      "run",
		Target:  handle,
		Payload: line,
	}
	return s.directiveRun(ctx, stmt, win)
}

// runOnce runs the line as a ONE-SHOT PIPE on the target's connection and reports what the
// process did.
//
// A NEW PROCESS PER CALL is what makes `exit_code` possible at all: a channel already
// running a program cannot take a second command, and an interactive shell has no exit to
// observe. So the target names the CONNECTION — a session id, a channel handle (whose
// parent session is used), or `use:<host>` for a host with no session — and the line runs
// there, in its own channel, exactly once.
func (s *Server) runOnce(ctx context.Context, target, line string, win termArgs) envelope.Slot {
	// A `use:<host>` TARGET TAKES THE EXISTING ROAD, which already knows how to bring a
	// container up, gate the call against the profile's approval mode, and report the host
	// and channel. Two implementations of "run one line on a host" is how they start
	// disagreeing about what a failure means.
	if host, isUse := s.parseUseHost(target); isUse {
		stmt := directive.Statement{Path: "run " + target + " " + line, Op: "run",
			Target: "use:" + host, Payload: line}
		return s.runUse(ctx, stmt, win, host)
	}

	sess, bad := s.sessionForShortcut(target)
	if bad != nil {
		return s.slotFromFailure(ctx, directive.Statement{Path: "run " + target}, failureFromResult(bad))
	}
	// THE SAME CAPABILITY THE WRITE WILL NEED, asked before a process exists: a session
	// whose whitelist omits shell.input must not get a channel started on its behalf.
	req := mcp.CallToolRequest{}
	req.Params.Name = "shell_input"
	req.Params.Arguments = argsOf("shell_id", target)
	if bad, _ := s.checkAdmission(ctx, "shell_input", req); bad != nil {
		return s.slotFromFailure(ctx, directive.Statement{Path: "run " + target}, failureFromResult(bad))
	}

	cs, err := sess.CreateChildShellLine(line, false, 0, 0, "")
	if err != nil {
		code := CodeOperationFailed
		if err == session.ErrClosing {
			code = CodeConflict
		}
		return s.slotFromFailure(ctx, directive.Statement{Path: "run " + target}, &toolFailure{
			code: code, msg: "start the one-shot channel: " + err.Error()})
	}
	// The reply is produced by the same code a channel run reaches, and it needs the
	// handle rather than the session: the bytes, the exit state and the window all belong
	// to this channel.
	stmt := directive.Statement{Path: "run " + target + " " + line, Op: "run",
		Target: cs.ID, Payload: line}
	slot := s.finishRun(ctx, stmt, win, 0)
	if slot.Headers == nil {
		slot.Headers = map[string]string{}
	}
	// THE HANDLE IS NAMED, because a window that expires leaves a process still running
	// and reading or closing it takes an address. Same two headers the `use:` road adds,
	// for the same reason.
	// AN ADDRESS `read` ACCEPTS (#303): the shell id is the registry's key, but a caller
	// holds handles, and `read <session>:<index>` is the one that names this channel.
	slot.Headers["Channel"] = fmt.Sprintf("%s:%d", sess.ID, cs.Info().Index)
	if sess.ID != "" {
		slot.Headers["Session"] = sess.ID
	}
	return slot
}

// sessionForShortcut resolves a session id, a channel handle or a session name to the
// connection a one-shot channel is opened on.
//
// A HANDLE NAMES ITS PARENT, not a second thing to learn: `run` with `session: "6a29:1"`
// is a caller that already holds a channel and should not have to strip the `:1` by hand.
func (s *Server) sessionForShortcut(target string) (*session.Session, *mcp.CallToolResult) {
	if s.sessMgr == nil {
		return nil, toolError(CodeNotConfigured, "%s", "no session manager")
	}
	// Full id, `--name`, or unique prefix: same resolver the DSL uses (session_ref.go), so
	// `session: "build"` works as well as `session: "6a29"`.
	if sess, err := s.resolveSessionRef(target); err == nil && sess != nil {
		return sess, nil
	}
	if cs, bad := s.requireShell(target); bad == nil && cs != nil {
		if sess := s.sessMgr.Get(cs.ParentSessionID()); sess != nil {
			return sess, nil
		}
	}
	// The refusal names both spellings, because "not found" about a handle the caller just
	// copied is the one message that leaves them guessing which part is wrong.
	return nil, toolError(CodeSessionNotFound, "%s", "no session or channel named "+target+
		". `ps` lists the handles (a channel is `<session>:<n>`), and `use:<host>` runs on a saved profile with no session at all.")
}
