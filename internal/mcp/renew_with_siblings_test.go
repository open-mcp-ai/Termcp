package mcp

import (
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/extensions"
)

// TestSiblingsOnOneConnectionDoNotStealTheRenew is the dogfood shape that the
// single-extension case hides (issue #183): several extensions share one
// connection, so every one of them matches the session id. Renewing a channel must
// renew the extension that OWNS it — not a sibling that happened to be visited
// first, which is what a single loop over the states did.
func TestSiblingsOnOneConnectionDoNotStealTheRenew(t *testing.T) {
	s := newExtensionTestServer(t)
	// Siblings: same target, so they share the connection they dial.
	for _, name := range []string{"st1", "st2", "st3", "st4", "st5"} {
		ext := extensions.Extension{Mode: "pty"}
		ext.Name = name
		ext.Target = "internal"
		ext.Command = blockingCommand()
		ext.Args = nil
		ext.IdleTimeout = "900ms"
		if err := s.extensions.Store().Save(&ext); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"st1", "st2", "st3", "st4", "st5"} {
		if hs := useExtension(t, s, name); hs["shell_id"] == nil {
			t.Fatalf("%s did not come up: %v", name, hs)
		}
	}

	// The extension under test comes up LAST so its window starts after the
	// siblings' slow starts (each ask waits out the startup grace).
	ext := extensions.Extension{Mode: "pty"}
	ext.Name = "elder"
	ext.Target = "internal"
	ext.Command = blockingCommand()
	ext.Args = nil
	ext.IdleTimeout = "900ms"
	if err := s.extensions.Store().Save(&ext); err != nil {
		t.Fatal(err)
	}
	handles := useExtension(t, s, "elder")
	shellID, _ := handles["shell_id"].(string)
	if shellID == "" {
		t.Fatalf("no channel to read: %v", handles)
	}

	// Read it past its own deadline, in steps. Every read is a use, so the channel
	// must still be there; without the fix the renew lands on whichever sibling is
	// visited first and this one is reaped on its original schedule.
	for step := 1; step <= 6; step++ {
		time.Sleep(250 * time.Millisecond)
		res := termCall(t, s, map[string]any{"cmd": "read " + shellID + " --tail 5", "timeout": 2})
		if res.IsError {
			t.Fatalf("step %d: a channel in use was reaped: %s", step, resultText(res))
		}
	}
}
