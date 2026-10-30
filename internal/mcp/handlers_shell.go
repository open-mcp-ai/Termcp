package mcp

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/locator"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/transcript"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func (s *Server) handleStartSession(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	command := getString(args, "command", "")
	toolArgs := getStringSlice(args, "args")
	if strings.TrimSpace(command) == "" && len(toolArgs) > 0 {
		return toolError(CodeInvalidArgument, "%s", "command is required when args are provided"), nil
	}
	if bad, _ := validateStartParams(args); bad != nil {
		return bad, nil
	}

	cfgName, ent, remote, err := s.resolveSSHFromArgs(args)
	if err != nil {
		return toolError(sshConfigErrCode(err), "%s", err.Error()), nil
	}

	cmd, execArgs := sshconfig.EffectiveCommand(ent, command, toolArgs)
	if strings.TrimSpace(cmd) == "" && len(execArgs) > 0 {
		return toolError(CodeInvalidArgument, "%s", "command is required when args are provided"), nil
	}

	// A PROFILE'S SHELL MUST EXIST, AND THAT IS CHECKABLE BEFORE ANYTHING STARTS.
	//
	// issue #154: a profile with default_shell = "nosuchshell_xyz" answered 200 with a
	// handle, and `ls` a moment later showed the channel already `exited` — the caller
	// was promised a pty and got a corpse, with the reason only visible by reading the
	// channel. For automation the difference is everything: 200 means "you have a
	// shell".
	//
	// The check is the PROFILE's shell, not the caller's command. A command is allowed
	// to be anything and to exit immediately (`--argv echo hi` is a working one-shot),
	// so a session that dies quickly is not by itself a failure. A profile is different:
	// it names the shell every session on that host gets, it was written once and
	// reused, and if the program is missing then every session from it is dead on
	// arrival. That is a fact about the config, so it is answered as one.
	//
	// Only the loopback case can be checked here — a remote profile's PATH lives on the
	// target, and probing it would mean running something on the host to answer a
	// question about the config. For remote the check is skipped rather than guessed.
	if command == "" && len(toolArgs) == 0 && ent != nil && remote == nil {
		if err := profileShellExists(ent); err != nil {
			return toolError(CodeShellNotFound, "%s", err.Error()), nil
		}
	}

	mode := sshconfig.EffectiveMode(ent, getString(args, "mode", ""))

	sessName := strings.TrimSpace(getString(args, "name", ""))
	if sessName == "" {
		sessName = cfgName
	}

	sess, err := s.sessMgr.Create(session.Config{
		Command:      cmd,
		Args:         execArgs,
		Mode:         api.SessionMode(mode),
		Name:         sessName,
		SSHConfig:    cfgName,
		Rows:         int(getFloat64(args, "rows", 24)),
		Cols:         int(getFloat64(args, "cols", 80)),
		Remote:       remote,
		DefaultShell: sshconfig.EffectiveDefaultShell(ent),
		Approval:     sshconfig.EffectiveApproval(ent),
		OnExit:       session.OnExitPolicy(strings.TrimSpace(strings.ToLower(getString(args, "on_exit", "")))),
		// A session started by a namespaced caller belongs to that caller: the
		// namespace travels with the resource, which is what makes the isolation
		// survive a restart (the tag is persisted in the manifest) and what makes
		// listing, by-id reach and the guard agree.
		Namespace: callerNamespace(ctx),
		// An aborted MCP call cancels this context, and the dial stops with it
		// rather than registering a session whose caller has already gone away.
		Ctx: ctx,
	})
	if err != nil {
		return toolError(CodeConnectionFailed, "%s", sshclient.DescribeDialError(err)), nil
	}

	// No pid: the process runs on the remote, and SSH does not report its number
	// back to us, so any value here could only be a constant. Clients that need a
	// pid can take it from the shell itself (e.g. `echo $$`).
	//
	// index is the primary channel's number, read from the session rather than
	// hardcoded: it must be the number the locator resolver will honour, because
	// the browser labels and copies the first tab from this value.
	//
	// stderr tells the caller whether the channel can ever report stderr
	// separately: pipe channels keep the two producers apart at the source and
	// shell_output(stream=) can select between them; a pty merges stderr into the
	// terminal stream at the source, so no read of this shell will ever isolate it.
	result := map[string]any{
		"session_id": sess.ID,
		"shell_id":   sess.PrimaryShellID(),
		"ssh_config": cfgName,
		"index":      sess.PrimaryShellIndex(),
		"stderr":     stderrReceipt(mode),
	}
	return jsonResult(result), nil
}

// stderrReceipt names a shell mode's stderr visibility, in the shell_open and
// session_start receipts: "separate" promises shell_output(stream="e") can
// select stderr bytes; "merged(pty)" promises they never appear apart.
func stderrReceipt(mode string) string {
	if mode == "pipe" {
		return "separate"
	}
	return "merged(pty)"
}

