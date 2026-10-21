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
	// c3 exists only to take index 3; its identity is not otherwise used.
	if _, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "c3"); err != nil {
		t.Fatal(err)
	}
	mgr1.CloseChildShell(c2.ID)

	// Purge through the manager before the temp dir goes away.
	//
	// A shell's exit watcher writes its final manifest AFTER Terminate returns, so
	// t.TempDir's RemoveAll can race that write and fail on Windows with "The
	// directory is not empty" -- reported against whichever test happened to own the
	// directory. Delete joins the watcher and removes the session directory, and
	// t.Cleanup is LIFO, so registering it here runs it before TempDir's own cleanup.
	// Same remedy as history_lifecycle_test.go.
	t.Cleanup(func() { _ = mgr1.Delete(sid) })

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

	// The restored manager sees only persisted state, so it holds no writer; the
	// directory is already gone by the time this cleanup pair runs.
}
