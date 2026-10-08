package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/ansi"
	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
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
	result := map[string]any{
		"session_id": sess.ID,
		"shell_id":   sess.PrimaryShellID(),
		"ssh_config": cfgName,
		"index":      sess.PrimaryShellIndex(),
	}
	return jsonResult(result), nil
}

func (s *Server) handleSendInput(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "shell_id", "")
	text := getString(args, "text", "")

	shell, bad := s.requireShell(shellID)
	if bad != nil {
		return bad, nil
	}

	// Under approval mode the text is held, not written and not yet queued.
	//
	// A command line is text plus the enter that ends it, and those arrive as two
	// calls. Staging the text keeps a reviewer's unit whole: they see `echo hi`
	// and the newline as one command, not two decisions that could disagree.
	if shell.RequiresApproval() {
		if err := shell.StageForApproval("mcp", text); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return reviewPendingResult(false), nil
	}

	// An agent's keystrokes are tagged as AI input, so a transcript can say who
	// typed what. The bytes themselves reach the log through the terminal's echo.
	if err := shell.SendTerminalBytesFrom([]byte(text), false, session.InputFromAI); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

func (s *Server) handlePressKey(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	shellID := getString(args, "shell_id", "")
	key := getString(args, "key", "")
	repeat := int(getFloat64(args, "repeat", 1))
	if key == "" {
		return toolError(CodeInvalidArgument, "%s", "key is required"), nil
	}
	shell, bad := s.requireShell(shellID)
	if bad != nil {
		return bad, nil
	}

	if shell.RequiresApproval() {
		// The ending key commits whatever text this shell staged, so the queue holds
		// one complete command line. With nothing staged the key alone is the unit:
		// ctrl+c interrupts a running process, and a bare enter runs an empty line,
		// either of which a reviewer must still see.
		if _, err := shell.CommitStagedForApproval("mcp", key, repeat); err != nil {
			return toolError(CodeOperationFailed, "%s", err.Error()), nil
		}
		return reviewPendingResult(true), nil
	}

	if err := shell.PressKeyFrom(key, repeat, session.InputFromAI); err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	return successResult(), nil
}

// reviewPendingResult is the reply to input that review mode intercepted. It is
// deliberately not an error: the call succeeded, it is the write that is waiting.
//
// The two cases need opposite next steps, so they get opposite messages: staged
// text still needs its ending key, queued text must not be retried (reviewWaitTail
// says so) and is checked later through the shell's output.
func reviewPendingResult(queued bool) *mcpgo.CallToolResult {
	if !queued {
		return reviewPendingReply("Held for review: send the ending key (shell_key enter) to submit the line for approval.")
	}
	return reviewPendingReply("Submitted for review. " + reviewWaitTail +
		" Read the shell's output later to see whether it ran.")
}

func (s *Server) handleStartSubShell(_ context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	parentID := getString(args, "session_id", "")
	name := getString(args, "name", "")
	command := getString(args, "command", "")
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
	cs, err := sess.CreateChildShell(command, nil, mode == "pty", rows, cols, name)
	if err != nil {
		return toolError(CodeOperationFailed, "%s", err.Error()), nil
	}
	// Echo sess.ID, not the argument: the response's ids must be usable as ids, and
	// a locator echoed into session_id would be pasted back verbatim by a caller
	// that trusts the response.
	return jsonResult(map[string]any{"shell_id": cs.ID, "session_id": sess.ID, "name": cs.Name, "index": cs.Index}), nil
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
	if p.readerID > 0 && src.live == nil {
		return toolError(CodeInvalidArgument, "%s", "reader_id requires a live shell; closed sessions are read with offset/tail_lines"), nil
	}

	win, bad := readOutputWindow(ctx, src, p)
	if bad != nil {
		return bad, nil
	}
	return jsonResult(win.result(src)), nil
}

// readParams is a shell_output request after parsing: which bytes to return, how
// to render them, and how long to wait for a live cursor.
type readParams struct {
	stripAnsi bool
	timeout   float64
	maxLines  int
	maxBytes  int
	readerID  int
	offset    int64
	tailLines int
}

// parseReadParams reads and range-checks the arguments of a shell_output call.
// The ranges are part of the tool contract rather than defensive noise: timeout,
// tail_lines and offset each have a documented domain, and a caller that sends
// something outside it gets told why instead of silently getting a different read.
func parseReadParams(args map[string]any) (readParams, *mcpgo.CallToolResult) {
	stripAnsi := getBool(args, "strip_ansi", true)
	timeout := getFloat64(args, "timeout", 3.0)
	if timeout < 0 || timeout > 60 {
		return readParams{}, toolError(CodeInvalidArgument, "%s", fmt.Sprintf("timeout must be between 0 and 60, got %v", timeout))
	}
	maxLines := int(getFloat64(args, "max_lines", 0))
	maxBytes := int(getFloat64(args, "max_bytes", 8192))
	readerID := int(getFloat64(args, "reader_id", 0))
	offset := int64(getFloat64(args, "offset", -1))
	tailLines := int(getFloat64(args, "tail_lines", 0))
	if tailLines < 0 {
		return readParams{}, toolError(CodeInvalidArgument, "%s", fmt.Sprintf("tail_lines must be >= 0, got %d", tailLines))
	}
	if offset < -1 {
		return readParams{}, toolError(CodeInvalidArgument, "%s", fmt.Sprintf("offset must be >= -1, got %d", offset))
	}
	return readParams{
		stripAnsi: stripAnsi,
		timeout:   timeout,
		maxLines:  maxLines,
		maxBytes:  maxBytes,
		readerID:  readerID,
		offset:    offset,
		tailLines: tailLines,
	}, nil
}

