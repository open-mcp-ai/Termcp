package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// These pin the manual send/recv pair on a peer, which is the form a caller needs when
// the frame it wants is not a request/answer — a notification, a subscription, an event
// it is waiting for.
//
// The pair is easy to get wrong in a way that looks fine: a recv that only reads
// EVENTS can never deliver the answer to a send (an answer carries an id, and the event
// reader skips those), so the frame would sit in its own buffer, reachable by nobody.
// That is what Take exists for, and these tests are what keeps it.

// TestPeerSendThenRecvDeliversTheAnswer pins the pair end to end.
//
// It also pins the two shapes a caller relies on: send answers 202 (the frame is on the
// wire, the answer is not yet), and recv hands back the frame that answers it.
func TestPeerSendThenRecvDeliversTheAnswer(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	fake, srv := fakeDevtools(t)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	sent := termText(t, s, map[string]any{"cmd": `call ` + h + ` --send {"id":7,"method":"Target.getTargets","params":{}}`})
	if !strings.HasPrefix(sent, "TERM/1 202") {
		t.Fatalf("send did not answer 202:\n%s", sent)
	}
	if !strings.Contains(sent, "read ") {
		t.Errorf("send does not say how to read the answer:\n%s", sent)
	}

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --frame", "timeout": 5})
	if !strings.HasPrefix(got, "TERM/1 2") {
		t.Fatalf("recv did not read a frame:\n%s", got)
	}
	if !strings.Contains(got, `"id":7`) {
		t.Errorf("recv did not hand back the answer to the frame that was sent:\n%s", got)
	}
	waitFor(t, "the frame to reach the peer", func() bool {
		_, ok := fake.saw("Target.getTargets")
		return ok
	})
}

// TestRecvOnAnHttpChannelExplainsItself pins the door where recv has nothing to wait
// for.
//
// A peer that sends nothing on its own has no stream to deliver unsolicited frames, so a
// recv that returned "nothing yet" would be indistinguishable from a slow peer. The
// reply says which shape to use instead.
//
// THIS IS NO LONGER A STATEMENT ABOUT HTTP. Streamable MCP carries server-initiated
// frames on a listening GET stream, so an HTTP endpoint that accepts one CAN be pushed
// to (issue #210) — the question is asked of the channel, not of the transport. The
// peer here is one that refuses that stream, which is the case that still has nothing
// to wait for.
func TestRecvOnAnHttpChannelExplainsItself(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	url := mcpJSONPeer(t, s)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal jrpc " + url}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	got := termText(t, s, map[string]any{"cmd": "read " + h + " --frame", "timeout": 0})
	if strings.HasPrefix(got, "TERM/1 5") {
		t.Fatalf("a channel with nothing to wait for reported a server failure:\n%s", got)
	}
	if !strings.Contains(got, "nothing to wait for") {
		t.Errorf("recv on an HTTP channel did not explain why it cannot wait:\n%s", got)
	}
}

