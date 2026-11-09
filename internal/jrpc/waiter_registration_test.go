package jrpc

import (
	"context"
	"testing"
	"time"
)

// TestTheWaiterIsRegisteredBeforeTheFrameGoesOut pins the ordering `wsCall` depends on.
//
// It used to write the frame and then register the waiter, and the gap between the two
// is a window in which the peer can answer. The dispatcher decides where a frame goes by
// looking up `wait[id]`, so an answer arriving inside that window found no waiter and was
// put in the backlog — where nothing reads it — while the call waited for a frame that
// had already arrived. On one ubuntu runner that was a 10-minute hang of
// `TestChannelRecordsEveryFrameAtTheWire`: the peer is an in-process websocket server, so
// "answers faster than the caller can register" is reachable, and a caller whose context
// carries no deadline waits forever rather than being told anything.
//
// THE ASSERTION IS THE ORDER, because the race itself cannot be forced from outside: the
// send callback IS the frame going out, so looking at the waiter map from inside it is
// exactly "is the waiter registered before the peer can possibly answer". With the write
// first, the map is empty here and the test fails; the answer would then be buffered for
// nobody.
func TestTheWaiterIsRegisteredBeforeTheFrameGoesOut(t *testing.T) {
	c := &Channel{
		done:    make(chan struct{}),
		dead:    make(chan struct{}),
		readErr: make(chan error, 1),
	}

	sawWaiter := false
	send := func() error {
		_, sawWaiter = c.wait.Load("0")
		// The peer answers as fast as it can, which is the situation the order exists
		// for: the frame goes to the waiter that is already registered.
		go func() { c.dispatch(Framed{N: 1, Data: `{"id":0,"result":{"echo":"early"}}`}) }()
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got, err := c.awaitAnswer(ctx, 0, send)
	if !sawWaiter {
		t.Fatalf("the frame went out before the waiter was registered: an answer arriving " +
			"in that window is buffered for nobody and the call waits for a frame that already came")
	}
	if err != nil {
		t.Fatalf("the answer was not delivered: %v", err)
	}
	if got != `{"id":0,"result":{"echo":"early"}}` {
		t.Errorf("delivered %q, want the answer", got)
	}
}

// TestAnAnswerWithNoWaiterIsStillBuffered is the other half: an answer nobody is waiting
// for is not rubbish, it goes to the backlog for a later reader. Registering first must
// not turn that case into a loss.
func TestAnAnswerWithNoWaiterIsStillBuffered(t *testing.T) {
	c := &Channel{
		done:    make(chan struct{}),
		dead:    make(chan struct{}),
		readErr: make(chan error, 1),
	}
	// A LITERAL Channel HAS NO WAKE GENERATION, and buffer/wakeAll handles that:
	// Swap returns nil and nothing is closed. This test only reads the backlog, so
	// the missing generation is not on its path.
	c.dispatch(Framed{N: 1, Data: `{"id":7,"result":{"late":true}}`})

	backlog := 0
	if p := c.ev.Load(); p != nil {
		backlog = len(*p)
	}
	if backlog != 1 {
		t.Fatalf("an answer with no waiter was dropped: ev holds %d frames, want 1", backlog)
	}
}
