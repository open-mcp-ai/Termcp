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

// TestRecvFrameHonoursCallersBytesLimit pins the frame window of `read --frame` and
// `call`: the default is the TRANSPORT'S OWN LIMIT, and the caller can name a larger
// one (issues #229 and #327).
//
// TWO DECISIONS ARE PINNED HERE, AND THEY CHANGED ONCE.
//
// #229 made the window the caller's: it used to be a hard 1 MiB wall with no knob, so
// an MCP server returning a base64 screenshot answered 413 for ever — no cursor pages
// inside a single frame, so the result was simply undeliverable.
//
// #327 moved the DEFAULT from 1 MiB to the transport's 8 MiB inbound limit. 1 MiB was
// still reachable in practice (a 4 MB JSON document is routine on real servers), and a
// server refusing to deliver what its own transport accepts is a disagreement inside
// one process. So the pair this test now holds is: up to the transport's limit, nothing
// to configure; above it, 413 that NAMES the ceiling and the flag that raises it.
func TestRecvFrameHonoursCallersBytesLimit(t *testing.T) {
	s := newTestServer(t)

	// Just over the transport limit, so the default window is what refuses it.
	payloadSize := minRecvWindow + 256*1024
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

	// 1. Without BytesLimitSet the transport's window applies, and this frame is over it.
	defaultSlot := s.mcpRecvFrame(context.Background(), directive.Statement{Path: "read " + handle + " --frame"}, handle, termArgs{})
	if defaultSlot.Code != 413 {
		t.Fatalf("without bytes_limit: expected 413, got %d: %s", defaultSlot.Code, defaultSlot.Body)
	}
	// The refusal has to be actionable: it names the window and points at the flag,
	// because a caller that reads only "too large" has no way to know the ceiling can
	// be raised at all.
	if !strings.Contains(defaultSlot.Body, "bytes_limit") {
		t.Errorf("the 413 does not name the flag that raises the window:\n%s", defaultSlot.Body)
	}

	// Reset the cursor to retry the same frame with a larger window.
	s.setRecvCursor(handle, 0)

	// 2. With a window above the frame, it must be delivered whole.
	win := termArgs{BytesLimitSet: true, BytesLimit: minRecvWindow + 2*1024*1024}
	largeSlot := s.mcpRecvFrame(context.Background(), directive.Statement{Path: "read " + handle + " --frame"}, handle, win)
	if largeSlot.Code != 200 {
		t.Fatalf("with a larger bytes_limit: expected 200, got %d: %s", largeSlot.Code, largeSlot.Body)
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
