package mcp

// The protocol gaps #327 closes at the transport and dispatch layer.
//
// These live in this package because what they exercise is private: the session
// table, the reaper's clock, the version check and the per-request progress sink are
// all inside the transport, and a test outside it could only observe the same facts
// through HTTP — slower, and blind to the state that makes them true.

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordingSession is a Session that records what reached it.
type recordingSession struct {
	id       string
	mu       sync.Mutex
	notified []string
}

func (s *recordingSession) SessionID() string { return s.id }

func (s *recordingSession) Notify(ctx context.Context, method string, params any) error {
	frame, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return err
	}
	return s.NotifyRaw(ctx, frame)
}

func (s *recordingSession) NotifyRaw(ctx context.Context, frame []byte) error {
	s.mu.Lock()
	s.notified = append(s.notified, string(frame))
	s.mu.Unlock()
	return nil
}

func (s *recordingSession) GetClientInfo() Implementation { return Implementation{Name: s.id} }

func (s *recordingSession) frames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.notified...)
}

// --- 1. notifications/cancelled ---

// TestCancelStopsTheCallItNames is #327 item 1: a client that cancels a long call must
// actually stop it. Before this the notification was dispatched into the "nothing to
// do" branch and the work ran to completion for nobody.
func TestCancelStopsTheCallItNames(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	var sawCancel atomic.Bool
	started := make(chan struct{})
	srv.AddTool(Tool{Name: "slow"}, func(ctx context.Context, req CallToolRequest) (*CallToolResult, error) {
		close(started)
		select {
		case <-ctx.Done():
			sawCancel.Store(true)
			return NewToolResultError("cancelled"), nil
		case <-time.After(5 * time.Second):
			return NewToolResultText("finished anyway"), nil
		}
	})

	sess := &recordingSession{id: "c"}
	done := make(chan []byte, 1)
	go func() {
		done <- srv.Handle(context.Background(),
			[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}`), sess)
	}()
	<-started

	srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`), sess)

	select {
	case <-done:
		if !sawCancel.Load() {
			t.Fatal("the tool's context was not cancelled")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the call did not stop after being cancelled")
	}
}

// TestCancelOfAnUnknownRequestIsIgnored is the spec's rule, and the only sane one: a
// client that cancels at the same moment the answer is written is racing.
func TestCancelOfAnUnknownRequestIsIgnored(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	sess := &recordingSession{id: "c"}
	if answer := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":999}}`), sess); answer != nil {
		t.Fatalf("a notification must not be answered, got %s", answer)
	}
}

// --- 2. session expiry ---

// TestSilentSessionIsReaped is #327 item 2. DELETE used to be the only way out, so a
// client that crashed kept its session and its fan-out slot for as long as the process
// ran.
func TestSilentSessionIsReaped(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	var unregistered atomic.Int32
	srv.hooks.OnUnregisterSession = append(srv.hooks.OnUnregisterSession,
		func(context.Context, Session) { unregistered.Add(1) })

	tr := NewStreamableHTTPServer(srv)
	sess := newStreamClient("s1")
	tr.sessions["s1"] = sess

	old := sessionIdleTimeout
	sessionIdleTimeout = time.Millisecond
	t.Cleanup(func() { sessionIdleTimeout = old })

	time.Sleep(2 * time.Millisecond)
	tr.reap(time.Now())

	tr.mu.Lock()
	_, still := tr.sessions["s1"]
	tr.mu.Unlock()
	if still {
		t.Fatal("a silent session survived the sweep")
	}
	if unregistered.Load() != 1 {
		t.Fatalf("the session was dropped without telling the server (hook fired %d times)", unregistered.Load())
	}
}

// TestSessionWithAnOpenStreamIsNotReaped: a GET is proof of life even when the client
// has nothing to say. Reaping it would close a working stream.
func TestSessionWithAnOpenStreamIsNotReaped(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	tr := NewStreamableHTTPServer(srv)
	sess := newStreamClient("s2")
	sess.streaming.Store(true)
	tr.sessions["s2"] = sess

	old := sessionIdleTimeout
	sessionIdleTimeout = time.Millisecond
	t.Cleanup(func() { sessionIdleTimeout = old })

	time.Sleep(2 * time.Millisecond)
	tr.reap(time.Now())

	tr.mu.Lock()
	_, still := tr.sessions["s2"]
	tr.mu.Unlock()
	if !still {
		t.Fatal("a session with an open GET stream was reaped")
	}
}

