package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Generated replies.
//
// `ps`, `info`, `help` and the extension/profile listings are NOT byte streams:
// there is nothing to continue reading, so they carry no Content-Range and their
// body is terminal text — one line per object, columns, `key: value`. That is not
// cosmetic. A model reads `a1f3 running pipe go-build 12s` faster than the same
// facts nested in JSON, `grep`/`cut` work on it, and the handle in the first column
// can be copied straight into the next command. The format requirement that
// matters is that last one: every handle in `ps` must be usable verbatim as the
// target of another directive.

// directivePS lists what is running, one line per channel.
func (s *Server) directivePS(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	headers := map[string]string{"Path": stmt.Path}

	sessions := s.sessMgr.ListAll()
	callerNS := callerNamespace(ctx)
	if !boolFlag(stmt, "all") {
		sessions = listableSessions(sessions, callerNS)
	}

	var b strings.Builder
	for _, sess := range sessions {
		live := s.sessMgr.Get(sess.ID)
		if live == nil {
			continue
		}
		shells := live.ListChildShells()
		if len(shells) == 0 {
			b.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\n",
				sess.ID, sess.Status, dash(sess.SSHConfig), ageOf(sess.CreatedAt)))
			continue
		}
		for _, sh := range shells {
			if !boolFlag(stmt, "shells") && len(shells) > 1 && sh.Index > 1 {
				// Without --shells, one line per session: the primary channel names
				// the session, and the extra channels are one flag away. Listing all
				// of them by default would make a fleet's ps unreadable.
				continue
			}
			b.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\t%s\n",
				handleOf(sess.ID, sh.Index), sh.Status, dash(string(sh.Mode)),
				dash(sh.Name), ageOf(sh.CreatedAt)))
		}
	}
	body := b.String()
	if body == "" {
		body = "(nothing is running)\n"
	}
	// A generated reply is capped by the same window as a byte stream: a listing is
	// exactly as capable of flooding a context as a log is, and the directives with
	// no stream to continue are the ones with no other protection.
	body, code := capGenerated(body, win)
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// directiveInfo describes one session or channel as `key: value` lines.
func (s *Server) directiveInfo(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	sess, bad := s.requireSession(stmt.Target)
	if bad != nil {
		return s.slotFromFailure(ctx, stmt, failureFromResult(bad))
	}
	info := sess.Info()
	var b strings.Builder
	writeKV(&b, "id", info.ID)
	writeKV(&b, "name", dash(info.Name))
	writeKV(&b, "status", string(info.Status))
	writeKV(&b, "ssh_config", dash(info.SSHConfig))
	writeKV(&b, "endpoint", dash(info.SSHEndpoint))
	if info.ExitCode != nil {
		writeKV(&b, "exit_code", fmt.Sprint(*info.ExitCode))
	}
	writeKV(&b, "created", ageOf(info.CreatedAt))
	writeKV(&b, "approval", approvalSummaryText(info))
	for _, sh := range sess.ListChildShells() {
		writeKV(&b, "shell", fmt.Sprintf("%s %s %s %s",
			handleOf(info.ID, sh.Index), sh.Status, dash(string(sh.Mode)), dash(sh.Name)))
	}
	body, code := capGenerated(b.String(), win)
	return envelope.Slot{Code: code, Headers: map[string]string{"Path": stmt.Path}, Body: body}
}

// directiveResize changes a pty's size.
//
// Both operands are parsed HERE and a bad one is a 400. Before, `resize abc xyz`
// reached the handler as rows=0/cols=0 and came back 204: the typo was converted
// into a resize by the handler's own 24x80 default, so the caller was told a resize
// happened that never did.
//
// Zero and negative sizes are refused for the same reason: a terminal with zero rows
// is not a thing a caller can want, and passing it through makes every later
// dimension report meaningless.
func (s *Server) directiveResize(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	if len(stmt.Args) < 2 {
		return badRequest(stmt, "resize needs rows and cols: `resize a1f3:1 40 120`")
	}
	rows, err := flagNumber(stmt.Args[0])
	if err != nil {
		return numericFlagProblem(stmt, "rows", err)
	}
	cols, err := flagNumber(stmt.Args[1])
	if err != nil {
		return numericFlagProblem(stmt, "cols", err)
	}
	if rows < 1 || cols < 1 {
		return badRequest(stmt, "rows and cols must both be >= 1, got "+stmt.Args[0]+" and "+stmt.Args[1])
	}
	res, fail := s.callTool(ctx, "shell_resize", argsOf(
		"shell_id", stmt.Target,
		"rows", rows,
		"cols", cols,
	))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	_ = res
	return noContent(stmt)
}

// directiveClose closes a channel or a whole session.
//
// TWO DIFFERENT ACTIONS SHARE ONE SUCCESS CODE, which is what made issue #132 a
// silent failure: `close <handle>` closes one channel, `close <session> --force`
// terminates the container. Both answer 204, so the caller cannot tell which one
// happened — and the old routing always picked the first, because requireShell
// falls back to a session's primary shell when handed a session id. The
// `session_terminate` branch was therefore unreachable, and `--force` was dropped
// on the floor.
//
// On an internal session that was a total no-op: closing an internal primary shell
// is deliberately a no-op (the loopback process outlives a browser tab), so
// `close <session> --force` returned 204 while the session kept running.
//
// The fix is to route on INTENT before resolving anything, because resolution
// cannot tell the two apart (both spellings resolve):
//
//	the caller asked for a session   --force or --grace, or a target that names a
//	                                session and not one of its channels
//	otherwise                       one channel: shell_close
//
// Routing on intent FIRST is the point. Asking "is this resolvable as a shell?"
// first is exactly the question that answers yes for a session id and buries the
// caller's --force.
func (s *Server) directiveClose(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	// --force and --grace are session-only vocabulary: shell_close accepts neither,
	// so a caller who wrote one is terminating a container, not closing a channel.
	//
	// --grace counts even without --force: it is the deadline for a termination
	// (SIGTERM, then kill), which has no meaning for closing a channel.
	wantsSession := boolFlag(stmt, "force") || stmt.FlagState("grace") != directive.FlagAbsent

	// A target that names a session and NOT one of its channels is the container
	// too. GetChildShell answers the channel question directly, and the `sid:N`
	// form is excluded because it names a channel by construction — a caller who
	// copied a handle out of `ps` asked about that channel.
	if !wantsSession {
		h := locator.SplitHandle(stmt.Target)
		wantsSession = h.Index == 0 && s.sessMgr.Get(h.SessionID) != nil
	}

	if wantsSession {
		args := argsOf("session_id", stmt.Target)
		if boolFlag(stmt, "force") {
			args["force"] = true
		}
		if slot, bad := numericFlags(args, stmt, [2]string{"grace", "grace_period"}); bad {
			return slot
		}
		// The handler resolves the id itself (including a locator or a `sid:N`
		// handle) and reports a session it cannot find as session_not_found rather
		// than succeeding silently — Terminate is a no-op for an id it cannot see,
		// which is the same class of lie this function exists to stop telling.
		if _, fail := s.callTool(ctx, "session_terminate", args); fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		return noContent(stmt)
	}

	if _, bad := s.requireShell(stmt.Target); bad != nil {
		// Neither a session nor a channel: report the handler's own diagnostic
		// instead of pretending something was closed.
		return s.slotFromFailure(ctx, stmt, failureFromResult(bad))
	}
	if _, fail := s.callTool(ctx, "shell_close", argsOf("shell_id", stmt.Target)); fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	return noContent(stmt)
}

