package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/session"
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
	// `ls <session>` narrows the same listing to one link. Done here rather than in a
	// second function because the columns, the caps and the emptiness message must
	// stay identical: two listings of the same tree that disagree are worse than one
	// that needs a flag.
	if only, ok := stmt.Flag("only-session"); ok && only != "" {
		narrowed := sessions[:0]
		for _, sess := range sessions {
			if sess.ID == only || sess.SSHConfig == only {
				narrowed = append(narrowed, sess)
			}
		}
		sessions = narrowed
	}

	var b strings.Builder
	// A FINISHED CHANNEL IS NOT THE DEFAULT VIEW (issue #193).
	//
	// `open --argv` per command is an ordinary way to work, and each of those leaves a
	// channel behind that stays addressable for reading — so a session ends up with one
	// live channel and thirty-eight dead ones, and `ls --shells` becomes a wall the
	// caller has to read past to find the one it is using. Measured in the report: 42
	// rows, 38 of them exited pipes.
	//
	// Folding is a VIEW, not reclamation: the bytes are still there and the address still
	// reads. Which is exactly why it has to be SAID — a listing that quietly drops 38
	// rows reads as "those channels are gone", and a caller that believes that will not
	// look for output it still has.
	hidden := 0
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
		shown := 0
		for _, sh := range shells {
			if !boolFlag(stmt, "shells") && len(shells) > 1 && sh.Index > 1 {
				// Without --shells, one line per session: the primary channel names
				// the session, and the extra channels are one flag away. Listing all
				// of them by default would make a fleet's ps unreadable.
				continue
			}
			if sh.Status != api.SessionRunning && !boolFlag(stmt, "all") {
				hidden++
				continue
			}
			b.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\t%s\n",
				handleOf(sess.ID, sh.Index), sh.Status, dash(string(sh.Mode)),
				dash(sh.Name), ageOf(sh.CreatedAt)))
			shown++
		}
		// A SESSION WHOSE CHANNELS ARE ALL HIDDEN MUST NOT VANISH: its own line is what
		// keeps it addressable from this listing, and the channels are one flag away.
		if shown == 0 && hidden > 0 {
			b.WriteString(fmt.Sprintf("%s\t%s\t%s\t%s\n",
				sess.ID, sess.Status, dash(sess.SSHConfig), ageOf(sess.CreatedAt)))
		}
	}
	if hidden > 0 {
		b.WriteString(fmt.Sprintf("(%d finished channel(s) not shown; `ls --all` lists them, and their output is still readable)\n", hidden))
	}
	body := b.String()
	// PEERS ARE NODES ON THE SAME LISTING. `ls <session>` is the only place a caller
	// can find out what an instance is holding without having memorised a reply, and a
	// peer that exists but is invisible there is an address lost the moment you look
	// away. They are printed with the address that addresses them, so column 1 is
	// copyable for either kind of node.
	if peers := peerLines(sessions); peers != "" {
		body += peers
	}
	if strings.TrimSpace(body) == "" {
		// AN EMPTY LISTING HAS TO SAY WHAT TO DO NEXT, or it reads as "there is
		// nothing here" when the truth is "nothing is RUNNING yet". The reporting
		// caller's first move was `ls`, it said only "nothing is running", and
		// `ls host` -- the one command that answers "what could I connect to" -- was a
		// guess it had to make from the guide (issue #161, P2-5).
		body = "(nothing is running)\n`ls host` shows the saved targets; `connect <host>` opens one.\n"
	}
	// PS IS THE STATE, SO ITS learn_mode TEACHES THE STATE VERBS.
	//
	// `learn_mode` attaches the CURRENT directive's usage line, which is right for a
	// reply about one command and useless here: a caller that just ran `ps` is holding
	// addresses and wondering what can be done with them. The verbs that consume an
	// address are exactly the ones worth naming at that moment — and they are not
	// discoverable from the listing, which shows ids and states but never says that
	// `close a1f3:1` archives one and `rm a1f3` erases the other.
	//
	// This is the manual's own ordering: learn what exists, then what can be done to
	// it. One `ps learn_mode:true` replaces four `help` round trips.
	if win.LearnMode {
		body += "\nwhat an address in column 1 can be used for:\n"
		for _, name := range []string{"get", "close", "rm", "read", "resize", "span", "open", "run"} {
			if op, ok := directive.Lookup(name); ok {
				body += "  " + op.Usage + "\n"
			}
		}
	}
	// A generated reply is capped by the same window as a byte stream: a listing is
	// exactly as capable of flooding a context as a log is, and the directives with
	// no stream to continue are the ones with no other protection.
	body, code, cut, total := capGeneratedCounted(body, win)
	noteTruncated(headers, code, cut, total, win)
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// directiveInfo describes one session or channel as `key: value` lines.
func (s *Server) directiveInfo(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	sess, bad := s.requireSession(stmt.Target)
	if bad != nil {
		return s.slotFromFailure(ctx, stmt, failureFromResult(bad))
	}
	// A CHANNEL ADDRESS IS A DIFFERENT NODE, AND IT USED TO BE ANSWERED WITH ITS
	// PARENT (issue #142/#148).
	//
	// `get a1f3:2` returned the session record — byte-for-byte the same reply as
	// `get a1f3`, with only `Path` differing. Nothing in it described channel 2, and
	// `get a1f3:99` returned the same 200, so a mistyped index was indistinguishable
	// from a hit. That is the worst shape for an answer: it looks like confirmation.
	if idx := locator.SplitHandle(stmt.Target).Index; idx > 0 {
		cs, ok := sess.ShellByIndex(idx)
		if !ok {
			return s.slotFromFailure(ctx, stmt, &toolFailure{
				code: CodeShellNotFound,
				msg:  fmt.Sprintf("no channel %d on %s; it has %s", idx, sess.ID, channelSummary(sess)),
			})
		}
		return s.channelSlot(stmt, win, cs)
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
	// WHY IT IS NOT RUNNING, right after the state it explains. A session that is
	// `exited` with no reason is the shape issue #176 reported: every surface agreed it
	// was gone and none said why. Deliberate closes record their reason too, so the line
	// never has to be guessed at.
	if info.ExitReason != "" {
		writeKV(&b, "exit_reason", info.ExitReason)
	}
	writeKV(&b, "approval", approvalSummaryText(info))
	for _, sh := range sess.ListChildShells() {
		writeKV(&b, "shell", fmt.Sprintf("%s %s %s %s",
			handleOf(info.ID, sh.Index), sh.Status, dash(string(sh.Mode)), dash(sh.Name)))
	}
	body, code, cut, total := capGeneratedCounted(b.String(), win)
	headers := map[string]string{"Path": stmt.Path}
	noteTruncated(headers, code, cut, total, win)
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// channelSlot renders one channel's own state.
//
// The fields are the ones a caller can act on and cannot get anywhere else: the
// index (which channel this is), the mode (a pty has no exit code, a pipe has no
// size), the status, the exit code when there is one, and where the transcript
// currently ends. `ls` shows the first three for every channel at once; this is the
// one address that answers about a single one, which is what a caller has after
// copying a handle out of a reply.
//
// The byte count is the transcript length, not a reader's cursor: a cursor belongs
// to a reader, and there is no reader here. It is the number that answers "is there
// anything new since I last looked", which is the question this address is for.
func (s *Server) channelSlot(stmt directive.Statement, win termArgs, cs *session.ChildShell) envelope.Slot {
	info := cs.Info()
	var b strings.Builder
	writeKV(&b, "channel", handleOf(cs.ParentSessionID(), info.Index))
	writeKV(&b, "index", fmt.Sprint(info.Index))
	writeKV(&b, "name", dash(info.Name))
	writeKV(&b, "mode", dash(string(info.Mode)))
	writeKV(&b, "status", string(info.Status))
	// A pty has a size; a pipe does not, and printing 24x80 for one would invent a
	// fact about a channel that has no terminal.
	if info.Mode == api.ModePTY {
		writeKV(&b, "size", fmt.Sprintf("%dx%d", info.Rows, info.Cols))
	}
	if info.ExitCode != nil {
		writeKV(&b, "exit_code", fmt.Sprint(*info.ExitCode))
	}
	// THE SOURCE'S LENGTH, NOT THE BUFFER'S. `bytes` answers "is there anything new
	// since I last looked", and the buffer is a CACHE of the log (see
	// transcript.Source.Len): for a few microseconds per chunk it is behind, and
	// `get <channel>` reported a smaller number than `read` would deliver. One
	// address cannot describe a stream two ways.
	writeKV(&b, "bytes", fmt.Sprint(s.streamLen(cs)))
	writeKV(&b, "created", ageOf(info.CreatedAt))
	body, code, cut, total := capGeneratedCounted(b.String(), win)
	headers := map[string]string{"Path": stmt.Path}
	noteTruncated(headers, code, cut, total, win)
	return envelope.Slot{Code: code, Headers: headers, Body: body}
}

// channelSummary names the channels a session does have, for a 404 that has to say
// what was available instead. A bare "no channel 5" sends the caller back to `ls`.
func channelSummary(sess *session.Session) string {
	shells := sess.ListChildShells()
	if len(shells) == 0 {
		return "no channels"
	}
	parts := make([]string, 0, len(shells))
	for _, sh := range shells {
		parts = append(parts, fmt.Sprintf(":%d", sh.Index))
	}
	return strings.Join(parts, ", ")
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
	// THE TARGET DECIDES WHAT IS CLOSED. NOTHING ELSE.
	//
	// This used to route on INTENT recovered from the flags — `--force` or `--grace`
	// meant "a session" — to escape issue #132, where `close <session> --force` was a
	// silent no-op because requireShell resolved a session id to its primary channel.
	// Reading intent out of a flag is the wrong lever: a flag modifies HOW an action
	// happens, never WHICH object it happens to. `close a1f3:1 --force` therefore
	// terminated the whole session and answered 204, taking every other channel down
	// with it — a caller naming one channel had no way to see the blast radius.
	//
	// So: a `sid:N` handle names a channel, a bare session id names the container, and
	// the two vocabularies belong to their own object. `--grace` is a session's
	// deadline; `--force` is gone entirely (see below).
	h := locator.SplitHandle(stmt.Target)
	isChannel := h.Index > 0

	if isChannel {
		// `--grace` has no meaning for a channel: closing one closes one SSH channel,
		// which has no SIGTERM-then-wait protocol. Refusing is better than ignoring it,
		// because a caller who wrote it expects a deadline that will never exist.
		if stmt.FlagState("grace") != directive.FlagAbsent {
			return badRequest(stmt, "`--grace` is a session deadline; `"+stmt.Target+
				"` is a channel, and closing a channel has no SIGTERM-then-wait phase.\n"+
				"To close the whole connection use `close "+h.SessionID+" --grace N`.")
		}
		if boolFlag(stmt, "force") {
			return badRequest(stmt, "`--force` is gone: `close` stops a session and keeps it readable, `rm` erases it.\n"+
				"`"+stmt.Target+"` is a channel — `close "+stmt.Target+"` stops just that channel and keeps its output.")
		}
		if _, bad := s.requireShell(stmt.Target); bad != nil {
			return s.slotFromFailure(ctx, stmt, failureFromResult(bad))
		}
		// The channel's own name, when the caller gave it one with --name: a bare `:1`
		// says which slot closed and nothing about what was in it. Read BEFORE the
		// archive, because a shell that has just been archived is exactly the thing
		// this handle may no longer resolve — and a close must not turn its own
		// confirmation into a 404.
		name := ""
		if sh, bad := s.requireShell(stmt.Target); bad == nil && sh != nil {
			name = sh.Info().Name
		}
		// ARCHIVE, NOT ERASE. shell_close deletes the shell's metadata, which leaves
		// the transcript on disk with nothing that addresses it: the caller reads 404
		// afterwards and cannot tell a closed channel from one that never existed.
		// Closing a channel is not asking for its record to be destroyed — `rm` is.
		if _, fail := s.callTool(ctx, "shell_archive", argsOf("shell_id", stmt.Target)); fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		// A CHANNEL'S PEER STATE GOES WITH IT. If a stdio peer was being read through
		// this channel, its cursor is keyed by the handle and would otherwise outlive
		// the channel: handles are minted monotonically, so nothing ever reclaims the
		// entry and a long-lived instance grows one per channel it ever opened.
		s.dropRecvCursor(stmt.Target)
		return closedSlot(stmt, stmt.Target, name)
	}

	// A bare session id, or a locator naming one: terminate the container.
	//
	// EXISTENCE IS CHECKED BEFORE THE HANDLER, because Terminate is a no-op for an id
	// it cannot see — and the handler reports that as an operation failure, which
	// reads like "the terminate broke" rather than "there is nothing by that name".
	// Every other close path answers 404 for an unknown target (rm, fwd close,
	// profile delete); this one has to agree.
	sess, bad := s.requireSession(stmt.Target)
	if bad != nil {
		return s.slotFromFailure(ctx, stmt, failureFromResult(bad))
	}
	if boolFlag(stmt, "force") {
		return badRequest(stmt, "`--force` is gone. `close` stops a session and leaves it readable;\n"+
			"`rm "+stmt.Target+"` erases it (and needs --force while it is running).")
	}
	args := argsOf("session_id", stmt.Target)
	if slot, bad := numericFlags(args, stmt, [2]string{"grace", "grace_period"}); bad {
		return slot
	}
	// The handler resolves the id itself (including a locator) and reports a session
	// it cannot find as session_not_found rather than succeeding silently —
	// Terminate is a no-op for an id it cannot see, which is the same class of lie
	// this function exists to stop telling.
	if _, fail := s.callTool(ctx, "session_terminate", args); fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	// A SESSION'S PEERS GO WITH IT, for the same reason its channels' cursors do: the
	// transport they talk through is this session's SSH channel, so a peer that outlives
	// it is a socket to nowhere that still holds its frame ring. Doing it HERE rather
	// than only on the DEAD hook is deliberate — a graceful close is not a dead
	// transition, and this function already releases what it releases by name.
	if sess != nil {
		closePeersBySession(sess.Info().ID)
		return closedSlot(stmt, sess.Info().ID, sess.Info().Name)
	}
	return closedSlot(stmt, stmt.Target, "")
}

// directiveRemove deletes a session and its logs.
func (s *Server) directiveRemove(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	// --dry-run ANSWERS WHAT WOULD HAPPEN WITHOUT DOING IT, and it runs FIRST.
	//
	// FIRST, because a preview that can itself be refused is not a preview: the
	// running-session guard below answered 409 to a `--dry-run` call, which is
	// exactly the call a cautious caller makes BEFORE deciding whether to pass
	// --force. The preview is the place where "it is running, you will need --force"
	// is stated, not the place where it is enforced.
	//
	// The shape exists now, before review mode does: a destructive directive either
	// supports a preview or it does not, and leaving it for later means the calls
	// written today cannot use it.
	if boolFlag(stmt, "dry-run") {
		what := "session '" + stmt.Target + "' does not exist, so nothing would be erased"
		if sess, bad := s.requireSession(stmt.Target); bad == nil && sess != nil {
			info := sess.Info()
			what = "would erase session '" + stmt.Target + "' (status " + string(info.Status) +
				"): manifests, transcript and every trace. This cannot be undone."
			if info.Status == api.SessionRunning {
				what += "\nIt is running, so the real call needs --force."
			}
		}
		return envelope.Slot{
			Code:    200,
			Headers: map[string]string{"Path": stmt.Path, "Dry-Run": "true"},
			Body:    what,
		}
	}

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
	// One line per span, and a busy channel has thousands of them: this is the reply
	// most able to flood a context, so it obeys the same window as everything else.
	return generatedSlot(stmt, win, b.String())
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
		return badRequest(stmt, "reader --drop needs a reader id: `reader <channel> --drop <id>`")
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
		return held.slot(stmt.Path)
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
		return badRequest(stmt, "file needs --action read|write|replace|stat|delete|rename|mkdir|urls|perm|link|fs|getwd, got "+quoteOrDash(action))
	}
	if slot, refused := s.refuseAction(ctx, stmt, capID); refused {
		return slot
	}

	args := argsOf("session_id", stmt.Target)
	if slot, bad := stringFlags(args, stmt,
		[2]string{"remote-path", "remote_path"},
		[2]string{"from-path", "from_path"},
		[2]string{"to-path", "to_path"},
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
	// --encoding IS THE SAME FLAG THE CHANNEL READ USES, and on a file read it names the
	// interpretation of the file's bytes. It was declared for `read`, accepted on a file
	// read, and never passed on — so a caller who named an encoding was handed a
	// rendering of the bytes as if they were UTF-8, with no way to tell (issue #177: a
	// PowerShell log is UTF-16LE with a BOM by default).
	if slot, bad := stringFlags(args, stmt, [2]string{"encoding", "charset"}); bad {
		return slot
	}
	if slot, bad := stringFlags(args, stmt,
		[2]string{"target", "target"},
		[2]string{"link-path", "link_path"},
		[2]string{"existing-path", "existing_path"},
		[2]string{"new-path", "new_path"},
		[2]string{"old-text", "old_text"},
		[2]string{"new-text", "new_text"},
	); bad {
		return slot
	}
	if slot, bad := numericFlags(args, stmt,
		[2]string{"offset", "offset"},
		[2]string{"length", "length"},
		[2]string{"size", "size"},
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
		// tool.
		//
		// A missing flag yields "", not nil — the previous `== nil` test therefore
		// never fired and the sub-action was silently empty, which is what made
		// these three reachable only as "action must be chmod, chown, or chtimes".
		//
		// THERE IS NO --action-verb FALLBACK HERE ANY MORE, and there never needed
		// to be: the parser refuses a flag the directive does not declare, so a call
		// carrying --action-verb died at parse time and this branch was unreachable.
		// It read as a supported spelling and was not one. The name is in the
		// directive's RetiredFlags now, so the refusal says what to write instead.
		switch action {
		case "perm", "link", "fs":
			sub, _ := stmt.Flag("op")
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
		return held.slot(stmt.Path)
	}
	doc, ok := toolResultMap(res)
	if !ok {
		text := toolResultText(res)
		return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path}, Body: text}
	}
	if taskID := getString(doc, "task_id", ""); taskID != "" {
		// A TRANSFER IS A RECEIPT (202 Accepted). The transfer has started in the
		// background; the reply names the task id and tells the caller how to wait
		// for it. A watchdog of 30s can now move a 30 MB binary without being killed.
		v, err := s.tasks.Get(taskID)
		body := ""
		if err == nil {
			body = renderTask(v) + "\n`get task " + taskID + "` waits for it (the wait is the call's `timeout`)."
		} else {
			body = "started " + taskID + "\n`get task " + taskID + "` waits for it."
		}
		return envelope.Slot{
			Code: 202,
			Headers: map[string]string{
				"Path": stmt.Path, "Task": taskID, "Status": "running",
			},
			Body: body,
		}
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
	case "replace":
		return "file_replace", capability.FileWrite, true
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

// directiveHost maps `host` onto the stored connection targets.
func (s *Server) directiveHost(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	action, rest := resolveAction(stmt, "list")
	if action == "get" {
		// Reading one entry needs no capability: it is the same information as the
		// listing, with the secrets redacted.
		name := strings.TrimSpace(stmt.Key)
		if name == "" && len(rest) > 0 {
			name = rest[0]
		}
		if name == "" {
			return badRequest(stmt, "get host needs a name: `get host <name>`")
		}
		res, fail := s.callTool(ctx, "ssh_config", map[string]any{"action": "get", "name": name})
		if fail != nil {
			return s.slotFromFailure(ctx, stmt, fail)
		}
		doc, _ := toolResultMap(res)
		return generatedSlot(stmt, win, compactDocument(doc))
	}
	capID, ok := profileCapability(action)
	if !ok {
		return badRequest(stmt, "host needs --action list|get|create|edit|copy|delete, got "+quoteOrDash(action))
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
	// A BASTION CARRIES ITS OWN CREDENTIAL. validateDialSpec requires one, and until
	// these flags existed every jump `create host` could express was refused — the
	// three jump flags above were reachable only in theory.
	if slot, bad := stringFlags(args, stmt,
		[2]string{"jump-password", "jump_password"},
		[2]string{"jump-private-key", "jump_private_key"},
		[2]string{"jump-key-passphrase", "jump_key_passphrase"},
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
		return held.slot(stmt.Path)
	}
	if action == "list" {
		// The profile listing is a set of names, one per line: copy-pasteable into
		// the next `pty`/`pipe`.
		//
		// THROUGH THE WINDOW, like every other generated reply. It used to build its own
		// slot, so `bytes_limit: 60` on a six-entry table answered 200 with the whole
		// thing: a parameter accepted and then not applied, which is the same failure as
		// a flag with no effect.
		return generatedSlot(stmt, win, listFromDoc(res, "ssh_configs", "name"))
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
		// The idle window of an on-demand extension. A string, not a number: it is a
		// duration the caller reads and writes the way it thinks about it ("10m"),
		// and the template stores exactly what was written.
		[2]string{"idle-timeout", "idle_timeout"},
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
		return held.slot(stmt.Path)
	}
	if action == "list" {
		// The boot manifest: one extension per line, with its state and handle, so
		// a caller that lost its context gets its toolset back in one call. Capped like
		// the rest: a host with twenty installed extensions is a reply, not a receipt.
		return generatedSlot(stmt, win, s.extensionListing(res, boolFlag(stmt, "all")))
	}
	doc, _ := toolResultMap(res)
	return generatedSlot(stmt, win, compactDocument(doc))
}

// directiveExtensionUse is `open ext <name>`: the ask that starts an on-demand
// extension and hands back its handles (issue #183).
//
// It renders the same document `ext get` does — template plus runtime — because
// the caller that just started an extension needs both halves in one reply: the
// notes are the operating instructions, and the handles are what they are for.
// The handle is also a header, which is how `open` answers everywhere else.
func (s *Server) directiveExtensionUse(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	name := strings.TrimSpace(stmt.Key)
	if name == "" {
		return badRequest(stmt, "open ext needs a name: open ext <name>")
	}
	// WORDS THIS FORM CANNOT HONOR ARE REFUSED, NOT IGNORED. `open ext boxfs extra`
	// and `open a1f3 ext boxfs` parse (the kind word is taken first, so what looks
	// like a target becomes one), and an extension's container is the reconciler's —
	// a caller cannot name it. Reading past either word would answer a question the
	// caller did not ask.
	if stmt.Target != "" {
		return badRequest(stmt, "open ext names no session: the extension's connection belongs to the reconciler. "+
			"Write `open ext "+name+"`, and drive the handles it returns.")
	}
	if len(stmt.Args) > 0 {
		return badRequest(stmt, "open ext takes one name, got extra word "+quoteOrDash(stmt.Args[0]))
	}
	if slot, refused := s.refuseAction(ctx, stmt, capability.ExtGet); refused {
		return slot
	}
	res, fail := s.callTool(ctx, "extension", map[string]any{"action": "use", "name": name})
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
	}
	doc, _ := toolResultMap(res)
	slot := generatedSlot(stmt, win, compactDocument(doc))
	if handle := shortHandle(doc); handle != "" {
		slot.Headers["Handle"] = handle
	}
	return slot
}

// directiveExtensionRelease is `close ext <name> [<channel>]`: the caller giving an
// instance back (issue #183).
//
// It exists because the idle window is a backstop, not the plan. A caller that has
// finished with an MCP server or a prompt extension knows that a moment after the
// daemon could infer it, and saying so stops the process on the target now instead
// of ten minutes from now.
func (s *Server) directiveExtensionRelease(ctx context.Context, stmt directive.Statement, win termArgs) envelope.Slot {
	name := strings.TrimSpace(stmt.Key)
	if name == "" {
		return badRequest(stmt, "close ext needs a name: close ext <name> [<shell_id>]")
	}
	if slot, refused := s.refuseAction(ctx, stmt, capability.ExtGet); refused {
		return slot
	}
	args := map[string]any{"action": "release", "name": name}
	// The channel id arrives in the key position after the name, or as --channel:
	// both are spellings of the same thing, and the second exists for a caller that
	// pasted a long id.
	handle := stmt.Target
	if v, ok := stmt.Flag("channel"); ok && v != "" {
		handle = v
	}
	if len(stmt.Args) > 0 && handle == "" {
		handle = stmt.Args[0]
	}
	if handle != "" {
		args["shell_id"] = handle
	}
	res, fail := s.callTool(ctx, "extension", args)
	if fail != nil {
		return s.slotFromFailure(ctx, stmt, fail)
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
	case "retry", "restart":
		return capability.ExtRetry, true
	case "use":
		// Starting an extension is the way to reach it, so the capability that
		// already governs reaching it is the one that governs starting it. A
		// separate bit would be a second place to grant the same access.
		return capability.ExtGet, true
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

// unquoteWord strips one layer of matching shell quotes, so `'{"a":1}'` and
// `"{\"a\":1}"` both hand the same text to the JSON parser. An unquoted word is
// returned unchanged.
// unquoteWord removes one layer of the caller's own quoting.
//
// That is ALL it does, and the restraint is the point. It is tempting to unescape
// while it is here — inside double quotes a shell would read `\"` as `"` — but
// unescaping is EXPANSION, and this layer is forbidden from evaluating. The parser
// agrees: its Lit.Value for a double-quoted part is the raw bytes, backslashes
// included, because turning them back into the bytes they stand for is the
// interpreter's job (mvdan.cc/sh/v3/expand), which would also expand the `$` in a
// perfectly ordinary JSON body like `{"$schema":"..."}`.
//
// So a backslash is a byte here, exactly as it is everywhere else in a payload, and
// a caller whose frame needs quotes writes it the way the JSON grammar wants it.
// Single quotes have no escapes, which is the shell's rule as well and is why the
// single-quoted spelling is the safe one to recommend.
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
			// A NOUN IS NOT A VERB, BUT IT IS A WORD IN THE GRAMMAR.
			//
			// `help file` answered "unknown directive: file" — true, and useless to a
			// caller holding that word from `ls <session> file`. The tree's nouns
			// (file, host, session, channel, forward, peer) are how a caller thinks
			// about the surface, while help is indexed by verb, so the word that sends
			// them here is the one word this page could not answer.
			//
			// The answer is DERIVED from the usage lines, not tabulated: every line that
			// names the word is a page of it. A hand-kept map would drift the first time
			// a verb was added, which is the same reason the flag list is derived.
			if lines := usageLinesNaming(stmt.Args[0]); lines != "" {
				// A NODE'S VERBS ARE NOT ALWAYS ITS WHOLE STORY. They are derived from the
				// usage lines, which is right; but a capability spelled as a flag on an
				// existing verb has no line to be derived from. `ext` has one — an extension
				// can be rebuilt — and this page listed five verbs, none of which said so,
				// which is how an agent ended up with no way to refresh one after a deploy.
				body := stmt.Args[0] + " is a node, not a verb. These act on it:\n" + lines
				if note, ok := directive.NodeNote(stmt.Args[0]); ok {
					body += "\n" + note + "\n"
				}
				return envelope.Slot{Code: 200, Headers: map[string]string{"Path": stmt.Path},
					Body: body}
			}
			// AN ARGUMENT IS A WORD IN THIS GRAMMAR TOO.
			//
			// The guide names `args`, `vars` and `secrets` in its first screen, so the next
			// thing a caller types is `help secrets` — which answered "unknown directive:
			// secrets" while the page already existed, in the argument table that the tool
			// schema and the generated docs are both rendered from. Deriving this page from
			// that table is the same rule the flag list follows: a hand-kept copy would
			// drift the first time an argument was added, and the one that matters most is
			// the one a caller reads BEFORE typing a credential.
			if decl, ok := termArgDeclFor(stmt.Args[0]); ok {
				body := decl.Name + " is an argument of the `term` tool, not a directive.\n\n" + decl.Desc + "\n"
				if decl.DocNote != "" {
					body += "\n" + decl.DocNote + "\n"
				}
				capped, code, cut, total := capGeneratedCounted(body, win)
				headers := map[string]string{"Path": stmt.Path}
				noteTruncated(headers, code, cut, total, win)
				return envelope.Slot{Code: code, Headers: headers, Body: capped}
			}
			// 400, NOT 404 — AND THE SAME 400 THE BARE WORD GETS.
			//
			// Typing the word directly answered 400 ("this line is malformed"), while
			// asking `help <word>` answered 404 ("this resource does not exist"). Same
			// body, same situation, two answers: a caller that branches on the status
			// concludes opposite things about the same missing verb depending on
			// whether it asked for help first. Nothing is a resource here; the caller
			// named a directive this build does not have, which is a bad request.
			return envelope.Slot{Code: 400, Headers: map[string]string{"Path": stmt.Path, "Error-Code": "unknown_directive"},
				Body: "unknown directive: " + stmt.Args[0]}
		}
		body := op.Usage + "\n"
		// THE FLAG LIST IS DERIVED, NOT WRITTEN. A verb with twenty-five flags cannot
		// spell them out in a usage line, and a hand-kept list drifts the moment one is
		// added — the check that each flag is documented somewhere then fails late and
		// vaguely. Printing the declared set makes the page true by construction.
		if flags := flagList(op); flags != "" {
			body += "\n" + flags + "\n"
		}
		// THE PAGE IS OPT-IN. `help read` was 10683 bytes with no smaller question to
		// ask, so a caller who wanted to know what `--grep` takes paid for the essay —
		// and the verbs with the longest pages are the ones with the most flags to ask
		// about. The summary answers "which flag does what" and names the page,
		// including its size, so spending it is a decision rather than a discovery.
		if table := flagTable(op); table != "" && !boolFlag(stmt, "all") {
			// The page is what the template, the flag list and the detail render to —
			// measured from the body BEFORE the table is appended, or the summary
			// quotes a price that includes itself.
			//
			// BOTH "+ 1"s ARE THE NEWLINES: the page is the template, a newline, the detail
			// and a final newline, and dropping either one quotes a price one byte off the
			// bill. The test only catches this when the reply under test is WHOLE — under the
			// default lines_limit it is cut, and then the quoted size and the delivered
			// body differ by the cut, which looks like the same off-by-one and is not.
			pageSize := len(body) + 1 + len(op.Detail) + 1
			body += "\n" + table
			if example, ok := directive.SummaryExample(op.Name); ok {
				body += "\n" + example + "\n"
			}
			// The number is the size `--all` will actually return, not the length of
			// op.Detail: the page is the template, the flag list and the detail, and a
			// caller deciding whether to spend a window is being quoted a price that has
			// to match the bill. The test asserts exactly this.
			body += fmt.Sprintf("\nThe full page is %d bytes: `help %s --all`.\n", pageSize, op.Name)
		} else if op.Detail != "" {
			// The detail is a page, so it follows the one-line template instead of
			// replacing it: the template is what a caller scans, the page is what it
			// reads once it has decided to produce the thing.
			body += "\n" + op.Detail + "\n"
		}
		capped, code, cut, total := capGeneratedCounted(body, win)
		headers := map[string]string{"Path": stmt.Path}
		noteTruncated(headers, code, cut, total, win)
		return envelope.Slot{Code: code, Headers: headers, Body: capped}
	}
	var b strings.Builder
	for _, op := range directive.Ops() {
		b.WriteString(op.Usage)
		b.WriteByte('\n')
	}
	capped, code, cut, total := capGeneratedCounted(b.String(), win)
	headers := map[string]string{"Path": stmt.Path}
	noteTruncated(headers, code, cut, total, win)
	return envelope.Slot{Code: code, Headers: headers, Body: capped}
}

// termGuide is the empty-call answer: where am I, what can I do, and what is here.
//
// A caller that has just been woken up knows nothing, so this call has to be legal,
// cheap, and dense — never an error. It is the first move of every recovery, and
// the recovery path must not depend on remembering anything.
func (s *Server) termGuide(ctx context.Context, win termArgs) termReply {
	var b strings.Builder

	b.WriteString("termcp — a shell-like DSL controlling hosts over SSH.\n")
	b.WriteString("Local (\"internal\", if enabled) and remote targets are treated identically.\n\n")

	// THE SHORTEST PATH COMES FIRST. This page used to open on the whole worldview —
	// six node kinds, two centres, nineteen verbs — which is the right reference and
	// the wrong first screen: a caller who only wants a shell and one command read
	// it, then asked "so what do I type first?". Three lines answer that before the
	// grammar arrives; everything below is for the second visit.
	b.WriteString("SHORTEST PATH — `run` does the whole loop, one call:\n")
	b.WriteString("  ls host                      what is there (local and ssh, in columns)\n")
	b.WriteString("  connect <host>               opens a channel, answers its channel (a1f3:1)\n")
	b.WriteString("  run a1f3:1 $1                types the line, presses Enter, reads what came back\n")
	// THE ONE LINE THAT PREVENTS THE WHOLE CLASS OF `run` MISREADING: the bytes go to
	// whoever owns the channel's input, not to a queue. A caller that thinks it is
	// submitting a job reads an eaten command as a successful one.
	b.WriteString("                               into the TERMINAL: whoever owns the input reads it\n")
	b.WriteString("                               (a program waiting for an answer TAKES your line).\n")
	// THE INTERFACE IS IMPRECISE, AND THAT IS THE INTERFACE. A caller that wants a
	// precise state (did it run? is something in the foreground?) is asking the terminal
	// for something a person at the same screen never had either — they read the screen.
	// Saying so here costs two lines and prevents the whole class of "the reply does not
	// tell me" reports about a pty.
	b.WriteString("                               A terminal is not precise: the bytes in the reply\n")
	b.WriteString("                               ARE the state — read them as you would a screen.\n")
	b.WriteString("                               `help run` has what that means for the reply.\n")
	b.WriteString("  args: [\"make && test\"]\n")
	b.WriteString("  input/key/read are the same three steps taken separately, for a TUI or a\n")
	b.WriteString("  long-running command you want to watch between calls.\n\n")

	b.WriteString("cmd = the DSL.  sh = a shell command.\n")
	b.WriteString("cmd holds directives only. sh never appears in cmd — it enters as a value through\n")
	b.WriteString("args ($1, $2) or vars ($NAME). cmd supports &&, ||, ;, and &.\n")
	// A CREDENTIAL HAS TO HAVE SOMEWHERE TO GO THAT IS NOT THE LOG, and the caller has to
	// know it exists before it types the password into args. One line, where the two
	// other value channels are named.
	b.WriteString("A value that must not be written to the server log goes in secrets ($NAME too);\n")
	b.WriteString("a password belongs there, never typed into cmd or args. The remote shell still\n")
	b.WriteString("echoes what it runs — that is the channel's transcript.\n")
	b.WriteString("  run a1f3:1 $1   args: [\"make && test\"]   one sh line, operators live\n")
	b.WriteString("  run a1f3:1 $1 && notify done             two cmd directives\n\n")

	// THE TREE IS THE GRAMMAR. Every statement is a verb plus the address of the
	// node it works on, so learning the tree teaches the whole surface: a caller who
	// knows `host -> session -> child` can read any usage line without being told
	// which word is a verb and which is a noun.
	b.WriteString("STRUCTURE — one tree, and every verb names the node it acts on:\n")
	b.WriteString("  host     a saved connection target. `ls host` shows them.\n")
	b.WriteString("  session  one SSH link. `connect <host>` makes one; `ls` lists live ones.\n")
	b.WriteString("  channel  one shell on that link, <session>:<index> (e.g. a1f3:1).\n")
	b.WriteString("           `open <session>` adds one; the mode follows ssh — --argv means\n")
	b.WriteString("           a command with no terminal, no --argv means a terminal.\n")
	b.WriteString("  file     <session> file <path>      — read/write carry BYTES; get carries\n")
	b.WriteString("           metadata and the upload/download urls; rm, create\n")
	b.WriteString("  forward  <session> forward <id>     — open/close/ls\n")
	b.WriteString("  peer     <session> mcp|cdp|jrpc <url> — open/read/call/close, through that\n")
	b.WriteString("           session's network, so localhost means the host's localhost.\n")
	b.WriteString("  Copy addresses from `ls`; never spell one.\n\n")

	// THE POINTER IS CONDITIONAL, and that is the fix a first-time caller asked for:
	// the guide used to open this section with "`ls ext` CAPABILITY HUB — start here",
	// so the loudest instruction in the whole reply led to "(no extensions installed)"
	// on an instance that has none. A recommendation that reads as "start here" has to
	// be true on the instance answering, and this function already looks up live state
	// (the `ls` block below), so it can look this up too.
	ext := s.directiveExtension(ctx, directive.Statement{Op: "ls", Kind: "ext", Target: "ext", Path: "ls ext"}, win)
	if ext.Code >= 200 && ext.Code < 300 && !strings.Contains(ext.Body, "no extensions") {
		b.WriteString("TWO CENTRES:\n")
		b.WriteString("  ls ext  CAPABILITY HUB — start here. Lists resident, capability-trimmed\n")
		b.WriteString("          extensions: mcp servers, skills (read-only SFTP), prompts.\n")
		b.WriteString("          Notes inside an extension are its operating manual.\n")
		b.WriteString("  ls      SSH STATE & LIFECYCLE — every link, channel, forward and peer.\n")
		b.WriteString("          Pass learn_mode:true and it expands the verbs that take the\n")
		b.WriteString("          addresses it just printed.\n\n")
	} else {
		b.WriteString("WHERE TO LOOK:\n")
		b.WriteString("  ls host  every saved connection target.\n")
		b.WriteString("  ls       SSH STATE & LIFECYCLE — every link, channel, forward and peer.\n")
		b.WriteString("           Pass learn_mode:true and it expands the verbs that take the\n")
		b.WriteString("           addresses it just printed.\n")
		b.WriteString("  ls ext   extensions (mcp servers, skills, prompts). None are installed\n")
		b.WriteString("           on this instance, so there is nothing to start with there.\n\n")
	}

	b.WriteString("ACTIONS:\n")
	for _, op := range directive.Ops() {
		if !op.Core {
			continue
		}
		b.WriteString("  " + op.Usage + "\n")
	}
	b.WriteString("  notify <text>           reach human before sudo/password/choice, then wait\n\n")

	b.WriteString("`help` lists all " + strconv.Itoa(len(directive.Ops())) + " verbs; `help <verb>` is a page.\n")
	b.WriteString("Pass learn_mode:true on any call and its usage arrives with the reply.\n")

	// Live state: ps listing
	ps := s.directivePS(ctx, directive.Statement{Op: "ls", Path: "ls"}, win)
	b.WriteString("\nrunning now (from ls):\n")
	if strings.TrimSpace(ps.Body) == "" {
		b.WriteString("  (nothing — `connect internal` opens a local shell; `ls host` shows targets)\n")
	} else {
		for _, line := range strings.Split(strings.TrimRight(ps.Body, "\n"), "\n") {
			b.WriteString("  " + line + "\n")
		}
	}
	body, code, cut, total := capGeneratedCounted(b.String(), win)
	headers := map[string]string{}
	noteTruncated(headers, code, cut, total, win)
	return termReply{win: win, slots: []envelope.Slot{{Code: code, Headers: headers, Body: body}}}
}

// usageLinesNaming returns every usage line that names word as a word, or "".
//
// Matching is on WORD BOUNDARIES: `help session` must not match `sessions`, and a
// noun that happens to be a substring of another verb's argument (`host` inside
// `--host`) is not what the caller typed. A line that names the word twice is still
// printed once — the caller wants the verbs, not the count.
func usageLinesNaming(word string) string {
	word = strings.TrimSpace(word)
	if word == "" {
		return ""
	}
	var b strings.Builder
	seen := map[string]bool{}
	for _, op := range directive.Ops() {
		if seen[op.Usage] {
			continue
		}
		for _, field := range strings.FieldsFunc(op.Usage, func(r rune) bool {
			return !(r == '_' || r == '-' || r == ':' || unicode.IsLetter(r) || unicode.IsDigit(r))
		}) {
			if field == word {
				seen[op.Usage] = true
				b.WriteString(op.Usage + "\n")
				break
			}
		}
	}
	return b.String()
}

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
		// WHERE IT IS, on the same line as the name. The kind column is the one that
		// separates "the machine I am on" from "a machine I reach over ssh" — the
		// first thing a reader of `ls host` is deciding — and `where` answers it
		// (a local OS, or the user@host:port a remote entry dials).
		if kind := getString(item, "kind", ""); kind != "" {
			b.WriteString("\t" + kind)
			b.WriteString("\t" + dash(getString(item, "where", "")))
		}
		if desc := getString(item, "description", ""); desc != "" {
			b.WriteString("\t" + desc)
		}
		// A BROKEN ENTRY SAYS WHY, on the same line as its name. The column exists
		// because a name alone cannot answer "can I connect to this?" — the reader of
		// a listing is deciding what to use, and the reason is the part that decides.
		if why := getString(item, "broken", ""); why != "" {
			b.WriteString("\tbroken: " + why)
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
//
// THE HANDLE IS THE ADDRESS, NOT THE RAW CHANNEL ID (#159 D2). The reconciler
// reports a shell id (`9ff338e1-68b`), which is a UUID fragment: it identifies the
// channel but is not what the rest of the surface takes. An address is
// `<session>:<index>` — the same spelling `ps` prints and every later command
// pastes — so a caller that copies this row can act on it directly instead of
// calling `get ext` to translate.
func (s *Server) extensionListing(res *mcpgo.CallToolResult, all bool) string {
	doc, ok := toolResultMap(res)
	if !ok {
		return toolResultText(res)
	}
	items, _ := doc["extensions"].([]any)
	// THE LIST CAPS ITSELF, AND NAMES WHAT IT WITHHELD (issue #185).
	//
	// The byte window would cut forty rows mid-line and report it in a header about
	// bytes: "Truncated: 900 bytes withheld" does not tell a caller whether it is
	// missing one extension or thirty, and the one it needs may be in the part it did
	// not see. A count plus a command is something it can act on; "and more" is a wall.
	//
	// This is a VIEW, not a limit on what can be reached: `ls ext --all` lists every
	// one, and an extension that is not shown is still addressable by name.
	const listed = 10
	hidden := 0
	if !all && len(items) > listed {
		hidden = len(items) - listed
		items = items[:listed]
	}
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
			// The address when it can be derived, the id otherwise: a shell-less
			// extension (an SFTP-only window) has no channel and reports its
			// session, which is exactly the address a file read takes.
			handle = s.addressOf(getString(item, "session_id", ""), shell)
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
	if hidden > 0 {
		b.WriteString(fmt.Sprintf("(%d more not shown; `ls ext --all` lists every extension)\n", hidden))
	}
	return b.String()
}

// addressOf turns a session id and a channel id into the address the rest of the
// surface speaks: `<session>:<index>`.
//
// It falls back to the channel id when the session is gone or the index is unknown
// — a restored manifest can predate the index — because a handle that is merely
// less convenient still lets the caller look the channel up, while an empty column
// says nothing at all.
func (s *Server) addressOf(sessionID, shellID string) string {
	if sessionID == "" || shellID == "" {
		return dash(shellID)
	}
	sess := s.sessMgr.Get(sessionID)
	if sess == nil {
		return shellID
	}
	for _, sh := range sess.ListChildShells() {
		if sh.ID == shellID && sh.Index > 0 {
			return handleOf(sessionID, sh.Index)
		}
	}
	return shellID
}

// capGenerated applies the byte and line windows to a generated reply, in that
// order, and reports whether it had to cut.
// generatedSlot renders a generated body through the caller's window: the same
// three axes that bound a stream bound a listing or a document.
func generatedSlot(stmt directive.Statement, win termArgs, body string) envelope.Slot {
	capped, code, cut, total := capGeneratedCounted(body, win)
	headers := map[string]string{"Path": stmt.Path}
	noteTruncated(headers, code, cut, total, win)
	return envelope.Slot{Code: code, Headers: headers, Body: capped}
}

func noteTruncated(headers map[string]string, code, cut, total int, win termArgs) {
	if code != 200 && total > 0 {
		// The same Content-Range a streamed read uses, saying only how long the reply
		// would have been. A generated reply has no cursor, so `bytes */N` is the whole
		// of what it can say — but that N is what lets a caller price the page instead
		// of discovering the size by paying it. The documented cheap form
		// (`bytes_limit: 0`) used to answer with an empty 204 carrying no size at all,
		// while the same call on a channel answered `bytes */2593`.
		headers["Content-Range"] = envelope.UnsatisfiedRange(int64(total))
	}
	if cut <= 0 {
		return
	}
	headers["Truncated"] = fmt.Sprintf("%d bytes withheld by this call's window (bytes_limit %d, lines_limit %d); raise the window to see the rest — a generated reply has no cursor to continue from",
		cut, win.BytesLimit, win.LinesLimit)
}

func capGenerated(body string, win termArgs) (string, int) {
	capped, code, _, _ := capGeneratedCounted(body, win)
	return capped, code
}

// capGeneratedCounted is capGenerated plus HOW MUCH IT TOOK, because a listing cut by
// the caller's own byte window used to be indistinguishable from a complete one.
//
// A generated reply has no stream, so there is no cursor to page it with and no
// Content-Range to say where it stopped: `ls host` under `bytes_limit: 60` answered 200
// with three of six entries, and the only hint was that the reply was shorter than the
// limit — which a caller reading a table it has never seen cannot notice. The count is
// what a header can be built from.
//
// The unit is bytes, not entries: a fold ("too big, ask for less") withholds the whole
// body, and reporting a count there would suggest a partial answer that does not exist.
// foldOrTruncate already appends its own notice to a truncated body; this number is for
// the reply's headers, which is where a caller looks before reading the body at all.
func capGeneratedCounted(body string, win termArgs) (capped string, code, cut, total int) {
	total = len(body)
	if win.BytesLimit == 0 {
		// The schema's zero-byte window: streamed replies hand back a handle and
		// consume nothing; a generated reply has no stream, so it is simply empty.
		// The total still travels, because "how big is this page" is the one fact a
		// caller needs before deciding to spend a window on it.
		return "", 204, 0, total
	}
	code = 200
	cut = 0
	if win.BytesLimit > 0 && len(body) > win.BytesLimit {
		before := len(body)
		body = foldOrTruncate(body, win.BytesLimit, win.OnBytesOverflow)
		code = 206
		if len(body) < before {
			cut = before - len(body)
		}
	}
	if win.LinesLimit > 0 {
		if lines := strings.Count(body, "\n"); lines > win.LinesLimit {
			before := len(body)
			body = foldOrTruncateLines(body, win.LinesLimit, win.OnLinesOverflow)
			code = 206
			if len(body) < before {
				cut += before - len(body)
			}
		}
	}
	return body, code, cut, total
}

// setFlagString copies a string flag into a handler argument.
//
// It copies EVERY explicit value, including an empty one, and reports a flag written
// without a value instead of dropping it.
//
// The old `ok && v != ""` test is why an explicitly empty `--data` could not write
// an empty file: the caller's explicit empty value was discarded here, the handler
// saw no `data` key at all, and answered that no data had been given — for a flag
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

// resolveAction reads the action the dispatcher put on the statement, and returns
// whatever positional words are left over.
//
// IT USED TO GUESS. Two spellings existed — `profile create demo` (action as the
// first positional word) and `profile demo --action edit` (action as a flag) — so
// "which word is the name" depended on which spelling was used, and the old code
// hardcoded one of them. The dispatcher now derives the action from the resource
// tree rather than from the caller's word order, so there is nothing left to guess:
// the action is always a flag, always set, and the words that remain are data.
func resolveAction(stmt directive.Statement, fallback string) (action string, rest []string) {
	if v, ok := stmt.Flag("action"); ok && v != "" {
		return v, stmt.Args
	}
	return fallback, stmt.Args
}

// flagList renders a verb's declared flags, marking the ones that take no value.
//
// It reads the SAME table the parser reads, so a flag that appears here is a flag
// that parses: there is no second list to keep in step.
func flagList(op directive.Op) string {
	if len(op.Flags) == 0 {
		return ""
	}
	// The names, not the declarations: the domain annotation is how the table records
	// what a flag accepts, and `--cursor:number` is not a flag a caller can pass.
	names := op.FlagNames()
	valueless := make(map[string]bool, len(op.Valueless))
	for _, v := range op.Valueless {
		valueless[v] = true
	}
	w := make([]string, 0, len(names))
	for _, f := range names {
		if valueless[f] {
			w = append(w, "--"+f)
			continue
		}
		w = append(w, "--"+f+" <value>")
	}
	sort.Strings(w)
	return "Flags: " + strings.Join(w, "  ")
}

// flagTable renders one line per declared flag: what it takes, and what happens if
// you get it wrong.
//
// It walks op.Flags rather than the summary map, for the reason flagList is derived
// too: a hand-kept list drifts the moment a flag is added, and the drift is silent —
// the flag simply stops being mentioned. Walking the declaration makes the table
// complete by construction.
//
// A verb with no summaries renders nothing, which is how it keeps its full page: the
// surface migrates one verb at a time, and a verb that has not migrated behaves
// exactly as it did before this existed.
func flagTable(op directive.Op) string {
	if !directive.HasSummaries(op.Name) {
		return ""
	}
	valueless := make(map[string]bool, len(op.Valueless))
	for _, v := range op.Valueless {
		valueless[v] = true
	}
	spellingOf := func(f string) string {
		if valueless[f] {
			return "--" + f
		}
		return "--" + f + " <value>"
	}
	// The names, not the declarations: the summary map is keyed by the name, so the raw
	// entries would render a column of `--cursor:number` with nothing beside it.
	names := op.FlagNames()
	// The column is measured, not guessed: a fixed width misaligns the moment a flag
	// spelling outgrows it, which is how `--from-line <value>` pushed its line out.
	width := 0
	for _, f := range names {
		if n := len(spellingOf(f)); n > width {
			width = n
		}
	}
	w := make([]string, 0, len(names))
	for _, f := range names {
		line, _ := directive.FlagSummary(op.Name, f)
		w = append(w, fmt.Sprintf("  %-*s  %s", width, spellingOf(f), line))
	}
	sort.Strings(w)
	return strings.Join(w, "\n") + "\n"
}

// peerLines renders the open peers of the sessions in a listing.
//
// The address is `<session> <kind>` and is printed verbatim: it is what `call`,
// `read`, `get` and `close` accept, so a caller can copy a line out of `ls` into the
// next command without translating anything. The session half is the id in column 1
// of the session's own line, which is why the id is repeated rather than implied.
func peerLines(sessions []api.Session) string {
	if len(sessions) == 0 {
		return ""
	}
	want := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		want[sess.ID] = true
	}
	jrpcChannelsMu.Lock()
	handles := make([]*jrpcHandle, 0, len(jrpcChannels))
	for _, h := range jrpcChannels {
		if want[h.sessionID] {
			handles = append(handles, h)
		}
	}
	jrpcChannelsMu.Unlock()
	if len(handles) == 0 {
		return ""
	}
	sort.Slice(handles, func(i, j int) bool {
		if handles[i].sessionID != handles[j].sessionID {
			return handles[i].sessionID < handles[j].sessionID
		}
		return handles[i].kind < handles[j].kind
	})
	var b strings.Builder
	b.WriteString("\npeers (address, transport, endpoint, attached):\n")
	for _, h := range handles {
		b.WriteString("  " + peerAddress(h.sessionID, h.kind, h.index) + "\t" +
			h.ch.Transport().String() + "\t" + h.ch.URL())
		if h.cdp != nil {
			b.WriteString(fmt.Sprintf("\t%d page(s) attached", len(h.cdp.snapshot())))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// streamLen is one channel's stream length, asked of the record rather than of the
// cache in front of it.
//
// It falls back to the buffer when there is no log to ask (a session with persistence
// off), which is the same answer the cache gives and the only one available there.
func (s *Server) streamLen(cs *session.ChildShell) int64 {
	n := cs.BufferLen()
	if s.msgMgr == nil {
		return n
	}
	sessID := cs.ParentSessionID()
	if sessID == "" {
		return n
	}
	if m, err := s.msgMgr.OutputSize(sessID, cs.ID); err == nil && m > n {
		return m
	}
	return n
}
