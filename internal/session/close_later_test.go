package session

import (
	"errors"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestCloseLaterRefusesPTY pins that a session with an active PTY shell cannot be
// deferred: an interactive pty does not end on its own, so --later would leave it
// waiting forever (#255).
func TestCloseLaterRefusesPTY(t *testing.T) {
	s := &Session{
		Session: api.Session{ID: "sess-test-pty", Status: api.SessionRunning},
	}
	sh := &ChildShell{
		ID:   "sh-1",
		mode: api.ModePTY,
	}
	sh.status.Store(api.SessionRunning)
	s.shells.Store(sh.ID, sh)

	err := s.CloseLater()
	if err == nil {
		t.Fatal("CloseLater must refuse a live pty shell")
	}
	if !errors.Is(err, ErrUnboundedWorkload) {
		t.Fatalf("CloseLater returned %v, want ErrUnboundedWorkload", err)
	}
	if s.Closing() {
		t.Fatal("a refused CloseLater must not leave s.closing set")
	}
}

// TestCloseLaterAcceptsPipeAndFulfillsOnExit pins that pipe shells are accepted and
// the session terminates automatically when the last one ends (#255).
func TestCloseLaterAcceptsPipeAndFulfillsOnExit(t *testing.T) {
	s := &Session{
		Session: api.Session{ID: "sess-test-pipe", Status: api.SessionRunning},
	}
	sh := &ChildShell{
		ID:   "sh-pipe",
		mode: api.ModePipe,
	}
	sh.status.Store(api.SessionRunning)
	s.shells.Store(sh.ID, sh)

	if err := s.CloseLater(); err != nil {
		t.Fatalf("CloseLater on pipe shell: %v", err)
	}
	if !s.Closing() {
		t.Fatal("s.closing must be true after successful CloseLater")
	}

	// Idempotent: a second call succeeds without error.
	if err := s.CloseLater(); err != nil {
		t.Fatalf("second CloseLater must succeed idempotently: %v", err)
	}

	// While the shell is still running, closeIfLastShellEnded must not terminate.
	s.closeIfLastShellEnded()
	st := s.liveStatus()
	if st != api.SessionRunning {
		t.Fatalf("session must stay running while shell is live, got %s", st)
	}

	// The shell ends cleanly.
	sh.status.Store(api.SessionExited)
	s.closeIfLastShellEnded()

	// Give the transition a moment to flip to DEAD.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st = s.liveStatus()
		if st == api.SessionExited || st == api.SessionError {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session did not terminate after last pipe shell exited (status: %s)", st)
}

// TestCloseLaterDeadSessionIsSatisfied pins that calling CloseLater on a session that
// has already exited returns nil (already ended is already satisfied).
func TestCloseLaterDeadSessionIsSatisfied(t *testing.T) {
	s := &Session{
		Session: api.Session{ID: "sess-dead", Status: api.SessionExited},
	}
	if err := s.CloseLater(); err != nil {
		t.Fatalf("CloseLater on dead session must succeed: %v", err)
	}
}