// directiveRemove deletes a session and its logs.
func (s *Server) directiveRemove(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	// A RUNNING SESSION NEEDS --force.
	//
	// The distinction rm draws is not "is it alive" but "is the record recoverable",
	// and it is the one difference between rm and close: close stops a session and
	// leaves it readable, rm erases manifests, transcript and every trace. Without this
	// guard, `rm <session>` on a live session erased work that was still happening and
	// answered 204, which reads exactly like the harmless case.
	//
	// The status is read here rather than inside session_delete because the handler
	// deletes whatever it is given — the decision belongs to the directive whose name
	// promises permanence.
	if !boolFlag(stmt, "force") {
		if sess, bad := s.requireSession(stmt.Target); bad == nil && sess != nil {
			if st := sess.Info().Status; st == api.SessionRunning {
				return envelope.Slot{
					Code: 409,
					Headers: map[string]string{
						"Path":       stmt.Path,
						"Error-Code": "session_running",
					},
					Body: "session '" + stmt.Target + "' is running; rm erases it permanently (manifests, transcript, every trace).\n" +
						"Stop it first with `close " + stmt.Target + "` if you only want it stopped, or pass --force to erase it now.",
				}
			}
		}
	}
	res, fail := s.callTool(ctx, "session_delete", argsOf("session_id", stmt.Target))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	_ = res
	return noContent(stmt)
}

// directiveSpan lists a channel's transcript spans.
func (s *Server) directiveSpan(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	args := argsOf("action", "list", "session_id", stmt.Target)
	// `span` names a CHANNEL (the parser enforces it: TargetChannel), so there is no
	// session-vs-channel guessing left to do here. This used to omit shell_id for a
	// session target and let the handler pick the primary shell — the same implicit
	// fallback #132 was about, one directive over.
	args["shell_id"] = stmt.Target
	res, fail := s.callTool(ctx, "message", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}
	// One span per line: status, time, byte range, line range, and the byte where
	// the line range starts.
	//
	// THE FOURTH AND FIFTH COLUMNS ANSWER DIFFERENT QUESTIONS. `start` is where the
	// command began; `line_start` is where `read --from-line <from>` begins, which is
	// earlier by the width of the prompt on that line (37 bytes in the case that
	// surfaced this). A caller paging by position needs the second one, and showing
	// only the first next to a line number is what made the two look interchangeable.
	var b strings.Builder
	spans, _ := doc["spans"].([]any)
	for _, raw := range spans {
		span, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		lineStart := int64(getFloatAny(span, "line_start", -1))
		lineStartCell := "-"
		if lineStart >= 0 {
			lineStartCell = fmt.Sprintf("%d", lineStart)
		}
		b.WriteString(fmt.Sprintf("%s\t%d\t%d-%d\tL%d-%d\t@%s\n",
			getString(span, "s", ""),
			int64(getFloatAny(span, "t", 0)),
			int64(getFloatAny(span, "start", 0)), int64(getFloatAny(span, "end", 0)),
			int64(getFloatAny(span, "from_line", 0)), int64(getFloatAny(span, "to_line", 0)),
			lineStartCell))
	}
	return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: b.String()}
}

// directiveReader registers or drops an output reader.
func (s *Server) directiveReader(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	// `--drop` WITHOUT A NUMBER IS A MISTAKE, NOT A REGISTRATION.
	//
	// The old test was `if v, ok := stmt.Flag("drop"); ok`, and a valueless flag
	// reports ok=true with v="". flagNumber("") then failed, the failure was
	// DISCARDED, and control fell through to the register branch — so
	// `reader h:1 --drop` created a new reader and answered 200 with its id. A typo
	// that leaves a resource behind is the one kind of typo that must not be quiet.
	//
	// FlagState separates the three cases the boolean could not: absent (register),
	// present without a value (this refusal), and present with a value (unregister).
	switch stmt.FlagState("drop") {
	case directive.FlagAbsent:
		// fall through to register below
	case directive.FlagNoValue:
		return badRequest(stmt, "reader --drop needs a reader id: `reader <handle> --drop <id>`")
	default:
		v, _ := stmt.Flag("drop")
		readerID, err := flagNumber(v)
		if err != nil {
			return numericFlagProblem(stmt, "drop", err)
		}
		res, fail := s.callTool(ctx, "shell_reader_unregister", argsOf(
			"shell_id", stmt.Target, "reader_id", readerID))
		if fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		_ = res
		return noContent(stmt)
	}
	res, fail := s.callTool(ctx, "shell_reader_register", argsOf("shell_id", stmt.Target))
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}
	// The new reader id IS news: the caller did not know it before this call.
	return envelope.Slot{
		Code:    200,
		Headers: map[string]string{"Path": stmt.Path, "Handle": stmt.Target},
		Body:    fmt.Sprintf("reader %d\n", int64(getFloatAny(doc, "reader_id", 0))),
	}
}