// TestTrafficKeepsASessionAlive: the window is measured from the last thing the
// session did, so a client that is posting is never reaped mid-conversation.
func TestTrafficKeepsASessionAlive(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	tr := NewStreamableHTTPServer(srv)
	sess := newStreamClient("s3")
	tr.sessions["s3"] = sess

	old := sessionIdleTimeout
	sessionIdleTimeout = 50 * time.Millisecond
	t.Cleanup(func() { sessionIdleTimeout = old })

	tr.reap(time.Now()) // nothing has gone by yet
	tr.mu.Lock()
	_, still := tr.sessions["s3"]
	tr.mu.Unlock()
	if !still {
		t.Fatal("a fresh session was reaped")
	}
}

// --- 3. MCP-Protocol-Version ---

// TestUnsupportedProtocolVersionIsRefused is #327 item 3: the header was written and
// never read, so two sides could interpret the same frames by different rules and the
// symptom never pointed at the version.
func TestUnsupportedProtocolVersionIsRefused(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	tr := NewStreamableHTTPServer(srv)
	handler := http.HandlerFunc(tr.handlePost)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/stream",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))
	sessionID := w.Header().Get(sessionHeader)
	if sessionID == "" {
		t.Fatalf("initialize did not create a session: %d %s", w.Code, w.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/stream",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set(versionHeader, "1999-01-01")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("an unsupported version should be 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), ProtocolVersion) {
		t.Errorf("the refusal must name the version this instance speaks, got %q", w.Body.String())
	}

	// The supported version still works.
	req = httptest.NewRequest(http.MethodPost, "/stream",
		strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set(versionHeader, ProtocolVersion)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("the supported version was refused: %d %s", w.Code, w.Body.String())
	}
}

// TestVersionHeaderMayBeOmitted: the header is optional in the spec, and a client that
// omits it has not claimed anything false.
func TestVersionHeaderMayBeOmitted(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	tr := NewStreamableHTTPServer(srv)
	handler := http.HandlerFunc(tr.handlePost)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/stream",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))
	sessionID := w.Header().Get(sessionHeader)

	req := httptest.NewRequest(http.MethodPost, "/stream",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	req.Header.Set(sessionHeader, sessionID)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("a missing version header should be accepted, got %d: %s", w.Code, w.Body.String())
	}
}

// TestInitializeIsNotRefusedOverAVersion: the handshake is where the version is
// negotiated, so requiring it there would refuse the one request that establishes it.
func TestInitializeIsNotRefusedOverAVersion(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	tr := NewStreamableHTTPServer(srv)
	handler := http.HandlerFunc(tr.handlePost)

	req := httptest.NewRequest(http.MethodPost, "/stream",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`))
	req.Header.Set(versionHeader, "2025-03-26")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize over another version should be answered, got %d: %s", w.Code, w.Body.String())
	}
}

// --- 5. progress ---

// TestProgressFramesRideTheRequestStream is #327 item 5's channel half: a call that
// asked for progress gets an event stream, and the frames arrive before the result.
func TestProgressFramesRideTheRequestStream(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	srv.AddTool(Tool{Name: "chatty"}, func(ctx context.Context, req CallToolRequest) (*CallToolResult, error) {
		total := 3.0
		for i := 1.0; i <= total; i++ {
			ReportProgress(ctx, i, &total, "step")
			time.Sleep(20 * time.Millisecond)
		}
		return NewToolResultText("done"), nil
	})

	tr := NewStreamableHTTPServer(srv)
	handler := http.HandlerFunc(tr.handlePost)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/stream",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))
	sessionID := w.Header().Get(sessionHeader)

	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"chatty","_meta":{"progressToken":"tok-1"}}}`
	req := httptest.NewRequest(http.MethodPost, "/stream", strings.NewReader(body))
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("a call with a progressToken should answer with a stream, got %q", ct)
	}
	var events []string
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			events = append(events, strings.TrimPrefix(line, "data: "))
		}
	}
	if len(events) < 2 {
		t.Fatalf("expected progress frames and a result, got %d event(s): %v", len(events), events)
	}
	var progressSeen int
	for i, ev := range events {
		if !strings.Contains(ev, "notifications/progress") {
			continue
		}
		progressSeen++
		if !strings.Contains(ev, `"tok-1"`) {
			t.Errorf("a progress frame must echo the client's own token: %s", ev)
		}
		if i == len(events)-1 {
			t.Error("the result must come after the progress frames, not before")
		}
	}
	if progressSeen == 0 {
		t.Fatalf("no progress frame was written: %v", events)
	}
	if !strings.Contains(events[len(events)-1], `"done"`) {
		t.Errorf("the last event should be the result: %s", events[len(events)-1])
	}
}

