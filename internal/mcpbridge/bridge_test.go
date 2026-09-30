package mcpbridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const initMessage = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"bridgetest","version":"0.0.1"}}}`

// syncBuffer tolerates writes from bridge goroutines that outlive Run (the
// failure paths return while a canceled forward is still unwinding).
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// harness drives one Run call over an io.Pipe.
type harness struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *syncBuffer
	errOut *syncBuffer
	result error
	dead   chan struct{}
	read   int // stdout bytes consumed by next()
}

func startBridge(t *testing.T, cfg Config) *harness {
	t.Helper()
	inR, inW := io.Pipe()
	h := &harness{t: t, in: inW, out: &syncBuffer{}, errOut: &syncBuffer{}, dead: make(chan struct{})}
	cfg.In, cfg.Out, cfg.Err = inR, h.out, h.errOut
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		h.result = Run(ctx, cfg)
		close(h.dead)
	}()
	t.Cleanup(func() {
		cancel()
		_ = inW.Close()
		select {
		case <-h.dead:
		case <-time.After(10 * time.Second):
			t.Error("bridge did not stop")
		}
	})
	return h
}

func (h *harness) send(msg string) {
	h.t.Helper()
	if _, err := io.WriteString(h.in, msg+"\n"); err != nil {
		h.t.Fatalf("send: %v", err)
	}
}

// next returns the next stdout message, failing the test if it is not valid
// JSON or does not arrive in time.
func (h *harness) next(timeout time.Duration) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		rest := h.out.String()[h.read:]
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line := rest[:i]
			h.read += i + 1
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				h.t.Fatalf("stdout line is not a JSON message: %q (%v)", line, err)
			}
			return m
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for a stdout message; unconsumed output: %q", rest)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitFor consumes stdout messages until one satisfies pred.
func (h *harness) waitFor(timeout time.Duration, pred func(map[string]any) bool) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if msg := h.next(time.Until(deadline)); pred(msg) {
			return msg
		}
	}
}

// waitDead blocks until Run returned and reports its result.
func (h *harness) waitDead(timeout time.Duration) error {
	h.t.Helper()
	select {
	case <-h.dead:
		return h.result
	case <-time.After(timeout):
		h.t.Fatal("bridge did not finish")
		return nil
	}
}

func hasID(n float64) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["id"] == n }
}

// newEchoServer returns a real mcp-go server with one echo tool.
func newEchoServer(t *testing.T) *mcpserver.MCPServer {
	t.Helper()
	srv := mcpserver.NewMCPServer("bridgetest", "0.0.1")
	srv.AddTool(
		mcpgo.NewTool("echo", mcpgo.WithString("text", mcpgo.Required())),
		func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			text, _ := req.GetArguments()["text"].(string)
			return mcpgo.NewToolResultText("echo:" + text), nil
		},
	)
	return srv
}

// newMCPEndpoint serves a real mcp-go streamable-HTTP server with one tool,
// and returns its /stream URL the way termcp mounts it.
func newMCPEndpoint(t *testing.T) (string, *mcpserver.MCPServer) {
	t.Helper()
	srv := newEchoServer(t)
	ts := httptest.NewServer(mcpserver.NewStreamableHTTPServer(srv))
	t.Cleanup(ts.Close)
	return ts.URL + "/stream", srv
}

// newSSEEndpoint serves a real mcp-go SSE server with one tool at the paths
// termcp mounts: GET /sse for the event stream, POST /message for JSON-RPC.
func newSSEEndpoint(t *testing.T) (string, *mcpserver.MCPServer) {
	t.Helper()
	srv := newEchoServer(t)
	sse := mcpserver.NewSSEServer(srv)
	mux := http.NewServeMux()
	mux.Handle("GET /sse", sse.SSEHandler())
	mux.Handle("POST /message", sse.MessageHandler())
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL + "/sse", srv
}

// An attached-but-silent bridge is a client presence: until it has an MCP
// session it pings the instance's version endpoint (so an idle countdown
// under it cannot fire), and stops once the session stream takes over.
func TestKeepAlivePingsUntilSessionStarts(t *testing.T) {
	srv := newEchoServer(t)
	stream := mcpserver.NewStreamableHTTPServer(srv)
	var hits atomic.Int64
	var unauthenticated atomic.Bool
	mux := http.NewServeMux()
	mux.Handle("/stream", stream)
	mux.HandleFunc("/api/version", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sekrit" {
			unauthenticated.Store(true)
		}
		hits.Add(1)
		_, _ = w.Write([]byte(`{"version":"test"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	h := startBridge(t, Config{URL: ts.URL + "/stream", Token: "sekrit", KeepAlive: 30 * time.Millisecond})

	// No stdin messages yet: only the pings prove the bridge is attached.
	deadline := time.Now().Add(3 * time.Second)
	for hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := hits.Load(); got < 2 {
		t.Fatalf("keep-alive pings never arrived (hits=%d)", got)
	}
	if unauthenticated.Load() {
		t.Error("keep-alive ping did not carry the bearer credential")
	}

	// initialize mints a session; the listening GET stream is the presence now.
	h.send(initMessage)
	h.waitFor(5*time.Second, hasID(1))
	time.Sleep(100 * time.Millisecond) // let any in-flight ping land
	before := hits.Load()
	time.Sleep(200 * time.Millisecond)
	if after := hits.Load(); after != before {
		t.Errorf("pings continued after the session started: %d -> %d", before, after)
	}
}