// profileShellExists reports why a loopback profile's default_shell cannot be run.
//
// The first field is the program and the rest are its arguments, matching how
// EffectiveDefaultShell splits the line — so `bash -l` checks `bash`, which is the
// only part that has to exist.
//
// A profile that names no shell is fine: the session then asks the server for its
// own, which is the case that cannot fail here.
func profileShellExists(ent *sshconfig.Entry) error {
	line := sshconfig.EffectiveDefaultShell(ent)
	if line == "" {
		return nil
	}
	// THE PROGRAM NAME COMES FROM THE GRAMMAR, not from splitting on whitespace: a shell
	// path is a shell WORD, and `"C:\Program Files\pwsh.exe" -NoLogo` is one word followed
	// by an argument. Splitting it by hand read the path as `"C:\Program` and reported a
	// profile as broken when the only thing wrong was how this function read it.
	prog, _, err := directive.SplitArgv(line)
	if err != nil || prog == "" {
		// An unparseable shell line is the profile's problem, and it is reported where the
		// profile is used rather than guessed at here.
		return nil
	}
	if _, err := exec.LookPath(prog); err != nil {
		return fmt.Errorf("default_shell %q is not runnable on this host, so every session from this profile would die on arrival: %w", prog, err)
	}
	return nil
}

func (s *Server) handleSendInput(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "shell_id", "")
	text := getString(args, "text", "")

	/* One command, many shells (issue #80). The comma form is the same one
	   session_terminate/session_delete already accept, so this is the lifecycle
	   batch behaviour applied to input: every entry is attempted independently and
	   the reply carries one outcome per entry. A single id keeps the original
	   reply shape exactly — an agent that types into one shell must not start
	   reading a results array. */
	if ids, isBatch := splitBatchIDs(shellID); isBatch {
		return s.batchShellResult(ids, func(id string) (map[string]any, *mcpgo.CallToolResult) {
			shell, bad := s.requireShell(id)
			if bad != nil {
				return nil, bad
			}
			res, bad := s.writeShellInput(ctx, shell, text)
			return res, bad
		}), nil
	}

	shell, bad := s.requireShell(shellID)
	if bad != nil {
		return bad, nil
	}

	// The write's handle in the transcript is read BEFORE the bytes go out: the
	// input itself reaches log.bin only through the terminal's echo (a pipe shell
	// records no echo at all), so the stream end at this moment is where this
	// input's echo will begin — the one per-call anchor a line-protocol consumer
	// needs to pair a later response with its request. A failed lookup only
	// costs the field, never the write: the index is advisory, the send is not.
	res, bad := s.writeShellInput(ctx, shell, text)
	if bad != nil {
		return bad, nil
	}
	return jsonResult(res), nil
}

// writeShellInput is the single-shell write shared by the one-id and batch
// paths, so the two cannot drift: the offset/line bookkeeping, the review stage
// and the AI tagging are identical whether one shell is addressed or ten.
func (s *Server) writeShellInput(ctx context.Context, shell *session.ChildShell, text string) (map[string]any, *mcpgo.CallToolResult) {
	result := map[string]any{"success": true}
	sessID := shell.ParentSessionID()
	if offset, err := s.sessMgr.OutputSize(sessID, shell.ID); err == nil {
		result["offset"] = offset
		if total, err := s.sessMgr.LineCount(sessID, shell.ID); err == nil {
			result["total_lines"] = total
		}
		if line, _, err := s.sessMgr.LineSpanOfOffsets(sessID, shell.ID, offset, offset); err == nil {
			// Zero-length position semantics, same as message spans: the line the
			// echo continues, or 0 when it starts a fresh (still unnumbered) line —
			// the consumer derives that as total_lines+1.
			result["line"] = line
		}
	}

	// Under review mode the text is held, not written and not yet queued.
	//
	// A command line is text plus the enter that ends it, and those arrive as two
	// calls. Staging the text keeps a reviewer's unit whole: they see `echo hi`
	// and the newline as one command, not two decisions that could disagree.
	//
	// The decision comes from the capability table (shell.input is a Staged
	// reviewable operation), not from an `if` written here: this was one of the
	// two places where review was gated from inside a handler, so a new reviewable
	// operation was covered only if someone remembered to add a third.
	if s.reviewHolds(shell) {
		if err := shell.StageForApproval(callerSource(ctx), text); err != nil {
			return nil, toolError(CodeOperationFailed, "%s", err.Error())
		}
		// The staged case is not a write that happened, so the reply is the
		// canonical review shape rather than the offset receipt: a model branches
		// on approved/review_pending, and two shapes for one state read as two
		// states.
		return reviewFields(false, nil), nil
	}

	// An agent's keystrokes are tagged as AI input, so a transcript can say who
	// typed what. The bytes themselves reach the log through the terminal's echo.
	if err := shell.SendTerminalBytesFrom([]byte(text), false, callerInputSource(ctx)); err != nil {
		return nil, toolError(inputErrCode(err), "%s", err.Error())
	}
	return result, nil
}