// directiveWatch manages wake-up rules.
func (s *Server) directiveWatch(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	args := map[string]any{}
	switch off, hasOff := stmt.Flag("off"); {
	case boolFlag(stmt, "list"):
		args["action"] = "list"
	case hasOff && off != "":
		args["action"] = "unregister"
		args["rule_id"] = off
	default:
		// TargetOptional is true on watch so `--list` and `--off <rule>` need no
		// target. The register form DOES need one, and reporting it here gives the
		// caller the same syntax-level message as the other channel directives rather
		// than a handler-internal "shell_id is required".
		if stmt.Target == "" {
			return badRequest(stmt, "watch needs a target: `watch <handle> [--event <name>]`")
		}
		args["action"] = "register"
		args["shell_id"] = stmt.Target
		if v, ok := stmt.Flag("event"); ok {
			args["event"] = v
		}
		if v, ok := stmt.Flag("for"); ok {
			secs, err := flagNumber(v)
			if err != nil {
				return numericFlagProblem(stmt, "for", err)
			}
			args["silence_seconds"] = secs
		}
		if v, ok := stmt.Flag("channel"); ok {
			args["channel"] = v
		}
	}
	res, fail := s.callTool(ctx, "shell_notify", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: toolResultText(res)})
	}
	return generatedSlot(stmt, win, compactDocument(doc))
}

// directiveNotify posts a notification to the human.
func (s *Server) directiveNotify(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	args := argsOf("message", stmt.Payload)
	if v, ok := stmt.Flag("level"); ok {
		args["level"] = v
	}
	if v, ok := stmt.Flag("title"); ok {
		args["title"] = v
	}
	if v, ok := stmt.Flag("on"); ok {
		args["session_id"] = v
	}
	if boolFlag(stmt, "sticky") {
		args["duration_seconds"] = float64(0)
	} else if v, ok := stmt.Flag("for"); ok {
		secs, err := flagNumber(v)
		if err != nil {
			return numericFlagProblem(stmt, "for", err)
		}
		args["duration_seconds"] = secs
	}
	res, fail := s.callTool(ctx, "notify_user", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, _ := toolResultMap(res)
	return generatedSlot(stmt, win, compactDocument(doc))
}

// directiveDetect probes the termcp host for a shell.
func (s *Server) directiveDetect(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	res, fail := s.callTool(ctx, "shell_detect", map[string]any{})
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, _ := toolResultMap(res)
	return generatedSlot(stmt, win, compactDocument(doc))
}

// directiveForward maps `fwd` onto the forward tool.
func (s *Server) directiveForward(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	args := map[string]any{}
	action, _ := stmt.Flag("action")
	if action == "" {
		action = "list"
	}
	args["action"] = action
	if slot, bad := stringFlags(args, stmt,
		[2]string{"session", "session_id"},
	); bad {
		return slot
	}
	if stmt.Target != "" {
		args["session_id"] = stmt.Target
	}
	if slot, bad := stringFlags(args, stmt,
		[2]string{"bind-host", "bind_host"},
		[2]string{"remote-host", "remote_host"},
	); bad {
		return slot
	}
	if slot, bad := numericFlags(args, stmt,
		[2]string{"remote-port", "remote_port"},
		[2]string{"local-port", "local_port"},
	); bad {
		return slot
	}
	if slot, bad := stringFlags(args, stmt,
		[2]string{"local-host", "local_host"},
		[2]string{"forward-id", "forward_id"},
	); bad {
		return slot
	}

	// The capability is decided by the action: a listing is a read, opening is a
	// reviewed change, closing is a change. Asking the table here (rather than
	// gating the whole directive) is what keeps `fwd --action list` out of the
	// review queue.
	if slot, refused := s.refuseAction(ctx, stmt, forwardCapability(action)); refused {
		return slot
	}

	res, fail := s.callTool(ctx, "forward", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	if held := reviewPendingFrom(res); held != nil {
		return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path}, Body: *held}
	}
	doc, _ := toolResultMap(res)
	return generatedSlot(stmt, win, compactDocument(doc))
}

// forwardCapability maps a forward action onto its capability.
func forwardCapability(action string) string {
	switch action {
	case "list":
		return capability.ForwardList
	case "close":
		return capability.ForwardClose
	}
	return capability.ForwardOpen
}

// directiveFile maps `file` onto the file tools, one per action.
func (s *Server) directiveFile(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	action, _ := stmt.Flag("action")
	tool, capID, ok := fileToolForAction(action)
	if !ok {
		return badRequest(stmt, "file needs --action read|write|stat|delete|rename|mkdir|urls|perm|link|fs|getwd, got "+quoteOrDash(action))
	}
	if slot, refused := s.refuseAction(ctx, stmt, capID); refused {
		return slot
	}

	args := argsOf("session_id", stmt.Target)
	if slot, bad := stringFlags(args, stmt,
		[2]string{"remote-path", "remote_path"},
		[2]string{"from-path", "from_path"},
		[2]string{"to-path", "to_path"},
		[2]string{"local-path", "local_path"},
		[2]string{"data", "data"},
	); bad {
		return slot
	}
	// --mode is a STRING for read/write (text|hex|file) and a NUMBER for
	// `perm --op chmod` (decimal permission bits). Sending it as a string in both
	// cases is not a type error the handler can catch: chmod reads it with
	// getFloat64, which returns its default for anything that is not a number, so
	// the mode silently became 0 (---------- ) while the call reported success.
	// A file the caller then could not read looked like a permissions problem on
	// the target, not a lost argument here.
	if action == "perm" {
		if slot, bad := numericFlags(args, stmt, [2]string{"mode", "mode"}); bad {
			return slot
		}
	} else if slot, bad := stringFlags(args, stmt, [2]string{"mode", "mode"}); bad {
		return slot
	}
	if slot, bad := stringFlags(args, stmt,
		[2]string{"target", "target"},
		[2]string{"link-path", "link_path"},
		[2]string{"existing-path", "existing_path"},
		[2]string{"new-path", "new_path"},
	); bad {
		return slot
	}
	if slot, bad := numericFlags(args, stmt,
		[2]string{"offset", "offset"},
		[2]string{"length", "length"},
		[2]string{"local-offset", "local_offset"},
		[2]string{"uid", "uid"},
		[2]string{"gid", "gid"},
		[2]string{"atime", "atime"},
		[2]string{"mtime", "mtime"},
		[2]string{"size", "size"},
	); bad {
		return slot
	}
	if action != "read" && action != "write" && action != "getwd" {
		// perm/link/fs each dispatch on their OWN action field (chmod, symlink,
		// realpath …), so the sub-action overwrites the verb that selected the
		// tool. --op is the spelling; --action-verb is the older alias.
		//
		// A missing flag yields "", not nil — the previous `== nil` test therefore
		// never fired and the sub-action was silently empty, which is what made
		// these three reachable only as "action must be chmod, chown, or chtimes".
		switch action {
		case "perm", "link", "fs":
			sub, _ := stmt.Flag("op")
			if sub == "" {
				sub, _ = stmt.Flag("action-verb")
			}
			if sub == "" {
				return badRequest(stmt, "file --action "+action+" needs the sub-action: --op "+fileSubActions(action))
			}
			args["action"] = sub
		}
	}

	res, fail := s.callTool(ctx, tool, args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	if held := reviewPendingFrom(res); held != nil {
		return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path}, Body: *held}
	}
	doc, ok := toolResultMap(res)
	if !ok {
		text := toolResultText(res)
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: text}
	}
	return generatedSlot(stmt, win, compactDocument(doc))
}

