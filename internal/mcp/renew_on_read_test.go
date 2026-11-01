package mcp

import (
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/extensions"
)

// TestReadingAnExtensionRenewsItsWindow pins the wiring, not the bookkeeping: the
// reconciler's own Touch works, and this is the half that decides whether a channel
// in use is a channel nobody asked about (dogfood on #183).
//
// It is the one test that fails when the guard's renew is removed, which is the
// point: the idle window's whole promise is "closed when nobody is using it", and
// nothing else in the suite can tell "using it" from "silence".
func TestReadingAnExtensionRenewsItsWindow(t *testing.T) {
	s := newExtensionTestServer(t)
	ext := extensions.Extension{Mode: "pty"}
	ext.Name = "renewed"
	ext.Target = "internal"
	ext.Command = blockingCommand()
	ext.Args = nil
	// A window short enough to see, long enough that the test's own calls fit.
	ext.IdleTimeout = "700ms"
	if err := s.extensions.Store().Save(&ext); err != nil {
		t.Fatal(err)
	}

	handles := useExtension(t, s, "renewed")
	shellID, _ := handles["shell_id"].(string)
	if shellID == "" {
		t.Fatalf("no channel to read: %v", handles)
	}

	// Read it well past the original deadline, in steps. Every call is a use, so the
	// channel must still be there; without the renewal it is gone by the third.
	for step := 1; step <= 6; step++ {
		time.Sleep(250 * time.Millisecond)
		res := termCall(t, s, map[string]any{"cmd": "read " + shellID + " --tail 5", "timeout": 2})
		if res.IsError {
			t.Fatalf("step %d: a channel in use was reaped: %s", step, resultText(res))
		}
	}
}