func TestRunRejectsEmptyURL(t *testing.T) {
	err := Run(context.Background(), Config{In: strings.NewReader(""), Out: io.Discard})
	if err == nil {
		t.Fatal("Run accepted an empty endpoint URL")
	}
}

// The full happy path against the real mcp-go stack: initialize (session id
// capture), a 202 notification with no stdout output, tools/list and a tool
// call — then stdin EOF shutting the bridge down cleanly.
func TestRoundTripOverRealStack(t *testing.T) {
	baseURL, _ := newMCPEndpoint(t)
	h := startBridge(t, Config{URL: baseURL})

	h.send(initMessage)
	initResp := h.waitFor(5*time.Second, hasID(1))
	result, _ := initResp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("initialize produced no result: %v", initResp)
	}
	if name := result["serverInfo"].(map[string]any)["name"]; name != "bridgetest" {
		t.Errorf("serverInfo.name = %v, want bridgetest", name)
	}

	// notifications/initialized answers 202 and must produce no stdout line:
	// the next message read is the tools/list reply.
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	h.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	toolsResp := h.next(5 * time.Second)
	if toolsResp["id"] != float64(2) {
		t.Fatalf("message after the 202 carried the wrong id: %v", toolsResp)
	}
	tools, _ := toolsResp["result"].(map[string]any)["tools"].([]any)
	if len(tools) == 0 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools/list did not return the echo tool: %v", toolsResp)
	}

	h.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	callResp := h.waitFor(5*time.Second, hasID(3))
	content, _ := callResp["result"].(map[string]any)["content"].([]any)
	if len(content) == 0 || content[0].(map[string]any)["text"] != "echo:hi" {
		t.Fatalf("tools/call returned %v", callResp)
	}

	// Clean shutdown on stdin EOF.
	_ = h.in.Close()
	if err := h.waitDead(5 * time.Second); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}

	for line := range strings.SplitSeq(h.out.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("stdout carries a non-message line: %q", line)
		}
	}
}

// A pipelined batch — a script writing initialize, the initialized
// notification and a request back to back — must work end to end. The bridge
// holds the batch in order until the initialize reply has supplied the session
// id every later POST needs. (Regression: the follow-ups used to race it and
// were rejected with "Invalid session ID", and the bridge exited.)
func TestPipelinedBatchOverRealStack(t *testing.T) {
	baseURL, _ := newMCPEndpoint(t)
	h := startBridge(t, Config{URL: baseURL})

	batch := initMessage + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	if _, err := io.WriteString(h.in, batch); err != nil {
		t.Fatalf("write batch: %v", err)
	}
	_ = h.in.Close()

	initResp := h.waitFor(5*time.Second, hasID(1))
	if initResp["result"] == nil {
		t.Fatalf("initialize produced no result: %v", initResp)
	}
	toolsResp := h.waitFor(5*time.Second, hasID(2))
	tools, _ := toolsResp["result"].(map[string]any)["tools"].([]any)
	if len(tools) == 0 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools/list did not return the echo tool: %v", toolsResp)
	}

	if err := h.waitDead(5 * time.Second); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
	if stderr := h.errOut.String(); stderr != "" {
		t.Errorf("a clean piped batch wrote diagnostics: %q", stderr)
	}
}

// A caller that piped its input and closed the pipe still gets the replies:
// stdin EOF must not cut off messages already in flight. (Regression: the
// bridge used to cancel pending POSTs on EOF, so `echo initialize | termcp
// stdio` printed nothing.)
func TestStdinEOFStillDeliversInFlightReplies(t *testing.T) {
	baseURL, _ := newMCPEndpoint(t)
	h := startBridge(t, Config{URL: baseURL})

	h.send(initMessage)
	_ = h.in.Close()

	msg := h.next(5 * time.Second)
	if msg["id"] != float64(1) {
		t.Fatalf("reply to the piped initialize = %v", msg)
	}
	if err := h.waitDead(5 * time.Second); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
}