func (s *Server) handlePressKey(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "shell_id", "")
	key := getString(args, "key", "")
	repeat := int(getFloat64(args, "repeat", 1))
	if key == "" {
		return toolError(CodeInvalidArgument, "%s", "key is required"), nil
	}

	// The batch form carries over to keys for the same reason it exists on
	// shell_input: ending ten command lines is one action, not ten round trips.
	if ids, isBatch := splitBatchIDs(shellID); isBatch {
		return s.batchShellResult(ids, func(id string) (map[string]any, *mcpgo.CallToolResult) {
			shell, bad := s.requireShell(id)
			if bad != nil {
				return nil, bad
			}
			return s.pressShellKey(ctx, shell, key, repeat)
		}), nil
	}

	shell, bad := s.requireShell(shellID)
	if bad != nil {
		return bad, nil
	}
	res, bad := s.pressShellKey(ctx, shell, key, repeat)
	if bad != nil {
		return bad, nil
	}
	if res["review_pending"] == nil {
		return successResult(), nil
	}
	return jsonResult(res), nil
}

// pressShellKey is the single-shell key press shared by the one-id and batch
// paths.
func (s *Server) pressShellKey(ctx context.Context, shell *session.ChildShell, key string, repeat int) (map[string]any, *mcpgo.CallToolResult) {
	if s.reviewHolds(shell) {
		// The ending key commits whatever text this shell staged, so the queue holds
		// one complete command line. With nothing staged the key alone is the unit:
		// ctrl+c interrupts a running process, and a bare enter runs an empty line,
		// either of which a reviewer must still see.
		requestID, err := shell.CommitStagedForApproval(callerSource(ctx), key, repeat)
		if err != nil {
			return nil, toolError(inputErrCode(err), "%s", err.Error())
		}
		return reviewFields(true, approvalCorrelation(shell.ApprovalQueue(), requestID, shell.ParentSessionID())), nil
	}

	if err := shell.PressKeyFrom(key, repeat, callerInputSource(ctx)); err != nil {
		return nil, toolError(inputErrCode(err), "%s", err.Error())
	}
	return map[string]any{"success": true}, nil
}

// reviewHolds reports whether review mode holds input to this shell.
//
// It is the ONE place the question "does a human have to approve typing here"
// is asked, and it answers from the shell's capability row (shell.input is a
// Staged, reviewable operation) rather than from a policy restated per surface.
// Before this, the `if shell.RequiresApproval()` lines sat in two handlers, which
// meant "the human's own keyboard is not reviewed" held only because twelve REST
// and WebSocket call sites each happened not to write the check — a decision
// living in the absence of code rather than in code.
//
// A detached shell (no parent session) never gates: there is no policy that could
// apply to it.
func (s *Server) reviewHolds(shell *session.ChildShell) bool {
	if shell == nil {
		return false
	}
	c, ok := capability.ForTool("shell_input")
	if !ok || !c.Staged {
		return false
	}
	return shell.RequiresApproval()
}

// reviewFields is the canonical review-mode payload as a map, shared by the
// single-call reply and by each entry of a batch. One constructor means a model
// cannot see two different shapes for the same state.
func reviewFields(queued bool, request *approval.Request) map[string]any {
	fields := map[string]any{
		"ok":             true,
		"approved":       false,
		"review_pending": true,
		"message":        reviewMessage(queued),
	}
	if request != nil {
		fields["approval_id"] = request.ID
		if request.SessionID != "" {
			fields["session_id"] = request.SessionID
		}
		if request.ExpiresAt > 0 {
			fields["expires_at"] = request.ExpiresAt
		}
	}
	return fields
}

// reviewPendingResult is the reply to input that review mode intercepted. It is
// deliberately not an error: the call succeeded, it is the write that is waiting.
//
// The two cases need opposite next steps, so they get opposite messages: staged
// text still needs its ending key, queued text must not be retried (reviewWaitTail
// says so) and is checked later through the shell's output.
func reviewPendingResult(queued bool) *mcpgo.CallToolResult {
	return jsonResult(reviewFields(queued, nil))
}

// reviewMessage is the one sentence that tells an agent what to do next in each
// of the two review states.
func reviewMessage(queued bool) string {
	if !queued {
		return "Held for review: send the ending key (shell_key enter) to submit the line for approval."
	}
	return "Submitted for review. " + reviewWaitTail +
		" Read the shell's output later to see whether it ran."
}

