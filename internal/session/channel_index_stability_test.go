package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestChannelIndexIsMonotonicAndNeverReused pins that channel indexes ONLY GROW:
//
//   - channel 1 gets index 1
//   - closing channel 1 and opening channel 2 gives index 2 (NOT 1: no reuse)
//   - opening channel 3 gives index 3
//   - closing channel 2 does not cause channel 4 to take 2
//
// This is the property that lets `sid:N` be a STABLE, unambiguous identity:
// because N is never reassigned, `sid:N` points at exactly one channel over the
// entire lifetime of the session. A caller addressing `sid:1` after channel 1
// closes hits an exited/not-found channel, NEVER a new channel that replaced it.
func TestChannelIndexIsMonotonicAndNeverReused(t *testing.T) {
	srv := startTestServer(t)
	mgr := NewManager(nil, nil, srv)

	s, err := mgr.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "parent"))
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Delete(s.ID)

	p := s.PrimaryShell()
	if p == nil {
		t.Fatal("primary shell missing")
	}
	if p.Index != 1 {
		t.Fatalf("primary shell index = %d, want 1", p.Index)
	}

	// Open child 2.
	c2, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "c2")
	if err != nil {
		t.Fatal(err)
	}
	if c2.Index != 2 {
		t.Fatalf("child 2 index = %d, want 2", c2.Index)
	}

	// Close child 2.
	if ok, err := mgr.CloseChildShell(c2.ID); !ok || err != nil {
		t.Fatalf("close child 2 failed: %v, %v", ok, err)
	}

	// Open child 3: it MUST get 3, NEVER reuse 2.
	c3, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "c3")
	if err != nil {
		t.Fatal(err)
	}
	if c3.Index != 3 {
		t.Fatalf("child 3 index = %d, want 3 (must not reuse index 2 of closed child)", c3.Index)
	}

	// Close primary (index 1).
	s.TerminateShellOnly()

	// Open child 4: it MUST get 4, NEVER reuse 1.
	c4, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "c4")
	if err != nil {
		t.Fatal(err)
	}
	if c4.Index != 4 {
		t.Fatalf("child 4 index = %d, want 4 (must not reuse index 1 of closed primary)", c4.Index)
	}
	_ = time.Second
	_ = fmt.Sprint
}
