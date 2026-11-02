package jrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	ch, err := Open(t.Context(), url, localDial)
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
	ch, err := Open(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), localDial)
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

	ch, err := Open(t.Context(), srv.URL, localDial)
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
	ch, err := Open(t.Context(), srv.URL, localDial)
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
	if _, err := Open(t.Context(), "ftp://example.com", localDial); err == nil ||
		!strings.Contains(err.Error(), "http(s)") {
		t.Fatalf("an unknown scheme was not explained: %v", err)
	}
}

// TestClosedChannelRefusesCalls pins that close is final rather than a hint.
func TestClosedChannelRefusesCalls(t *testing.T) {
	srv, _ := wsPeer(t)
	ch, err := Open(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), localDial)
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
	ch, err := Open(t.Context(), srv.URL, localDial)
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

// TestAListeningStreamIsNotEndedByItsTotalSize pins the unit the frame limit is
// counted in (issue #213).
//
// The listening stream is a persistent connection that is expected to deliver an
// unbounded number of frames, so a limit wrapped around its BODY ends the stream once
// the running total passes it — turning "this peer has been chatty" into "this peer
// stopped talking", which is the kind of silence that looks like a quiet peer. The cap
// belongs on one frame.
//
// The sizes are small enough to run fast and large enough that the total crosses the
// per-frame cap: the old code returned EOF partway through.
func TestAListeningStreamIsNotEndedByItsTotalSize(t *testing.T) {
	const frames = 24
	payload := strings.Repeat("x", 64<<10) // 64 KiB per frame, ~1.5 MiB total
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Mcp-Session-Id", "sess-big")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			for i := 0; i < frames; i++ {
				_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"n/%d\",\"params\":{\"pad\":\"%s\"}}\n\n", i, payload)
			}
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	ch, err := Open(t.Context(), srv.URL, localDial)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := ch.Call(ctx, map[string]any{"id": 1, "method": "initialize"}); err != nil {
		t.Fatalf("initialize call failed: %v", err)
	}

	// The last frame is the one past the old cap, so reading all of them is what
	// proves the stream survived the total.
	//
	// ORDER AND NO REPEATS, because that is what a broken stream actually looks
	// like from here: a body limit ends the carrier mid-way, the reconnect loop
	// opens a new one, and the server pushes its backlog again from the start. A
	// count alone cannot see that — the duplicates fill the same total — so each
	// read has to be the frame it should be.
	for i := 0; i < frames; i++ {
		readCtx, readCancel := context.WithTimeout(t.Context(), 5*time.Second)
		ev, ok := ch.Next(readCtx)
		readCancel()
		if !ok {
			t.Fatalf("the stream ended after %d of %d frames; its total size must not close it", i, frames)
		}
		var got struct {
			Method string `json:"method"`
		}
		if err := json.Unmarshal([]byte(ev), &got); err != nil {
			t.Fatalf("frame %d is not a JSON object: %v", i, err)
		}
		if want := fmt.Sprintf("n/%d", i); got.Method != want {
			t.Fatalf("read %d was %q, want %q; a restarted stream re-delivers frames it already sent", i, got.Method, want)
		}
	}
}

// not implement streamable GET (405 Method Not Allowed) does not cause infinite
// retry storms or errors.
func TestHTTPChannelSilentlyHandlesUnsupportedGET(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Mcp-Session-Id", "sess-405")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
		case http.MethodGet:
			http.Error(w, "not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	ch, err := Open(t.Context(), srv.URL, localDial)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := ch.Call(ctx, map[string]any{"id": 1, "method": "initialize"}); err != nil {
		t.Fatalf("call failed: %v", err)
	}

	// Wait briefly to confirm no panic, busy-loop, or hang.
	time.Sleep(100 * time.Millisecond)
}

