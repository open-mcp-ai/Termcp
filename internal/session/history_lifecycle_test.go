package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// "Which session owns this shell?" is answered two ways: the shell carries its
// parent, and the manager can scan every session's shell map. The output path now
// uses the former (the latter was a second full scan of the table to learn what the
// shell object already held), so the two must agree - the id decides which
// transcript gets read.
func TestChildShell_ParentSessionIDMatchesManagerLookup(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	m := NewManager(message.NewManager(store), store, srv)

	command, args := testSleepCommand("60")
	s, err := m.Create(testConfig(command, args, api.ModePipe, "parent-lookup"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Delete(s.ID) })

	// The root shell, the case the output path hits for a fresh session.
	root := s.PrimaryShell()
	if root == nil {
		t.Fatal("session has no primary shell")
	}
	if got := root.ParentSessionID(); got != s.ID {
		t.Errorf("root.ParentSessionID() = %q, want %q", got, s.ID)
	}
	if owner := m.GetByShellID(root.ID); owner == nil || owner.ID != s.ID {
		t.Errorf("GetByShellID(%q) = %v, want %q", root.ID, owner, s.ID)
	}
	// And through the manager-level lookup the output path starts from.
	if cs := m.GetChildShell(root.ID); cs == nil {
		t.Fatalf("GetChildShell(%q) = nil; the output path would not find the shell", root.ID)
	} else if cs.ParentSessionID() != s.ID {
		t.Errorf("GetChildShell().ParentSessionID() = %q, want %q", cs.ParentSessionID(), s.ID)
	}
}

// waitFor polls cond until it holds or the timeout passes, reporting whether it
// held. Polling is required wherever the thing awaited is an asynchronous effect
// of the thing observed.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func shellManifestStatus(t *testing.T, dataDir, sessionID string) api.SessionStatus {
	t.Helper()
	base := filepath.Join(dataDir, "sessions", sessionID)
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("read session dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(base, e.Name(), "manifest.json"))
		if err != nil {
			continue
		}
		var sh api.Session
		if err := json.Unmarshal(data, &sh); err != nil {
			continue
		}
		return sh.Status
	}
	return ""
}

func TestManager_ShellExitStatusIsPersisted(t *testing.T) {
	srv := startTestServer(t)
	dir := t.TempDir()
	store := storage.New(dir)
	t.Cleanup(func() { _ = store.Close() })
	m := NewManager(message.NewManager(store), store, srv)

	var command string
	var args []string
	if runtime.GOOS == "windows" {
		command = "powershell.exe"
		args = []string{"-NoProfile", "-Command", "Write-Output bye"}
	} else {
		command = "/bin/sh"
		args = []string{"-c", "echo bye"}
	}
	s, err := m.Create(testConfig(command, args, api.ModePipe, "exit-persist"))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID
	t.Cleanup(func() { _ = m.Delete(id) })

	// Wait for the shell to exit in memory first: that is the event whose
	// persistence is under test, and it is set before the write happens.
	waitFor(t, 10*time.Second, func() bool {
		shells := s.SnapshotShells()
		return len(shells) > 0 && shells[0].Status == api.SessionExited
	}, "shell to exit in memory")

	// Then wait for the disk to catch up. Polling the disk rather than reading it
	// once is what makes this a test of persistence instead of a test of timing:
	// the write is asynchronous to the in-memory status change, so a single read
	// races it (and did - this passed alone and failed under full-package load,
	// where the gap between the two is wider).
	inMemory := s.SnapshotShells()[0].Status
	var onDisk api.SessionStatus
	persisted := waitFor(t, 5*time.Second, func() bool {
		onDisk = shellManifestStatus(t, dir, id)
		return onDisk == api.SessionExited
	}, "shell exit status to reach disk")

	t.Logf("in-memory shell status = %s", inMemory)
	t.Logf("on-disk  shell status = %s (persisted=%v)", onDisk, persisted)

	if !persisted {
		t.Errorf("SHELL EXIT NOT PERSISTED: memory=%s disk=%s after 5s", inMemory, onDisk)
	}
}

// A session is usable the moment Create returns. The handlers used to sleep 100ms
// after Create before answering session_start, which suggested the session needed
// time to settle; it does not, and the sleep could not have helped even if it did.
// First output takes ~240ms to arrive on this machine (measured), so no fixed sleep
// shorter than that could guarantee output was ready, while input sent immediately
// after Create is accepted and echoed. The sleep therefore only added latency to
// every session creation, and this test is what keeps it from coming back: it fails
// if a future change makes input require a settling period.
func TestSession_InputWorksImmediatelyAfterCreate(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	m := NewManager(message.NewManager(store), store, srv)

	// The process must *answer* the input, not merely echo it back: the read
	// below looks for a string the shell had to produce after receiving the
	// line, so a transport that lost the write cannot look like a success. Both
	// branches echo a line back under a marker of its own, which is what makes
	// the assertion below the same one on every OS.
	//
	// `/bin/cat` was here and it could never pass: the assertion looks for
	// "GOT_hello", which only the PowerShell branch prints, so on unix the test
	// waited out its five seconds and then failed for the one reason the test is
	// named after — input that had in fact arrived perfectly well.
	var command string
	var args []string
	wantOutput := "hello"
	if runtime.GOOS == "windows" {
		command = "powershell.exe"
		args = []string{"-NoProfile", "-Command", "$input | ForEach-Object { Write-Output ('GOT_' + $_) }"}
		wantOutput = "GOT_hello"
	} else {
		command = "/bin/sh"
		args = []string{"-c", `while IFS= read -r line; do printf 'GOT_%s\n' "$line"; done`}
	}
	s, err := m.Create(testConfig(command, args, api.ModePipe, "immediate-input"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Delete(s.ID) })

	cs := s.PrimaryShell()
	if cs == nil {
		t.Fatal("session has no primary shell")
	}
	// No sleep: send as soon as Create has returned.
	if err := cs.SendTerminalBytes([]byte("hello"), true); err != nil {
		t.Fatalf("input immediately after Create was rejected: %v", err)
	}

	// And it must actually reach the process, not merely be accepted.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := s.ReadOutput(nil, 0, false, 0, 0); strings.Contains(out, wantOutput) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("input sent immediately after Create never reached the process")
}
