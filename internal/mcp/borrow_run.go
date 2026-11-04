package mcp

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/encoding"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// refuseBorrowedReview enforces a host profile's default review policy on
// operations the capability table marks reviewable.
func (s *Server) refuseBorrowedReview(target, tool string) *mcpgo.CallToolResult {
	host, borrowed := s.parseBorrowedHost(target)
	if !borrowed || s.sshConfigs == nil {
		return nil
	}
	c, known := capability.ForTool(tool)
	if !known || !c.Reviewable() {
		return nil
	}
	entry, err := s.sshConfigs.Load(host)
	if err != nil || entry == nil || !sshconfig.EffectiveApproval(entry) {
		return nil
	}
	return toolError(CodeForbidden, "%s", fmt.Sprintf(
		"host %q requires human review (default_approval), but a short-circuit operation has no session to hold that decision. "+
			"`connect %s` first: the operation then enters that session's review queue and a human decides it there.", host, host))
}

// checkBorrowedRunAdmission sends a short-circuit run through the same capability
// admission exit used by every handler.
func (s *Server) checkBorrowedRunAdmission(ctx context.Context, target string) *mcpgo.CallToolResult {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "shell_input"
	req.Params.Arguments = argsOf("shell_id", target)
	bad, _ := s.checkAdmission(ctx, "shell_input", req)
	return bad
}

// ensureBorrowedContainer returns the live `use:<host>` session for a host,
// creating it if one does not already exist (issue #255).
func (s *Server) ensureBorrowedContainer(ctx context.Context, host string) (*session.Session, error) {
	if s.sessMgr == nil {
		return nil, errors.New("no session manager")
	}
	cfg := session.Config{
		Mode:      "pipe",
		NoShell:   true,
		SSHConfig: host,
		Borrowed:  true,
	}
	// AN UNKNOWN PROFILE MUST NOT SILENTLY BECOME LOOPBACK. If host is not
	// `internal`, Load must succeed; leaving cfg.Remote nil would start a loopback
	// session and run the caller's command on THIS machine under the typo's name.
	if !sshconfig.IsInternalName(host) {
		if s.sshConfigs == nil {
			return nil, fmt.Errorf("%w: ssh config store not initialized", sshconfig.ErrNotFound)
		}
		entry, err := s.sshConfigs.Load(host)
		if err != nil || entry == nil {
			return nil, fmt.Errorf("%w: %q. `ls host` lists the saved profiles; a profile name is also accepted without the `use:` prefix", sshconfig.ErrNotFound, host)
		}
		remote, err := sshconfig.RemoteFromEntry(entry, s.sshConfigs.ConfigDir(host))
		if err != nil {
			return nil, fmt.Errorf("resolve profile %s: %w", host, err)
		}
		cfg.Remote = remote
	}
	return s.sessMgr.FindBorrowed(host, cfg)
}