// SSH path is covered where it is built (internal/mcp), and this package is about the
// channel's framing and locking rather than where the bytes came from.
func localDial(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

// TestHTTPChannelReceivesUnsolicitedEvents pins unsolicited server-initiated events
// over the HTTP transport (#210).
//
// A spec-compliant streamable HTTP MCP server sends unsolicited notifications on the
// listening GET stream (text/event-stream with Mcp-Session-Id). The channel must open
// this stream as soon as a session id is assigned, and unsolicited events must land in
// the event backlog where Next/Take can read them without waiting for an active POST.
//
// THE READ HAPPENS IMMEDIATELY AFTER THE CALL, ON PURPOSE (issue #213). The listening
// GET is opened by a goroutine, so there is a window in which the POST has answered and
// the stream has not finished handshaking. A reader that asks "is the stream up" in that
// window gets false and gives up, even though the frame is microseconds away. So this
// waits for the FRAME and never for the handshake.
func TestHTTPChannelReceivesUnsolicitedEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Mcp-Session-Id", "sess-123")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
		case http.MethodGet:
			if r.Header.Get("Mcp-Session-Id") != "sess-123" {
				http.Error(w, "missing session id", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			// Push an unsolicited notification.
			_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"text\":\"hello-from-server\"}}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		default:
			http.Error(w, "bad method", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	ch, err := Open(t.Context(), srv.URL, localDial)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	// Initial call establishes the session id.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := ch.Call(ctx, map[string]any{"id": 1, "method": "initialize"}); err != nil {
		t.Fatalf("initialize call failed: %v", err)
	}

	// NO WAITING FOR THE HANDSHAKE. The event is pushed by the server as soon as the
	// GET arrives, and Next is what has to be willing to wait for it.
	readCtx, readCancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer readCancel()
	ev, ok := ch.Next(readCtx)
	if !ok {
		t.Fatal("ch.Next failed to read the unsolicited event")
	}
	if !strings.Contains(ev, "hello-from-server") {
		t.Fatalf("unexpected event payload: %s", ev)
	}
}

// TestTakeDoesNotStealTheAnswerACallIsWaitingFor pins the race between the two
// readers of one socket.
//
// Call waits for the frame carrying its id; Take waits for the next frame of any
// kind. Both read c.in, and only Call takes the callGate, so a Take issued while a
// call is in flight can lift that call's answer out of the channel first. The call
// then sits until its window expires and reports "no answer" — while the answer was
// delivered to somebody else, which is what "the result was swallowed" looks like
// from the caller's side.
//
// The peer here answers slowly on purpose: the answer must still be in flight when
// Take starts waiting, otherwise the test proves nothing.
func TestTakeDoesNotStealTheAnswerACallIsWaitingFor(t *testing.T) {
	up := websocket.Upgrader{}
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
			// Long enough that Take is already parked on c.in when this lands.
			time.Sleep(150 * time.Millisecond)
			_ = conn.WriteJSON(map[string]any{
				"id":     frame["id"],
				"result": map[string]any{"echo": frame["method"]},
			})
		}
	}))
	defer srv.Close()

	ch, err := Open(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), localDial)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	// A reader that starts before the call and waits across it: exactly what a
	// `read` or a `recv` issued in the same turn as a `call` does.
	taken := make(chan string, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		if frame, ok := ch.Take(ctx); ok {
			taken <- frame
		}
	}()
	// Let Take reach its wait before the call writes anything.
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	answer, err := ch.Call(ctx, map[string]any{"method": "Runtime.evaluate"})
	if err != nil {
		t.Fatalf("the call lost its answer to a concurrent Take: %v", err)
	}
	if !strings.Contains(answer, "Runtime.evaluate") {
		t.Fatalf("the call returned something else: %s", answer)
	}
	select {
	case got := <-taken:
		t.Fatalf("Take returned the frame the call was waiting for: %s", got)
	default:
	}
}