func (s *Server) handleStartSubShell(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	parentID := getString(args, "session_id", "")
	name := getString(args, "name", "")
	command := getString(args, "command", "")
	toolArgs := getStringSlice(args, "args")
	if strings.TrimSpace(command) == "" && len(toolArgs) > 0 {
		return toolError(CodeInvalidArgument, "%s", "command is required when args are provided"), nil
	}
	mode := strings.TrimSpace(getString(args, "mode", "pty"))
	if mode != "pty" && mode != "pipe" {
		return toolError(CodeInvalidArgument, "%s", fmt.Sprintf("mode must be 'pty' or 'pipe', got %q", mode)), nil
	}
	rows := int(getFloat64(args, "rows", 24))
	cols := int(getFloat64(args, "cols", 80))

	sess, bad := s.requireRunningSession(parentID)
	if bad != nil {
		return bad, nil
	}
	cs, err := sess.CreateChildShell(command, toolArgs, mode == "pty", rows, cols, name)
	if err != nil {
		// A LABEL ALREADY IN USE IS AN ARGUMENT PROBLEM (issue #188): the caller can fix
		// it by choosing another name, and answering 500 would say the server failed.
		if errors.Is(err, session.ErrLabelTaken) {
			return toolError(CodeInvalidArgument, "%s", err.Error()), nil
		}
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	// Echo sess.ID, not the argument: the response's ids must be usable as ids, and
	// a locator echoed into session_id would be pasted back verbatim by a caller
	// that trusts the response.
	return jsonResult(map[string]any{"shell_id": cs.ID, "session_id": sess.ID, "name": cs.Name, "index": cs.Index, "stderr": stderrReceipt(mode)}), nil
}

func (s *Server) handleListSubshells(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	parentID := getString(args, "session_id", "")

	sess, bad := s.requireSession(parentID)
	if bad != nil {
		return bad, nil
	}
	all := sess.ListChildShells()
	return jsonResult(map[string]any{"session_id": sess.ID, "shells": filterRunning(all)}), nil
}

// handleCloseShell closes a single shell channel without tearing down the parent session.
// For a parent session id: closes the root shell channel only (remote) / no-op (internal);
// the SSH connection and other child shells keep running. For a child shell id: closes
// just that channel. Use session_terminate to fully stop a session.
//
// shell_id accepts everything requireShell accepts (raw id, session id, or a
// termcp:// locator); the close acts on the resolved shell, not the argument.
func (s *Server) handleCloseShell(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "shell_id", "")
	if shellID == "" {
		return toolError(CodeInvalidArgument, "%s", "shell_id is required"), nil
	}
	shell, bad := s.requireShell(shellID)
	if bad != nil {
		return bad, nil
	}
	// Internal primary shell: tab close is a no-op (process outlives the tab).
	if sess := s.sessMgr.GetByShellID(shell.ID); sess != nil && sess.IsInternalPrimaryShell(shell.ID) {
		return successResult(), nil
	}
	found, err := s.sessMgr.CloseChildShell(shell.ID)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if !found {
		return toolError(CodeShellNotFound, "%s", fmt.Sprintf("Shell '%s' not found", shell.ID)), nil
	}
	return successResult(), nil
}

// handleArchiveShell closes ONE channel and keeps its record readable.
//
// This is what the `close <handle>` directive means, and it is deliberately not
// handleCloseShell: that one erases the shell's metadata, so a closed channel
// answered 404 afterwards and the caller could not tell "I closed it" from "it never
// existed" — while the transcript sat on disk with nothing addressing it.
//
// The internal-primary no-op that handleCloseShell applies does NOT belong here.
// That rule exists for the browser tab: closing a tab must not kill a loopback
// process. A directive that names a channel is asking for the channel to end, and a
// silent success that leaves everything running is the failure this whole change
// exists to remove. The tab's behaviour stays where it belongs, in the Web UI route.
func (s *Server) handleArchiveShell(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "shell_id", "")
	if shellID == "" {
		return toolError(CodeInvalidArgument, "%s", "shell_id is required"), nil
	}
	shell, bad := s.requireShell(shellID)
	if bad != nil {
		return bad, nil
	}
	found, err := s.sessMgr.ArchiveChildShell(shell.ID)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	if !found {
		return toolError(CodeShellNotFound, "%s", fmt.Sprintf("Shell '%s' not found", shell.ID)), nil
	}
	return successResult(), nil
}

