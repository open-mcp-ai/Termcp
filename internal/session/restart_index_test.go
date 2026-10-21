package session

import (
	"testing"

	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestChannelIndexSurvivesRestart pins that restarting termcp does NOT reset the
// channel numbering: a session restored from disk reads the highest index among its
// persisted shells and continues from there, so new channels never collide with
// closed ones.
func TestChannelIndexSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	srv := startTestServer(t)

	// Instance 1: create session, create child 2 and child 3, close child 2.
	mgr1 := NewManager(nil, store, srv)
	s, err := mgr1.Create(testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "persisted"))
	if err != nil {
		t.Fatal(err)
	}
	sid := s.ID

	c2, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "c2")
	if err != nil {
		t.Fatal(err)
	}
	c3, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "c3")
	if err != nil {
		t.Fatal(err)
	}
	mgr1.CloseChildShell(c2.ID)
	// Persist the state.
	mgr1.Terminate(sid, false, 0)

	// Instance 2: reconstruct manager over the same store directory.
	mgr2 := NewManager(nil, store, srv)
	if err := mgr2.RestoreDead(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	restored := mgr2.Get(sid)
	if restored == nil {
		t.Fatalf("session %s not restored", sid)
	}

	// Verify nextShellIndex is at least 3 (max of 1, 2, 3), not reset to 1.
	if restored.nextShellIndex < 3 {
		t.Errorf("restored nextShellIndex = %d, want >= 3", restored.nextShellIndex)
	}
	_ = c3
}