// runBorrowedHost runs one shell command on the host's borrowed container.
//
// IT REUSES THE REAL CHANNEL PATH (#255). A borrowed container is an ordinary session
// (`use:<host>`, no primary shell), and CreateChildShell creates an authentic channel
// on its transport: its own buffer, its own log.bin, its own exit watcher, and its
// own tab in the Web UI. Nothing is simulated on the side; the terminal reads from the
// exact same byte log this command writes.
func (s *Server) runBorrowedHost(ctx context.Context, host, command string, win termArgs, captureLimit int) (output []byte, total int64, exitCode int, exited bool, waited time.Duration, err error) {
	started := time.Now()
	defer func() { waited = time.Since(started) }()

	container, err := s.ensureBorrowedContainer(ctx, host)
	if err != nil {
		return nil, 0, -1, false, 0, err
	}

	// A REAL CHANNEL, created with the LINE spelling: the command came from a person
	// or an agent as shell syntax, so pipes and redirects in it must reach the remote
	// shell unmangled. Everything downstream is the ordinary channel machinery —
	// buffer, log, exit watcher, Web UI tab, `--later` accounting.
	cs, err := container.CreateChildShellLine(command, false, 0, 0, "")
	if err != nil {
		return nil, 0, -1, false, 0, fmt.Errorf("open command channel on %s: %w", host, err)
	}
	shellID := cs.ID

	// THE WAIT: a pipe channel's process exiting is the completion signal.
	wait := time.Duration(win.TimeoutSeconds * float64(time.Second))
	if wait <= 0 {
		wait = defaultTimeoutSeconds * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()

	stopped := false
	select {
	case <-cs.Done():
		exited = true
	case <-timer.C:
		stopped = true
	case <-ctx.Done():
		stopped = true
	}

	// READ THE CHANNEL'S OWN BUFFER via shell_output.
	args := argsOf("shell_id", shellID, "offset", float64(0), "max_bytes", float64(captureLimit))
	res, fail := s.callTool(ctx, "shell_output", args)
	if fail != nil {
		err = errors.New(fail.msg)
	} else if doc, ok := toolResultMap(res); ok {
		if text, ok := doc["output"].(string); ok {
			output = []byte(text)
		}
		total = int64(getFloatAny(doc, "total_bytes", float64(len(output))))
	}

	if code := cs.Info().ExitCode; code != nil {
		exitCode = *code
	} else if exited {
		exitCode = 0
	} else {
		exitCode = -1
	}

	if stopped {
		// A stopped run terminates its channel so the process does not linger.
		cs.TerminateShell()
		if ctx.Err() != nil {
			return output, total, -1, false, 0, ctx.Err()
		}
		return output, total, -1, false, 0, nil
	}
	return output, total, exitCode, exited, 0, err
}

// borrowedRunSlot renders a short-circuit run.
func (s *Server) borrowedRunSlot(ctx context.Context, stmt directive.Statement, host string, output []byte, total int64, exitCode int, exited bool, waited time.Duration, runErr error, win termArgs) envelope.Slot {
	if runErr != nil && !exited {
		return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeOperationFailed, msg: runErr.Error() +
			". A short-circuit run keeps no channel to read later; use `connect` when a command must outlive the call."})
	}

	body := string(output)
	if charset, ok := stmt.Flag("encoding"); ok && charset != "" {
		decoded, err := encoding.Decode(charset, output)
		if err != nil {
			return s.slotFromFailure(ctx, stmt, &toolFailure{code: CodeInvalidArgument, msg: err.Error()})
		}
		body = string(decoded)
	}

	headers := map[string]string{
		"Path":         stmt.Path,
		"Host":         host,
		"Output-Bytes": strconv.FormatInt(total, 10),
		"Waited":       fmt.Sprintf("%dms", waited.Milliseconds()),
	}
	if len(output) > 0 {
		headers["Content-Range"] = envelope.ContentRange(0, int64(len(output)-1), total)
	} else {
		headers["Content-Range"] = envelope.UnsatisfiedRange(total)
	}
	var note string
	if exited {
		headers["Status"] = "exited"
		headers["Exit-Code"] = strconv.Itoa(exitCode)
		if exitCode != 0 {
			note = fmt.Sprintf("the command exited with status %d and printed nothing on stdout or stderr. Nothing was left running; a short-circuit run has no channel to read afterwards.", exitCode)
		}
	} else {
		headers["Status"] = "stopped"
		headers["Error-Code"] = "command_timeout"
		note = fmt.Sprintf(
			"the command had not exited when this call's window ran out (%gs), so it was stopped and its channel closed — a short-circuit run has nowhere to keep a live process. "+
				"Raise `timeout` for a longer command, or `connect %s` and drive a channel when it must keep running.", win.TimeoutSeconds, host)
	}
	if note != "" {
		headers["Hint"] = note
		if len(output) == 0 {
			body = note
		}
	}
	if runErr != nil {
		headers["Hint"] = runErr.Error()
	}
	if omitted := total - int64(len(output)); omitted > 0 {
		headers["Truncated"] = fmt.Sprintf(
			"%d output bytes were not retained: a short-circuit run has no stream to page. "+
				"Redirect to a file and read it with `read use:%s file <path>` when the whole output matters.", omitted, host)
	}

	if pattern, ok := stmt.Flag("grep"); ok {
		slot, _ := grepBorrowed(stmt, pattern, body, win)
		slot.Headers["Host"] = host
		slot.Headers["Output-Bytes"] = headers["Output-Bytes"]
		slot.Headers["Waited"] = headers["Waited"]
		if truncated := headers["Truncated"]; truncated != "" {
			slot.Headers["Truncated"] = truncated
		}
		if !exited {
			slot.Code = 504
			slot.Headers["Status"] = "stopped"
			slot.Headers["Error-Code"] = "command_timeout"
		}
		if exited && exitCode != 0 {
			slot.Headers["Error-Code"] = "command_failed"
		}
		return slot
	}
	slot := envelope.Slot{
		Code:    200,
		Headers: headers,
		Body:    body,
	}
	if !exited {
		slot.Code = 504
	}
	if exited && exitCode != 0 {
		slot.Headers["Error-Code"] = "command_failed"
	}
	return slot
}
// The filters used to be parsed in grepBorrowed, which is downstream of the
// execution: a typo in `--grep` answered 400 with `Output-Bytes: 20` and
// `Status: exited`, i.e. the 400 was a lie — the command had already run and its
// effect could not be undone by retrying with a fixed regex. Same rule the window
// check follows: every refusal this call can make is made before its effect.
func validateBorrowedRunFlags(stmt directive.Statement) *toolFailure {
	if pattern, ok := stmt.Flag("grep"); ok {
		if _, err := regexp.Compile(pattern); err != nil {
			return &toolFailure{code: CodeInvalidArgument, msg: "`--grep` is a Go regular expression and this one does not compile: " + err.Error()}
		}
	}
	for _, flag := range []string{"grep-limit", "tail"} {
		if v, ok := stmt.Flag(flag); ok && v != "" {
			if n, err := strconv.Atoi(v); err != nil || n <= 0 {
				return &toolFailure{code: CodeInvalidArgument, msg: "`--" + flag + "` must be a positive integer, got " + strconv.Quote(v)}
			}
		}
	}
	if charset, ok := stmt.Flag("encoding"); ok && charset != "" {
		if _, err := encoding.Decode(charset, nil); err != nil {
			return &toolFailure{code: CodeInvalidArgument, msg: err.Error()}
		}
	}
	return nil
}

