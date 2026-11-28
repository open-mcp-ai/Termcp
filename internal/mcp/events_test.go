package mcp

// EXTENSION EVENTS ON THE MAIN ENDPOINT (#275's narrow half).
//
// The tests here use a recording session rather than an HTTP client, because what they
// pin is the table and the delivery decision — who asked, what they get, what happens
// when the client is gone. The transport's own job (writing a notification to an open
// GET stream) is already covered by mcp/streamable's tests, and exercising it again
// through HTTP here would test the transport twice and the subscription not at all.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/mcp"
)

// recordingSession is a client that keeps what it was sent.
type recordingSession struct {
	id string
	// fail makes every delivery fail, which is the "client is not reading" case.
	fail bool

	mu    sync.Mutex
	notes []recordedNote
}

type recordedNote struct {
	method string
	params map[string]any
}

func (r *recordingSession) SessionID() string { return r.id }

func (r *recordingSession) GetClientInfo() mcp.Implementation {
	return mcp.Implementation{Name: "plugin"}
}

func (r *recordingSession) Notify(_ context.Context, method string, params any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail {
		return context.Canceled
	}
	body, _ := json.Marshal(params)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	r.notes = append(r.notes, recordedNote{method: method, params: decoded})
	return nil
}

func (r *recordingSession) got() []recordedNote {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recordedNote(nil), r.notes...)
}

// extPeerFixture creates one extension template so a subscription can name it.
func extPeerFixture(t *testing.T, name string) *Server {
	t.Helper()
	s := newExtensionTestServer(t)
	callExtension(t, s, map[string]any{
		"action": "create", "name": name, "target": "internal",
		"command": "cat", "enabled": false,
	})
	return s
}

func subscribeAs(t *testing.T, s *Server, sess *recordingSession, ext string) map[string]any {
	t.Helper()
	s.mcpServer.RegisterSession(context.Background(), sess)
	raw, _ := json.Marshal(map[string]any{"ext": ext})
	out, rpcErr := s.handleExtSubscribe(mcp.WithSessionContext(context.Background(), sess), raw)
	if rpcErr != nil {
		t.Fatalf("subscribe(%q): %s", ext, rpcErr.Message)
	}
	body, _ := json.Marshal(out)
	var decoded map[string]any
	_ = json.Unmarshal(body, &decoded)
	return decoded
}

// A SUBSCRIBER RECEIVES THE PEER'S OWN METHOD NAME, wrapped so it knows whose it is.
func TestASubscriberReceivesTheExtensionsNotification(t *testing.T) {
	s := extPeerFixture(t, "peer")
	sess := &recordingSession{id: "s-1"}
	reply := subscribeAs(t, s, sess, "peer")

	if subs, _ := reply["subscribed"].([]any); len(subs) != 1 || subs[0] != "peer" {
		t.Fatalf("subscribe answered %v, want [peer]", reply["subscribed"])
	}
	// The alternative to a replay is silence, so the answer has to say so.
	if note, _ := reply["from_now"].(string); !strings.Contains(note, "not") {
		t.Errorf("the subscribe reply does not say that nothing is replayed: %v", reply)
	}

	s.publishExtEvent(context.Background(), "peer", "notifications/acme/job", json.RawMessage(`{"seq":7}`))

	notes := sess.got()
	if len(notes) != 1 {
		t.Fatalf("delivered %d notifications, want 1: %v", len(notes), notes)
	}
	if notes[0].method != extEventMethod {
		t.Fatalf("method = %q, want %q", notes[0].method, extEventMethod)
	}
	if notes[0].params["extension"] != "peer" || notes[0].params["method"] != "notifications/acme/job" {
		t.Fatalf("the envelope lost which extension spoke: %v", notes[0].params)
	}
	// THE PEER'S PAYLOAD IS UNTOUCHED: a wrapped frame that re-encoded its params would
	// be a second implementation of the peer's own payload.
	inner, _ := notes[0].params["params"].(map[string]any)
	if inner["seq"] != float64(7) {
		t.Fatalf("the payload did not survive the envelope: %v", notes[0].params["params"])
	}
}

