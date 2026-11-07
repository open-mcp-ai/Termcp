package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/mcp"
)

// TestATruncatedToastSaysItWasTruncated is the third finding in issue #204's review.
//
// The 2000-rune cap is a legitimate protection — a toast is not a document — but the
// cut used to be SILENT: a caller who sent a long message got "ok" and the human read a
// sentence that stopped mid-word with nothing saying anything was missing. The issue's
// rule is the useful half of it: "拒绝可以，静默不行" — a cap that announces itself is a
// different thing from one that hides.
func TestATruncatedToastSaysItWasTruncated(t *testing.T) {
	s := newTestServer(t)
	// The message the browser will show is what has to carry the note, so capture it at
	// the delivery boundary rather than guessing from the reply.
	var delivered string
	s.uiNotify = func(level, title, message, sessionID string, durationSec int) int {
		delivered = message
		return 1
	}
	long := strings.Repeat("x", maxNotifyMessageLen+250)

	res, err := s.handleNotifyUser(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Arguments: map[string]any{"message": long}},
	})
	if err != nil {
		t.Fatalf("handleNotifyUser: %v", err)
	}
	if res.IsError {
		t.Fatalf("a long message is not a failure: %s", toolResultText(res))
	}
	if !strings.Contains(delivered, "more characters were not shown") {
		t.Fatalf("a truncated toast must say so, got %q", delivered[max(0, len(delivered)-140):])
	}
	if !strings.Contains(delivered, strings.Repeat("x", 40)) {
		t.Error("the message itself was lost")
	}
}
