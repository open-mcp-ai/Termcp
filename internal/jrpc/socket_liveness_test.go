package jrpc

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"
)

// A recv window that expires must NOT poison the socket.
//
// gorilla caches the first read error in Conn.readErr and never clears it, so a
// read deadline that fires leaves the connection permanently unreadable: every
// later read returns that cached timeout immediately, without touching the wire.
// The caller sees "no frame within the window" forever, even after the peer has
// answered.
func TestIdleRecvDoesNotPoisonTheSocket(t *testing.T) {
	srv, _ := wsPeer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ch, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	// 1. An idle window that expires with nothing to read.
	ctx1, cancel1 := context.WithTimeout(t.Context(), 150*time.Millisecond)
	_, ok := ch.Next(ctx1)
	cancel1()
	if ok {
		t.Fatal("the idle window returned a frame")
	}

	// 2. The peer is still healthy: ask it something.
	if err := ch.Send(t.Context(), map[string]any{"jsonrpc": "2.0", "method": "Ping"}); err != nil {
		t.Fatalf("send after an idle window: %v", err)
	}

	// 3. The answer must arrive. Before the fix this returns immediately with
	// nothing, because the cached read error short-circuits every read.
	// The peer pushes one event before every answer, so the frames that must arrive
	// are the event AND the answer carrying the id. Getting either proves the socket
	// survived; before the fix BOTH were lost to the cached read error.
	ctx2, cancel2 := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel2()
	event, ok := ch.Next(ctx2)
	if !ok {
		t.Fatal("the socket was poisoned by one idle window: nothing arrived after the peer answered")
	}
	if !strings.Contains(event, "Page.lifecycleEvent") {
		t.Fatalf("expected the peer's event first, got: %s", event)
	}

	ctx3, cancel3 := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel3()
	answer, ok := ch.Next(ctx3)
	if !ok {
		t.Fatal("the socket delivered its event but not the answer that followed it")
	}
	if !strings.Contains(answer, "Ping") {
		t.Fatalf("expected the answer to Ping, got: %s", answer)
	}
}

// A frame that Next skips (because it carries an id) must still be REACHABLE.
//
// Next buffers an answer it was not waiting for, and Recv pops the buffer — but a
// caller that only ever calls Next (which is what the `recv` directive does) has no
// way to get it back. So `cdp send` + `cdp recv` never delivered the answer: the
// frame was on the channel, in its own buffer, unreachable.
func TestAFrameSkippedByNextIsStillReachable(t *testing.T) {
	srv, _ := wsPeer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ch, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	// Send a frame with an id: the peer answers with that id (preceded by an event).
	if err := ch.Send(t.Context(), map[string]any{"jsonrpc": "2.0", "id": 7, "method": "Ping"}); err != nil {
		t.Fatal(err)
	}
	// Take is the door that sees buffered frames AND the socket.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		frame, ok := ch.Take(ctx)
		if !ok {
			t.Fatal("the answer to Send was never reachable")
		}
		if strings.Contains(frame, `"echo"`) {
			return // the answer was reachable
		}
	}
}

// Closing a channel while a reader is waiting must release it promptly, not park
// it for the rest of its window.
func TestCloseReleasesAWaitingReader(t *testing.T) {
	srv, _ := wsPeer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ch, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan bool, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, ok := ch.Take(ctx)
		done <- ok
	}()

	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	_ = ch.Close()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("the reader reported a frame after Close")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Close left the reader waiting %v", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not release the waiting reader")
	}
}

// Many open/close cycles must not leave goroutines behind.
func TestRepeatedCloseIsCheap(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 30; i++ {
		srv, _ := wsPeer(t)
		url := "ws" + strings.TrimPrefix(srv.URL, "http")
		ch, err := Open(t.Context(), url)
		if err != nil {
			t.Fatal(err)
		}
		_ = ch.Send(t.Context(), map[string]any{"jsonrpc": "2.0", "id": 1, "method": "Ping"})
		_ = ch.Close()
		srv.Close()
	}
	// Give the loops a moment to unwind, then require they did.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+5 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("goroutines leaked: %d before, %d after", before, runtime.NumGoroutine())
}
