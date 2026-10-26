package mcp

import (
	"strings"
	"testing"
	"time"

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

	// WAIT FOR THE EXPLANATION, NOT FOR THE FIRST ERROR. Two asynchronies stack here:
	// CloseChildShell is asynchronous (a read fired the microsecond the extension dropped
	// the id can answer 204), and the closed record is written by the reaper while the
	// session removes the shell — so the first 404 can be the bare "Shell 'x' not found"
	// for a moment. Asserting on that first error made this test order-dependent: it
	// passed 10/10 alone and under 14x load, and went red in a shuffled full run where the
	// reason had not landed yet. The promise is that the handle answers with what
	// happened, so the wait is for that answer; the deadline only bounds how long a
	// failure takes to report.
	var text string
	deadline = time.Now().Add(8 * time.Second)
	for {
		res := termCall(t, s, map[string]any{"cmd": "read " + shellID + " --tail 1", "timeout": 1})
		text = resultText(res)
		if res.IsError && (strings.Contains(text, "idle") || strings.Contains(text, "closed")) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the closed channel never explained itself:\n%s", text)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(text, "open ext") {
		t.Errorf("the 404 should say what to do next, got: %s", text)
	}
}
