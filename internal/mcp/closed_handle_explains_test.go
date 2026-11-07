package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/mcp"

	"github.com/open-mcp-ai/termcp/internal/extensions"
)

// TestAClosedChannelExplainsItself pins the promise docs/api.md makes: the id that
// named an instance answers with what happened to it (dogfood on #183). The session
// 404 already did; the channel 404 did not, and the channel id is what a caller
// actually keeps.
func TestAClosedChannelExplainsItself(t *testing.T) {
	s := newExtensionTestServer(t)
	ext := extensions.Extension{Mode: "pty"}
	ext.Name = "gone"
	ext.Target = "internal"
	ext.Command = blockingCommand()
	ext.Args = nil
	ext.IdleTimeout = "300ms"
	if err := s.extensions.Store().Save(&ext); err != nil {
		t.Fatal(err)
	}
	handles := useExtension(t, s, "gone")
	shellID, _ := handles["shell_id"].(string)
	if shellID == "" {
		t.Fatalf("no channel: %v", handles)
	}

	deadline := time.Now().Add(8 * time.Second)
	for {
		cur := callExtension(t, s, map[string]any{"action": "get", "name": "gone"})
		if cur["shell_id"] == nil || cur["shell_id"] == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the window never ran out: %v", cur)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Wait for the channel itself to observe the closure, not just the extension's
	// state map: CloseChildShell is asynchronous, so a read fired the microsecond the
	// extension removed the id can catch the shell before its dead transition lands
	// and answer 204 (empty output) rather than 404 (closed).
	var res *mcp.CallToolResult
	deadline = time.Now().Add(5 * time.Second)
	for {
		res = termCall(t, s, map[string]any{"cmd": "read " + shellID + " --tail 1", "timeout": 1})
		if res.IsError {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the closed channel never refused the read: %v", resultText(res))
		}
		time.Sleep(20 * time.Millisecond)
	}
	text := resultText(res)
	if !strings.Contains(text, "idle") && !strings.Contains(text, "closed") {
		t.Errorf("the 404 should say why it went, got: %s", text)
	}
	if !strings.Contains(text, "open ext") {
		t.Errorf("the 404 should say what to do next, got: %s", text)
	}
}
