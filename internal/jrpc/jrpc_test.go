package jrpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsPeer is a socket peer that answers by id and pushes one event before each
// answer — the shape that breaks a client assuming "one write, one read".
func wsPeer(t *testing.T) (*httptest.Server, *int) {
	t.Helper()
	up := websocket.Upgrader{}
	var events int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var frame map[string]any
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			// An unsolicited event first: no id, so it is not the answer.
			events++
			_ = conn.WriteJSON(map[string]any{
				"method": "Page.lifecycleEvent",
				"params": map[string]any{"name": "load", "n": events},
			})
			// Then the answer, echoing the id and the method.
			_ = conn.WriteJSON(map[string]any{
				"id":     frame["id"],
				"result": map[string]any{"echo": frame["method"]},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &events
}

// TestSocketCallsSurviveInterleavedEvents is the reason the channel buffers: a
// peer that pushes an event before every answer would, without the skip, hand
// the event back as if it were the answer.
func TestSocketCallsSurviveInterleavedEvents(t *testing.T) {
	srv, _ := wsPeer(t)
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ch, err := Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	if ch.Transport() != WS {
		t.Fatalf("transport = %v, want ws", ch.Transport())
	}

	reply, err := ch.Call(t.Context(), map[string]any{"method": "Page.navigate"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Page.navigate") {
		t.Fatalf("the reply is not the answer to this call: %s", reply)
	}
	// The event that arrived while waiting is not lost: it is buffered.
	event, ok := ch.Recv()
	if !ok {
		t.Fatal("the interleaved event was dropped instead of buffered")
	}
	if !strings.Contains(event, "Page.lifecycleEvent") {
		t.Fatalf("buffered %s, want the lifecycle event", event)
	}
	if _, ok := ch.Recv(); ok {
		t.Fatal("Recv returned the same event twice")
	}
}

// TestSocketIdsAreAllocatedAndEchoed pins that a caller writing a frame with no
// id gets one, and that the matching is done on that id.
func TestSocketIdsAreAllocatedAndEchoed(t *testing.T) {
	srv, _ := wsPeer(t)
	ch, err := Open(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	for i := 0; i < 3; i++ {
		if _, err := ch.Call(t.Context(), map[string]any{"method": "Runtime.evaluate"}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}

// TestHTTPCallMatchesTheEventStreamAnswer is the streamable-MCP shape: the reply
// arrives as one SSE event among others, not as the body.
func TestHTTPCallMatchesTheEventStreamAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var frame map[string]any
		_ = json.NewDecoder(r.Body).Decode(&frame)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "sess-1")
		flusher, _ := w.(http.Flusher)
		// An unrelated notification first, then the answer.
		_, _ = w.Write([]byte(":keepalive\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		answer, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": frame["id"], "result": map[string]any{"ok": true}})
		_, _ = w.Write([]byte("data: " + string(answer) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	ch, err := Open(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	reply, err := ch.Call(t.Context(), map[string]any{"jsonrpc": "2.0", "method": "tools/list"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, `"ok":true`) {
		t.Fatalf("did not find the answer on the stream: %s", reply)
	}
	// The notification rode the same body and must not be lost.
	if ev, ok := ch.Recv(); !ok || !strings.Contains(ev, "notifications/progress") {
		t.Fatalf("the stream's notification was not buffered: %q (ok=%v)", ev, ok)
	}
}

// TestHTTPReportsAStreamThatEndsWithoutTheAnswer pins the failure a peer sees
// when its server hangs up early: an error, never an empty success.
func TestHTTPReportsAStreamThatEndsWithoutTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"method\":\"only-an-event\"}\n\n"))
	}))
	defer srv.Close()
	ch, err := Open(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	if _, err := ch.Call(t.Context(), map[string]any{"method": "x"}); err == nil {
		t.Fatal("a stream that ended before the answer was reported as success")
	}
}

// TestOpenRefusesAnUnknownScheme keeps the scheme honest: a caller that writes a
// bare host gets a sentence naming what works, not a dial failure.
func TestOpenRefusesAnUnknownScheme(t *testing.T) {
	if _, err := Open(t.Context(), "ftp://example.com"); err == nil ||
		!strings.Contains(err.Error(), "http(s)") {
		t.Fatalf("an unknown scheme was not explained: %v", err)
	}
}

// TestClosedChannelRefusesCalls pins that close is final rather than a hint.
func TestClosedChannelRefusesCalls(t *testing.T) {
	srv, _ := wsPeer(t)
	ch, err := Open(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Call(t.Context(), map[string]any{"method": "x"}); err == nil {
		t.Fatal("a closed channel accepted a call")
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("closing twice is not an error: %v", err)
	}
}

// TestCallDeadlineIsNotUnbounded pins the guard that keeps a silent peer from
// hanging an agent turn: the context's deadline reaches the socket read.
func TestCallDeadlineIsNotUnbounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // never answer
	}))
	defer srv.Close()
	ch, err := Open(t.Context(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if _, err := ch.Call(ctx, map[string]any{"method": "x"}); err == nil {
		t.Fatal("a peer that never answers returned success")
	}
}
