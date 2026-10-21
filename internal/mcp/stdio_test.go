package mcp

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Stream filtering (#104): a pipe channel keeps stdout and stderr apart at the
// source, and shell_output(stream=) selects between them without ever moving
// an offset. The per-stream assertions below are exact under any interleaving
// of the two pumps, because per-stream order is what the transport preserves.

func startPipeStdioSession(t *testing.T, script string) (*Server, string, string) {
	t.Helper()
	s, _, _, _ := newTestServerWithHistory(t)

	var command string
	var args []any
	if runtime.GOOS == "windows" {
		command = "powershell.exe"
		args = testShellArgs("-NoLogo", "-NoProfile", "-Command", script)
	} else {
		command = "/bin/sh"
		args = testShellArgs("-c", script)
	}
	startRes, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    command,
		"args":       args,
		"mode":       "pipe",
		"ssh_config": "internal",
	}))
	if err != nil || startRes.IsError {
		t.Fatalf("start session failed: %v %+v", err, startRes)
	}
	m := parseResult(t, startRes)
	if got := m["stderr"].(string); got != "separate" {
		t.Fatalf("pipe receipt stderr = %q, want separate", got)
	}
	sessID := m["session_id"].(string)
	t.Cleanup(func() {
		_, _ = s.handleTerminateSession(context.Background(), makeRequest(map[string]any{"session_id": sessID, "force": true}))
	})
	return s, sessID, m["shell_id"].(string)
}

func waitForOutput(t *testing.T, s *Server, shellID, marker string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m := readOutputMap(t, s, map[string]any{"shell_id": shellID, "tail_lines": 5.0})
		if containsLine(m["output"].(string), marker) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q", marker)
}

// waitForSubstrings waits for every substring to appear in the default read.
// The boundary test needs this: depending on how the two output pumps
// interleave, "PART" and "OUT" are two raw lines or one merged line, but the
// substrings show up either way once all three writes have landed.
func waitForSubstrings(t *testing.T, s *Server, shellID string, subs ...string) {
	t.Helper()
	var last string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m := readOutputMap(t, s, map[string]any{"shell_id": shellID, "tail_lines": 5.0})
		out := m["output"].(string)
		all := true
		for _, sub := range subs {
			if !strings.Contains(out, sub) {
				all = false
				break
			}
		}
		if all {
			return
		}
		last = m["output"].(string)
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %v; last read: %q", subs, last)
}

func containsLine(out, want string) bool {
	for _, l := range splitLines(out) {
		if l == want {
			return true
		}
	}
	return false
}

func splitLines(out string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(out); i++ {
		if out[i] == '\n' {
			lines = append(lines, trimCR(out[start:i]))
			start = i + 1
		}
	}
	if start < len(out) {
		lines = append(lines, trimCR(out[start:]))
	}
	return lines
}

func trimCR(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\r' {
		return s[:len(s)-1]
	}
	return s
}

// readStream reads positionally (offset 0, no byte cap): a positional window is
// the deterministic way to test the filter. The live cursor path shares the
// same filterStream helper, but a cursor consumes what it returns, so two
// cursor reads cannot both see the stream.
func readStream(t *testing.T, s *Server, shellID, stream string, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"shell_id": shellID, "offset": 0.0, "max_bytes": 0.0}
	if stream != "" {
		args["stream"] = stream
	}
	for k, v := range extra {
		args[k] = v
	}
	return readOutputMap(t, s, args)
}

// ① + ② + ③ + ④: a pipe session writing stdout, stderr, stdout separates
// exactly; the filter shrinks only the returned content; the cursor keeps
// meaning raw-window end.
func TestStreamFilter_PipeSeparatesStderrCrossOS(t *testing.T) {
	var script string
	if runtime.GOOS == "windows" {
		// The sleeps keep each write in its own scheduling window: a process
		// that writes three times across two pipes and exits immediately can
		// lose the last frame to the transport's drain race, which is a
		// pre-existing property of fast pipe commands, not what these tests
		// are about.
		script = `[Console]::Out.WriteLine('OUT-1'); Start-Sleep -m 200; [Console]::Error.WriteLine('ERR-1'); Start-Sleep -m 200; [Console]::Out.WriteLine('OUT-2'); Start-Sleep -m 200`
	} else {
		script = `echo OUT-1; sleep 0.2; echo ERR-1 >&2; sleep 0.2; echo OUT-2; sleep 0.2`
	}
	s, _, shellID := startPipeStdioSession(t, script)
	waitForOutput(t, s, shellID, "OUT-2")

	// The receipt promises separate stderr for a pipe channel.
	// (asserted on the start call in startPipeStdioSession's caller below)

	// ② stream="o" returns exactly the stdout bytes; the offsets still describe
	// the raw window, so the stderr bytes show up as returned < raw.
	m := readStream(t, s, shellID, "o", nil)
	if out := m["output"].(string); out != "OUT-1\nOUT-2\n" {
		t.Fatalf("stream=o output = %q, want exactly the two stdout lines", out)
	}
	returned := int64(m["bytes_returned"].(float64))
	raw := int64(m["raw_bytes"].(float64))
	if raw <= returned {
		t.Fatalf("raw_bytes %d <= bytes_returned %d: the stderr bytes were not filtered out", raw, returned)
	}
	if end := int64(m["end_offset"].(float64)); end != int64(m["total_bytes"].(float64)) {
		t.Fatalf("end_offset %d != total_bytes %v: the filter moved the cursor", end, m["total_bytes"])
	}

	// stream="e" returns exactly the stderr bytes - via the offset window...
	m = readStream(t, s, shellID, "e", nil)
	if out := m["output"].(string); out != "ERR-1\n" {
		t.Fatalf("stream=e output = %q, want exactly the stderr line", out)
	}
	// ...and via the tail window, which filters the same raw bytes. No offset is
	// passed: absent is how a caller says "no positional read" now, since -1 was a
	// sentinel that had to be sent as an invalid value.
	m = readStream(t, s, shellID, "e", map[string]any{"tail_lines": 5.0})
	if out := m["output"].(string); out != "ERR-1\n" {
		t.Fatalf("stream=e tail output = %q, want exactly the stderr line", out)
	}

	// ④ the default read is unfiltered: it sees both streams.
	m = readStream(t, s, shellID, "", nil)
	out := m["output"].(string)
	if !containsLine(out, "OUT-1") || !containsLine(out, "ERR-1") || !containsLine(out, "OUT-2") {
		t.Fatalf("default read lost a stream: %q", out)
	}
	all := readStream(t, s, shellID, "all", nil)
	if all["output"].(string) != out {
		t.Fatalf("stream=all differs from the default read")
	}
}

