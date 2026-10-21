package mcp

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestReadRefusesBytesJSONWouldCorrupt is the end-to-end half of the byte-fidelity
// contract: it runs a real command that writes bytes no JSON consumer can
// reproduce, then drives the real read tool over the stream and asserts the call
// FAILS instead of returning mojibake.
//
// The parse-level test (TestParseReadParamsValues) only proves RequireUTF8 is set.
// That is not the property that matters: the parser could set it and the read path
// never consult it, and every offset a caller pages with would still be silently
// wrong. What must hold is that the value reaching the model is refused at the
// moment it would be corrupted.
//
// Why it matters concretely: encoding/json replaces invalid UTF-8 with U+FFFD, a
// 3-byte sequence where the original had 1. A caller paging by end_offset then
// resumes inside data it already read — with no error anywhere — and a caller
// inspecting magic bytes sees U+FFFD where the signature was.
//
// Pipe mode is used on purpose: it records exactly the command's stdout, with no
// prompt echo or shell banner that could put the offending byte outside the window
// the test reads (see TestUnifiedOutput_TailLinesLive for the same reasoning).
func TestReadRefusesBytesJSONWouldCorrupt(t *testing.T) {
	s, shellID := runPipeAndRead(t, writeRawBytesCommand())

	res := readFromStart(t, s, shellID)
	if !res.IsError {
		t.Fatalf("the read returned success for a window containing 0x89; a JSON reply "+
			"would have replaced it with U+FFFD and shifted every later offset: %s", toolResultText(res))
	}
	body := toolResultText(res)
	// The message must locate the byte and point at a byte-exact route: "not valid
	// UTF-8" alone leaves the caller with no way to read the stream at all.
	for _, want := range []string{"not valid UTF-8", "byte ", "mode=hex"} {
		if !strings.Contains(body, want) {
			t.Errorf("refusal is missing %q: %s", want, body)
		}
	}
}

// TestReadAcceptsOrdinaryOutput is the companion case. Without it, refusing EVERY
// window would satisfy the test above while breaking the tool completely.
//
// The output is deliberately ASCII, and that is not laziness: which bytes a child
// produces for non-ASCII text on Windows depends on the console code page, which
// belongs to the console (conhost) and is therefore shared with whatever else the
// test binary or the runner has done to it. An assertion of the form "this child
// emits GBK" passes alone and fails in the suite, because an earlier test moved the
// code page. Bytes that are never valid UTF-8 are covered deterministically by
// TestReadRefusesBytesJSONWouldCorrupt, which asks the child for them explicitly
// rather than inferring them from the platform.
func TestReadAcceptsOrdinaryOutput(t *testing.T) {
	s, shellID := runPipeAndRead(t, echoCommand("plain text from the shell"))
	res := readFromStart(t, s, shellID)
	if res.IsError {
		t.Fatalf("an ordinary read was refused: %s", toolResultText(res))
	}
	if !strings.Contains(toolResultText(res), "plain text from the shell") {
		t.Errorf("output did not survive the read: %s", toolResultText(res))
	}
}

// writeRawBytesCommand prints a PNG signature, whose first byte (0x89) is not
// valid UTF-8 on its own. printf is used rather than echo so the shell appends no
// newline that could move the offending byte relative to what the test expects.
func writeRawBytesCommand() string {
	if runtime.GOOS == "windows" {
		return `[Console]::OpenStandardOutput().Write([byte[]](0x89,0x50,0x4E,0x47),0,4)`
	}
	return `printf '\211PNG'`
}

// echoCommand renders a command that prints s.
func echoCommand(s string) string {
	if runtime.GOOS == "windows" {
		return `Write-Output '` + s + `'`
	}
	return `printf '%s\n' '` + s + `'`
}

// testShellForPipe is the shell a pipe-mode session runs on this platform.
func testShellForPipe() (string, []any) {
	if runtime.GOOS == "windows" {
		return "powershell.exe", testShellArgs("-NoLogo", "-NoProfile", "-Command")
	}
	return "/bin/sh", testShellArgs("-c")
}

// runPipeAndRead starts a fresh pipe session that runs command to completion and
// returns the server and shell id once the command has exited. A pipe session is
// used because its buffer contains exactly the command's output -- no prompt echo
// and no shell banner to shift the bytes the read will see.
func runPipeAndRead(t *testing.T, command string) (*Server, string) {
	t.Helper()
	s := newTestServer(t)
	cmd, baseArgs := testShellForPipe()
	args := append(append([]any{}, baseArgs...), command)
	startRes, err := s.handleStartSession(context.Background(), makeRequest(map[string]any{
		"command":    cmd,
		"args":       args,
		"mode":       "pipe",
		"ssh_config": "internal",
	}))
	if err != nil || startRes.IsError {
		t.Fatalf("start session failed: %v %s", err, toolResultText(startRes))
	}
	start := parseResult(t, startRes)
	shellID := start["shell_id"].(string)
	t.Cleanup(func() {
		_, _ = s.handleTerminateSession(context.Background(), makeRequest(map[string]any{
			"session_id": start["session_id"], "force": true,
		}))
	})
	waitForShellToExit(t, s, shellID)
	return s, shellID
}

// waitForShellToExit waits for the pipe command to finish.
//
// It polls the SHELL's status rather than the read's total_bytes, and that choice
// is forced: a read whose window would be refused returns an error with no counts
// in it, so there is nothing to compare across polls. Status is the one signal that
// exists on both paths -- and it is the honest one anyway, because "the command has
// exited" is what makes its output complete rather than "the numbers stopped moving
// while I looked" (see dead_session_test.go, which waits the same way).
func waitForShellToExit(t *testing.T, s *Server, shellID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		cs := s.sessMgr.GetChildShell(shellID)
		if cs != nil && cs.Info().Status != api.SessionRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the pipe command in %s never exited", shellID)
}

// readFromStart performs the read under test: offset 0, the defaults a
// model-facing caller would use.
func readFromStart(t *testing.T, s *Server, shellID string) *mcpgo.CallToolResult {
	t.Helper()
	res, err := s.handleReadOutput(context.Background(), mcpgo.CallToolRequest{
		Params: mcpgo.CallToolParams{Arguments: map[string]any{
			"shell_id": shellID,
			"offset":   float64(0),
			"timeout":  float64(0.5),
		}},
	})
	if err != nil {
		t.Fatalf("handleReadOutput: %v", err)
	}
	return res
}