// grepBorrowed filters a short-circuit run's output with the same shape a channel
// grep produces, because a caller branches on that shape.
func grepBorrowed(stmt directive.Statement, pattern, body string, win termArgs) (envelope.Slot, int) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return badRequest(stmt, "`--grep` is a Go regular expression and this one does not compile: "+err.Error()), 400
	}
	limit := grepDefaultLimit
	if v, ok := stmt.Flag("grep-limit"); ok && v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return badRequest(stmt, "`--grep-limit` must be a positive integer, got "+strconv.Quote(v)), 400
		}
		limit = n
	}

	body = strings.ReplaceAll(body, "\r\n", "\n")
	lines := strings.Split(body, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1] // the trailing newline is not a line
	}
	matched := make([]string, 0, min(limit, len(lines)))
	hits := 0
	for i, line := range lines {
		if !re.MatchString(line) {
			continue
		}
		hits++
		if len(matched) < limit {
			matched = append(matched, fmt.Sprintf("%d:%s", i+1, line))
		}
	}

	headers := map[string]string{
		"Path":          stmt.Path,
		"Grep":          pattern,
		"Lines-Scanned": strconv.Itoa(len(lines)),
		"Matches":       strconv.Itoa(hits),
	}
	if hits == 0 {
		return noContentSaying(headers, "no line matched `"+pattern+"` in the "+strconv.Itoa(len(lines))+" lines scanned"), 204
	}
	if hits > len(matched) {
		headers["Truncated"] = fmt.Sprintf("showing %d of %d matches; raise --grep-limit", len(matched), hits)
	}
	if tail, ok := stmt.Flag("tail"); ok {
		n, err := strconv.Atoi(tail)
		if err != nil || n <= 0 {
			return badRequest(stmt, "`--tail` must be a positive integer, got "+strconv.Quote(tail)), 400
		}
		if n < len(matched) {
			matched = matched[len(matched)-n:]
		}
	}

	filtered, code, cut, total := capGeneratedCounted(strings.Join(matched, "\n"), win)
	if cut > 0 {
		noteTruncated(headers, code, cut, total, win)
	}
	return envelope.Slot{Code: code, Headers: headers, Body: filtered}, code
}
