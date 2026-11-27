package core

import (
	"fmt"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/config"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// THE PORT DOES NOT WAIT FOR HISTORY (issue #353).
//
// Measured before this: core.New took 980ms on 60 archived sessions, all of it reading
// manifests, and it ran BEFORE the port bound. An MCP client that handshook during that
// window got `initialize timed out` and reported zero tools, which reads as "the server is
// down" while the server was reading history.
//
// The assertion is on the STATE, not on a duration: after Start returns the instance must
// be usable, and the archive must arrive afterwards — with RestoreStatus saying which of
// the two a caller is looking at.
func TestTheArchiveIsRestoredAfterTheInstanceIsUsable(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	for i := 0; i < 20; i++ {
		if err := store.SaveSession(api.Session{
			ID:     fmt.Sprintf("arch%08d", i),
			Status: api.SessionExited,
		}); err != nil {
			t.Fatalf("seed session %d: %v", i, err)
		}
	}
	_ = store.Close()

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Headless = true
	srv, err := New(Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// THE WHOLE POINT: New no longer reads the archive, so it cannot be what a client
	// waits behind.
	if n := len(srv.sessMgr.ListAll()); n != 0 {
		t.Fatalf("New restored %d sessions synchronously; the archive belongs in the background", n)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Close("test done") })

	// USABLE IMMEDIATELY: Term answers before any of the archive has arrived.
	reply := srv.Term(t.Context(), "ls", nil)
	if len(reply.Slots) == 0 {
		t.Fatal("the instance could not answer a command right after Start")
	}

	// AND THE RESTORE REPORTS ITSELF WHILE IT RUNS. Polling is the honest shape here:
	// the job may finish before the first probe on a fast machine, so the loop asserts on
	// the END state and records whether it ever saw the middle one.
	sawRestoring := false
	deadline := time.Now().Add(20 * time.Second)
	for {
		restoring, done, total := srv.RestoreStatus()
		if restoring {
			sawRestoring = true
			// `total` is 0 for the instant between "the walk started" and "the directory
			// listing came back", which is a real state rather than a bug: the count of
			// sessions on disk is not known until the listing answers. What must never
			// happen is a total below what is on disk, or progress past it.
			if total > 0 && total < 20 {
				t.Errorf("restore total is %d, want at least the 20 seeded sessions", total)
			}
			if done > total {
				t.Errorf("restore progress %d/%d runs past its total", done, total)
			}
		}
		if !restoring && len(srv.sessMgr.ListAll()) == 20 {
			break
		}
		if time.Now().After(deadline) {
			restoring, done, total := srv.RestoreStatus()
			t.Fatalf("the archive never finished restoring: restoring=%v progress=%d/%d sessions=%d",
				restoring, done, total, len(srv.sessMgr.ListAll()))
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !sawRestoring {
		t.Log("the restore finished before the first probe; the end state is what this test pins")
	}
	// A RESTORED SESSION IS DEAD, never advertised as running: this process holds no
	// connection for it.
	for _, s := range srv.sessMgr.ListAll() {
		if s.Status == api.SessionRunning {
			t.Errorf("restored session %s claims to be running", s.ID)
		}
	}
}

// A CLOSING INSTANCE STOPS RESTORING (issue #353): the walk is cancelled, so a process on
// its way out does not keep reading a history nobody will see.
func TestClosingStopsTheRestore(t *testing.T) {
	dir := t.TempDir()
	store := storage.New(dir)
	for i := 0; i < 50; i++ {
		if err := store.SaveSession(api.Session{ID: fmt.Sprintf("slow%08d", i), Status: api.SessionExited}); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.Close()

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.Headless = true
	srv, err := New(Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	srv.Close("test done")

	// AFTER Close THE FLAG MUST SETTLE, and it must not be left true forever: a caller
	// asking a closing instance whether history is still loading gets "no".
	deadline := time.Now().Add(20 * time.Second)
	for {
		if restoring, _, _ := srv.RestoreStatus(); !restoring {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the restore never stopped after Close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