// THE WILDCARD COVERS AN EXTENSION THAT DID NOT EXIST WHEN THE CLIENT ASKED, which is
// the shape a plugin needs: it is running before the peers are.
func TestWildcardSubscriptionFollowsEveryExtension(t *testing.T) {
	s := extPeerFixture(t, "peer")
	sess := &recordingSession{id: "s-2"}
	subscribeAs(t, s, sess, "*")

	s.publishExtEvent(context.Background(), "peer", "notifications/a", nil)
	s.publishExtEvent(context.Background(), "created-later", "notifications/b", nil)

	if got := len(sess.got()); got != 2 {
		t.Fatalf("the wildcard delivered %d of 2 notifications", got)
	}
}

// A SESSION THAT ASKED FOR NOTHING IS NOT DELIVERED TO, and neither is one that left.
func TestDeliveryFollowsTheSubscriptionAndTheSession(t *testing.T) {
	s := extPeerFixture(t, "peer")
	quiet := &recordingSession{id: "s-3"}
	watch := &recordingSession{id: "s-4"}
	s.mcpServer.RegisterSession(context.Background(), quiet)
	s.mcpServer.RegisterSession(context.Background(), watch)
	subscribeAs(t, s, watch, "peer")

	s.publishExtEvent(context.Background(), "peer", "notifications/a", nil)
	if n := len(quiet.got()); n != 0 {
		t.Fatalf("a session that subscribed to nothing received %d notifications", n)
	}
	if n := len(watch.got()); n != 1 {
		t.Fatalf("the subscriber received %d of 1 notifications", n)
	}

	// AN ENDED SESSION LEAVES NOTHING BEHIND: the table holds the session object, and
	// writing to one that is gone is a delivery nobody can receive.
	s.mcpServer.UnregisterSession(context.Background(), watch)
	s.publishExtEvent(context.Background(), "peer", "notifications/b", nil)
	if n := len(watch.got()); n != 1 {
		t.Fatalf("an unregistered session was still delivered to (%d notifications)", n)
	}
	if n := s.extEvents.names("s-4"); len(n) != 0 {
		t.Fatalf("the subscription outlived its session: %v", n)
	}
}

// UNSUBSCRIBE IS THE OTHER HALF OF A TABLE THAT CAN BE WRITTEN: a client that stops
// caring must be able to say so, and `*` clears everything it had.
func TestUnsubscribeStopsDelivery(t *testing.T) {
	s := extPeerFixture(t, "peer")
	sess := &recordingSession{id: "s-5"}
	s.mcpServer.RegisterSession(context.Background(), sess)
	subscribeAs(t, s, sess, "peer")

	raw, _ := json.Marshal(map[string]any{"ext": "peer"})
	out, rpcErr := s.handleExtUnsubscribe(mcp.WithSessionContext(context.Background(), sess), raw)
	if rpcErr != nil {
		t.Fatalf("unsubscribe: %s", rpcErr.Message)
	}
	body, _ := json.Marshal(out)
	if !strings.Contains(string(body), `"subscribed":[]`) {
		t.Fatalf("unsubscribe answered %s, want an empty set", body)
	}
	s.publishExtEvent(context.Background(), "peer", "notifications/a", nil)
	if n := len(sess.got()); n != 0 {
		t.Fatalf("an unsubscribed session received %d notifications", n)
	}
}

// A NAME THAT IS NOT AN EXTENSION IS REFUSED WITH THE VOCABULARY, because the silent
// version of that typo is a subscription that never fires (the reason the old
// `shell_notify` mechanism was removed).
func TestSubscribingToAnUnknownExtensionIsRefused(t *testing.T) {
	s := extPeerFixture(t, "peer")
	sess := &recordingSession{id: "s-6"}
	s.mcpServer.RegisterSession(context.Background(), sess)

	raw, _ := json.Marshal(map[string]any{"ext": "pear"})
	_, rpcErr := s.handleExtSubscribe(mcp.WithSessionContext(context.Background(), sess), raw)
	if rpcErr == nil {
		t.Fatal("a misspelled extension name was accepted")
	}
	if !strings.Contains(rpcErr.Message, "peer") || !strings.Contains(rpcErr.Message, `"*"`) {
		t.Fatalf("the refusal does not name the extensions nor the wildcard: %s", rpcErr.Message)
	}

	// The wildcard is always legal, even with no extensions at all.
	if _, err := s.handleExtSubscribe(mcp.WithSessionContext(context.Background(), sess), json.RawMessage(`{"ext":"*"}`)); err != nil {
		t.Fatalf("the wildcard was refused: %s", err.Message)
	}
}