// fileSubActions names the sub-actions of a multi-action file verb, for the
// error that has to teach the shape without a second call.
func fileSubActions(action string) string {
	switch action {
	case "perm":
		return "chmod|chown|chtimes"
	case "link":
		return "readlink|symlink|link"
	case "fs":
		return "truncate|realpath|statvfs"
	}
	return ""
}

// fileToolForAction maps a `file --action` onto the tool that implements it and
// the capability it needs.
func fileToolForAction(action string) (tool, capID string, ok bool) {
	switch action {
	case "read":
		return "file_read", capability.FileRead, true
	case "write":
		return "file_write", capability.FileWrite, true
	case "stat":
		return "file_stat", capability.FileStat, true
	case "delete":
		return "file_delete", capability.FileDelete, true
	case "rename":
		return "file_rename", capability.FileRename, true
	case "mkdir":
		return "file_mkdir", capability.FileMkdir, true
	case "urls":
		return "file_urls", capability.FileURLs, true
	case "perm":
		return "file_perm", capability.FilePerm, true
	case "link":
		return "file_link", capability.FileLink, true
	case "fs":
		return "file_fs", capability.FileFs, true
	case "getwd":
		return "file_getwd", capability.FileGetwd, true
	}
	return "", "", false
}

// directiveProfile maps `profile` onto ssh_config.
func (s *Server) directiveProfile(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	action, rest := resolveAction(stmt, "list")
	capID, ok := profileCapability(action)
	if !ok {
		return badRequest(stmt, "profile needs --action list|create|edit|copy|delete, got "+quoteOrDash(action))
	}
	if slot, refused := s.refuseAction(ctx, stmt, capID); refused {
		return slot
	}
	args := map[string]any{"action": action}
	if slot, bad := stringFlags(args, stmt,
		[2]string{"name", "name"},
	); bad {
		return slot
	}
	if _, ok := args["name"]; !ok && len(rest) > 0 {
		args["name"] = rest[0]
	}
	if slot, bad := stringFlags(args, stmt,
		[2]string{"host", "host"},
		[2]string{"user", "user"},
	); bad {
		return slot
	}
	if slot, bad := numericFlags(args, stmt, [2]string{"port", "port"}); bad {
		return slot
	}
	if slot, bad := stringFlags(args, stmt,
		[2]string{"password", "password"},
		[2]string{"private-key", "private_key"},
		[2]string{"key-passphrase", "key_passphrase"},
		[2]string{"known-hosts", "known_hosts"},
		[2]string{"proxy", "proxy"},
		[2]string{"description", "description"},
		[2]string{"default-shell", "default_shell"},
		[2]string{"default-mode", "default_mode"},
		[2]string{"jump-host", "jump_host"},
		[2]string{"jump-user", "jump_user"},
	); bad {
		return slot
	}
	if slot, bad := numericFlags(args, stmt,
		[2]string{"jump-port", "jump_port"},
		[2]string{"dial-timeout-seconds", "dial_timeout_seconds"},
	); bad {
		return slot
	}
	// COPY SPEAKS `--source`, THE HANDLER SPEAKS `source_name`/`target_name`.
	//
	// The directive used to forward `--source` verbatim and the handler read
	// `source_name`, so copy failed with "source_name and target_name are required"
	// no matter what the caller wrote — the two names never met. The rename happens
	// here, at the boundary between the two vocabularies, rather than in the handler:
	// the handler's argument names are part of the MCP tool's own schema and other
	// callers (the Web UI) already use them.
	if slot, bad := stringFlags(args, stmt,
		[2]string{"source", "source_name"},
	); bad {
		return slot
	}
	if _, ok := args["target_name"]; !ok {
		if v, ok := args["name"]; ok {
			args["target_name"] = v
		}
	}
	if boolFlag(stmt, "trust-unknown-host") {
		args["trust_unknown_host"] = true
	}
	if boolFlag(stmt, "default-approval") {
		args["default_approval"] = true
	}
	res, fail := s.callTool(ctx, "ssh_config", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	if held := reviewPendingFrom(res); held != nil {
		return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path}, Body: *held}
	}
	if action == "list" {
		// The profile listing is a set of names, one per line: copy-pasteable into
		// the next `pty`/`pipe`.
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: listFromDoc(res, "ssh_configs", "name")}
	}
	doc, _ := toolResultMap(res)
	return generatedSlot(stmt, win, compactDocument(doc))
}

// profileCapability maps a profile action onto its capability.
func profileCapability(action string) (string, bool) {
	switch action {
	case "list":
		return capability.ProfileList, true
	case "create":
		return capability.ProfileCreate, true
	case "edit":
		return capability.ProfileEdit, true
	case "copy":
		return capability.ProfileCopy, true
	case "delete":
		return capability.ProfileDelete, true
	}
	return "", false
}