// TestAnAnswerNobodyIsWaitingForStaysReadable pins the other half of the routing
// rule: a frame carrying an id is NOT an event, but it is not rubbish either.
//
// A peer answers a `send` (a frame written without waiting), and nothing is
// registered for that id. It has to reach the reader that comes looking later —
// through the same queue events go to — because otherwise the only copy is the
// one the wire already delivered and dropped.
func TestAnAnswerNobodyIsWaitingForStaysReadable(t *testing.T) {
	up := websocket.Upgrader{}
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
			// Always an answer, never an event: the case Take has to be able to
			// hand back and Next has to be able to skip.
			_ = conn.WriteJSON(map[string]any{
				"id":     frame["id"],
				"result": map[string]any{"echo": frame["method"]},
			})
		}
	}))
	defer srv.Close()

	ch, err := Open(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), localDial)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	if err := ch.Send(t.Context(), map[string]any{"id": 7, "method": "Page.navigate"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	frame, ok := ch.Take(ctx)
	if !ok {
		t.Fatal("the answer to a send never reached the reader")
	}
	if !strings.Contains(frame, "Page.navigate") {
		t.Fatalf("Take returned something else: %s", frame)
	}
}

// TestNextSkipsAnAnswerAndKeepsItForTake pins that "next event" is a filter, not a
// drain.
//
// Next must not hand an answer to an event reader, and it must not destroy it
// either: it is still the frame a later Take is owed. Re-queueing it (what the
// previous implementation did) is not the same thing — with the queue full of
// answers, push-then-pop spins forever.
func TestNextSkipsAnAnswerAndKeepsItForTake(t *testing.T) {
	up := websocket.Upgrader{}
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
			_ = conn.WriteJSON(map[string]any{"id": frame["id"], "result": map[string]any{"echo": 1}})
			_ = conn.WriteJSON(map[string]any{"method": "Page.loadEventFired"})
		}
	}))
	defer srv.Close()

	ch, err := Open(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), localDial)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	if err := ch.Send(t.Context(), map[string]any{"id": 11, "method": "Page.enable"}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	event, ok := ch.Next(ctx)
	if !ok {
		t.Fatal("Next never returned the event")
	}
	if strings.Contains(event, `"id"`) {
		t.Fatalf("Next handed an answer to an event reader: %s", event)
	}
	// The answer is still there: skipping is not consuming.
	answer, ok := ch.Take(ctx)
	if !ok {
		t.Fatal("Next consumed the answer instead of skipping it")
	}
	if !strings.Contains(answer, `"id"`) {
		t.Fatalf("the frame left behind is not the answer: %s", answer)
	}
}

// TestAnAnswerArrivingAsTheWindowClosesIsNotLost pins the delivery/timeout race.
//
// The dispatcher can hand a call its answer in the same instant the call's window
// expires. If the call simply returns, that frame is in a channel nobody will read
// again and the result is gone — the same disappearance as the stealing bug, only
// harder to see because it needs a deadline to land on a delivery.
func TestAnAnswerArrivingAsTheWindowClosesIsNotLost(t *testing.T) {
	up := websocket.Upgrader{}
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
			// Land the answer right around the caller's deadline.
			time.Sleep(60 * time.Millisecond)
			_ = conn.WriteJSON(map[string]any{"id": frame["id"], "result": map[string]any{"echo": 1}})
		}
	}))
	defer srv.Close()

	ch, err := Open(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), localDial)
	if err != nil {
		t.Fatal(err)
	}
	defer ch.Close()

	// A window that closes while the answer is in flight, over and over: whichever
	// side of the deadline each attempt lands on, the frame must be findable.
	for i := 0; i < 12; i++ {
		ctx, cancel := context.WithTimeout(t.Context(), 55*time.Millisecond)
		_, err := ch.Call(ctx, map[string]any{"id": 100 + i, "method": "Runtime.evaluate"})
		cancel()
		if err == nil {
			continue // answered in time; nothing to recover
		}
		// The call gave up. The answer may still arrive, and must be readable.
		readCtx, readCancel := context.WithTimeout(t.Context(), 2*time.Second)
		found := false
		for !found {
			frame, ok := ch.Take(readCtx)
			if !ok {
				break
			}
			if strings.Contains(frame, `"id":`+strconv.Itoa(100+i)) {
				found = true
			}
		}
		readCancel()
		if !found {
			t.Fatalf("round %d: the answer to a call that timed out is nowhere", i)
		}
	}
}
