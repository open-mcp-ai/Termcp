package mcp

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Line addressing (from_line) and the raw-stream byte fields are the parts of
// shell_output that line-protocol consumers build cursors on. The persisted
// tests below write a log deterministically, so every offset and line number
// asserted here is exact, not "contains".

// persistDeadSessionWithLines archives a DEAD session and fills its log with n
// numbered lines, the same way unified_output_test.go builds one.
func persistDeadSessionWithLines(t *testing.T, store *storage.Store, sessMgr *session.Manager, msgMgr *message.Manager, n int) (string, string) {
	t.Helper()
	sessID, shellID := "lines-arch", "lines-shell"
	meta := api.Session{
		ID:        sessID,
		Name:      "Lines",
		Status:    api.SessionExited,
		CreatedAt: clock.Now() - (10 * 60 * 1000),
		Shells:    []api.Session{{ID: shellID, Name: "main", Status: api.SessionExited}},
	}
	if err := store.SaveSession(meta); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	if err := store.SaveShell(meta.ID, meta.Shells[0]); err != nil {
		t.Fatalf("SaveShell: %v", err)
	}
	if err := sessMgr.RestoreDead(); err != nil {
		t.Fatalf("RestoreDead: %v", err)
	}
	for i := 1; i <= n; i++ {
		if _, err := msgMgr.AppendOutput(sessID, shellID, []byte(fmt.Sprintf("LINE_%02d\n", i))); err != nil {
			t.Fatalf("AppendOutput %d: %v", i, err)
		}
	}
	return sessID, shellID
}

func readOutputMap(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	res, err := s.handleReadOutput(context.Background(), makeRequest(args))
	if err != nil {
		t.Fatalf("handleReadOutput: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleReadOutput returned error: %v", res.Content[0])
	}
	return parseResult(t, res)
}

func TestFromLine_ReadsWholeLinesFromTheLog(t *testing.T) {
	s, store, sessMgr, msgMgr := newTestServerWithHistory(t)
	_, shellID := persistDeadSessionWithLines(t, store, sessMgr, msgMgr, 50)

	m := readOutputMap(t, s, map[string]any{
		"shell_id":  shellID,
		"from_line": 48.0,
		"max_lines": 2.0,
	})
	out := m["output"].(string)
	if out != "LINE_48\nLINE_49\n" {
		t.Fatalf("from_line=48 max_lines=2 output = %q, want exactly lines 48 and 49", out)
	}
	if got := m["from_line"].(float64); got != 48 {
		t.Errorf("from_line echo = %v, want 48", got)
	}
	if got := m["to_line"].(float64); got != 49 {
		t.Errorf("to_line = %v, want 49", got)
	}
	if got := m["total_lines"].(float64); got != 50 {
		t.Errorf("total_lines = %v, want 50", got)
	}
	// LINE_48\n is 8 bytes, LINE_49\n too.
	lb := m["line_bytes"].([]any)
	if len(lb) != 2 || lb[0].(float64) != 8 || lb[1].(float64) != 8 {
		t.Errorf("line_bytes = %v, want [8 8]", lb)
	}
	// The raw-stream invariant a paging cursor relies on.
	start := int64(m["start_offset"].(float64))
	end := int64(m["end_offset"].(float64))
	if start+int64(m["raw_bytes"].(float64)) != end {
		t.Errorf("start_offset %d + raw_bytes %d != end_offset %d", start, m["raw_bytes"], end)
	}
	if got := m["partial_tail_bytes"].(float64); got != 0 {
		t.Errorf("partial_tail_bytes = %v, want 0: the window ends on a line boundary", got)
	}
	if !m["has_more"].(bool) {
		t.Error("has_more = false, want true: line 50 follows")
	}
}

