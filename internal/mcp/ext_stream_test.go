package mcp

// The endpoint-level promises of /ext/{name}/stream (issues #234, #328).
//
// These tests drive the endpoint directly rather than over HTTP, because the HTTP
// layer is the streamable transport's own and already has its tests: what is new
// and worth pinning here is what "Handle" means for an extension — the four routes,
// the id rewriting, the cached initialize, and the fact that a request never comes
// back as 202.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcplib "github.com/open-mcp-ai/termcp/mcp"
)

// fakeChannel is an extension's line-delimited stream, in memory.
type fakeChannel struct {
	mu      sync.Mutex
	written []string
	inbound chan []byte
	closed  bool
}

func newFakeChannel() *fakeChannel {
	return &fakeChannel{inbound: make(chan []byte, 64)}
}

func (c *fakeChannel) Write(ctx context.Context, frame string) error {
	c.mu.Lock()
	c.written = append(c.written, frame)
	c.mu.Unlock()
	return nil
}

func (c *fakeChannel) Open(ctx context.Context) (extReader, error) {
	return &fakeReader{ch: c.inbound}, nil
}

func (c *fakeChannel) Touch() {}

func (c *fakeChannel) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

func (c *fakeChannel) take() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.written
	c.written = nil
	return out
}

type fakeReader struct{ ch chan []byte }

func (r *fakeReader) Read(ctx context.Context) ([]byte, error) {
	select {
	case b := <-r.ch:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *fakeReader) Close() {}

// extTestSession is one attached MCP client: it records what reached its stream.
type extTestSession struct {
	id       string
	mu       sync.Mutex
	received [][]byte
	notified chan struct{}
}

func newExtTestSession(id string) *extTestSession {
	return &extTestSession{id: id, notified: make(chan struct{}, 64)}
}

func (s *extTestSession) SessionID() string { return s.id }

func (s *extTestSession) Notify(ctx context.Context, method string, params any) error {
	frame, err := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	return s.NotifyRaw(ctx, frame)
}

func (s *extTestSession) NotifyRaw(ctx context.Context, frame []byte) error {
	s.mu.Lock()
	s.received = append(s.received, append([]byte(nil), frame...))
	s.mu.Unlock()
	select {
	case s.notified <- struct{}{}:
	default:
	}
	return nil
}

func (s *extTestSession) GetClientInfo() mcplib.Implementation {
	return mcplib.Implementation{Name: s.id}
}

func (s *extTestSession) frames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.received))
	for _, f := range s.received {
		out = append(out, string(f))
	}
	return out
}

// waitForFrame blocks until the session has received at least n frames.
func (s *extTestSession) waitForFrame(t *testing.T, n int, d time.Duration) []string {
	t.Helper()
	deadline := time.After(d)
	for {
		if got := s.frames(); len(got) >= n {
			return got
		}
		select {
		case <-s.notified:
		case <-deadline:
			t.Fatalf("session %s received %d frames, wanted %d", s.id, len(s.frames()), n)
		}
	}
}

// newTestEndpoint builds an endpoint wired to a fake channel, with a short reply
// window so a timeout test does not sleep a minute.
func newTestEndpoint(t *testing.T) (*extEndpoint, *fakeChannel) {
	t.Helper()
	s := &Server{}
	ep := newExtEndpoint(s, "acme")
	ch := newFakeChannel()
	ep.newChannel = func(ctx context.Context) (extChannel, error) { return ch, nil }
	old := extReplyWindow
	extReplyWindow = 300 * time.Millisecond
	t.Cleanup(func() { extReplyWindow = old })
	return ep, ch
}

// TestExtNotificationReachesAStreamWithNoPostInFlight is acceptance #1: an event the
// extension raises on its own must arrive on the GET stream while nothing is being
// POSTed. It is the whole reason this endpoint gained a pump.
func TestExtNotificationReachesAStreamWithNoPostInFlight(t *testing.T) {
	ep, ch := newTestEndpoint(t)
	sess := newExtTestSession("client-a")
	ep.RegisterSession(context.Background(), sess)
	defer ep.UnregisterSession(context.Background(), sess)

	// Nothing has been POSTed. The extension emits an event.
	ch.inbound <- []byte(`{"jsonrpc":"2.0","method":"notifications/acme/job","params":{"seq":7}}` + "\n")

	got := sess.waitForFrame(t, 1, time.Second)
	if !strings.Contains(got[0], `"notifications/acme/job"`) || !strings.Contains(got[0], `"seq":7`) {
		t.Fatalf("the event reached the stream changed: %s", got[0])
	}
}

