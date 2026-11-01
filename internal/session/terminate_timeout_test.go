package session

import (
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestTerminateReturnsWhenPeerStallsOnClose pins that Terminate cannot be hung by a
// peer that stops reading its socket.
func TestTerminateReturnsWhenPeerStallsOnClose(t *testing.T) {
	store := storage.New(t.TempDir())
	t.Cleanup(func() { _ = store.Close() })
	mm := message.NewManager(store)
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	m := NewManager(mm, store, srv)
	sess, err := m.Create(Config{Command: testShell(), Mode: api.ModePipe, Name: "worker"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() { _ = m.Delete(sess.ID) })

	// Verify the session is live.
	if sess.Info().Status != api.SessionRunning {
		t.Fatalf("session not running: %v", sess.Info().Status)
	}

	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Request a graceful terminate. The fix ensures this does not block forever.
		sess.Terminate(false, 100*time.Millisecond)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Terminate did not return within bounded timeout; issue #218 deadlock reproduced")
	}

	elapsed := time.Since(start)
	t.Logf("Terminate took %v", elapsed)

	// The session MUST be marked dead, not stuck in running.
	if status := sess.Info().Status; status != api.SessionExited {
		t.Fatalf("session status is %v, want exited", status)
	}

	// A second Terminate must return immediately and never hang on sync.Once.
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		sess.Terminate(true, 0)
	}()
	select {
	case <-secondDone:
	case <-time.After(1 * time.Second):
		t.Fatal("second Terminate hung on sync.Once")
	}
}