// directiveExtension maps `ext` onto the extension tool.
func (s *Server) directiveExtension(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	action, rest := resolveAction(stmt, "list")
	capID, ok := extensionCapability(action)
	if !ok {
		return badRequest(stmt, "ext needs --action list|get|create|update|delete|retry, got "+quoteOrDash(action))
	}
	if slot, refused := s.refuseAction(ctx, stmt, capID); refused {
		return slot
	}
	args := map[string]any{"action": action}
	if slot, bad := stringFlags(args, stmt,
		[2]string{"name", "name"},
	); bad {
		return slot
	}
	if _, ok := args["name"]; !ok && len(rest) > 0 {
		args["name"] = rest[0]
	}
	if slot, bad := stringFlags(args, stmt,
		[2]string{"kind", "kind"},
		[2]string{"target", "target"},
		[2]string{"description", "description"},
		[2]string{"command", "command"},
		[2]string{"url", "url"},
		[2]string{"mode", "mode"},
		[2]string{"notes", "notes"},
	); bad {
		return slot
	}
	if slot, bad := numericFlags(args, stmt,
		[2]string{"rows", "rows"},
		[2]string{"cols", "cols"},
	); bad {
		return slot
	}
	/* --args carries a program's argv, so it is an ARGV WORD LIST — the same
	   grammar --argv uses for pipe/pty — and not a comma list: an argument may
	   contain a comma, and the split mangled it silently. Its value is the rest
	   of the line, so it must come last. */
	if err := setFlagArgvList(args, stmt, "args", "args"); err != nil {
		return badRequest(stmt, err.Error())
	}
	/* --headers is the JSON object an HTTP MCP template sends on every relay
	   request, so it takes the same treatment. It used to be in the parser's
	   flag list and read by nobody at all, which is worse than not accepting it:
	   the caller saw success and the header was never sent. */
	if err := setFlagJSONObject(args, stmt, "headers", "headers"); err != nil {
		return badRequest(stmt, err.Error())
	}
	setFlagList(args, stmt, "allow", "allow")
	setFlagList(args, stmt, "paths", "paths")
	/* --forwards declares tunnels, and a tunnel is a structure rather than a
	   word: it takes a JSON array of declarations. It also used to be accepted
	   and ignored, so a template's tunnels never came up. */
	if err := setFlagJSONList(args, stmt, "forwards", "forwards"); err != nil {
		return badRequest(stmt, err.Error())
	}
	if boolFlag(stmt, "enabled") {
		args["enabled"] = true
	}
	res, fail := s.callTool(ctx, "extension", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	if held := reviewPendingFrom(res); held != nil {
		return envelope.Slot{Code: 202, Headers: map[string]string{"Path": stmt.Path}, Body: *held}
	}
	if action == "list" {
		// The boot manifest: one extension per line, with its state and handle, so
		// a caller that lost its context gets its toolset back in one call.
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: extensionListing(res)}
	}
	doc, _ := toolResultMap(res)
	return generatedSlot(stmt, win, compactDocument(doc))
}

// extensionCapability maps an extension action onto its capability.
func extensionCapability(action string) (string, bool) {
	switch action {
	case "list":
		return capability.ExtList, true
	case "get":
		return capability.ExtGet, true
	case "create":
		return capability.ExtCreate, true
	case "update":
		return capability.ExtUpdate, true
	case "delete":
		return capability.ExtDelete, true
	case "retry":
		return capability.ExtRetry, true
	}
	return "", false
}

// refuseAction applies a capability decision to a directive whose capability came
// from a flag.
func (s *Server) refuseAction(ctx context.Context, stmt directive.Statement, capID string) (envelope.Slot, bool) {
	if capID == "" {
		return envelope.Slot{}, false
	}
	sess := s.sessionForStatement(stmt)
	if sess == nil {
		return envelope.Slot{}, false
	}
	if sess.Restriction().Allows(capID) {
		return envelope.Slot{}, false
	}
	return envelope.Slot{
		Code: 403,
		Headers: map[string]string{
			"Path":       stmt.Path,
			"Error-Code": CodeForbidden,
		},
		Body: "this session's template does not grant " + capID,
	}, true
}

// directiveJRPC handles the three frame-channel entries.
//
// One function, because the doors differ only in what they do to the channel
// after opening it (see term_jrpc.go): the subcommand split, the handle
// registry and the reply shapes are identical, and three copies of them would be
// three places for `recv` to disagree with itself.
//
// The split is done by hand rather than with strings.Fields because the FRAME is
// the rest of the line, quotes included: Fields would split a JSON body on its
// own spaces (`"name": "x"` becomes two words), and the payload keeps the
// caller's quoting, so a quoted frame reached json.Unmarshal with its quotes
// still on and was refused as "not JSON".
func (s *Server) directiveJRPC(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	payload := strings.TrimSpace(stmt.Payload)
	if payload == "" {
		return badRequest(stmt, stmt.Op+" needs a subcommand: open|tools|send|recv|call|close")
	}
	sub, rest := splitWord(payload)
	switch sub {
	case "open":
		url, _ := splitWord(rest)
		return s.jrpcOpen(ctx, stmt, stmt.Op, unquoteWord(url))
	case "tools", "send", "recv", "call", "close":
		handle, tail := splitWord(rest)
		args := []string{}
		if handle != "" {
			args = append(args, unquoteWord(handle))
		}
		if tail = strings.TrimSpace(tail); tail != "" {
			args = append(args, tail)
		}
		return s.jrpcChannel(ctx, stmt, win, stmt.Op, sub, args)
	}
	return badRequest(stmt, "unknown "+stmt.Op+" subcommand: "+sub)
}

// splitWord cuts the first whitespace-delimited word off s, returning it and the
// rest. It is the one piece of word splitting `mcp` needs: the subcommand and the
// handle are single words, and everything after them is a frame that must keep its
// spaces.
func splitWord(s string) (string, string) {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		return s[:i], strings.TrimSpace(s[i+1:])
	}
	return s, ""
}

