package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Channel index (the N of termcp://#<session>:N) is assigned at creation and
// never renumbered. These tests pin that contract, because the Web UI copies the
// number into a locator the user pastes elsewhere: if closing an earlier channel
// shifted the survivors, a copied :2 would silently start naming a different
// shell.

func waitForShells(t *testing.T, s *Session, want int, timeout time.Duration) []api.Session {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var shells []api.Session
	for time.Now().Before(deadline) {
		shells = s.ListChildShells()
		if len(shells) == want {
			return shells
		}
		time.Sleep(10 * time.Millisecond)
	}
	return shells
}

// TestShellIndexIsStableAcrossClose is the regression for issue #73: the index a
// copied locator carries must keep addressing the same channel after an earlier
// channel is closed.
func TestShellIndexIsStableAcrossClose(t *testing.T) {
	srv := startTestServer(t)
	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "index-stable"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	if got := s.PrimaryShellID(); got == "" {
		t.Fatal("primary shell id is empty")
	}
	primary := s.PrimaryShell()
	if primary == nil {
		t.Fatal("primary shell missing")
	}
	if primary.Index != 1 {
		t.Fatalf("primary shell index = %d, want 1", primary.Index)
	}

	second, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "")
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Index != 2 || third.Index != 3 {
		t.Fatalf("second/third indexes = %d/%d, want 2/3", second.Index, third.Index)
	}

	// Close the FIRST channel. Indexes 2 and 3 must not become 1 and 2.
	if err := s.CloseChildShell(primary.ID); err != nil {
		t.Fatal(err)
	}
	waitForShells(t, s, 2, 3*time.Second)

	cs, ok := s.ShellByIndex(2)
	if !ok || cs.ID != second.ID {
		t.Fatalf("index 2 = %v (ok=%v), want the second channel %q", cs, ok, second.ID)
	}
	cs, ok = s.ShellByIndex(3)
	if !ok || cs.ID != third.ID {
		t.Fatalf("index 3 = %v (ok=%v), want the third channel %q", cs, ok, third.ID)
	}

	// The closed channel's number is gone, not reused: a locator for it must fail
	// rather than address a different shell (which is what a positional lookup did).
	if cs, ok := s.ShellByIndex(1); ok {
		t.Fatalf("index 1 resolved to %q after the first channel was closed, want no match", cs.ID)
	}

	// A channel opened now takes the next number, not the freed one.
	fourth, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "")
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Index != 4 {
		t.Fatalf("channel opened after a close has index %d, want 4 (numbers are never reused)", fourth.Index)
	}
}

// TestShellIndexIsListedInMetadata locks the wiring of the number into the API
// records: the REST shell list and the MCP shell list are built from these, and
// the Web UI labels its tabs and copy buttons from Index.
func TestShellIndexIsListedInMetadata(t *testing.T) {
	srv := startTestServer(t)
	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "index-listed"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	if _, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "custom-name"); err != nil {
		t.Fatal(err)
	}
	shells := waitForShells(t, s, 2, 3*time.Second)
	if len(shells) != 2 {
		t.Fatalf("shell count = %d, want 2", len(shells))
	}
	for i, sh := range shells {
		if want := i + 1; sh.Index != want {
			t.Errorf("shells[%d].Index = %d, want %d", i, sh.Index, want)
		}
	}
	// A user-supplied name must not change the channel number.
	if shells[1].Name != "custom-name" {
		t.Errorf("shells[1].Name = %q, want the caller-supplied name", shells[1].Name)
	}

	// The session record itself is not a channel and carries no index.
	if info := s.Info(); info.Index != 0 {
		t.Errorf("session Info().Index = %d, want 0 (a session is not a channel)", info.Index)
	}
}

// The creation-order fallback exists only for a session where NO shell carries an
// index (shells built directly, as tests and legacy code do). Once any shell is
// numbered, the numbering is authoritative: an absent index means the channel is
// gone, never "the Nth remaining one". Both halves matter — the fallback keeps
// unnumbered shells reachable, and refusing it afterwards is what stops the
// numbering from shifting under a copied locator.
func TestShellByIndexFallbackOnlyForUnindexedSessions(t *testing.T) {
	srv := startTestServer(t)
	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "fallback"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	// This session has an indexed primary shell, so an unindexed shell added on
	// top must NOT be reachable by guessing a position: that is exactly the bug
	// (a positional lookup naming a different channel than the locator meant).
	raw := &ChildShell{ID: "raw-shell", CreatedAt: clock.Now()}
	s.shells.Store(raw.ID, raw)
	defer s.shells.Delete(raw.ID)
	if cs, ok := s.ShellByIndex(2); ok {
		t.Fatalf("index 2 resolved to %q in an indexed session, want no match", cs.ID)
	}

	// A session with no numbered shell at all falls back to creation order, which
	// is how a directly-built shell stays addressable.
	bare := &Session{}
	older := &ChildShell{ID: "bare-1", CreatedAt: 1}
	newer := &ChildShell{ID: "bare-2", CreatedAt: 2}
	bare.shells.Store(newer.ID, newer)
	bare.shells.Store(older.ID, older)
	if cs, ok := bare.ShellByIndex(1); !ok || cs.ID != older.ID {
		t.Fatalf("unindexed session: index 1 = %v (ok=%v), want %q", cs, ok, older.ID)
	}
	if cs, ok := bare.ShellByIndex(2); !ok || cs.ID != newer.ID {
		t.Fatalf("unindexed session: index 2 = %v (ok=%v), want %q", cs, ok, newer.ID)
	}
	if cs, ok := bare.ShellByIndex(3); ok {
		t.Fatalf("unindexed session: index 3 = %q, want no match", cs.ID)
	}
}

