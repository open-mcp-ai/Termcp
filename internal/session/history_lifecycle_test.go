package session

import (
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Terminate / Disconnect must NOT delete the session from the registry: it
// becomes a retained DEAD (exited) session whose byte log stays on disk
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
	// Persisted history must survive the DEAD transition for read-only replay.
	if !store.HasPersistedHistory(id) {
		t.Fatal("expected persisted history retained after terminate")
	}
}

// Manual delete is the only permanent release: it drops the session from the
// registry and removes its on-disk directory. Irreversible.
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
	if store.HasPersistedHistory(id) {
		t.Fatal("expected persisted history removed after delete")
	}
}

// A DEAD session is persisted as a manifest and reconstructed as a read-only
// session by a fresh manager (restart). No SSH transport is created, but its
// tabs and byte log remain viewable.
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

	// Write a mark so the session has something persisted on disk, then DEAD the
	// session (which persists the manifest via onDead → manager.persist).
	if err := mm.AppendMarkOnly(id, s.PrimaryShellID(), api.LogAPIInput); err != nil {
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
	if !store.HasPersistedHistory(id) {
		t.Fatal("expected persisted history to survive restart")
	}
}

// Session-scoped output reads must land on a real shell. A byte log belongs to a
// shell, so a caller that only holds a session id (the REST session-scoped
// output-range endpoint, the message tool without shell_id) relied on an empty
// shellID being translated to the session's primary shell. Passing it through
// unresolved addressed a path that does not exist and read back nothing, which a
// restored DEAD session could not paper over.
func TestManager_OutputReadsResolveEmptyShellID(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	// AppendOutput keeps log.bin open; close it so the temp dir can be removed.
	t.Cleanup(func() { _ = store.Close() })
	mm := message.NewManager(store)

	m := NewManager(mm, store, srv)

	command, args := testSleepCommand("60")
	s, err := m.Create(testConfig(command, args, api.ModePipe, "empty-shell-id"))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID

	// Bytes must exist for the read to be meaningful.
	payload := []byte("hello byte log\n")
	if _, err := mm.AppendOutput(id, s.PrimaryShellID(), payload); err != nil {
		t.Fatal(err)
	}

	// DEAD the session and restore it, so there are no live shell objects left to
	// resolve against and only the persisted snapshot remains.
	m.Terminate(id, true, 0)
	m2 := NewManager(mm, store, srv)
	if err := m2.RestoreDead(); err != nil {
		t.Fatal(err)
	}
	if restored := m2.Get(id); restored == nil {
		t.Fatal("expected restored DEAD session")
	} else if restored.PrimaryShell() != nil {
		t.Fatal("expected restored session to hold no live shell")
	}

	// Empty shellID: the read must find the shell's log, not a path named by "".
	size, err := m2.OutputSize(id, "")
	if err != nil {
		t.Fatalf("OutputSize with empty shellID: %v", err)
	}
	if size != int64(len(payload)) {
		t.Fatalf("OutputSize with empty shellID = %d, want %d", size, len(payload))
	}

	data, total, err := m2.OutputByteRange(id, "", 0, 1024)
	if err != nil {
		t.Fatalf("OutputByteRange with empty shellID: %v", err)
	}
	if total != int64(len(payload)) {
		t.Fatalf("OutputByteRange total = %d, want %d", total, len(payload))
	}
	if string(data) != string(payload) {
		t.Fatalf("OutputByteRange with empty shellID = %q, want %q", data, payload)
	}

	// An explicit shell id must keep working and agree with the resolved read.
	explicitSize, err := m2.OutputSize(id, s.PrimaryShellID())
	if err != nil {
		t.Fatalf("OutputSize with explicit shellID: %v", err)
	}
	if explicitSize != size {
		t.Fatalf("explicit shellID size = %d, resolved size = %d", explicitSize, size)
	}

	// Marks go through the same translation for the message tool.
	marks, err := m2.Marks(id, "")
	if err != nil {
		t.Fatalf("Marks with empty shellID: %v", err)
	}
	if len(marks) == 0 {
		t.Fatal("Marks with empty shellID returned no marks for a session that has output")
	}
}