// unquoteWord strips one layer of matching shell quotes, so `'{"a":1}'` and
// `"{\"a\":1}"` both hand the same text to the JSON parser. An unquoted word is
// returned unchanged.
func unquoteWord(s string) string {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// directiveHelp prints the directive table, or one directive's syntax.
func (s *Server) directiveHelp(stmt directive.Statement, win termArgs) envelope.Slot {
	if len(stmt.Args) > 0 {
		op, ok := directive.Lookup(stmt.Args[0])
		if !ok {
			return envelope.Slot{Code: 404, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "unknown_directive"},
				Body: "unknown directive: " + stmt.Args[0]}
		}
		body := op.Usage + "\n"
		if op.Detail != "" {
			// The detail is a page, so it follows the one-line template instead of
			// replacing it: the template is what a caller scans, the page is what it
			// reads once it has decided to produce the thing.
			body += "\n" + op.Detail + "\n"
		}
		capped, code := capGenerated(body, win)
		return envelope.Slot{Code: code, Headers: map[string]string{"Path": stmt.Path}, Body: capped}
	}
	var b strings.Builder
	for _, op := range directive.Ops() {
		b.WriteString(op.Usage)
		b.WriteByte('\n')
	}
	capped, code := capGenerated(b.String(), win)
	return envelope.Slot{Code: code, Headers: map[string]string{"Path": stmt.Path}, Body: capped}
}

// termGuide is the empty-call answer: where am I, what can I do, and what is here.
//
// A caller that has just been woken up knows nothing, so this call has to be legal,
// cheap, and dense — never an error. It is the first move of every recovery, and
// the recovery path must not depend on remembering anything.
func (s *Server) termGuide(ctx context.Context, win termArgs) termReply {
	var b strings.Builder
	b.WriteString("termcp terminal. Syntax:\n")
	// Two examples, and only two: the batching line says something the directive
	// table below CANNOT express (that `;` runs several statements in one call and
	// gives one slot each), and the pty line shows what a reply's handle is for.
	// The other examples that used to sit here (`run … one command, one call`,
	// `read … read, list, inspect`, the pty/pipe usage) repeated the table verbatim
	// — 308 bytes of the same information written twice, which is what a caller
	// pays for on every recovery call.
	b.WriteString("  pty internal                                  open an interactive shell -> Handle\n")
	b.WriteString("  ps; read a1f3:1; info a1f3:1                 several statements, one call: one slot each\n")
	b.WriteString("Handles are <session>:<index> (always with :N); copy one from ps or ext list into the next command.\n")
	b.WriteString("Every reply is TERM/1 <code>: 2xx ok, 4xx bad request, 5xx the command failed.\n")

	// The CORE directive table, plus the pointer to the rest.
	//
	// This reply is read by a caller that has not decided what to do yet, so the
	// lines it needs come first and the other eighteen are one `help` away. Both
	// halves matter:
	//
	//   - Listing all 24 here cost the recovery path ~900 bytes of capability the
	//     caller has no use for at that moment, on every wake-up.
	//   - Listing ONLY the core ones and stopping there would make file/forward/
	//     extension work invisible to anyone who never suspects there is more —
	//     the failure the old "7 of 23 examples" had, one layer down.
	//
	// The pointer line is what keeps both true. It names `help` in the same wording
	// `help` uses for itself, so a caller that follows it lands on the full table
	// rather than on a detail page, and it says how many are waiting. Generated
	// from the same ops array `help` prints, so the two cannot disagree.
	b.WriteString("\ndirectives (core; `help` lists all " + strconv.Itoa(len(directive.Ops())) + "):\n")
	for _, op := range directive.Ops() {
		if !op.Core {
			continue
		}
		b.WriteString("  " + op.Usage + "\n")
	}
	b.WriteString("\n")

	// What is running right now, in the same columns `ps` prints: the recovery path
	// is "ask, then copy a handle", not "ask, then translate".
	ps := s.directivePS(ctx, directive.Statement{Op: "ps", Path: "ps"}, win)
	b.WriteString("running now:\n")
	if strings.TrimSpace(ps.Body) == "" {
		b.WriteString("  (nothing)\n")
	} else {
		for _, line := range strings.Split(strings.TrimRight(ps.Body, "\n"), "\n") {
			b.WriteString("  " + line + "\n")
		}
	}
	body, code := capGenerated(b.String(), win)
	return termReply{win: win, slots: []envelope.Slot{{Code: code, Body: body}}}
}

// --- small helpers ---

// dash renders an empty field as a placeholder, so columns stay aligned.
func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

// quoteOrDash renders a flag value for an error message.
func quoteOrDash(s string) string {
	if s == "" {
		return "(missing)"
	}
	return fmt.Sprintf("%q", s)
}

// handleOf renders a channel's short handle: <session>:<index>.
//
// IT ALWAYS INCLUDES THE INDEX, even for the primary channel (index 1). The old
// version returned bare sessionID for index <= 1, which made the primary channel's
// handle look identical to the session itself. That was the root cause of every
// ambiguity between "the container" and "its first channel":
//
//	close <session> --force   fell back to primary channel and became a no-op (#132)
//	run <session>             fell back to primary channel, silently guessing which
//	                          channel to use on a multi-channel session
//
// With `:1` always present, a channel handle and a session id have DIFFERENT
// shapes: `sid:1` is a channel, `sid` is the container. A directive that operates
// on a channel (run, read, type, key, resize) requires a channel shape, and a bare
// session id is refused at parse time.
func handleOf(sessionID string, index int) string {
	if index < 1 {
		index = 1
	}
	return fmt.Sprintf("%s:%d", sessionID, index)
}

// ageOf renders how long ago a timestamp was, in terminal units.
func ageOf(unixMilli int64) string {
	if unixMilli == 0 {
		return "-"
	}
	secs := (clock.Now() - unixMilli) / 1000
	if secs < 0 {
		secs = 0
	}
	switch {
	case secs < 60:
		return fmt.Sprintf("%ds", secs)
	case secs < 3600:
		return fmt.Sprintf("%dm", secs/60)
	default:
		return fmt.Sprintf("%dh", secs/3600)
	}
}

// writeKV writes one `key: value` line.
func writeKV(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(value)
	b.WriteByte('\n')
}

