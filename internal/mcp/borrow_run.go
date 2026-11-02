package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/directive"
	"github.com/open-mcp-ai/termcp/internal/encoding"
	"github.com/open-mcp-ai/termcp/internal/envelope"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// Short-circuit execution (#237): `run use:<host> $1`.
//
// WHY THIS IS NOT A SESSION. `connect` + `run` + `close` is three calls and leaves a
// DEAD tile in the session list for a command that printed one line. A session is a
// user-facing record with a manifest, a transcript and an archive entry; a one-off
// `run use:prod $1` has none of those to keep. So the command runs on a POOLED
// transport — the same physical connection an ordinary session on that host would
// hold — and nothing is registered.
//
// WHAT IS GIVEN UP, SAID PLAINLY. There is no channel, so nothing is retained: no
// cursor, no later `read`, no `key`/`input`. The reply is whatever the process
// printed before it exited or the caller's window ran out. A command that must be
// watched, typed into, or paged afterwards wants `connect`, which is the only way to
// get a channel.
//
// THE WINDOW IS A DEADLINE, NOT A POLL. A channel `run` waits for a stream to go
// quiet because a terminal has no completion signal. Here the SSH channel's process
// IS the signal — Wait returns when it exits — so the caller's timeout bounds a real
// wait, and a command that outlives it is reported as stopped, not as silence.

// refuseBorrowedReview enforces a host profile's default review policy on
// operations the capability table marks reviewable.
//
// A BORROWED OPERATION HAS NO SESSION, and review mode is a session's queue. Running
// a reviewable operation here would make `default_approval` a gate that only applies
// to the `connect` spelling. Refuse, and name the route that can hold the decision.
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
// admission exit used by every handler. It is a synthetic shell_input target: there is
// no channel to resolve, so the ordinary tool guard has nothing to restrict, but the
// borrowed-host review rule at that same exit still applies.
func (s *Server) checkBorrowedRunAdmission(ctx context.Context, target string) *mcpgo.CallToolResult {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = "shell_input"
	req.Params.Arguments = argsOf("shell_id", target)
	bad, _ := s.checkAdmission(ctx, "shell_input", req)
	return bad
}

// capture is a bounded writer: it keeps at most limit bytes and still counts
// everything, so a huge output neither exhausts memory nor blocks the remote
// process on a full SSH channel.
type capture struct {
	mu    sync.Mutex
	data  []byte
	limit int
	total int64
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total += int64(len(p))
	if left := c.limit - len(c.data); left > 0 {
		if left > len(p) {
			left = len(p)
		}
		c.data = append(c.data, p[:left]...)
	}
	return len(p), nil
}

func (c *capture) snapshot() ([]byte, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.data...), c.total
}