// TestNoProgressTokenMeansPlainJSON: the channel must not change the answer for a
// client that did not ask.
func TestNoProgressTokenMeansPlainJSON(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	srv.AddTool(Tool{Name: "plain"}, func(ctx context.Context, req CallToolRequest) (*CallToolResult, error) {
		return NewToolResultText("ok"), nil
	})
	tr := NewStreamableHTTPServer(srv)
	handler := http.HandlerFunc(tr.handlePost)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/stream",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)))
	sessionID := w.Header().Get(sessionHeader)

	req := httptest.NewRequest(http.MethodPost, "/stream",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"plain"}}`))
	req.Header.Set(sessionHeader, sessionID)
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("a call with no progressToken should answer JSON, got %q", ct)
	}
}

// TestReportProgressWithoutASinkIsFree: every tool calls it unconditionally, so the
// no-sink path must not panic and must report nothing.
func TestReportProgressWithoutASinkIsFree(t *testing.T) {
	total := 1.0
	ReportProgress(context.Background(), 0.5, &total, "nobody asked")
	if HasProgress(context.Background()) {
		t.Fatal("a bare context reported progress")
	}
}

// --- 6. title and output schema ---

// TestToolCarriesTitleAndOutputSchema is #327 item 6.
func TestToolCarriesTitleAndOutputSchema(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	srv.AddTool(NewTool("term",
		WithTitle("Terminal"),
		WithDescription("d"),
		WithOutputSchema(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
	), func(ctx context.Context, req CallToolRequest) (*CallToolResult, error) {
		return NewToolResultText("x"), nil
	})

	sess := &recordingSession{id: "c"}
	answer := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), sess)
	var res struct {
		Result ListToolsResult `json:"result"`
	}
	if err := json.Unmarshal(answer, &res); err != nil {
		t.Fatalf("tools/list answer is not JSON: %s", answer)
	}
	if len(res.Result.Tools) != 1 {
		t.Fatalf("expected one tool, got %d", len(res.Result.Tools))
	}
	tool := res.Result.Tools[0]
	if tool.Title != "Terminal" {
		t.Errorf("the tool lost its title: %q", tool.Title)
	}
	if len(tool.OutputSchema) == 0 || !strings.Contains(string(tool.OutputSchema), `"ok"`) {
		t.Errorf("the tool lost its output schema: %s", tool.OutputSchema)
	}
}

// TestInvalidOutputSchemaIsNotPublished: a schema that is not JSON would produce a
// tools/list no client can parse, so it is dropped rather than published broken.
func TestInvalidOutputSchemaIsNotPublished(t *testing.T) {
	tool := NewTool("x", WithOutputSchema("not json"))
	if len(tool.OutputSchema) != 0 {
		t.Fatalf("an invalid schema was published: %s", tool.OutputSchema)
	}
}

// --- 7. completion/complete ---

// TestCompletionCapabilityFollowsRegistration: the claim and the implementation cannot
// disagree. A server with no provider must not advertise completions.
func TestCompletionCapabilityFollowsRegistration(t *testing.T) {
	without := NewServer("t", "1", "", Hooks{})
	res := without.initialize(context.Background(), &InitializeRequest{}, &recordingSession{id: "c"})
	if res.Capabilities.Completions != nil {
		t.Error("a server with no completion handler advertised completions")
	}

	with := NewServer("t", "1", "", Hooks{})
	with.OnComplete(func(context.Context, json.RawMessage) (any, *RPCError) { return nil, nil })
	res = with.initialize(context.Background(), &InitializeRequest{}, &recordingSession{id: "c"})
	if res.Capabilities.Completions == nil {
		t.Error("a registered completion handler was not advertised")
	}
}

// TestCompletionMethodIsRefusedWhenUnregistered keeps the escape-hatch contract: an
// unregistered method is method-not-found, which is what a client should see from a
// server that does not offer it.
func TestCompletionMethodIsRefusedWhenUnregistered(t *testing.T) {
	srv := NewServer("t", "1", "", Hooks{})
	sess := &recordingSession{id: "c"}
	answer := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"completion/complete","params":{}}`), sess)
	if !strings.Contains(string(answer), "-32601") {
		t.Fatalf("expected method-not-found, got %s", answer)
	}
}