// A CLIENT THAT CANNOT TAKE THE FRAME IS COUNTED, not silently treated as delivered:
// "nothing happened" and "I was not reading" are different answers for a plugin.
func TestAFailedDeliveryIsCounted(t *testing.T) {
	s := extPeerFixture(t, "peer")
	sess := &recordingSession{id: "s-7", fail: true}
	subscribeAs(t, s, sess, "peer")

	before := s.extEvents.dropped.Load()
	s.publishExtEvent(context.Background(), "peer", "notifications/a", nil)
	if got := s.extEvents.dropped.Load(); got != before+1 {
		t.Fatalf("a failed delivery was not counted: dropped went %d → %d", before, got)
	}

	out, rpcErr := s.handleExtSubscriptions(mcp.WithSessionContext(context.Background(), sess), nil)
	if rpcErr != nil {
		t.Fatalf("subscriptions: %s", rpcErr.Message)
	}
	body, _ := json.Marshal(out)
	if !strings.Contains(string(body), `"dropped":1`) {
		t.Fatalf("the counters are not readable back: %s", body)
	}
}

// THE PUMP IS THE PRODUCER, and this is the one test that proves the wiring rather than
// the table: a real stdio peer says something on its own, and a session attached only to
// the MAIN endpoint — never to /ext/<name>/stream — receives it, wrapped.
//
// The peer emits on notifications/initialized rather than at startup, so the moment of
// the event is one the test chose: the pump's reader is registered before that frame is
// forwarded, which is the ordering the assertion depends on.
const chattyPeerStub = `
while IFS= read -r line; do
  case "$line" in
    *notifications/initialized*)
      printf '{"jsonrpc":"2.0","method":"notifications/acme/job","params":{"seq":1}}\n' ;;
    *initialize*)
      id=$(printf '%s' "$line" | sed -n 's/.*"id":\([^,}]*\).*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"chatty","version":"1"}}}\n' "$id" ;;
  esac
done
`

func TestThePumpDeliversToASubscriberOfTheMainEndpoint(t *testing.T) {
	s := newExtensionTestServer(t)
	script := writeScript(t, chattyPeerStub)
	callExtension(t, s, map[string]any{
		"action": "create", "name": "chatty", "target": "internal",
		"command": "sh", "args": []any{script}, "enabled": true,
	})
	useExtension(t, s, "chatty")

	sess := &recordingSession{id: "s-8"}
	subscribeAs(t, s, sess, "chatty")

	mux := http.NewServeMux()
	mux.Handle("/ext/{name}/stream", s.ExtMCPHandler())
	ts := httptest.NewServer(mux)
	defer ts.Close()
	url := ts.URL + "/ext/chatty/stream"

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"watcher","version":"1"}}}`
	resp, err := http.Post(url, "application/json", strings.NewReader(init))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	sid := resp.Header.Get("Mcp-Session-Id")

	if code := extPost(t, url, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, sid); code != http.StatusAccepted {
		t.Fatalf("notifications/initialized answered %d", code)
	}

	// A DEADLINE, NOT A BUDGET: the frame arrives when it arrives, and the poll returns
	// on the first observation, so a slow machine costs iterations rather than a delay.
	deadline := time.Now().Add(20 * time.Second)
	for {
		notes := sess.got()
		if len(notes) > 0 {
			if notes[0].method != extEventMethod {
				t.Fatalf("method = %q, want %q", notes[0].method, extEventMethod)
			}
			if notes[0].params["extension"] != "chatty" || notes[0].params["method"] != "notifications/acme/job" {
				t.Fatalf("the wrap lost the provenance: %v", notes[0].params)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the peer's notification never reached the main endpoint's subscriber")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