// TestExtIdsAreRewrittenPerClient is acceptance #2: two clients that both number a
// request `1` must each get their own answer. The peer has one id space; the
// multiplexing is this endpoint's.
func TestExtIdsAreRewrittenPerClient(t *testing.T) {
	ep, ch := newTestEndpoint(t)
	a, b := newExtTestSession("a"), newExtTestSession("b")
	ep.RegisterSession(context.Background(), a)
	ep.RegisterSession(context.Background(), b)
	defer ep.UnregisterSession(context.Background(), a)
	defer ep.UnregisterSession(context.Background(), b)

	// Bring the channel up, which starts the pump, so replies are read.
	if _, err := ep.ensureChannel(context.Background()); err != nil {
		t.Fatalf("ensureChannel: %v", err)
	}

	type result struct {
		who   string
		frame string
	}
	done := make(chan result, 2)
	call := func(sess *extTestSession, payload string) {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + payload + `"}}`
		frame := ep.Handle(context.Background(), []byte(body), sess)
		done <- result{sess.SessionID(), string(frame)}
	}
	go call(a, "alpha")
	go call(b, "beta")

	// Read both upstream frames the endpoint typed, and answer them in reverse
	// order: replies arriving out of order is correct JSON-RPC.
	var upstream []string
	deadline := time.Now().Add(2 * time.Second)
	for len(upstream) < 2 && time.Now().Before(deadline) {
		upstream = append(upstream, ch.take()...)
		if len(upstream) < 2 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if len(upstream) != 2 {
		t.Fatalf("the peer saw %d frames, wanted 2", len(upstream))
	}
	if upstream[0] == upstream[1] {
		t.Fatalf("both clients' requests reached the peer with the same id: %s", upstream[0])
	}
	for i := len(upstream) - 1; i >= 0; i-- {
		id := frameID(upstream[i])
		ch.inbound <- []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"content":"ok"}}` + "\n")
	}

	got := map[string]string{}
	for i := 0; i < 2; i++ {
		select {
		case r := <-done:
			got[r.who] = r.frame
		case <-time.After(3 * time.Second):
			t.Fatal("a client did not get its answer")
		}
	}
	for _, who := range []string{"a", "b"} {
		if !strings.Contains(got[who], `"id":1`) {
			t.Errorf("client %s got a frame whose id is not its own: %s", who, got[who])
		}
		if !strings.Contains(got[who], `"result"`) {
			t.Errorf("client %s got no result: %s", who, got[who])
		}
	}
}

// TestExtUnansweredRequestIsAnErrorNot202 is acceptance #3, the part a status code
// can carry: a REQUEST that the peer does not answer is an error frame. A 202 would
// tell the client to go and wait on its GET stream for something that may never
// come, and the spec does not allow "accepted" for a request in the first place.
func TestExtUnansweredRequestIsAnErrorNot202(t *testing.T) {
	ep, _ := newTestEndpoint(t)
	sess := newExtTestSession("client")
	ep.RegisterSession(context.Background(), sess)
	defer ep.UnregisterSession(context.Background(), sess)

	answer := ep.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":42,"method":"tools/call","params":{}}`), sess)
	if answer == nil {
		t.Fatal("an unanswered request returned nil, which the transport reads as 202")
	}
	var probe struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(answer, &probe); err != nil {
		t.Fatalf("the answer is not JSON: %s", answer)
	}
	if string(probe.ID) != "42" {
		t.Errorf("the error must carry the client's own id, got %s", probe.ID)
	}
	if probe.Error.Code == 0 || probe.Error.Message == "" {
		t.Errorf("the error must say something: %s", answer)
	}
}