func TestFromLine_LastLineCarriesThePartialTail(t *testing.T) {
	s, store, sessMgr, msgMgr := newTestServerWithHistory(t)
	sessID, shellID := persistDeadSessionWithLines(t, store, sessMgr, msgMgr, 50)
	// A trailing write without a newline: unnumbered, but part of the stream.
	if _, err := msgMgr.AppendOutput(sessID, shellID, []byte("PARTIAL")); err != nil {
		t.Fatalf("AppendOutput partial: %v", err)
	}

	m := readOutputMap(t, s, map[string]any{
		"shell_id":  shellID,
		"from_line": 50.0,
	})
	out := m["output"].(string)
	if out != "LINE_50\nPARTIAL" {
		t.Fatalf("output = %q, want line 50 plus the partial tail", out)
	}
	if got := m["to_line"].(float64); got != 50 {
		t.Errorf("to_line = %v, want 50: the partial tail takes no line number", got)
	}
	if got := m["total_lines"].(float64); got != 50 {
		t.Errorf("total_lines = %v, want 50", got)
	}
	if got := m["partial_tail_bytes"].(float64); got != 7 {
		t.Errorf("partial_tail_bytes = %v, want 7 (len PARTIAL)", got)
	}
	// Once the line completes, the bytes the partial tail held become line 51
	// itself, and the same number names the same line.
	if _, err := msgMgr.AppendOutput(sessID, shellID, []byte("\n")); err != nil {
		t.Fatalf("AppendOutput newline: %v", err)
	}
	m = readOutputMap(t, s, map[string]any{
		"shell_id":  shellID,
		"from_line": 51.0,
	})
	if out := m["output"].(string); out != "PARTIAL\n" {
		t.Fatalf("line 51 after completion = %q, want PARTIAL plus the newline", out)
	}
	if got := m["total_lines"].(float64); got != 51 {
		t.Errorf("total_lines = %v, want 51", got)
	}
	if got := m["partial_tail_bytes"].(float64); got != 0 {
		t.Errorf("partial_tail_bytes = %v, want 0 after completion", got)
	}
}

func TestFromLine_PastTheEndFailsLoudly(t *testing.T) {
	s, store, sessMgr, msgMgr := newTestServerWithHistory(t)
	_, shellID := persistDeadSessionWithLines(t, store, sessMgr, msgMgr, 3)

	res, err := s.handleReadOutput(context.Background(), makeRequest(map[string]any{
		"shell_id":  shellID,
		"from_line": 9.0,
	}))
	if err != nil {
		t.Fatalf("handleReadOutput: %v", err)
	}
	if !res.IsError {
		t.Fatal("from_line past the end must be an error, not a wrong window")
	}
	if !strings.Contains(res.Content[0].(mcpgo.TextContent).Text, "out of range") {
		t.Errorf("error should say the line is out of range: %v", res.Content[0])
	}
}

// TestOffsetRead_RawAndPartialByteFields pins the two fields every other read
// form gained: raw_bytes (cursor-safe) and partial_tail_bytes (line-boundary
// marker), both about the RAW stream.
func TestOffsetRead_RawAndPartialByteFields(t *testing.T) {
	s, store, sessMgr, msgMgr := newTestServerWithHistory(t)
	sessID, shellID := persistDeadSessionWithLines(t, store, sessMgr, msgMgr, 2)
	if _, err := msgMgr.AppendOutput(sessID, shellID, []byte("tail-no-newline")); err != nil {
		t.Fatalf("AppendOutput: %v", err)
	}

	m := readOutputMap(t, s, map[string]any{
		"shell_id":  shellID,
		"offset":    0.0,
		"max_bytes": 40.0,
	})
	start := int64(m["start_offset"].(float64))
	end := int64(m["end_offset"].(float64))
	if start+int64(m["raw_bytes"].(float64)) != end {
		t.Errorf("raw_bytes inconsistent: %d + %v != %d", start, m["raw_bytes"], end)
	}
	// bytes_returned counts cleaned text, raw_bytes the stream; with nothing to
	// strip they agree — and the alias keeps old clients working.
	if m["bytes_returned"].(float64) != m["returned_bytes"].(float64) {
		t.Error("bytes_returned must stay an alias of returned_bytes")
	}
	if got := m["partial_tail_bytes"].(float64); got != 15 {
		t.Errorf("partial_tail_bytes = %v, want 15 (tail-no-newline)", got)
	}
	if _, ok := m["total_lines"]; ok {
		t.Error("total_lines must not appear on a non-from_line read")
	}
	if _, ok := m["from_line"]; ok {
		t.Error("from_line must not appear on a byte-addressed read")
	}
}

// TestMessageSpans_CarryLineNumbers pins message(action=list)'s from_line/to_line.
func TestMessageSpans_CarryLineNumbers(t *testing.T) {
	s, store, sessMgr, msgMgr := newTestServerWithHistory(t)
	sessID, _ := persistDeadSessionWithLines(t, store, sessMgr, msgMgr, 10)

	res, err := s.handleMessageOps(context.Background(), makeRequest(map[string]any{
		"action":     "list",
		"session_id": sessID,
	}))
	if err != nil {
		t.Fatalf("message list: %v", err)
	}
	if res.IsError {
		t.Fatalf("message list error: %v", res.Content[0])
	}
	m := parseResult(t, res)
	if got := m["total_lines"].(float64); got != 10 {
		t.Errorf("total_lines = %v, want 10", got)
	}
	spans := m["spans"].([]any)
	if len(spans) == 0 {
		t.Fatal("no spans returned")
	}
	first := spans[0].(map[string]any)
	// One output span covering all ten lines: from 1 to 10.
	if got := first["from_line"].(float64); got != 1 {
		t.Errorf("first span from_line = %v, want 1", got)
	}
	if got := first["to_line"].(float64); got != 10 {
		t.Errorf("first span to_line = %v, want 10", got)
	}
}

