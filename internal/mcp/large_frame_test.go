package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/term/directive"
)

// TestRecvFrameHonoursCallersBytesLimit pins that frames larger than the default
// floor are deliverable when the caller names a larger window (issue #229).
//
// The window used to be `window := defaultBytesLimit; if window < minRecvWindow { window = minRecvWindow }`
// which locked the window to a constant regardless of what the caller passed — leaving
// MCP tools that return base64 images (2-4 MB screenshots) permanently dead with
// 413, and no flag or cursor could page through them.
//
// THE FLOOR MOVED AGAIN IN #327: it is now the transport's own frame ceiling (8 MiB)
// rather than 1 MiB, because a window smaller than what the transport allows in was a
// wall this package invented for itself. So a frame under 8 MiB needs no bytes_limit
// at all, and the test's "too large" case has to be larger than that.
func TestRecvFrameHonoursCallersBytesLimit(t *testing.T) {
	s := newTestServer(t)

	// Write the frame to a file, then `cat` it: no shell quoting, no f-string escaping.
	//
	// Sized past the floor so the first assertion still has something to refuse. The
	// floor is 8 MiB, so this is 9 MiB: writing it costs a moment, and there is no
	// smaller number that is both over the floor and under it.
	const payloadSize = 9 * 1024 * 1024
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

	// 1. Without BytesLimitSet, the floor applies and a 9 MiB frame answers 413.
	defaultSlot := s.mcpRecvFrame(context.Background(), directive.Statement{Path: "read " + handle + " --frame"}, handle, termArgs{})
	if defaultSlot.Code != 413 {
		t.Fatalf("without bytes_limit: expected 413, got %d: %s", defaultSlot.Code, defaultSlot.Body)
	}
	if !strings.Contains(defaultSlot.Body, "bytes_limit") {
		t.Errorf("the 413 should name the knob that raises the window, got: %s", defaultSlot.Body)
	}

	// Reset the cursor to retry the same frame with a larger window.
	s.setRecvCursor(handle, 0)

	// 2. With BytesLimitSet to 12 MiB, the frame must be delivered.
	win := termArgs{BytesLimitSet: true, BytesLimit: 12 * 1024 * 1024}
	largeSlot := s.mcpRecvFrame(context.Background(), directive.Statement{Path: "read " + handle + " --frame"}, handle, win)
	if largeSlot.Code != 200 {
		t.Fatalf("with bytes_limit=12MiB: expected 200, got %d: %s", largeSlot.Code, largeSlot.Body)
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
