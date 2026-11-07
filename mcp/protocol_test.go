package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The protocol rules this package is responsible for, each stated as the failure
// it prevents. A test here is worth writing only if breaking the rule would be
// invisible at a glance — an id echoed wrongly, a notification answered, a session
// not carried between requests.

func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv := NewServer("termcp", "test", "instructions", Hooks{})
	srv.AddTool(NewTool("term",
		WithDescription("run a command line"),
		WithString("cmd", Description("the line")),
		WithRequired("cmd"),
	), func(ctx context.Context, req CallToolRequest) (*CallToolResult, error) {
		cmd, _ := req.GetArguments()["cmd"].(string)
		return NewToolResultText("ran: " + cmd), nil
	})
	srv.AddPrompt(NewPrompt("learn", WithPromptDescription("playbook")),
		func(ctx context.Context, req GetPromptRequest) (*GetPromptResult, error) {
			return &GetPromptResult{
				Messages: []PromptMessage{
					{Role: RoleUser, Content: NewTextContent("read the docs")},
				},
			}, nil
		})
	return srv
}

// TestAnAnswerCarriesTheIDItWasAskedWith is the one thing a client cannot recover
// from: a mismatched id leaves the caller waiting for an answer that already came.
func TestAnAnswerCarriesTheIDItWasAskedWith(t *testing.T) {
	srv := newTestServer(t)
	for _, id := range []string{`7`, `"abc"`, `0`} {
		out := srv.Handle(context.Background(),
			[]byte(`{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`), nil)
		var res Response
		if err := json.Unmarshal(out, &res); err != nil {
			t.Fatalf("id %s: %v", id, err)
		}
		if got, want := string(res.ID), id; got != want {
			t.Errorf("id echoed as %s, asked with %s", got, want)
		}
		if res.Error != nil {
			t.Errorf("id %s: ping answered with error %v", id, res.Error)
		}
	}
}

// TestANotificationIsNeverAnswered: a frame with no id gets no frame back. Sending
// one is a protocol violation, and a server that does it desynchronises clients
// that pair answers to requests.
func TestANotificationIsNeverAnswered(t *testing.T) {
	srv := newTestServer(t)
	out := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), nil)
	if out != nil {
		t.Errorf("a notification was answered with %s", out)
	}
}

// TestAnUnknownMethodIsAnErrorNotASilentNoOp: silence reads as "the server is
// broken" to a client that mistyped a method.
func TestAnUnknownMethodIsAnErrorNotASilentNoOp(t *testing.T) {
	srv := newTestServer(t)
	out := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/nope"}`), nil)
	var res Response
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Error == nil || res.Error.Code != CodeMethodNotFound {
		t.Fatalf("unknown method answered with %+v, want code %d", res.Error, CodeMethodNotFound)
	}
}

// TestInitializeAdvertisesWhatIsRegistered: a capability claimed but not served
// makes a client call something that is not there.
func TestInitializeAdvertisesWhatIsRegistered(t *testing.T) {
	srv := newTestServer(t)
	out := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"c","version":"1"}}}`), nil)
	var res struct {
		Result InitializeResult `json:"result"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Result.Capabilities.Tools == nil {
		t.Error("a registered tool is not advertised in capabilities")
	}
	if res.Result.Capabilities.Prompts == nil {
		t.Error("a registered prompt is not advertised in capabilities")
	}
	if res.Result.ServerInfo.Name != "termcp" || res.Result.ServerInfo.Version != "test" {
		t.Errorf("serverInfo is %+v", res.Result.ServerInfo)
	}
}

// TestAnEmptyRegistryClaimsNoCapability is the same rule from the other side: with
// nothing registered, claiming tools would invite a call that cannot be answered.
func TestAnEmptyRegistryClaimsNoCapability(t *testing.T) {
	srv := NewServer("termcp", "test", "", Hooks{})
	out := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`), nil)
	var res struct {
		Result InitializeResult `json:"result"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Result.Capabilities.Tools != nil {
		t.Error("tools claimed with no tool registered")
	}
}

// TestToolArgumentsThatAreNotAnObjectAreNilNotAPanic: arguments arrive as whatever
// the client encoded, and a handler that assumed a map would panic on the request.
func TestToolArgumentsThatAreNotAnObjectAreNilNotAPanic(t *testing.T) {
	srv := newTestServer(t)
	out := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"term","arguments":[1,2]}}`), nil)
	var res Response
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Error != nil {
		t.Fatalf("a non-object argument list failed the call: %v", res.Error)
	}
}

// TestAnUnknownToolIsAnError: calling a tool that is not registered must say so,
// not answer with an empty success.
func TestAnUnknownToolIsAnError(t *testing.T) {
	srv := newTestServer(t)
	out := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope","arguments":{}}}`), nil)
	var res Response
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Error == nil || res.Error.Code != CodeInvalidParams {
		t.Fatalf("unknown tool answered with %+v", res.Error)
	}
}