// TestExtInitializeIsForwardedOnceAndReplayed is the caching rule: the peer is one
// process, so its handshake is performed once and its answer is replayed — with each
// client's own id — to everyone after that.
func TestExtInitializeIsForwardedOnceAndReplayed(t *testing.T) {
	ep, ch := newTestEndpoint(t)
	first := newExtTestSession("first")
	ep.RegisterSession(context.Background(), first)

	body := `{"jsonrpc":"2.0","id":9,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"first"}}}`
	go func() {
		// Answer the upstream frame once it has been written.
		for {
			frames := ch.take()
			if len(frames) == 0 {
				time.Sleep(2 * time.Millisecond)
				continue
			}
			id := frameID(frames[len(frames)-1])
			ch.inbound <- []byte(`{"jsonrpc":"2.0","id":` + id + `,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"acme","version":"2.0"}}}` + "\n")
			return
		}
	}()
	answer := ep.Handle(context.Background(), []byte(body), first)
	if !strings.Contains(string(answer), `"serverInfo"`) {
		t.Fatalf("the first initialize should have reached the peer: %s", answer)
	}
	if got := ep.ProtocolVersion(); got != "2025-03-26" {
		t.Errorf("the endpoint should report the peer's version, got %q", got)
	}

	// A second client: the peer must not be asked again.
	second := newExtTestSession("second")
	ep.RegisterSession(context.Background(), second)
	before := len(ch.take())
	answer2 := ep.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":7,"method":"initialize","params":{}}`), second)
	if len(ch.take()) != 0 {
		t.Errorf("the second initialize was forwarded to the peer (wrote %d frames, had %d before)", len(ch.take()), before)
	}
	if !strings.Contains(string(answer2), `"serverInfo"`) {
		t.Fatalf("the replay should still be the peer's answer: %s", answer2)
	}
	if !strings.Contains(string(answer2), `"id":7`) {
		t.Errorf("the replay must carry the second client's id: %s", answer2)
	}
}

