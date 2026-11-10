package sshserver

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/charmbracelet/ssh"
)

// within fails the test if fn does not return inside d. The bound is a HANG
// DETECTOR, not a budget: every call here returns in microseconds on a correct
// implementation, and the deadline only decides how long a broken one takes to
// report. Without it a regression parks the test binary until the package timeout,
// which names no test.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not return: the owner goroutine is not serving its channel", what)
	}
}

// THE TEAR-DOWN MUST REACH A PTY THAT WAS NEVER STARTED.
//
// A client may send pty-req and close the channel without ever sending exec or
// shell. Then no handler goroutine exists to close anything, and the library's
// deferred closer is the only thing that arrives. An owner anchored to the
// allocation (ptx-req time) rather than to the handler is what makes that case work
// without a flag saying whether a start happened.
func TestPTYOwnerClosesAnUnstartedPTY(t *testing.T) {
	var closed int
	ps := newSessionPTY(ssh.Pty{}, func() error {
		closed++
		return nil
	})

	within(t, "close without a start", func() {
		if err := ps.close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	if closed != 1 {
		t.Fatalf("the platform closers ran %d times, want exactly 1", closed)
	}
	select {
	case <-ps.quit:
	case <-time.After(2 * time.Second):
		t.Fatal("the owner did not exit after serving close")
	}
}

// A SECOND CLOSE MUST NOT PARK. The library can register more than one deferred
// closer for a session (a second pty-req does it), and the owner has already exited
// by then, so the second call has to answer instead of waiting for a reader that is
// gone.
func TestPTYOwnerCloseIsIdempotent(t *testing.T) {
	closed := 0
	ps := newSessionPTY(ssh.Pty{}, func() error {
		closed++
		return errors.New("first close complained")
	})
	if err := ps.close(); err == nil {
		t.Fatal("close lost the platform's error")
	}
	within(t, "second close", func() {
		if err := ps.close(); err != nil {
			t.Errorf("second close = %v, want nil: the PTY is already gone", err)
		}
	})
	if closed != 1 {
		t.Fatalf("platform close ran %d times, want 1", closed)
	}
}

// A RESIZE AFTER THE PTY IS GONE IS DROPPED, NOT WAITED ON. The window-change
// consumer outlives the session by a moment; sending into a channel nobody reads
// would leak that goroutine forever.
func TestPTYOwnerDropsResizeAfterClose(t *testing.T) {
	ps := newSessionPTY(ssh.Pty{}, func() error { return nil })
	if err := ps.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	within(t, "resize after close", func() { ps.resize(ssh.Window{Width: 80, Height: 24}) })
}

// A START AFTER THE PTY IS GONE IS REFUSED rather than executed on a handle the
// platform has already torn down. The command still needs an answer, because the
// caller reports it as "could not start".
func TestPTYOwnerRefusesStartAfterClose(t *testing.T) {
	ps := newSessionPTY(ssh.Pty{}, func() error { return nil })
	if err := ps.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	within(t, "start after close", func() {
		err := ps.start(exec.Command("true"))
		if !errors.Is(err, errPTYClosed) {
			t.Errorf("start after close = %v, want errPTYClosed", err)
		}
	})
}