// ⑤ a line spanning the o/e boundary is split by the filter and rejoined
// without losing a byte: stdout "PART" (no newline), stderr "ERR\n", stdout
// "OUT\n" reassembles, on the stdout side, into "PARTOUT\n".
func TestStreamFilter_LineAcrossTheBoundary(t *testing.T) {
	var script string
	if runtime.GOOS == "windows" {
		script = `[Console]::Out.Write('PART'); Start-Sleep -m 200; [Console]::Error.WriteLine('ERR'); Start-Sleep -m 200; [Console]::Out.WriteLine('OUT'); Start-Sleep -m 200`
	} else {
		script = `printf PART; sleep 0.2; echo ERR >&2; sleep 0.2; echo OUT; sleep 0.2`
	}
	s, _, shellID := startPipeStdioSession(t, script)
	waitForSubstrings(t, s, shellID, "ERR", "OUT")

	m := readStream(t, s, shellID, "o", nil)
	if out := m["output"].(string); out != "PARTOUT\n" {
		t.Fatalf("stream=o output = %q, want PARTOUT (split line rejoined, no bytes lost)", out)
	}
	m = readStream(t, s, shellID, "e", nil)
	if out := m["output"].(string); out != "ERR\n" {
		t.Fatalf("stream=e output = %q, want ERR line", out)
	}
}

// ⑥ pty merges stderr at the source: no stderr ranges exist, "e" is empty,
// "o" equals the unfiltered read, and the receipt says so.
func TestStreamFilter_PtyMergesStderr(t *testing.T) {
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
	if got := mStart["stderr"].(string); got != "merged(pty)" {
		t.Fatalf("pty receipt stderr = %q, want merged(pty)", got)
	}
	t.Cleanup(func() {
		_, _ = s.handleTerminateSession(context.Background(), makeRequest(map[string]any{"session_id": sessID, "force": true}))
	})

	testRunLine(t, s, shellID, testInteractiveOutputCommand("PTY_STDIO_PROBE"))
	testReadOutputUntil(t, s, shellID, "PTY_STDIO_PROBE", 5*time.Second)

	m := readStream(t, s, shellID, "e", map[string]any{"offset": 0.0})
	if out := m["output"].(string); out != "" {
		t.Fatalf("pty stream=e output = %q, want empty: a pty has no separate stderr", out)
	}
	// Wait for the transcript to stop growing before comparing snapshots.
	//
	// Each read below is an independent snapshot of the same window, and a pty
	// echoes the command line before the command's own output arrives. Seeing the
	// marker therefore does not mean the line is complete: the read above can land
	// between "…PROBE" and its newline, and the next snapshot then differs by that
	// newline — which is not the filter disagreeing with the unfiltered read, which
	// is what this test is about.
	var prev string
	stableDeadline := time.Now().Add(5 * time.Second)
	for {
		cur := readStream(t, s, shellID, "", map[string]any{"offset": 0.0})["output"].(string)
		if cur == prev {
			break
		}
		if time.Now().After(stableDeadline) {
			t.Fatalf("the pty transcript never settled, last: %q", cur)
		}
		prev = cur
		time.Sleep(100 * time.Millisecond)
	}
	filtered := readStream(t, s, shellID, "o", map[string]any{"offset": 0.0})
	all := readStream(t, s, shellID, "", map[string]any{"offset": 0.0})
	if filtered["output"].(string) != all["output"].(string) {
		t.Fatalf("pty stream=o differs from the unfiltered read: %q vs %q",
			filtered["output"], all["output"])
	}

	// A pipe child shell of the same session reports the other receipt.
	openRes, err := s.handleStartSubShell(context.Background(), makeRequest(map[string]any{
		"session_id": sessID,
		"mode":       "pipe",
		"command":    testShell(),
	}))
	if err != nil || openRes.IsError {
		t.Fatalf("shell_open(pipe) failed: %v %+v", err, openRes)
	}
	if got := parseResult(t, openRes)["stderr"].(string); got != "separate" {
		t.Fatalf("pipe receipt stderr = %q, want separate", got)
	}
}
