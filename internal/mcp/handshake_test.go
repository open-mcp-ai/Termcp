package mcp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/open-mcp-ai/termcp/internal/jrpc"
)

// countingPeer is a socket peer that counts `initialize` frames and answers every
// call at once. It is the observation point for the handshake tests.
func countingPeer(t *testing.T, blockFirstInitialize <-chan struct{}) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	inits := 0
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var f map[string]any
			if err := conn.ReadJSON(&f); err != nil {
				return
			}
			if f["method"] == "initialize" {
				mu.Lock()
				inits++
				first := inits == 1
				mu.Unlock()
				if first && blockFirstInitialize != nil {
					<-blockFirstInitialize
				}
			}
			_ = conn.WriteJSON(map[string]any{"id": f["id"], "result": map[string]any{"ok": true}})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return inits
	}
}

// TestMcpHandshakeRunsOncePerHandle pins the invariant the flag exists for: the
// MCP handshake happens ONCE per handle, however many calls follow.
//
// The bug this guards against is a check-then-act: jrpcReady read h.init, ran the
// handshake, and only then set the flag, so two callers could both read "not
// initialized" and both send one. A real MCP server answers the second initialize
// with an error and the caller sees a broken connection rather than a race.
func TestMcpHandshakeRunsOncePerHandle(t *testing.T) {
	srv, inits := countingPeer(t, nil)
	s := newTestServer(t)
	internalSession(t, s) // gateways need a session to tunnel through
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal mcp ws" + strings.TrimPrefix(srv.URL, "http")}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	for i := 0; i < 4; i++ {
		out := termText(t, s, map[string]any{"cmd": `call ` + h + ` {"jsonrpc":"2.0","id":1,"method":"tools/list"}`})
		if !strings.Contains(out, `"ok":true`) {
			t.Fatalf("call %d did not reach the peer:\n%s", i, out)
		}
	}
	if n := inits(); n != 1 {
		t.Fatalf("initialize ran %d times for 4 calls on one handle; it must run once", n)
	}
}

// TestJrpcReadyHoldsItsLockAcrossTheHandshake is the concurrency half, aimed at
// jrpcReady itself rather than at Term.
//
// The peer BLOCKS inside the first handshake, and a second jrpcReady is started
// while it is blocked. If the lock is held across the call, the second waits and
// then finds the flag set — one initialize. If the lock is released before the
// call (which is what the original code did), the second walks straight in and
// sends its own — two.
//
// It drives jrpcReady directly because the property under test is that ONE helper
// makes the handshake happen exactly once even when two callers overlap. A Term-level
// test would exercise the whole dispatch path and could pass or fail for reasons
// unrelated to this guarantee; the helper is where it lives.
//
// THE SECOND CALLER WAITS ON A CHANNEL, NOT A MUTEX (issue #221): it must still be
// parked while the first handshake is in flight, but it is parked somewhere its own
// context can release it, which the third assertion below covers.
//
// (This used to say Server was unsafe for concurrent Term calls, because
// s.currentReply was per-call state on the shared value. That field is gone — see
// TestLearnModeUsageIsPerReply — so a concurrent Term is now legal.)
func TestJrpcReadyRunsTheHandshakeOnceForOverlappingCallers(t *testing.T) {
	release := make(chan struct{})
	srv, inits := countingPeer(t, release)

	ctx := t.Context()
	// A direct dialer: this test exercises the channel's own locking, not the SSH
	// path a gateway takes, so it hands jrpc.Open the local network explicitly.
	direct := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	ch, err := jrpc.Open(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), direct)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	h := &jrpcHandle{ch: ch, kind: "mcp"}
	srvObj := newTestServer(t)

	// The first handshake is in flight and parked inside the peer.
	first := make(chan error, 1)
	go func() { first <- srvObj.jrpcReady(ctx, h) }()
	// Enough for the frame to be on the wire.
	deadline := time.Now().Add(5 * time.Second)
	for inits() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if inits() == 0 {
		t.Fatal("the first handshake never reached the peer")
	}

	// The second ready runs WHILE the first is still blocked.
	second := make(chan error, 1)
	go func() { second <- srvObj.jrpcReady(ctx, h) }()
	// A moment to let it slip through if the lock is not held across the call.
	select {
	case err := <-second:
		if err == nil && inits() > 1 {
			t.Fatalf("a second jrpcReady ran through while the first was mid-handshake: "+
				"initialize ran %d times; the lock must be held ACROSS the call, not just around the flag", inits())
		}
		// It blocked on the channel and returned early for another reason.
	case <-time.After(200 * time.Millisecond):
		// Expected: it is waiting for the lock.
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if n := inits(); n != 1 {
		t.Fatalf("initialize ran %d times for two overlapping ready calls; it must run once", n)
	}
}

// TestASecondCallerCanLeaveTheHandshakeItIsWaitingFor pins the reason the wait moved
// off a mutex (issue #221).
//
// The first handshake is parked inside the peer and does not finish. A second caller
// arrives with a SHORT context. With a mutex around the handshake it had no way out:
// Lock does not take a context, so it waited for the first caller's whole window —
// which for a 60s call is a minute of an agent's budget spent on somebody else's
// handshake. Waiting on a channel gives it a door, and the assertion is that it uses
// it: the call returns its own deadline error while the first is still in flight.
func TestASecondCallerCanLeaveTheHandshakeItIsWaitingFor(t *testing.T) {
	release := make(chan struct{})
	srv, inits := countingPeer(t, release)
	s := newTestServer(t)

	direct := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	ch, err := jrpc.Open(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), direct)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	h := &jrpcHandle{ch: ch, kind: "mcp"}

	// First caller: a long window, parked inside the peer.
	go func() {
		long, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.jrpcReady(long, h)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for inits() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if inits() == 0 {
		t.Fatal("the first handshake never reached the peer")
	}

	// Second caller: its own short window, while the first is still stuck.
	short, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = s.jrpcReady(short, h)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the second caller reported success while the handshake was still in flight")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the second caller returned %v, want its own deadline", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the second caller waited %v; it must honour its own context", elapsed)
	}
	close(release)
}