// handleReadOutput is the ONE unified output reader. It serves live shells
// (in-memory buffer), exited-but-retained shells, and closed/restored-DEAD
// sessions (persisted log.bin) with identical byte-stream cursor semantics.
// Three read modes:
//   - tail_lines > 0, or a closed id with no offset: read the tail of the
//     stream (token-safe default; never a full dump);
//   - offset >= 0: stateless positional read of [offset, offset+max_bytes);
//   - otherwise: live streaming cursor on reader_id (new bytes since last read).
//
// Every response carries start_offset/end_offset/total_bytes/has_more so the
// caller can page the stream without server-side state.
func (s *Server) handleReadOutput(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	id := getString(args, "shell_id", "")
	if id == "" {
		return toolError(CodeInvalidArgument, "%s", "shell_id is required"), nil
	}
	p, bad := parseReadParams(args)
	if bad != nil {
		return bad, nil
	}

	src, bad := s.resolveOutputSource(id)
	if bad != nil {
		return bad, nil
	}

	// A CLOSED CHANNEL KEEPS ITS OWN POSITION (#159 C1).
	//
	// `read <ch>` on a live channel means "what is new since my last read". On a
	// closed one it used to mean "the tail", which reads the same and is not: the
	// same call returned the same bytes on every poll, so a caller watching an
	// extension's output re-read its whole history each time and the offsets never
	// moved. The cursor makes the closed case mean what the live case means; an
	// explicit offset (0 included) is still the full replay, and an explicit tail
	// is still a tail.
	key := ""
	if src.Live == nil && p.Offset == nil && p.FromLine == 0 && p.TailLines == nil && !p.TailBytes {
		key = src.SessID + "\x00" + src.ShellID
		at := s.closedReadCursor(key)
		p.Offset = &at
	}

	w, err := transcript.Read(ctx, src, p)
	if err != nil {
		return toolResultForErr(err), nil
	}
	if key != "" {
		s.setClosedReadCursor(key, w.End)
	}
	return jsonResult(windowResult(w, src)), nil
}

// resolveOutputSource maps an id onto the unified output stream (raw shell id,
// session id, or termcp:// locator). Resolution lives in the transcript
// package; this wrapper only maps its typed errors onto the tool vocabulary.
func (s *Server) resolveOutputSource(id string) (*transcript.Source, *mcpgo.CallToolResult) {
	src, err := transcript.Resolve(s.sessMgr, s.msgMgr, id)
	if err != nil {
		return nil, toolResultForErr(err)
	}
	return src, nil
}

// toolResultForErr maps a transcript read error onto the tool's error-code
// vocabulary, so the shared read path can stay transport-neutral.
func toolResultForErr(err error) *mcpgo.CallToolResult {
	var te *transcript.Error
	if errors.As(err, &te) {
		switch te.Kind {
		case transcript.ErrShellNotFound:
			return toolError(CodeShellNotFound, "%s", te.Msg)
		case transcript.ErrSessionNotFound:
			return toolError(CodeSessionNotFound, "%s", te.Msg)
		case transcript.ErrReader:
			return toolError(CodeReaderNotRegistered, "%s", te.Msg)
		case transcript.ErrInvalid:
			return toolError(CodeInvalidArgument, "%s", te.Msg)
		}
	}
	return toolError(CodeOperationFailed, "%s", err.Error())
}

// readParams is the shell_output request after JSON mapping: the shared
// transcript read contract. Validation and the read itself live in the
// transcript package, so the HTTP API consumes the identical contract.
type readParams = transcript.Params