// TestExtPeerRequestWithNoCapableClientIsAnswered is the fourth route's promise: a
// request the peer makes is never dropped on the floor. With no client that declared
// anything, the peer gets an error it can act on.
func TestExtPeerRequestWithNoCapableClientIsAnswered(t *testing.T) {
	ep, ch := newTestEndpoint(t)
	sess := newExtTestSession("plain")
	ep.RegisterSession(context.Background(), sess)
	defer ep.UnregisterSession(context.Background(), sess)
	if _, err := ep.ensureChannel(context.Background()); err != nil {
		t.Fatalf("ensureChannel: %v", err)
	}

	ch.inbound <- []byte(`{"jsonrpc":"2.0","id":3,"method":"sampling/createMessage","params":{}}` + "\n")

	deadline := time.Now().Add(time.Second)
	for {
		frames := ch.take()
		if len(frames) > 0 {
			if !strings.Contains(frames[0], `"error"`) || !strings.Contains(frames[0], `"id":3`) {
				t.Fatalf("the peer should have been answered with an error: %s", frames[0])
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the peer was never told that nobody could answer")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestExtChannelEndingFailsWaitingRequests is acceptance #6: a restarted extension
// produces a readable error rather than a hang.
func TestExtChannelEndingFailsWaitingRequests(t *testing.T) {
	ep, _ := newTestEndpoint(t)
	sess := newExtTestSession("client")
	ep.RegisterSession(context.Background(), sess)
	defer ep.UnregisterSession(context.Background(), sess)
	if _, err := ep.ensureChannel(context.Background()); err != nil {
		t.Fatalf("ensureChannel: %v", err)
	}

	done := make(chan []byte, 1)
	go func() {
		done <- ep.Handle(context.Background(),
			[]byte(`{"jsonrpc":"2.0","id":"keep","method":"tools/call","params":{}}`), sess)
	}()
	time.Sleep(30 * time.Millisecond)
	ep.endChannel("the extension's process exited")

	select {
	case answer := <-done:
		if !strings.Contains(string(answer), "stopped") {
			t.Fatalf("the client should be told the extension stopped: %s", answer)
		}
		if !strings.Contains(string(answer), `"id":"keep"`) {
			t.Errorf("the error must carry the client's own id: %s", answer)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a request waited past the channel's death")
	}
}

// TestExtGetWithoutSessionNamesTheHeader is acceptance #5. The transport owns this,
// and the endpoint only works because it reuses it — so the test asserts it through
// the transport the endpoint actually mounts.
func TestExtGetWithoutSessionNamesTheHeader(t *testing.T) {
	s := &Server{}
	handler := s.ExtMCPHandler()

	req := httptest.NewRequest(http.MethodGet, "/ext/acme/stream", nil)
	req.SetPathValue("name", "acme")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a GET with no session should be 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Mcp-Session-Id") {
		t.Errorf("the 400 must name the missing header, got %q", w.Body.String())
	}
}

// TestExtPostToAnUnknownSessionIs404 pins the other half: a session id this
// endpoint never issued gets the "start over" signal, not a 400.
func TestExtPostToAnUnknownSessionIs500(t *testing.T) {
	s := &Server{}
	handler := s.ExtMCPHandler()

	req := httptest.NewRequest(http.MethodPost, "/ext/acme/stream",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.SetPathValue("name", "acme")
	req.Header.Set("Mcp-Session-Id", "never-issued")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("an unknown session should be 404, got %d: %s", w.Code, w.Body.String())
	}
}

// TestExtBatchFrameIsRefused: 2025-06-18 has no batch, and a batch has no single id
// to route. It is answered as invalid rather than fanned out element by element.
func TestExtBatchFrameIsRefused(t *testing.T) {
	ep, _ := newTestEndpoint(t)
	sess := newExtTestSession("client")
	ep.RegisterSession(context.Background(), sess)
	defer ep.UnregisterSession(context.Background(), sess)

	answer := ep.Handle(context.Background(),
		[]byte(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`), sess)
	if !strings.Contains(string(answer), "batch") {
		t.Fatalf("a batch should be refused by name: %s", answer)
	}
}

// TestRewriteFrameIDLeavesEverythingElseAlone is the byte-fidelity promise. The
// reply the client reads has to be the extension's own frame; only the id is ours.
func TestRewriteFrameIDLeavesEverythingElseAlone(t *testing.T) {
	if got := string(rewriteFrameIDMust(t, `{"jsonrpc":"2.0","id":1,"result":{"b":2,"a":1}}`, `"x"`)); got != `{"jsonrpc":"2.0","id":"x","result":{"b":2,"a":1}}` {
		t.Fatalf("rewrite changed more than the id: %s", got)
	}
	if got := string(rewriteFrameIDMust(t, `{"id":"keep","result":{"a":{"b":[1,2,{"c":"}"}]}},"jsonrpc":"2.0"}`, `7`)); got != `{"id":7,"result":{"a":{"b":[1,2,{"c":"}"}]}},"jsonrpc":"2.0"}` {
		t.Fatalf("rewrite mishandled a nested object or a reordered member: %s", got)
	}
	if got := string(rewriteFrameIDMust(t, `{"jsonrpc":"2.0","id":null,"error":{"code":-1}}`, `"y"`)); got != `{"jsonrpc":"2.0","id":"y","error":{"code":-1}}` {
		t.Fatalf("rewrite mishandled a null id: %s", got)
	}
}

func rewriteFrameIDMust(t *testing.T, frame, id string) []byte {
	t.Helper()
	out, ok := rewriteFrameID([]byte(frame), []byte(id))
	if !ok {
		t.Fatalf("could not find the id member in %s", frame)
	}
	return out
}

// TestNextFrameConsumesNoiseAndWaitsForPartials: the scanner must hand back whole
// frames while not swallowing a frame that is still arriving.
func TestNextFrameConsumesNoiseAndWaitsForPartials(t *testing.T) {
	buf := []byte("banner line\r\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/acme/x\"}\n{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":")
	frame, consume, needMore := nextFrame(buf)
	if string(frame) != `{"jsonrpc":"2.0","method":"notifications/acme/x"}` {
		t.Fatalf("the first frame was not extracted: %q", frame)
	}
	if needMore {
		t.Error("a complete frame followed by a partial must not report needMore before the partial is examined")
	}
	rest := buf[consume:]
	if frame, consume2, needMore := nextFrame(rest); frame != nil || !needMore {
		t.Fatalf("a partial frame should be kept, not returned: frame=%q consume=%d needMore=%v", frame, consume2, needMore)
	}
}