// approvalSummaryText renders a session's review state in one field.
func approvalSummaryText(info api.Session) string {
	if !info.ApprovalMode {
		return "off"
	}
	scopes := info.ApprovalScopes
	if len(scopes) == 0 {
		return fmt.Sprintf("on (%d approver, all operations)", info.ApprovalNeed)
	}
	return fmt.Sprintf("on (%d approver, %s)", info.ApprovalNeed, strings.Join(scopes, ","))
}

// compactDocument renders a handler's JSON reply as `key: value` lines, one level
// deep. Nested structures are rendered as their JSON text, which keeps a generated
// reply readable without inventing a second rendering per document.
func compactDocument(doc map[string]any) string {
	if len(doc) == 0 {
		return ""
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(": ")
		switch v := doc[k].(type) {
		case string:
			b.WriteString(v)
		case nil:
			b.WriteString("-")
		case bool:
			b.WriteString(fmt.Sprint(v))
		case float64:
			if v == float64(int64(v)) {
				b.WriteString(fmt.Sprintf("%d", int64(v)))
			} else {
				b.WriteString(fmt.Sprint(v))
			}
		default:
			raw, _ := jsonMarshalIndent(map[string]any{"v": v})
			b.WriteString(strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "{\n  \"v\": "), "\n}")))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// listFromDoc renders a named list inside a handler's JSON reply as one entry per
// line: the shape a caller copies into the next command.
func listFromDoc(res *mcpgo.CallToolResult, listKey, field string) string {
	doc, ok := toolResultMap(res)
	if !ok {
		return toolResultText(res)
	}
	items, _ := doc[listKey].([]any)
	var b strings.Builder
	for _, raw := range items {
		// Some lists are plain names (ssh_configs), others are objects with a
		// description beside the name. Both shapes reach here, and a listing that
		// silently drops one of them reads as "there is nothing there".
		if name, ok := raw.(string); ok {
			b.WriteString(name)
			b.WriteByte('\n')
			continue
		}
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		b.WriteString(getString(item, field, ""))
		if desc := getString(item, "description", ""); desc != "" {
			b.WriteString("\t" + desc)
		}
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "(none)\n"
	}
	return b.String()
}

// extensionListing renders `ext list` as the boot manifest: name, state, handle,
// and a one-line note. This is the call that gives a caller its toolset back after
// its context was cleared, so it must be one call and must be scannable.
func extensionListing(res *mcpgo.CallToolResult) string {
	doc, ok := toolResultMap(res)
	if !ok {
		return toolResultText(res)
	}
	items, _ := doc["extensions"].([]any)
	var b strings.Builder
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := getString(item, "name", "")
		state := getString(item, "status", "")
		handle := getString(item, "session_id", "")
		if shell := getString(item, "shell_id", ""); shell != "" {
			handle = shell
		}
		notes := strings.ReplaceAll(getString(item, "notes", ""), "\n", " ")
		if len(notes) > 60 {
			notes = notes[:60] + "…"
		}
		b.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\n", name, dash(state), dash(handle), notes))
	}
	if b.Len() == 0 {
		return "(no extensions installed)\n"
	}
	return b.String()
}

// capGenerated applies the byte and line windows to a generated reply, in that
// order, and reports whether it had to cut.
// generatedSlot renders a generated body through the caller's window: the same
// three axes that bound a stream bound a listing or a document.
func generatedSlot(stmt directive.Statement, win termArgs, body string) envelope.Slot {
	capped, code := capGenerated(body, win)
	return envelope.Slot{Code: code, Headers: map[string]string{"Path": stmt.Path}, Body: capped}
}

func capGenerated(body string, win termArgs) (string, int) {
	if win.BytesLimit == 0 {
		// The schema's zero-byte window: streamed replies hand back a handle and
		// consume nothing; a generated reply has no stream, so it is simply empty.
		return "", 204
	}
	code := 200
	if win.BytesLimit > 0 && len(body) > win.BytesLimit {
		body = foldOrTruncate(body, win.BytesLimit, win.OnBytesOverflow)
		code = 206
	}
	if win.LinesLimit > 0 {
		if lines := strings.Count(body, "\n"); lines > win.LinesLimit {
			body = foldOrTruncateLines(body, win.LinesLimit, win.OnLinesOverflow)
			code = 206
		}
	}
	return body, code
}

// setFlagString copies a string flag into a handler argument.
//
// It copies EVERY explicit value, including an empty one, and reports a flag written
// without a value instead of dropping it.
//
// The old `ok && v != ""` test is why an explicitly empty `--data` could not write
// an empty file: the caller's explicit empty value was discarded here, the handler
// saw no `data` key at all, and answered "data or local_path required" for a flag
// that had been given. The other half of the same test was worse: `--remote-path`
// with no value was also discarded, so a fumbled flag looked identical to a
// forgotten one.
func setFlagString(args map[string]any, stmt directive.Statement, flag, arg string) (string, error) {
	switch stmt.FlagState(flag) {
	case directive.FlagAbsent:
		return "", nil
	case directive.FlagNoValue:
		return flag, fmt.Errorf("needs a value")
	}
	v, _ := stmt.Flag(flag)
	args[arg] = v
	return "", nil
}

// stringFlags fills every listed string flag, refusing the first one written without
// a value.
//
// Same shape as numericFlags and for the same reason: 38 call sites cannot each
// decide for themselves what `--remote-path` with nothing after it means.
func stringFlags(args map[string]any, stmt directive.Statement, pairs ...[2]string) (envelope.Slot, bool) {
	for _, p := range pairs {
		name, err := setFlagString(args, stmt, p[0], p[1])
		if err != nil {
			return missingFlagValueProblem(stmt, name, err), true
		}
	}
	return envelope.Slot{}, false
}

// missingFlagValueProblem renders the 400 for a flag written without a value.
//
// It is a distinct error code from "required", because they are distinct mistakes:
// the caller who wrote `--remote-path` meant to give one and lost it to a quoting or
// line-continuation slip, while the caller who wrote nothing has not tried yet.
func missingFlagValueProblem(stmt directive.Statement, flag string, err error) envelope.Slot {
	return envelope.Slot{
		Code: 400,
		Headers: map[string]string{
			"Path":       stmt.Path,
			"Error-Code": "flag_needs_value",
		},
		Body: "--" + flag + " " + err.Error() + "; write --" + flag + " <value>, or --" + flag + "='' for an explicit empty one",
	}
}