// TestParseReadParams_FromLineConflicts pins the one-selector-per-read rule.
func TestParseReadParams_FromLineConflicts(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"with tail_lines", map[string]any{"from_line": 1.0, "tail_lines": 3.0}},
		{"with offset", map[string]any{"from_line": 1.0, "offset": 0.0}},
		{"with reader_id", map[string]any{"from_line": 1.0, "reader_id": 2.0}},
		{"negative", map[string]any{"from_line": -1.0}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, bad := parseReadParams(c.args); bad == nil {
				t.Errorf("accepted conflicting arguments %v", c.args)
			}
		})
	}
	t.Run("alone is valid", func(t *testing.T) {
		p, bad := parseReadParams(map[string]any{"from_line": 5.0})
		if bad != nil {
			t.Fatalf("rejected from_line alone: %v", bad.Content[0])
		}
		if p.FromLine != 5 {
			t.Errorf("fromLine = %d, want 5", p.FromLine)
		}
	})
}

// TestFromLine_LiveSession reads lines from a live internal pipe shell: the
// numbering must match the log the shell is still writing.
func TestFromLine_LiveSession(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)

	var command string
	var args []any
	if runtime.GOOS == "windows" {
		command = "powershell.exe"
		args = testShellArgs("-NoLogo", "-NoProfile", "-Command",
			"1..20 | ForEach-Object { Write-Output (\"LINE_{0:D2}\" -f $_) }")
	} else {
		command = "/bin/sh"
		args = testShellArgs("-c", "i=1; while [ $i -le 20 ]; do echo LINE_$(printf '%02d' $i); i=$((i+1)); done")
	}
	startRes, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    command,
		"args":       args,
		"mode":       "pipe",
		"ssh_config": "internal",
	}))
	if err != nil || startRes.IsError {
		t.Fatalf("start session failed: %v", err)
	}
	mStart := parseResult(t, startRes)
	sessID := mStart["session_id"].(string)
	shellID := mStart["shell_id"].(string)
	t.Cleanup(func() {
		_, _ = s.handleTerminateSession(context.Background(), makeRequest(map[string]any{"session_id": sessID, "force": true}))
	})

	// Wait until the stream holds all 20 lines, then read the first two by number.
	// Early polls legitimately fail (the log is empty until the process prints),
	// so only the deadline turns into a test failure.
	deadline := time.Now().Add(6 * time.Second)
	for {
		res, err := s.handleReadOutput(context.Background(), makeRequest(map[string]any{
			"shell_id":  shellID,
			"from_line": 20.0,
			"max_lines": 1.0,
		}))
		if err == nil && !res.IsError {
			m := parseResult(t, res)
			if strings.Contains(m["output"].(string), "LINE_20") {
				break
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("timed out waiting for line 20")
		}
		time.Sleep(50 * time.Millisecond)
	}
	m := readOutputMap(t, s, map[string]any{
		"shell_id":  shellID,
		"from_line": 1.0,
		"max_lines": 2.0,
	})
	if out := m["output"].(string); !strings.HasPrefix(out, "LINE_01\nLINE_02\n") {
		t.Fatalf("live from_line=1 output = %q, want it to start with LINE_01/LINE_02", out)
	}
	if got := m["total_lines"].(float64); got != 20 {
		t.Errorf("live total_lines = %v, want 20", got)
	}
}