// TestAPanickingHandlerAnswersAnError: one bad call must not take down the process
// and every attached session with it.
func TestAPanickingHandlerAnswersAnError(t *testing.T) {
	srv := NewServer("termcp", "test", "", Hooks{})
	srv.AddTool(NewTool("boom"), func(ctx context.Context, req CallToolRequest) (*CallToolResult, error) {
		panic("handler blew up")
	})
	out := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"boom","arguments":{}}}`), nil)
	if len(out) == 0 {
		t.Fatal("a panicking handler produced no answer at all")
	}
	var res Response
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("a panicking handler produced an unparseable answer: %s", out)
	}
}

// TestToolListOrderIsStable: a map iterated twice would produce two listings, and
// a client that diffs them reports phantom changes.
func TestToolListOrderIsStable(t *testing.T) {
	srv := NewServer("termcp", "test", "", Hooks{})
	for _, name := range []string{"zebra", "alpha", "middle"} {
		srv.AddTool(NewTool(name), func(ctx context.Context, req CallToolRequest) (*CallToolResult, error) {
			return NewToolResultText(""), nil
		})
	}
	var first string
	for i := 0; i < 20; i++ {
		out := srv.Handle(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), nil)
		if first == "" {
			first = string(out)
			continue
		}
		if string(out) != first {
			t.Fatalf("listing %d differs:\n%s\n%s", i, first, out)
		}
	}
}

// --- transports ---

// TestStreamableHTTPCarriesTheSessionBetweenRequests: the session lives in a
// header, so a transport that forgets to echo it makes the second call fail.
func TestStreamableHTTPCarriesTheSessionBetweenRequests(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(NewStreamableHTTPServer(srv))
	defer ts.Close()

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"c","version":"1"}}}`
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(init))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize answered %d", resp.StatusCode)
	}
	sessionID := resp.Header.Get(sessionHeader)
	if sessionID == "" {
		t.Fatal("initialize did not hand out a session id")
	}

	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"term","arguments":{"cmd":"ls session"}}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(call))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(sessionHeader, sessionID)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("tools/call with the session answered %d", resp2.StatusCode)
	}
	var res Response
	if err := json.NewDecoder(resp2.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(res.Result)
	if !strings.Contains(string(body), "ran: ls session") {
		t.Fatalf("the tool answer did not reach the client: %s", body)
	}
}

// TestStreamableHTTPRefusesACallWithNoSession: without this a caller could skip
// the handshake and get answers from a server that never learned who it is.
func TestStreamableHTTPRefusesACallWithNoSession(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(NewStreamableHTTPServer(srv))
	defer ts.Close()

	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"term","arguments":{"cmd":"x"}}}`
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(call))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a call with no session was answered as if it had one")
	}
}

// TestStreamableHTTPNotificationIsAcceptedWithNoBody: a notification is 202 with
// nothing to read, and a client that parsed the 202 as a result would find nothing.
func TestStreamableHTTPNotificationIsAcceptedWithNoBody(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(NewStreamableHTTPServer(srv))
	defer ts.Close()

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`
	resp, _ := http.Post(ts.URL, "application/json", strings.NewReader(init))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	sessionID := resp.Header.Get(sessionHeader)

	req, _ := http.NewRequest(http.MethodPost, ts.URL,
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	req.Header.Set(sessionHeader, sessionID)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("notification answered %d, want 202", resp2.StatusCode)
	}
	body, _ := io.ReadAll(resp2.Body)
	if len(body) != 0 {
		t.Errorf("notification answered with a body: %s", body)
	}
}

// TestStreamableHTTPRejectsAnUnknownVerb: the spec allows exactly POST/GET/DELETE,
// and a client sending anything else should be told which.
func TestStreamableHTTPRejectsAnUnknownVerb(t *testing.T) {
	srv := newTestServer(t)
	ts := httptest.NewServer(NewStreamableHTTPServer(srv))
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPut, ts.URL, strings.NewReader("{}"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PUT answered %d, want 405", resp.StatusCode)
	}
	if allow := resp.Header.Get("Allow"); !strings.Contains(allow, "POST") {
		t.Errorf("405 did not say what is allowed: %q", allow)
	}
}