// TestShellIndexSurvivesRestart locks the DEAD/restored half: a retained channel
// must answer its locator with the number it had while running, and a channel
// opened later must not reuse it.
func TestShellIndexSurvivesRestart(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })

	m1 := NewManager(nil, store, srv)
	command, args := testSleepCommand("60")
	s, err := m1.Create(testConfig(command, args, api.ModePipe, "restart-index"))
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID
	primary := s.PrimaryShellID()

	second, err := s.CreateChildShell(command, args, false, 24, 80, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.Index != 2 {
		t.Fatalf("second channel index = %d, want 2", second.Index)
	}

	// DEAD the session (this persists the shell manifests via onDead).
	m1.Terminate(id, true, 0)
	if m1.Get(id).Info().Status != api.SessionExited {
		t.Fatal("expected session DEAD before restart")
	}

	// "Restart": a fresh manager over the same store.
	m2 := NewManager(nil, store, srv)
	if err := m2.RestoreDead(); err != nil {
		t.Fatal(err)
	}
	restored := m2.Get(id)
	if restored == nil {
		t.Fatal("expected restored DEAD session")
	}

	snap, ok := restored.SnapshotShellByIndex(2)
	if !ok || snap.ID != second.ID {
		t.Fatalf("restored index 2 = %+v (ok=%v), want %q", snap, ok, second.ID)
	}
	snap, ok = restored.SnapshotShellByIndex(1)
	if !ok || snap.ID != primary {
		t.Fatalf("restored index 1 = %+v (ok=%v), want the primary %q", snap, ok, primary)
	}
	// The numbering continues past the retained channels: a restored session that
	// somehow opens a channel must not collide with one it already has.
	if _, ok := restored.SnapshotShellByIndex(3); ok {
		t.Fatal("restored session reports a channel 3 that was never created")
	}
	if restored.nextShellIndex != 2 {
		t.Fatalf("restored nextShellIndex = %d, want 2 (one past the highest retained channel)", restored.nextShellIndex)
	}
}

