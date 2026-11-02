package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// TestRecvFrameHonoursCallersBytesLimit pins that frames larger than the default
// 1 MiB floor are deliverable when the caller names a larger window (issue #229).
//
// The window used to be `window := defaultBytesLimit; if window < minRecvWindow { window = minRecvWindow }`
// which locked the window to 1 MiB regardless of what the caller passed — leaving
// MCP tools that return base64 images (2-4 MB screenshots) permanently dead with
// 413, and no flag or cursor could page through them.
func TestRecvFrameHonoursCallersBytesLimit(t *testing.T) {
	s := newTestServer(t)

	// Write the frame to a file, then `cat` it: no shell quoting, no f-string escaping.
	const payloadSize = 1200 * 1024
	frame := fmt.Sprintf(`{"jsonrpc":"2.0","id":42,"result":{"data":"%s"}}`+"\n",
		strings.Repeat("A", payloadSize))
	framePath := filepath.Join(t.TempDir(), "frame.json")
	if err := os.WriteFile(framePath, []byte(frame), 0o600); err != nil {
		t.Fatal(err)
	}

	handle := openPipe(t, s, "cat "+framePath)
	defer func() {
		termText(t, s, map[string]any{"cmd": "close " + handle})
	}()

	waitForBytes(t, s, handle, len(frame))

	// 1. Without BytesLimitSet, the 1 MiB floor applies and the 1.2 MiB frame answers 413.
	defaultSlot := s.mcpRecvFrame(context.Background(), directive.Statement{Path: "read " + handle + " --frame"}, handle, termArgs{})
	if defaultSlot.Code != 413 {
		t.Fatalf("without bytes_limit: expected 413, got %d: %s", defaultSlot.Code, defaultSlot.Body)
	}

	// Reset the cursor to retry the same frame with a larger window.
	s.setRecvCursor(handle, 0)

	// 2. With BytesLimitSet to 2 MiB, the frame must be delivered.
	win := termArgs{BytesLimitSet: true, BytesLimit: 2 * 1024 * 1024}
	largeSlot := s.mcpRecvFrame(context.Background(), directive.Statement{Path: "read " + handle + " --frame"}, handle, win)
	if largeSlot.Code != 200 {
		t.Fatalf("with bytes_limit=2MiB: expected 200, got %d: %s", largeSlot.Code, largeSlot.Body)
	}
	if !strings.Contains(largeSlot.Body, `"id":42`) {
		t.Fatal("delivered frame does not contain the expected JSON-RPC body")
	}
}

// waitForBytes blocks until the channel has buffered at least n bytes.
func waitForBytes(t *testing.T, s *Server, handle string, n int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		cs, bad := s.requireShell(handle)
		if bad == nil && cs != nil {
			b, _, _ := cs.OutputByteRange(0, n)
			if len(b) >= n {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("channel %s never buffered %d bytes", handle, n)
}