// A server broadcast reaches the stdio client — through the listening GET
// stream, or drained into the next POST reply (which then arrives as SSE).
func TestServerNotificationsReachClient(t *testing.T) {
	baseURL, mcpSrv := newMCPEndpoint(t)
	h := startBridge(t, Config{URL: baseURL})

	h.send(initMessage)
	h.waitFor(5*time.Second, hasID(1))

	mcpSrv.SendNotificationToAllClients("notifications/files/updated", map[string]any{"uri": "test://watch"})
	h.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	var sawNotification, sawReply bool
	deadline := time.Now().Add(5 * time.Second)
	for !sawNotification || !sawReply {
		msg := h.next(time.Until(deadline))
		switch {
		case msg["method"] == "notifications/files/updated":
			sawNotification = true
		case msg["id"] == float64(2):
			sawReply = true
		}
	}

	_ = h.in.Close()
	if err := h.waitDead(5 * time.Second); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
}

// Once a session has been upgraded to SSE, POST replies arrive as
// text/event-stream frames; each frame must surface as one stdout message.
func TestSSEPostRepliesAreRelayed(t *testing.T) {
	reply := `{"jsonrpc":"2.0","id":7,"result":{"ok":true}}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\n")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", reply)
			w.(http.Flusher).Flush()
		}
	}))
	t.Cleanup(ts.Close)

	h := startBridge(t, Config{URL: ts.URL})
	h.send(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`)

	notification := h.next(5 * time.Second)
	if notification["method"] != "notifications/message" {
		t.Fatalf("first frame relayed as %v", notification)
	}
	resp := h.next(5 * time.Second)
	if resp["id"] != float64(7) || resp["result"].(map[string]any)["ok"] != true {
		t.Fatalf("second frame relayed as %v", resp)
	}

	_ = h.in.Close()
	if err := h.waitDead(5 * time.Second); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
}

// A rejected request must answer the client with a JSON-RPC error carrying the
// original id, land the status on stderr, and end the bridge.
func TestHTTPErrorIsTerminal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing or invalid token", http.StatusUnauthorized)
	}))
	t.Cleanup(ts.Close)

	h := startBridge(t, Config{URL: ts.URL})
	h.send(initMessage)

	resp := h.waitFor(5*time.Second, hasID(1))
	if resp["error"] == nil {
		t.Fatalf("no JSON-RPC error for the rejected request: %v", resp)
	}
	if err := h.waitDead(5 * time.Second); err == nil {
		t.Fatal("Run returned nil after a 401")
	}
	if stderr := h.errOut.String(); !strings.Contains(stderr, "401") {
		t.Errorf("stderr does not mention the status: %q", stderr)
	}

	// A notification has no id, so it must produce no stdout message at all.
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	time.Sleep(100 * time.Millisecond)
	lines := 0
	for line := range strings.SplitSeq(h.out.String(), "\n") {
		if strings.TrimSpace(line) != "" {
			lines++
		}
	}
	if lines != 1 {
		t.Errorf("stdout carries %d messages, want only the error response: %q", lines, h.out.String())
	}
}

// An endpoint that is not listening must fail on the first message.
func TestUnreachableEndpointIsTerminal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := ts.URL
	ts.Close()

	h := startBridge(t, Config{URL: baseURL + "/stream"})
	h.send(initMessage)

	resp := h.waitFor(5*time.Second, hasID(1))
	if resp["error"] == nil {
		t.Fatalf("no JSON-RPC error for the failed request: %v", resp)
	}
	if err := h.waitDead(5 * time.Second); err == nil {
		t.Fatal("Run returned nil for an unreachable endpoint")
	}
	if stderr := h.errOut.String(); !strings.Contains(stderr, "/stream") {
		t.Errorf("stderr does not name the failing call: %q", stderr)
	}
}