// TestRecvOnAStreamableHttpMcpChannelReadsUnsolicitedEvents pins unsolicited server
// events over HTTP MCP (#210).
//
// A spec-compliant streamable HTTP MCP server sends unsolicited notifications on the
// listening GET stream (text/event-stream with Mcp-Session-Id). A `recv` (read --frame)
// issued over such a channel must deliver that event rather than reporting that there is
// nothing to wait for.
func TestRecvOnAStreamableHttpMcpChannelReadsUnsolicitedEvents(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.Header().Set("Mcp-Session-Id", "sess-test-recv")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`))
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/test\",\"params\":{\"val\":42}}\n\n"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}
	}))
	defer srv.Close()

	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal jrpc " + srv.URL}))
	defer termText(t, s, map[string]any{"cmd": "close " + h})

	// One call to negotiate the session ID.
	initGot := termText(t, s, map[string]any{"cmd": "call " + h + ` '{"id":1,"method":"init"}'`})
	if !strings.HasPrefix(initGot, "TERM/1 2") {
		t.Fatalf("init call failed:\n%s", initGot)
	}

	// NO WAITING FOR THE HANDSHAKE (issue #213). The read runs immediately after the
	// call, so it exercises the window where the listening GET has not yet finished
	// connecting.
	got := termText(t, s, map[string]any{"cmd": "read " + h + " --frame", "timeout": 2})
	if !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("expected 200 with the unsolicited event, got:\n%s", got)
	}
	if !strings.Contains(got, `"notifications/test"`) || !strings.Contains(got, `"val":42`) {
		t.Fatalf("the reply did not contain the pushed notification:\n%s", got)
	}
}

// TestCloseForgetsTheChannel pins that `close` really forgets it: the second close is a
// 404 naming the handle, not a silent success on a channel that is already gone.
func TestCloseForgetsTheChannel(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	_, srv := fakeDevtools(t)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))

	if got := termText(t, s, map[string]any{"cmd": "close " + h}); !strings.HasPrefix(got, "TERM/1 204") {
		t.Fatalf("close did not answer 204:\n%s", got)
	}
	again := termText(t, s, map[string]any{"cmd": "close " + h})
	if !strings.HasPrefix(again, "TERM/1 404") {
		t.Fatalf("closing an already-closed channel was not a 404:\n%s", again)
	}
	if !strings.Contains(again, h) {
		t.Errorf("the 404 does not name the handle that is gone:\n%s", again)
	}
}

// TestCallOnAHandleThatIsNotThere pins the 404 a caller gets for a handle that was
// never opened.
//
// The fallback is deliberate and is what the message has to distinguish: a handle that
// is not a peer might still be a SHELL (the stdio form), so the answer names the thing
// that was looked for instead of claiming the frame failed.
func TestCallOnAHandleThatIsNotThere(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	got := termText(t, s, map[string]any{"cmd": `call nosuchhandle {"jsonrpc":"2.0","id":1,"method":"x"}`})
	if !strings.HasPrefix(got, "TERM/1 4") && !strings.HasPrefix(got, "TERM/1 5") {
		t.Fatalf("a frame sent to a handle that does not exist was accepted:' + bs + 'n%s", got)
	}
	if !strings.Contains(got, "nosuchhandle") {
		t.Errorf("the failure does not name the handle:' + bs + 'n%s", got)
	}
}

// mcpJSONPeer serves this daemon's own MCP handler as a peer: a real HTTP JSON-RPC
// endpoint that answers initialize and tools/list, so the channel under test is on the
// HTTP transport without a second binary.
func mcpJSONPeer(t *testing.T, s *Server) string {
	t.Helper()
	ts := httptest.NewServer(s.StreamableHTTPHandler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// deadPeer advertises a socket and then drops it, which is what a peer that crashed
// mid-channel looks like from this side: discovery still works, the frames do not.
func deadPeer(t *testing.T) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/json/version" {
			_, _ = w.Write([]byte(`{"webSocketDebuggerUrl":"ws://` + r.Host + `/devtools/browser/x"}`))
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestACallToADeadPeerIsAConnectionFailure pins what a caller sees when the peer goes
// away underneath an open channel.
//
// The distinction matters: the channel was real, the frame was well formed, and the
// failure is neither the caller's mistake nor the daemon's — so the reply names the
// METHOD that could not be sent and the code that says the connection went, rather
// than a generic 500 that reads like a bug here.
//
// `call`, not `--send`: a send into a socket that has been closed can succeed locally
// (the bytes reach the OS buffer before the reset does), so a 202 there is not a
// promise that the frame arrived. A call WAITS for the answer, which is where a dead
// peer cannot hide.
func TestACallToADeadPeerIsAConnectionFailure(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	srv := deadPeer(t)
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal cdp " + srv.URL}))

	got := termText(t, s, map[string]any{"cmd": `call ` + h + ` {"id":1,"method":"Target.getTargets","params":{}}`, "timeout": 2})
	if !strings.HasPrefix(got, "TERM/1 5") {
		t.Fatalf("a call to a dead peer was answered as if it succeeded:\n%s", got)
	}
	if !strings.Contains(got, "connection_failed") {
		t.Errorf("the failure does not say the connection is what went:\n%s", got)
	}
	if !strings.Contains(got, "Target.getTargets") {
		t.Errorf("the failure does not name the method that could not be sent:\n%s", got)
	}
}

// TestACallOnAJrpcChannelThatDiedIsAlsoAConnectionFailure covers the other door: the
// same failure on a plain JSON-RPC channel, which reaches the error through jrpcChannel
// rather than through the CDP path.
func TestACallOnAJrpcChannelThatDiedIsAlsoAConnectionFailure(t *testing.T) {
	s := newTestServer(t)
	internalSession(t, s)
	srv := deadPeer(t)
	// A jrpc door takes the socket as given: no discovery, so a dead socket is a dead
	// channel from the first frame.
	ws := "ws" + strings.TrimPrefix(srv.URL, "http") + "/rpc"
	h := handleFrom(t, termText(t, s, map[string]any{"cmd": "open internal jrpc " + ws}))

	got := termText(t, s, map[string]any{"cmd": `call ` + h + ` {"id":1,"method":"ping"}`, "timeout": 2})
	if !strings.HasPrefix(got, "TERM/1 5") {
		t.Fatalf("a call on a dead jrpc channel was answered as if it succeeded:\n%s", got)
	}
	if !strings.Contains(got, "connection_failed") {
		t.Errorf("the failure does not say the connection is what went:\n%s", got)
	}
}