// parseReadParams reads and range-checks the arguments of a shell_output call.
// The ranges are part of the tool contract rather than defensive noise: a
// caller that sends something outside the documented domain gets told why
// instead of silently getting a different read. The blocking timeout is the one
// surface-specific range (the Web UI has no waiting reads).
func parseReadParams(args map[string]any) (readParams, *mcpgo.CallToolResult) {
	p := readParams{
		StripAnsi: getBool(args, "strip_ansi", true),
		// This surface's reply becomes JSON, and encoding/json replaces bytes it
		// cannot decode with U+FFFD instead of reporting them. That substitution
		// shifts every offset after it, so a caller paging the stream reads from the
		// wrong byte while being told the read succeeded. Ask the shared reader to
		// refuse such a window instead; it names the offending offset, and
		// `mode=hex`/`mode=file` on file_read remain the byte-exact route.
		//
		// The Web UI (handler_shell.go) deliberately does NOT set this: it repaints
		// raw bytes into a terminal, where non-UTF-8 output is normal and lossless.
		RequireUTF8: true,
		Timeout:     getFloat64(args, "timeout", 3.0),
		MaxLines:    int(getFloat64(args, "max_lines", 0)),
		MaxBytes:    int(getFloat64(args, "max_bytes", 8192)),
		ReaderID:    int(getFloat64(args, "reader_id", 0)),
		// A pointer for the same reason TailLines is one: 0 is a real byte position,
		// so "no positional read" and "read from the first byte" cannot share it.
		Offset: int64PtrFromArgs(args, "offset"),
		// ABSENT AND ZERO ARE DIFFERENT REQUESTS, so this is a pointer: nil means
		// the caller named no tail at all, 0 means they asked for zero lines.
		//
		// The field is an int in the wire format (a JSON number), and the old
		// default of 0 made "--tail 0" indistinguishable from "no tail" — which
		// scanTailWindow read as "no line limit" and answered with the ENTIRE
		// buffer. The smallest possible request produced the largest possible
		// answer.
		// THE SESSION'S ENCODING, when the caller knows it. A terminal is not a
		// UTF-8 device: a zh-CN Windows console writes GBK, and without this the
		// reply was a refusal the caller could not act on (issue #149).
		Charset:   getString(args, "charset", ""),
		TailLines: intPtrFromArgs(args, "tail_lines"),
		FromLine:  int64(getFloat64(args, "from_line", 0)),
		Stream:    getString(args, "stream", ""),
	}
	if p.Timeout < 0 {
		// THERE IS NO UPPER BOUND. `timeout` is documented as a recommendation, not a
		// ceiling (see the window section of the guide): how long a command may take is
		// the caller's bill to pay, and the caller is the only party that can see its own
		// link and its own watchdog. A ceiling here made the tool's own schema a lie.
		//
		// It used to read `> 60` while the comment beside it said NO UPPER BOUND, so any
		// window above a minute was refused — and the message described only the negative
		// case, naming the positive number it had just rejected (issue #168: `timeout: 90`
		// answered "timeout must not be negative, got 89.35"). A caller cannot tell from
		// that whether to lower the number or wait.
		return readParams{}, toolError(CodeInvalidArgument, "%s", fmt.Sprintf("timeout must not be negative, got %v", p.Timeout))
	}
	if err := transcript.ValidateParams(p); err != nil {
		return readParams{}, toolResultForErr(err)
	}
	return p, nil
}

// windowResult renders a read window as the tool's JSON payload. The byte
// fields are the raw-stream facts transcript.Read computed; this only names
// them for the model and adds what the source knows (identity, uptime).
func windowResult(w transcript.Window, src *transcript.Source) map[string]any {
	result := map[string]any{
		"output":         w.Output,
		"has_more":       w.HasMore,
		"lines_returned": strings.Count(w.Output, "\n"),
		"bytes_returned": len(w.Output),
		"returned_bytes": len(w.Output),
		// raw_bytes is the cursor-safe quantity: raw window bytes, so
		// start_offset + raw_bytes == end_offset holds whatever stripping did.
		// Cursors advance by end_offset (or raw_bytes), never bytes_returned.
		"raw_bytes":    w.End - w.Start,
		"start_offset": w.Start,
		"end_offset":   w.End,
		"total_bytes":  w.Total,
		// THE REPLY'S OWN ADDRESSES (issue #202).
		//
		// start/end/total above are CHANNEL positions, because the cursor is a channel
		// walk. On a `--stream e` read they count stdout too, so paging with them lands
		// on the other side of the channel — and `total` described the channel while the
		// body was one stream, which is how an empty stderr read came back as
		// `bytes */7`. These three are the same window numbered in the stream the caller
		// asked for; the channel group is kept for the cursor and named as such.
		"stream":             w.Stream,
		"stream_start":       w.StreamStart,
		"stream_end":         w.StreamEnd,
		"stream_total_bytes": w.StreamTotal,
		"stream_raw_bytes":   w.StreamEnd - w.StreamStart,
		"partial_tail_bytes": w.PartialTail,
		// Non-zero only when the window was rolled back to avoid ending inside a
		// character. The bytes are still in the stream (the window stops before them),
		// so this is a count, not a loss.
		"trimmed_bytes": w.Trimmed,
		// Bytes inside the range that a stream filter passed over. Non-zero means the
		// cursor moved past bytes this reply did not show.
		"stream_omitted_bytes": w.StreamOmitted,
		"source":               src.SourceName(),
		"session_id":           src.SessID,
		"shell_id":             src.ShellID,
		"session_status":       string(src.Status),
	}
	if w.TotalLines > 0 {
		result["total_lines"] = w.TotalLines
	}
	if w.FromLine > 0 {
		result["from_line"] = w.FromLine
		result["to_line"] = w.ToLine
		if w.LineBytes != nil {
			result["line_bytes"] = w.LineBytes
		}
	}
	if src.Live != nil {
		result["session_uptime_seconds"] = int(clock.Since(src.Created).Seconds())
	}
	return result
}

func (s *Server) handleResizePty(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "shell_id", "")
	rows := int(getFloat64(args, "rows", 24))
	cols := int(getFloat64(args, "cols", 80))

	shell, bad := s.requireShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	if err := shell.ResizePty(rows, cols); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