// runBorrowedHost runs one shell command on a borrowed pooled transport. Nothing is
// registered; the SSH channel and the pool lease are released on return.
func (s *Server) runBorrowedHost(ctx context.Context, host, command string, win termArgs, captureLimit int) (output []byte, total int64, exitCode int, exited bool, waited time.Duration, err error) {
	started := time.Now()
	defer func() { waited = time.Since(started) }()

	cli, release, err := s.acquireSSHTransport(ctx, "use:"+host)
	if err != nil {
		return nil, 0, -1, false, 0, err
	}
	defer release()

	ch, err := cli.NewSession()
	if err != nil {
		return nil, 0, -1, false, 0, fmt.Errorf("open command channel on %s: %w", host, err)
	}
	ch.Stdin = bytes.NewReader(nil) // EOF: this form does not converse.
	out := &capture{limit: captureLimit}
	ch.Stdout, ch.Stderr = out, out
	// The command goes to the server as ONE string, which is what an SSH exec is: the
	// remote login shell expands pipes and redirections, exactly as it would for
	// `ssh host 'ls | head'`. Splitting it here would run a different command.
	if err := ch.Start(command); err != nil {
		closeErr := closeBorrowedCommand(ch, cli)
		if closeErr != nil {
			return nil, 0, -1, false, 0, fmt.Errorf("start command: %w; %v", err, closeErr)
		}
		return nil, 0, -1, false, 0, fmt.Errorf("start command on %s: %w", host, err)
	}

	wait := time.Duration(win.TimeoutSeconds * float64(time.Second))
	if wait <= 0 {
		wait = defaultTimeoutSeconds * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	done := make(chan error, 1)
	go func() { done <- ch.Wait() }()

	var waitErr error
	stopped := false
	select {
	case waitErr = <-done:
		exited = true
	case <-timer.C:
		// The process outlived the caller's window. It is CLOSED rather than left
		// running: a one-shot reply has no handle that could reach it again, so
		// leaving it would be a process nobody can address or report on.
		stopped = true
	case <-ctx.Done():
		stopped = true
	}

	if closeErr := closeBorrowedCommand(ch, cli); closeErr != nil {
		output, total = out.snapshot()
		return output, total, -1, false, 0, closeErr
	}
	if stopped {
		select {
		case <-done:
		case <-time.After(time.Second):
			output, total = out.snapshot()
			return output, total, -1, false, 0, fmt.Errorf("command channel on %s did not stop within 1s of being closed", host)
		}
		output, total = out.snapshot()
		if ctx.Err() != nil {
			return output, total, -1, false, 0, ctx.Err()
		}
		return output, total, -1, false, 0, nil // stopped by the deadline; Status says so
	}

	output, total = out.snapshot()
	exitCode = borrowedExitCode(waitErr)
	if waitErr != nil && exitCode < 0 {
		err = waitErr
	}
	if closeErr := closeBorrowedCommand(ch, cli); err == nil {
		err = closeErr
	}
	return output, total, exitCode, true, 0, err
}

// borrowedExitCode reads the process status out of Wait's error. -1 means "there was
// no exit status", which is not the same fact as exit code 0.
func borrowedExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus()
	}
	return -1
}

// closeBorrowedCommand bounds channel teardown. If the peer stops reading the
// transport, SSH channel close can block on its write; force-closing that shared
// connection is then the only way to release the waiter and avoid leaking it.
func closeBorrowedCommand(ch *ssh.Session, client *ssh.Client) error {
	done := make(chan error, 1)
	go func() { done <- ch.Close() }()
	select {
	case err := <-done:
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	case <-time.After(time.Second):
		go func() { _ = client.Close() }()
		return fmt.Errorf("closing the SSH command channel timed out; closed its shared transport")
	}
}

// borrowedRunSlot renders a short-circuit run.
//
// THE HEADERS ARE A CHANNEL RUN'S HEADERS, because a caller should not learn a second
// vocabulary for the same facts: Status/Exit-Code say how the process ended, a
// non-zero exit is a 2xx failing slot with Error-Code command_failed, and Content-Range
// addresses the bytes this reply carries.
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
	// A STOPPED RUN AND A FAILED ONE BOTH NEED A SENTENCE, not just a header.
	// `Status: stopped` says what happened but not what to do, and an empty body with
	// `Exit-Code: 3` is a question the caller has to answer alone. The note goes in
	// Hint (the reply's own voice) and, when there is no output at all, in the body —
	// there is nothing there for it to corrupt.
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
			slot.Headers["Hint"] = note
			if slot.Body == "" {
				slot.Body = note
			}
		}
		if exited {
			slot.Headers["Status"] = "exited"
			slot.Headers["Exit-Code"] = headers["Exit-Code"]
			if exitCode != 0 {
				slot.Headers["Error-Code"] = "command_failed"
				slot.Headers["Hint"] = note
			}
		}
		return slot
	}

	slot := tailSlot(envelope.Slot{Code: 200, Headers: headers, Body: body}, stmt)
	slot.Body, slot.Code, _, _ = capGeneratedCounted(slot.Body, win)
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

// validateBorrowedRunFlags refuses a bad filter BEFORE the command runs.
//
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