// outputWindow is the slice of a shell's output that a read returned, plus where
// that slice sits in the whole stream. Those offsets are what lets a caller page:
// end_offset is where the next read starts, and has_more says whether anything
// follows it.
type outputWindow struct {
	output  string
	start   int64
	end     int64
	total   int64
	hasMore bool
}

// result renders the window as the tool's JSON payload. The stream identity comes
// from the source, so it is passed in rather than stored: a window is a fact about
// one read, not about the shell it was read from.
func (w outputWindow) result(src *outputSource) map[string]any {
	result := map[string]any{
		"output":         w.output,
		"has_more":       w.hasMore,
		"lines_returned": strings.Count(w.output, "\n"),
		"bytes_returned": len(w.output),
		"start_offset":   w.start,
		"end_offset":     w.end,
		"total_bytes":    w.total,
		"source":         src.source(),
		"session_id":     src.sessID,
		"shell_id":       src.shellID,
		"session_status": string(src.status),
	}
	if src.live != nil {
		result["session_uptime_seconds"] = int(clock.Since(src.created).Seconds())
	}
	return result
}

// readOutputWindow produces the window a read asked for. The three forms share a
// result shape but not a source, which is the point of shell_output being one tool:
// a tail or a positional offset reads bytes, so it works on a live shell and on a
// closed one alike, while a reader_id read is a live cursor that consumes the stream
// and only exists while the shell does.
func readOutputWindow(ctx context.Context, src *outputSource, p readParams) (outputWindow, *mcpgo.CallToolResult) {
	clean := func(raw []byte) string {
		if !p.stripAnsi {
			return string(raw)
		}
		return ansi.Compact(ansi.Strip(string(raw)))
	}

	var w outputWindow
	switch {
	case p.tailLines > 0 || (src.live == nil && p.offset < 0):
		raw, st, tot, err := src.scanTailWindow(p.tailLines, p.maxBytes)
		if err != nil {
			return outputWindow{}, toolError(CodeOperationFailed, "%s", err.Error())
		}
		w = outputWindow{output: clean(raw), start: st, end: tot, total: tot}
	case p.offset >= 0:
		total, err := src.Len()
		if err != nil {
			return outputWindow{}, toolError(CodeOperationFailed, "%s", err.Error())
		}
		max := p.maxBytes
		if max <= 0 {
			max = int(total - p.offset)
			if max < 0 {
				max = 0
			}
		}
		raw, _, err := src.ByteRange(p.offset, max)
		if err != nil {
			return outputWindow{}, toolError(CodeOperationFailed, "%s", err.Error())
		}
		raw = truncateAtLines(raw, p.maxLines, p.offset+int64(len(raw)) >= total)
		end := p.offset + int64(len(raw))
		w = outputWindow{output: clean(raw), start: p.offset, end: end, total: total, hasMore: end < total}
	default:
		// Live streaming cursor path (unchanged semantics).
		pre := src.live.ReaderCursor(p.readerID)
		if pre < 0 {
			return outputWindow{}, toolError(CodeReaderNotRegistered, "%s", fmt.Sprintf("reader_id %d is not registered on this shell", p.readerID))
		}
		out, err := src.live.ReadTerminalStream(ctx, p.readerID, time.Duration(p.timeout*float64(time.Second)), p.stripAnsi, p.maxLines, p.maxBytes)
		if err != nil {
			return outputWindow{}, toolError(CodeOperationFailed, "%s", err.Error())
		}
		end := src.live.ReaderCursor(p.readerID)
		total := src.live.BufferLen()
		w = outputWindow{output: out, start: pre, end: end, total: total, hasMore: end < total}
	}
	return w, nil
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
	spans := make([]map[string]any, 0, len(marks))
	for i, m := range marks {
		end := size
		if i+1 < len(marks) {
			end = marks[i+1].Offset
		}
		spans = append(spans, map[string]any{
			"status": string(m.Status),
			"time":   m.Time,
			"start":  m.Offset,
			"end":    end,
		})
	}
	return jsonResult(map[string]any{
		"spans":       spans,
		"total_bytes": size,
		"session_id":  sess.ID,
		"shell_id":    shellID,
	}), nil
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
	if looksLikeResourceLocator(shellID) {
		p, err := parseResourceURL(shellID)
		if err != nil {
			return "", toolError(CodeInvalidArgument, "%s", err.Error())
		}
		if p.Kind == resourceURLEntry {
			return "", toolError(CodeInvalidArgument, "%s", fmt.Sprintf("resource URL %q names an entry, not a session or shell", shellID))
		}
		// A locator may name a different session than the session_id argument; the
		// shell it resolves to is what the caller asked to read.
		if p.SessionID != sess.ID {
			target := s.sessMgr.Get(p.SessionID)
			if target == nil {
				return "", toolError(CodeSessionNotFound, "%s", fmt.Sprintf("Session '%s' not found", p.SessionID))
			}
			sess = target
		}
		idx := p.Index
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
	return "", toolError(CodeShellNotFound, "%s", fmt.Sprintf("Shell '%s' not found", shellID))
}

func (s *Server) handleRegisterReader(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	args := request.GetArguments()
	sessionID := getString(args, "shell_id", "")

	shell, bad := s.requireShell(sessionID)
	if bad != nil {
		return bad, nil
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