// TestShellIndexBackfilledFromLegacyManifest covers a manifest written before the
// index field existed: restore must number its shells by creation order, so the
// number a locator resolves to does not depend on whether the session has been
// restarted.
func TestShellIndexBackfilledFromLegacyManifest(t *testing.T) {
	srv := startTestServer(t)
	store := storage.New(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })

	const sessID = "legacy-sess"
	// Manifests as an older build wrote them: no index on either shell, ordered by
	// creation time, which is what the numbering counted before it was stored.
	if err := store.SaveSession(api.Session{ID: sessID, Name: "legacy", Status: api.SessionExited, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShell(sessID, api.Session{ID: "legacy-sh-1", Name: "shell-1", Status: api.SessionExited, CreatedAt: 10}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShell(sessID, api.Session{ID: "legacy-sh-2", Name: "shell-2", Status: api.SessionExited, CreatedAt: 20}); err != nil {
		t.Fatal(err)
	}

	m := NewManager(nil, store, srv)
	if err := m.RestoreDead(); err != nil {
		t.Fatal(err)
	}
	sess := m.Get(sessID)
	if sess == nil {
		t.Fatal("expected the persisted session to be restored")
	}

	first, ok := sess.SnapshotShellByIndex(1)
	if !ok || first.ID != "legacy-sh-1" {
		t.Fatalf("backfilled index 1 = %+v (ok=%v), want legacy-sh-1", first, ok)
	}
	second, ok := sess.SnapshotShellByIndex(2)
	if !ok || second.ID != "legacy-sh-2" {
		t.Fatalf("backfilled index 2 = %+v (ok=%v), want legacy-sh-2", second, ok)
	}
	if _, ok := sess.SnapshotShellByIndex(3); ok {
		t.Fatal("backfill invented a third channel")
	}
	if sess.nextShellIndex != 2 {
		t.Fatalf("backfilled nextShellIndex = %d, want 2", sess.nextShellIndex)
	}
}

// TestShellIndexIsUniqueUnderConcurrentCreation pins that the counter is assigned
// under the session's shell lock: several channels opened at once must get
// distinct numbers, since a duplicated number would make a locator ambiguous.
func TestShellIndexIsUniqueUnderConcurrentCreation(t *testing.T) {
	srv := startTestServer(t)
	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "index-concurrent"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	const extra = 4
	created := make(chan *ChildShell, extra)
	errs := make(chan error, extra)
	for i := 0; i < extra; i++ {
		go func() {
			cs, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "")
			if err != nil {
				errs <- err
				return
			}
			created <- cs
		}()
	}
	seen := map[int]string{1: s.PrimaryShellID()}
	for i := 0; i < extra; i++ {
		select {
		case err := <-errs:
			t.Fatalf("concurrent CreateChildShell: %v", err)
		case cs := <-created:
			if prev, dup := seen[cs.Index]; dup {
				t.Fatalf("index %d handed to both %q and %q", cs.Index, prev, cs.ID)
			}
			seen[cs.Index] = cs.ID
		case <-time.After(10 * time.Second):
			t.Fatal("timed out creating channels concurrently")
		}
	}
	for i := 1; i <= extra+1; i++ {
		if seen[i] == "" {
			t.Errorf("index %d was never assigned; got %v", i, seen)
		}
	}
	// Every assigned number must resolve back to the shell that owns it.
	waitForShells(t, s, extra+1, 5*time.Second)
	for index, id := range seen {
		cs, ok := s.ShellByIndex(index)
		if !ok || cs.ID != id {
			t.Errorf("ShellByIndex(%d) = %v (ok=%v), want %q", index, cs, ok, id)
		}
	}
}

// TestShellIndexErrorCountsLiveShells keeps the locator failure message honest:
// it reports how many channels actually exist, which is what a caller needs to
// pick a valid index.
func TestShellIndexErrorCountsLiveShells(t *testing.T) {
	srv := startTestServer(t)
	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "index-count"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	if got := s.LiveShellCount(); got != 1 {
		t.Fatalf("LiveShellCount() = %d, want 1", got)
	}
	if !s.HasLiveShells() {
		t.Fatal("HasLiveShells() = false for a session with a live primary shell")
	}
	second, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.LiveShellCount(); got != 2 {
		t.Fatalf("LiveShellCount() = %d, want 2", got)
	}
	// Closing every channel leaves the container running but with no live shells,
	// which is what routes a locator to the snapshot instead of the live map.
	if err := s.CloseChildShell(second.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseChildShell(s.PrimaryShellID()); err != nil {
		t.Fatal(err)
	}
	waitForShells(t, s, 0, 3*time.Second)
	if s.HasLiveShells() {
		t.Fatal("HasLiveShells() = true after every channel was closed")
	}
	if got := s.LiveShellCount(); got != 0 {
		t.Fatalf("LiveShellCount() = %d, want 0", got)
	}
	// The container itself must still be running (closing shells is not closing it).
	if info := s.Info(); info.Status != api.SessionRunning {
		t.Fatalf("session status = %q after closing every channel, want running", info.Status)
	}
}

// TestShellIndexNotDerivedFromName pins that the channel number is carried in its
// own field rather than parsed back out of the display name: a caller-supplied
// name must not be able to make two channels claim one number.
func TestShellIndexNotDerivedFromName(t *testing.T) {
	srv := startTestServer(t)
	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, "index-name"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	// A name that looks like another channel's label.
	cs, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "shell-1")
	if err != nil {
		t.Fatal(err)
	}
	if cs.Index != 2 {
		t.Fatalf("index = %d, want 2 even though the name says shell-1", cs.Index)
	}
	viaIndex, ok := s.ShellByIndex(2)
	if !ok || viaIndex.ID != cs.ID {
		t.Fatalf("ShellByIndex(2) = %v (ok=%v), want %q", viaIndex, ok, cs.ID)
	}
	primary := s.PrimaryShell()
	if primary == nil || primary.ID == cs.ID {
		t.Fatal("primary shell must stay distinguishable from the named channel")
	}
	viaIndex, ok = s.ShellByIndex(1)
	if !ok || viaIndex.ID != primary.ID {
		t.Fatalf("ShellByIndex(1) = %v (ok=%v), want the primary %q", viaIndex, ok, primary.ID)
	}
}

// TestShellIndexDefaultNameIsUnchanged guards the public default name: it is part
// of what a client sees today, so making indexes stable must not rewrite it.
func TestShellIndexDefaultNameIsUnchanged(t *testing.T) {
	srv := startTestServer(t)
	s, err := New(srv, testConfig(testShell(), testInteractiveShellArgs(), api.ModePTY, ""), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Terminate(true, 0)

	// An unnamed session is named after its session id (existing behavior).
	if name := s.Info().Name; name == "" || name[:8] != "session-" {
		t.Fatalf("unnamed session name = %q, want a session- prefix", name)
	}
	cs, err := s.CreateChildShell(testShell(), testInteractiveShellArgs(), true, 24, 80, "")
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("shell-%s", cs.ID); cs.Name != want {
		t.Fatalf("default channel name = %q, want %q", cs.Name, want)
	}
}
