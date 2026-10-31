package session

import (
	"runtime"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestWindowsInternalPtyCloseSkipsTheGraceWait pins issue #239.
//
// ConPTY cannot deliver SIGTERM to a local child, so a graceful close of an
// internal Windows pty waited out the full grace period for a signal that would
// never be acted on: `close <session>` took 5 s on a machine where the process
// was already idle. The wait is only meaningful on a transport that can act on
// the signal, so this one drops it — and the test states the cost it removes
// rather than just that the call returned.
func TestWindowsInternalPtyCloseSkipsTheGraceWait(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the ineffective SIGTERM wait only exists on Windows ConPTY")
	}
	store := storage.New(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	mm := message.NewManager(store)
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	m := NewManager(mm, store, srv)
	sess, err := m.Create(Config{Mode: api.ModePTY})
	if err != nil {
		t.Fatalf("create pty session: %v", err)
	}
	if sess.SSHEndpoint != "internal" {
		t.Fatalf("this test needs the internal endpoint, got %q", sess.SSHEndpoint)
	}

	began := time.Now()
	sess.Terminate(false, 5*time.Second)
	elapsed := time.Since(began)
	if elapsed > 200*time.Millisecond {
		t.Fatalf("an internal Windows pty close waited %v for a SIGTERM ConPTY cannot deliver", elapsed)
	}
	if status := sess.Info().Status; status != api.SessionExited {
		t.Fatalf("session status is %v, want exited", status)
	}
}
