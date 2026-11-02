package extensions

import (
	"context"
	"errors"
	"github.com/open-mcp-ai/termcp/internal/session"
	"testing"
	"time"
)

// TestEnsureGroupSessionSecondCallerCanLeaveItsDial pins that a member of a group
// waiting for the group's connection obeys its OWN context (issue #222).
//
// ensureGroupSession used to hold g.mu across sessMgr.Create — a full SSH dial and
// handshake — so a second caller waited inside Mutex.Lock, which takes no context:
// it could not leave until the first dial's entire timeout elapsed, and the same
// lock serialised every other operation on that group.
//
// The wait is a channel now, so this asserts the property that a mutex cannot
// offer: a caller with a short deadline returns with that deadline while the dial
// it was waiting for is still in flight.
func TestEnsureGroupSessionSecondCallerCanLeaveItsDial(t *testing.T) {
	r := &Reconciler{}
	g := &extGroup{key: "test-group", target: "internal"}

	// A dial in flight: set, and never finished. This is what a second caller finds
	// while the first is still inside the handshake.
	d := &groupDial{done: make(chan struct{})}
	g.mu.Lock()
	g.dialing = d
	g.mu.Unlock()

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer shortCancel()

	start := time.Now()
	_, err := r.ensureGroupSession(shortCtx, g, session.Config{})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second caller returned %v, want its own deadline", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("second caller waited %v; it must obey its own context rather than the dial", elapsed)
	}
}

// TestEnsureGroupSessionSharesTheDialsResult pins the other half: waiting is not
// skipping. Every caller that stayed finds the same session, which is what "two
// extensions coming up at once open ONE connection" means.
func TestEnsureGroupSessionSharesTheDialsResult(t *testing.T) {
	r := &Reconciler{}
	g := &extGroup{key: "test-group", target: "internal"}

	d := &groupDial{done: make(chan struct{})}
	g.mu.Lock()
	g.dialing = d
	g.mu.Unlock()

	type outcome struct {
		sess *session.Session
		err  error
	}
	got := make(chan outcome, 2)
	for i := 0; i < 2; i++ {
		go func() {
			s, err := r.ensureGroupSession(context.Background(), g, session.Config{})
			got <- outcome{sess: s, err: err}
		}()
	}

	// Both are parked on d.done. Publish the first dial's result.
	time.Sleep(50 * time.Millisecond)
	d.sess = nil
	d.err = nil
	close(d.done)

	for i := 0; i < 2; i++ {
		o := <-got
		if o.err != nil {
			t.Fatalf("waiter %d got %v, want the dial's result", i, o.err)
		}
	}
}