// TestSSEDeliversTheEndpointBeforeAnythingElse: the endpoint event is how a client
// learns where to POST, so a stream that omits it leaves the client with no way to
// ask anything.
func TestSSEDeliversTheEndpointBeforeAnythingElse(t *testing.T) {
	srv := newTestServer(t)
	sse := NewSSEServer(srv)
	ts := httptest.NewServer(sse.SSEHandler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type is %q", ct)
	}

	// Read until the endpoint event arrives. It is the FIRST event, but the
	// deadline is a hang detector rather than a budget: a slow machine costs extra
	// reads, not extra wall time.
	buf := make([]byte, 4096)
	var seen strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		seen.Write(buf[:n])
		if strings.Contains(seen.String(), "event: endpoint") {
			break
		}
		if err != nil {
			t.Fatalf("stream ended before the endpoint event: %v (saw %q)", err, seen.String())
		}
	}
	if !strings.Contains(seen.String(), "event: endpoint") {
		t.Fatalf("no endpoint event within the deadline; saw %q", seen.String())
	}
	if !strings.Contains(seen.String(), sseSessionParam+"=") {
		t.Errorf("endpoint event carries no session id: %q", seen.String())
	}
}

// TestSSEMessageAnswersOnTheStream: a POST /message answers 202 and the real answer
// arrives on the GET stream. A client that read the 202 as the answer would see
// nothing.
func TestSSEMessageAnswersOnTheStream(t *testing.T) {
	srv := newTestServer(t)
	sse := NewSSEServer(srv)
	mux := http.NewServeMux()
	mux.Handle("/sse", sse.SSEHandler())
	mux.Handle("/message", sse.MessageHandler())
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// The endpoint event names the POST target, which carries the session id.
	buf := make([]byte, 4096)
	var seen strings.Builder
	var endpoint string
	scanDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(scanDeadline) {
		n, _ := resp.Body.Read(buf)
		seen.Write(buf[:n])
		for _, line := range strings.Split(seen.String(), "\n") {
			if strings.HasPrefix(line, "data: /message?") {
				endpoint = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
				break
			}
		}
		if endpoint != "" {
			break
		}
	}
	if endpoint == "" {
		t.Fatalf("no endpoint event; saw %q", seen.String())
	}

	post, _ := http.NewRequest(http.MethodPost, ts.URL+endpoint,
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	postResp, err := http.DefaultClient.Do(post)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, postResp.Body)
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /message answered %d, want 202", postResp.StatusCode)
	}

	// The answer is on the stream. Poll reads until it appears; the deadline only
	// bounds a failure.
	var rest strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		rest.Write(buf[:n])
		if strings.Contains(rest.String(), `"id":1`) {
			break
		}
		if err != nil {
			break
		}
	}
	if !strings.Contains(rest.String(), `"id":1`) {
		t.Fatalf("the answer never reached the stream; saw %q", rest.String())
	}
}

// TestTheEscapeHatchMakesAMethodReachable is the other half of "unused code costs
// nothing": an unregistered method is refused, and REGISTERING it is what makes it
// answer. That order is the whole reason this package models only the three
// surfaces termcp uses — a surface nobody registers never enters the binary.
func TestTheEscapeHatchMakesAMethodReachable(t *testing.T) {
	srv := newTestServer(t)

	// Unregistered: refused, and refused with the code a client can act on.
	out := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`), nil)
	var res Response
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Error == nil || res.Error.Code != CodeMethodNotFound {
		t.Fatalf("an unregistered method was not refused with method-not-found: %s", out)
	}

	// Registered: answered by the handler, not by this package.
	srv.OnMethod("resources/list", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		return map[string]any{"resources": []any{}}, nil
	})
	out = srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"resources/list"}`), nil)
	// A FRESH struct: json.Unmarshal into an existing one leaves fields the new
	// document omits untouched, so reusing `res` would carry the previous error
	// into a successful answer.
	var ok Response
	if err := json.Unmarshal(out, &ok); err != nil {
		t.Fatal(err)
	}
	if ok.Error != nil {
		t.Fatalf("a registered method was still refused: %+v", ok.Error)
	}
	body, _ := json.Marshal(ok.Result)
	if !strings.Contains(string(body), "resources") {
		t.Fatalf("the registered handler's result did not reach the client: %s", body)
	}
}

// TestAnInboundHandlerCanReturnAnError: the escape hatch must be able to refuse a
// call it registered, or a surface would be all-or-nothing per method name.
func TestAnInboundHandlerCanReturnAnError(t *testing.T) {
	srv := newTestServer(t)
	srv.OnMethod("elicitation/create", func(ctx context.Context, params json.RawMessage) (any, *RPCError) {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "no user to ask"}
	})
	out := srv.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"elicitation/create"}`), nil)
	var res Response
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Error == nil || res.Error.Message != "no user to ask" {
		t.Fatalf("the handler's own error did not reach the client: %s", out)
	}
}
