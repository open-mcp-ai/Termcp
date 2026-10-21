package mcp

import (
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
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "mcp open ws" + strings.TrimPrefix(srv.URL, "http")}))
	defer termText(t, s, map[string]any{"cmd": "mcp close " + h})

	for i := 0; i < 4; i++ {
		out := termText(t, s, map[string]any{"cmd": `mcp call ` + h + ` {"method":"tools/list"}`})
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
// holds its lock across the handshake. A Term-level test would exercise the whole
// dispatch path and could pass or fail for reasons unrelated to this lock; the
// helper is where the guarantee lives.
//
// (This used to say Server was unsafe for concurrent Term calls, because
// s.currentReply was per-call state on the shared value. That field is gone — see
// TestLearnModeUsageIsPerReply — so a concurrent Term is now legal.)
func TestJrpcReadyHoldsItsLockAcrossTheHandshake(t *testing.T) {
	release := make(chan struct{})
	srv, inits := countingPeer(t, release)

	ctx := t.Context()
	ch, err := jrpc.Open(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"))
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