// TestHandleStartSubShell_CommandWithArgs pins the shell_open args contract from
// the pipe-channel issue: command+args reach the process as argv, while a
// command string containing spaces stays ONE executable name (and fails), so no
// caller can accidentally get shell line evaluation.
func TestHandleStartSubShell_CommandWithArgs(t *testing.T) {
	s := newTestServer(t)

	startRes, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"mode":       "pty",
		"ssh_config": "internal",
	}))
	if err != nil || startRes.IsError {
		t.Fatalf("start session failed: %v %+v", err, startRes)
	}
	sessionID := parseResult(t, startRes)["session_id"].(string)

	waitForOutput := func(t *testing.T, shellID string) string {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			res, err := s.handleReadOutput(context.Background(), makeRequest(map[string]any{
				"shell_id": shellID, "timeout": 0.5,
			}))
			if err == nil && !res.IsError {
				m := parseResult(t, res)
				if out := m["output"].(string); out != "" {
					return out
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("timed out waiting for pipe shell output")
		return ""
	}

	// args reach the process: `go version` runs and prints its version line.
	openRes, err := s.handleStartSubShell(context.Background(), makeRequest(map[string]any{
		"session_id": sessionID,
		"mode":       "pipe",
		"command":    "go",
		"args":       []string{"version"},
	}))
	if err != nil || openRes.IsError {
		t.Fatalf("shell_open with args failed: %v %+v", err, openRes)
	}
	if out := waitForOutput(t, parseResult(t, openRes)["shell_id"].(string)); !strings.Contains(out, "go version go") {
		t.Fatalf("shell_open(command=go, args=[version]) output = %q, want the go version line", out)
	}

	// A command string with spaces is ONE TOKEN: the argv spelling quotes it, so the
	// remote shell looks for a program literally named `go version` and fails.
	//
	// The wording is the SHELL's (`sh: go version: command not found`), not Go's
	// `executable file not found`: the loopback now runs an exec request through a
	// login shell exactly as a remote sshd does, which is what makes its answer
	// match a real host's. The invariant this pins is the one that matters — the
	// token is named WHOLE and was never evaluated as a shell line.
	spaceRes, err := s.handleStartSubShell(context.Background(), makeRequest(map[string]any{
		"session_id": sessionID,
		"mode":       "pipe",
		"command":    "go version",
	}))
	if err != nil || spaceRes.IsError {
		t.Fatalf("shell_open with a spaced command must still open the channel: %v %+v", err, spaceRes)
	}
	out := waitForOutput(t, parseResult(t, spaceRes)["shell_id"].(string))
	if strings.Contains(out, "go version go") {
		t.Fatalf("spaced command was evaluated as a shell line: %q", out)
	}
	if !strings.Contains(out, "go version") {
		t.Fatalf("spaced command output = %q, want an error naming the whole token", out)
	}
}

// TestShellInput_ReturnsTranscriptHandle pins shell_input's return: the
// submission position a line-protocol consumer pairs responses with. The bytes
// reach the log through the terminal's echo (or, on pipe, not at all), so the
// only per-call anchor is the stream end read before the write.
func TestShellInput_ReturnsTranscriptHandle(t *testing.T) {
	s, _, _, _ := newTestServerWithHistory(t)

	startRes, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    testShell(),
		"mode":       "pty",
		"ssh_config": "internal",
	}))
	if err != nil || startRes.IsError {
		t.Fatalf("start session failed: %v %+v", err, startRes)
	}
	mStart := parseResult(t, startRes)
	sessID := mStart["session_id"].(string)
	shellID := mStart["shell_id"].(string)
	t.Cleanup(func() {
		_, _ = s.handleTerminateSession(context.Background(), makeRequest(map[string]any{"session_id": sessID, "force": true}))
	})

	sendInput := func(t *testing.T, text string) map[string]any {
		t.Helper()
		res, err := s.handleSendInput(context.Background(), makeRequest(map[string]any{
			"shell_id": shellID, "text": text,
		}))
		if err != nil || res.IsError {
			t.Fatalf("shell_input(%q) failed: %v %+v", text, err, res)
		}
		return parseResult(t, res)
	}

	m1 := sendInput(t, "echo HANDLE_ONE\n") // submitted line: the only kind that gets a mark
	if m1["success"] != true {
		t.Fatalf("success = %v, want true", m1["success"])
	}
	offset1, ok := m1["offset"].(float64)
	if !ok || offset1 < 0 {
		t.Fatalf("offset = %v, want a non-negative number", m1["offset"])
	}
	if _, ok := m1["total_lines"].(float64); !ok {
		t.Fatalf("total_lines missing from return: %v", m1)
	}
	if _, ok := m1["line"].(float64); !ok {
		t.Fatalf("line missing from return: %v", m1)
	}

	// The handle moves forward: a later submission never lands earlier.
	m2 := sendInput(t, "echo HANDLE_TWO\n")
	offset2, ok := m2["offset"].(float64)
	if !ok {
		t.Fatalf("second input returned no Offset: %v", m2)
	}
	if offset2 < offset1 {
		t.Fatalf("second offset %v < first %v: the handle went backwards", offset2, offset1)
	}

	// The transcript's own index saw the input: an AI-input ('a') span must
	// exist at or after the reported submission position.
	listRes, err := s.handleMessageOps(context.Background(), makeRequest(map[string]any{
		"action": "list", "session_id": sessID,
	}))
	if err != nil || listRes.IsError {
		t.Fatalf("message list failed: %v %+v", err, listRes)
	}
	spans := parseResult(t, listRes)["spans"].([]any)
	found := false
	for _, raw := range spans {
		span := raw.(map[string]any)
		if span["status"] == "a" && span["start"].(float64) >= offset1 {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no 'a' span at or after offset %v in %v", offset1, spans)
	}
}