// The SSE transport happy path against the real mcp-go stack: the bridge opens
// GET /sse, learns the message endpoint from its `endpoint` event, relays
// initialize / tools/list / a tool call, and shuts down cleanly on stdin EOF.
func TestSSERoundTripOverRealStack(t *testing.T) {
	sseURL, _ := newSSEEndpoint(t)
	h := startBridge(t, Config{URL: sseURL, SSE: true})

	h.send(initMessage)
	initResp := h.waitFor(5*time.Second, hasID(1))
	result, _ := initResp["result"].(map[string]any)
	if result == nil {
		t.Fatalf("initialize produced no result: %v", initResp)
	}

	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	h.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	toolsResp := h.waitFor(5*time.Second, hasID(2))
	tools, _ := toolsResp["result"].(map[string]any)["tools"].([]any)
	if len(tools) == 0 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools/list did not return the echo tool: %v", toolsResp)
	}

	h.send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`)
	callResp := h.waitFor(5*time.Second, hasID(3))
	content, _ := callResp["result"].(map[string]any)["content"].([]any)
	if len(content) == 0 || content[0].(map[string]any)["text"] != "echo:hi" {
		t.Fatalf("tools/call returned %v", callResp)
	}

	// Clean shutdown on stdin EOF.
	_ = h.in.Close()
	if err := h.waitDead(5 * time.Second); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}

	for line := range strings.SplitSeq(h.out.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Errorf("stdout carries a non-message line: %q", line)
		}
	}
}

// A pipelined batch over the SSE transport must deliver its replies even
// though stdin hits EOF while they are still on their way: SSE replies arrive
// on the event stream, decoupled from POST acceptance, so the bridge waits for
// every owed reply before tearing the stream down. (Regression: closing stdin
// used to cancel the stream immediately and the batch produced no output.)
func TestSSEPipelinedBatchOverRealStack(t *testing.T) {
	sseURL, _ := newSSEEndpoint(t)
	h := startBridge(t, Config{URL: sseURL, SSE: true})

	batch := initMessage + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n"
	if _, err := io.WriteString(h.in, batch); err != nil {
		t.Fatalf("write batch: %v", err)
	}
	_ = h.in.Close()

	// Both requests are in flight concurrently, so their replies may land in
	// either order; collect them by id instead of assuming a sequence.
	replies := make(map[float64]map[string]any)
	deadline := time.Now().Add(5 * time.Second)
	for len(replies) < 2 {
		msg := h.next(time.Until(deadline))
		if id, ok := msg["id"].(float64); ok && (id == 1 || id == 2) {
			replies[id] = msg
		}
	}
	initResp, toolsResp := replies[1], replies[2]
	if initResp["result"] == nil {
		t.Fatalf("initialize produced no result: %v", initResp)
	}
	tools, _ := toolsResp["result"].(map[string]any)["tools"].([]any)
	if len(tools) == 0 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools/list did not return the echo tool: %v", toolsResp)
	}

	if err := h.waitDead(5 * time.Second); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
	if stderr := h.errOut.String(); stderr != "" {
		t.Errorf("a clean piped batch wrote diagnostics: %q", stderr)
	}
}

// A server broadcast over the SSE transport reaches the stdio client through
// the same event stream that carries the replies.
func TestSSEServerNotificationsReachClient(t *testing.T) {
	sseURL, mcpSrv := newSSEEndpoint(t)
	h := startBridge(t, Config{URL: sseURL, SSE: true})

	h.send(initMessage)
	h.waitFor(5*time.Second, hasID(1))

	mcpSrv.SendNotificationToAllClients("notifications/files/updated", map[string]any{"uri": "test://watch"})
	h.waitFor(5*time.Second, func(m map[string]any) bool {
		return m["method"] == "notifications/files/updated"
	})

	_ = h.in.Close()
	if err := h.waitDead(5 * time.Second); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
}

// A rejected SSE stream must end the bridge with a diagnostic naming the
// status, before any stdin is read.
func TestSSEUnauthorizedIsTerminal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "missing or invalid token", http.StatusUnauthorized)
	}))
	t.Cleanup(ts.Close)

	h := startBridge(t, Config{URL: ts.URL + "/sse", SSE: true})
	if err := h.waitDead(5 * time.Second); err == nil {
		t.Fatal("Run returned nil after a 401")
	}
	if stderr := h.errOut.String(); !strings.Contains(stderr, "401") {
		t.Errorf("stderr does not mention the status: %q", stderr)
	}
}

// An SSE endpoint that is not listening fails at startup, before any stdin.
func TestSSEUnreachableEndpointIsTerminal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := ts.URL
	ts.Close()

	h := startBridge(t, Config{URL: baseURL + "/sse", SSE: true})
	if err := h.waitDead(5 * time.Second); err == nil {
		t.Fatal("Run returned nil for an unreachable endpoint")
	}
	if stderr := h.errOut.String(); !strings.Contains(stderr, "GET") {
		t.Errorf("stderr does not name the failing call: %q", stderr)
	}
}