// handleListMessages returns a shell's status marks: the spans of its byte log,
// in order, with what produced each one.
//
// This replaced a per-message index. The transcript is one byte stream per shell,
// so "what happened" is a list of spans rather than a list of payloads — the
// payloads all live in log.bin and are read through shell_output.
func (s *Server) handleListMessages(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "session_id", "")
	shellID := getString(args, "shell_id", "")

	// Resolve both ids before reading logs. The manager looks a shell up by id, so
	// a locator would silently address nothing and return an empty transcript.
	// shell_id may name the session instead (the primary shell), and an empty
	// shell_id means the same thing.
	sess, bad := s.requireSession(sessionID)
	if bad != nil {
		return bad, nil
	}
	if shellID == "" {
		shellID = sess.PrimaryShellID()
	} else {
		// A READ must tolerate a DEAD session: the marks live in log.bin, which
		// outlives the transport. requireShell refuses a closed session (writes must
		// not reach a dead transport), so resolving through it here would make
		// message(shell_id=...) fail on exactly the sessions its output is still
		// readable from — while message(session_id=...) succeeded. Resolve the
		// spelling, then accept the shell if the session still knows it, live or
		// retained.
		resolved, bad := s.resolveShellIDForRead(sess, shellID)
		if bad != nil {
			return bad, nil
		}
		shellID = resolved
	}

	marks, err := s.sessMgr.Marks(sess.ID, shellID)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	// Each span's end is the next mark's start; the last span runs to the current
	// end of the log, so it is derived rather than stored.
	size, _ := s.sessMgr.OutputSize(sess.ID, shellID)
	// Line numbers come from the line index over the same log. The index is
	// advisory, like the marks themselves: if it cannot answer (no persistence,
	// unreadable files), the spans stay correct and simply go out without line
	// numbers rather than the whole transcript failing. The session manager
	// resolves an empty shell_id to the primary shell, exactly as Marks does.
	totalLines, lineErr := s.sessMgr.LineCount(sess.ID, shellID)
	spans := make([]map[string]any, 0, len(marks))
	for i, m := range marks {
		end := size
		if i+1 < len(marks) {
			end = marks[i+1].Offset
		}
		span := map[string]any{
			"status": string(m.Status),
			"time":   m.Time,
			"start":  m.Offset,
			"end":    end,
		}
		if lineErr == nil {
			if fromLine, toLine, lerr := s.sessMgr.LineSpanOfOffsets(sess.ID, shellID, m.Offset, end); lerr == nil {
				span["from_line"] = fromLine
				span["to_line"] = toLine
				// A MARK RECORDS WHERE A COMMAND BEGAN, NOT WHERE ITS FIRST LINE
				// BEGAN: the prompt that preceded the command belongs to the same
				// line, so start and line_start differ by the prompt's width (37
				// bytes in the case that surfaced this). Reporting only `start`
				// next to a line number invited callers to read from it and land
				// mid-line.
				//
				// line_start is the byte `read --from-line <from_line>` starts
				// from, so a caller can go from this listing straight to the bytes.
				if fromLine > 0 {
					if ls, lserr := s.sessMgr.LineStartOffset(sess.ID, shellID, fromLine); lserr == nil {
						span["line_start"] = ls
					}
				}
			}
		}
		spans = append(spans, span)
	}
	result := map[string]any{
		"spans":       spans,
		"total_bytes": size,
		"session_id":  sess.ID,
		"shell_id":    shellID,
	}
	if lineErr == nil {
		result["total_lines"] = totalLines
	}
	return jsonResult(result), nil
}

