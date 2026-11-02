package mcp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/directive"
)

// TestASecondRecvLeavesWhenItsOwnWindowEnds pins that the per-handle reader gate can
// be given up on (issue #224).
//
// The gate that gives one channel one reader used to be a sync.Mutex held across the
// whole poll loop — which is as long as the caller's timeout. Mutex.Lock takes no
// context, so a second recv could not leave when its own window expired: it waited
// out the first caller's entire window first, and its MCP request hung there.
func TestASecondRecvLeavesWhenItsOwnWindowEnds(t *testing.T) {
	s := newTestServer(t)
	const handle = "test:1"

	// Simulate a reader holding the token (inside its poll loop).
	first := s.recvReader(handle)
	<-first
	defer func() { first <- struct{}{} }()

	// Second reader calls mcpRecvFrame with a SHORT context.
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	slot := s.mcpRecvFrame(short, directive.Statement{Path: "read " + handle + " --frame"}, handle, termArgs{})
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("the second reader waited %v; it must obey its own context rather than the gate", elapsed)
	}
	if slot.Code != 204 {
		t.Fatalf("expected 204 saying the call gave up waiting, got %d: %s", slot.Code, slot.Body)
	}
	if hint := slot.Headers["Hint"]; !strings.Contains(hint, "gave up waiting") {
		t.Fatalf("unexpected hint header: %q", hint)
	}
}