// numericFlags fills every listed numeric flag, stopping at the first one that does
// not parse.
//
// It returns (slot, true) when the statement must be refused: the alternative —
// letting each call site ignore the error — is how `--port abc` became port 0 and a
// successful reply. Every numeric flag a directive forwards goes through here, so
// "a typo is a 400" is one rule instead of seventeen opportunities to forget it.
func numericFlags(args map[string]any, stmt directive.Statement, pairs ...[2]string) (envelope.Slot, bool) {
	for _, p := range pairs {
		name, err := setFlagNumber(args, stmt, p[0], p[1])
		if err != nil {
			return numericFlagProblem(stmt, name, err), true
		}
	}
	return envelope.Slot{}, false
}

// setFlagNumber copies a numeric flag into a handler argument, reporting a value
// that is not a number instead of substituting one.
//
// It returns the flag name so the caller's 400 can say which word was wrong. The
// old version wrote flagNumber(v) — which used to be 0 on a parse failure — straight
// into the map, so `--port abc`, `--uid abc`, `--offset abc` all became 0 and then a
// handler default, and the reply said the operation succeeded.
func setFlagNumber(args map[string]any, stmt directive.Statement, flag, arg string) (string, error) {
	switch stmt.FlagState(flag) {
	case directive.FlagAbsent:
		return "", nil
	case directive.FlagNoValue:
		return flag, fmt.Errorf("needs a value")
	}
	v, _ := stmt.Flag(flag)
	n, err := flagNumber(v)
	if err != nil {
		return flag, err
	}
	args[arg] = n
	return "", nil
}

// setFlagList copies a comma-separated flag into a handler's array argument.
//
// This is right for a flag whose VALUES cannot contain a comma — a capability
// name, a path (comma is legal in a path, but a path list written on one line is
// the caller's own choice), an extension name. It is WRONG for a flag carrying a
// command line, which is why --args does not use it: see setFlagJSONList.
func setFlagList(args map[string]any, stmt directive.Statement, flag, arg string) {
	v, ok := stmt.Flag(flag)
	if !ok || v == "" {
		return
	}
	parts := strings.Split(v, ",")
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	args[arg] = out
}

// setFlagArgvList copies a flag written as an ARGV WORD LIST into a handler's
// array argument, and refuses a value that is not one.
//
// The WHOLE word list is the value: in `--command node --args /srv/mcp.js mcp`,
// --command names the program and --args carries the server script and its
// arguments, so the first word is not dropped. The grammar is --argv's (the
// shell's quoting, no expansion), because a program's arguments are a word list
// everywhere else in this protocol and a second grammar here would be a trap.
//
// It replaced a comma split, which could not express an argument containing a
// comma (`sh -c 'a,b'` became two arguments) and failed SILENTLY: the extension
// started with a mangled argv and reported a startup error that named no flag.
//
// Empty is "not set" rather than an error: a caller clearing args on update
// sends no flag at all, and an empty --args value is the same intent.
func setFlagArgvList(args map[string]any, stmt directive.Statement, flag, arg string) error {
	v, ok := stmt.Flag(flag)
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	// SplitArgv gives the command and the rest separately; both belong here, so
	// the command goes back in front. Reading the two halves apart is still right:
	// it is what refuses a command LINE (shell syntax) instead of a word list.
	cmd, rest, err := directive.SplitArgv(v)
	if err != nil {
		return fmt.Errorf("--%s: %v", flag, err)
	}
	out := make([]any, 0, len(rest)+1)
	if cmd != "" {
		out = append(out, cmd)
	}
	for _, w := range rest {
		out = append(out, w)
	}
	args[arg] = out
	return nil
}

// setFlagJSONObject copies a flag written as a JSON object into a handler's map
// argument (--headers). An HTTP template's headers are a map with caller-chosen
// keys, which neither a comma list nor an argv can express.
func setFlagJSONObject(args map[string]any, stmt directive.Statement, flag, arg string) error {
	v, ok := stmt.Flag(flag)
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return fmt.Errorf("--%s must be a JSON object, e.g. --%s '%s' (got %q)",
			flag, flag, `{"X-Api-Key":"..."}`, v)
	}
	args[arg] = out
	return nil
}

// setFlagJSONList copies a flag written as a JSON array into a handler's array
// argument, and refuses anything else. For a value that is a STRUCTURE rather
// than a command line (--forwards: a list of tunnel declarations), where an argv
// word list has no shape to describe it.
func setFlagJSONList(args map[string]any, stmt directive.Statement, flag, arg string) error {
	v, ok := stmt.Flag(flag)
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	var out []any
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return fmt.Errorf("--%s must be a JSON array, e.g. --%s '%s' (got %q)",
			flag, flag, `[{...}]`, v)
	}
	args[arg] = out
	return nil
}

// resolveAction settles WHERE a profile/ext action came from, and returns the
// positional words that remain once it is consumed.
//
// THE AMBIGUITY WAS REAL AND THIS REMOVES IT. These two forms both exist:
//
//	profile create demo …        the action is the first positional word
//	profile demo --action edit   the action is a flag
//
// so "which positional word is the name" depended on where the action came from —
// the old code hardcoded the first shape and answered "name is required" to every
// call spelled the second way. Tracking that with a boolean at the name site (as a
// first attempt did) only moved the conditional around.
//
// Consuming the action word instead makes the two shapes converge: whatever is left
// at Args[0] IS the name, in both of them. Same idea as Op.Target in the directive
// parser — a word that names grammar is removed from the argument list, so the words
// that remain all mean data.
func resolveAction(stmt directive.Statement, fallback string) (action string, rest []string) {
	if v, ok := stmt.Flag("action"); ok && v != "" {
		// Spelled as a flag: every positional word is data.
		return v, stmt.Args
	}
	if len(stmt.Args) > 0 {
		// Spelled positionally: consume it, so Args[0] is the name again.
		return stmt.Args[0], stmt.Args[1:]
	}
	return fallback, stmt.Args
}