// resolveShellIDForRead maps a shell_id argument onto a shell id that a READ can
// address, on a session that may already be DEAD.
//
// It differs from requireShell in exactly one way: it does not reject a closed
// session. Reading a shell's log is legal after the session ends — that is what
// keeps DEAD sessions useful — so a read path must not inherit the write path's
// status check. A shell the session does not know at all is still an error.
func (s *Server) resolveShellIDForRead(sess *session.Session, shellID string) (string, *mcpgo.CallToolResult) {
	// Live and retained shells are both in the session's own map.
	if cs := sess.GetChildShell(shellID); cs != nil {
		return cs.ID, nil
	}
	// A locator is the one spelling that can be MALFORMED (an entry name with a
	// channel index), so it keeps its own diagnostics; every other spelling is
	// decomposed by SplitHandle. Both end at the same index lookup below.
	var h locator.Handle
	if looksLikeResourceLocator(shellID) {
		p, err := parseResourceURL(shellID)
		if err != nil {
			return "", toolError(CodeInvalidArgument, "%s", err.Error())
		}
		if p.Kind == resourceURLEntry {
			return "", toolError(CodeInvalidArgument, "%s", fmt.Sprintf("resource URL %q names an entry, not a session or shell", shellID))
		}
		h = locator.Handle{SessionID: p.SessionID, Index: p.Index}
	} else {
		h = locator.SplitHandle(shellID)
		if h.SessionID == shellID && h.Index == 0 {
			// No channel named: this may still be a bare session id, which is legal
			// here ("read the session's primary channel"). Only a value that names
			// neither a known session nor a channel is unknown; the lookup below
			// decides, and the SessionID stays as given.
			if s.sessMgr.Get(h.SessionID) == nil {
				if cs := sess.GetChildShell(h.SessionID); cs != nil {
					return cs.ID, nil
				}
				return "", toolError(CodeShellNotFound, "%s", fmt.Sprintf("Shell '%s' not found", shellID))
			}
		}
	}
	// A handle may name a different session than the shell_id argument; the shell it
	// resolves to is what the caller asked to read.
	if h.SessionID != sess.ID {
		target := s.sessMgr.Get(h.SessionID)
		if target == nil {
			return "", toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' not found", h.SessionID))
		}
		sess = target
	}
	idx := h.Index
	if idx == 0 {
		idx = 1
	}
	if cs, ok := sess.ShellByIndex(idx); ok {
		return cs.ID, nil
	}
	if sh, ok := sess.SnapshotShellByIndex(idx); ok {
		return sh.ID, nil
	}
	return "", toolError(CodeShellNotFound, "%s", session.ShellIndexOutOfRangeError(sess.ID, idx, sess.LiveShellCount()).Error())
}

func (s *Server) handleRegisterReader(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "shell_id", "")

	shell, bad := s.requireShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	// A CHANNEL WHOSE PROCESS HAS EXITED CANNOT TAKE A READER, and that is a fact about
	// the channel, not a failure inside termcp. It used to surface as "buffer: closed"
	// with a 500, which reads as a bug in the server; the caller's mistake was asking a
	// finished channel to start a stream, and the answer should say so — along with what
	// still works, which is reading what it already produced.
	if status := shell.Info().Status; status != api.SessionRunning {
		return toolError(CodeConflict, "%s", fmt.Sprintf(
			"channel %s has %s, so no reader can be attached; its output is still readable with `read`",
			shell.ID, status)), nil
	}
	readerID, err := shell.RegisterReader()
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	// A registered reader pins the buffer's compaction watermark at its cursor
	// until it is unregistered (maybeCompactLocked takes the minimum readPos), so
	// an agent that forgets shell_reader_unregister would grow the transcript
	// without bound. Attach it to the parent session scope so deletion still
	// releases it; an explicit unregister simply makes this cleanup a no-op.
	if pid := shell.ParentSessionID(); pid != "" {
		if sess := s.sessMgr.Get(pid); sess != nil {
			sess.AttachCleanup(func() { shell.UnregisterReader(readerID) })
		}
	}
	result := map[string]any{"reader_id": readerID}
	return jsonResult(result), nil
}

func (s *Server) handleUnregisterReader(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "shell_id", "")
	readerID := int(getFloat64(args, "reader_id", 0))

	shell, bad := s.requireShell(sessionID)
	if bad != nil {
		return bad, nil
	}
	shell.UnregisterReader(readerID)
	return successResult(), nil
}

// intPtrFromArgs reads an optional integer argument, distinguishing "absent" (nil)
// from "present and zero" (pointer to 0).
func intPtrFromArgs(args map[string]any, key string) *int {
	if _, ok := args[key]; !ok {
		return nil
	}
	v := int(getFloat64(args, key, 0))
	return &v
}

// int64PtrFromArgs is intPtrFromArgs for the 64-bit fields (byte offsets).
func int64PtrFromArgs(args map[string]any, key string) *int64 {
	if _, ok := args[key]; !ok {
		return nil
	}
	v := int64(getFloat64(args, key, 0))
	return &v
}

// inputErrCode classifies a failed write to a channel.
//
// THE TWO FAILURES ON THIS PATH ARE OPPOSITES AND USED TO SHARE ONE STATUS. A typo
// in a key name answered 500 operation_failed, and so did writing to a pipe whose
// process had already finished: a caller that retries on 5xx, alarms on 5xx, or
// files a bug against 5xx does the wrong thing in both cases. One of them is the
// caller's line being wrong (400), the other is the resource's state (409).
//
// The sentinels are in internal/session, next to the errors they name, because
// that is where the distinction is created; a surface that guessed from the
// message text would be re-deriving it in the wrong layer.
func inputErrCode(err error) string {
	switch {
	case errors.Is(err, session.ErrUnknownKey):
		return CodeInvalidArgument
	case errors.Is(err, session.ErrProcessGone):
		return CodeSessionNotRunning
	}
	return CodeOperationFailed
}
