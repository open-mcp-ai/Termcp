package session

import (
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Terminate / Disconnect must NOT delete the session from the registry: it
// becomes a retained DEAD (exited) session whose message history stays on disk
// for read-only replay. The tile stays visible and its output remains readable.
func TestManager_TerminateKeepsSessionDead(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)

	m := NewManager(mm, store, srv)

	command, args := testSleepCommand("60")
	s, err := m.Create(testConfig(command, args, api.ModePipe, "ctf-box"))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	m.Terminate(id, true, 0)

	// Retained in the active registry, moved to DEAD (exited).
	if m.Get(id) == nil {
		t.Fatal("expected session retained (DEAD) after terminate")
	}
	if got := m.Get(id).Info().Status; got != api.SessionExited {
		t.Fatalf("expected 'exited' after terminate, got %q", got)
	}
	// Message history must survive the DEAD transition for read-only replay.
	if !store.MessageDirExists(id) {
		t.Fatal("expected message history retained after terminate")
	}
}

// Manual delete is the only permanent release: it drops the session from the
// registry and purges the persisted messages on disk. Irreversible.
func TestManager_DeletePurgesMessages(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)

	m := NewManager(mm, store, srv)

	command, args := testSleepCommand("60")
	s, err := m.Create(testConfig(command, args, api.ModePipe, "ctf-box"))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	if err := m.Delete(id); err != nil {
		t.Fatal(err)
	}

	if m.Get(id) != nil {
		t.Fatal("expected session removed from registry after delete")
	}
	time.Sleep(200 * time.Millisecond)
	if store.MessageDirExists(id) {
		t.Fatal("expected message history purged after delete")
	}
}

// A DEAD session is persisted to sessions.json and reconstructed as a read-only
// session by a fresh manager (restart). No SSH transport is created, but its
// tabs and message log remain viewable.
func TestManager_RestoreDeadAfterRestart(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	mm := message.NewManager(store)

	m1 := NewManager(mm, store, srv)

	command, args := testSleepCommand("60")
	s, err := m1.Create(testConfig(command, args, api.ModePipe, "persistent"))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	// Write input so a message exists on disk, then DEAD the session (persists
	// sessions.json via onDead → manager.persist).
	if _, err := mm.AppendShell(id, s.PrimaryShellID(), api.MsgInput, "hello\n"); err != nil {
		t.Fatal(err)
	}
	m1.Terminate(id, true, 0)
	if m1.Get(id).Info().Status != api.SessionExited {
		t.Fatal("expected session DEAD before restart")
	}

	// "Restart": fresh manager over the same store.
	m2 := NewManager(mm, store, srv)
	if err := m2.RestoreDead(); err != nil {
		t.Fatal(err)
	}

	restored := m2.Get(id)
	if restored == nil {
		t.Fatal("expected restored DEAD session in registry")
	}
	if got := restored.Info().Status; got != api.SessionExited {
		t.Fatalf("expected restored status 'exited', got %q", got)
	}
	if restored.SSHClient() != nil {
		t.Fatal("expected restored session to hold no live SSH transport")
	}
	// Shell snapshot is repopulated so tabs can render.
	if len(restored.ShellsForView()) == 0 {
		t.Fatal("expected restored session to expose its persisted shell metadata")
	}
	if !store.MessageDirExists(id) {
		t.Fatal("expected persisted messages to survive restart")
	}
}
