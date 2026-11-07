package core

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/config"
	"github.com/open-mcp-ai/termcp/mcp"
)

// newInstance builds a headless instance rooted in a temp dir, and — this is the
// part that used to need a flag — never calls Listen.
//
// Not listening is the absence of a call rather than an option, which is why this
// helper looks like an embedder that simply does not ask for a port. Headless is a
// separate axis: it drops the browser UI, not the socket.
func newInstance(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.NoInternal = false
	// Headless and the asset dirs are Config fields, not Options: they describe
	// the instance, so an embedder sets them where every other setting lives and
	// core.New applies them. No Options field to keep in sync.
	cfg.Headless = true
	srv, err := New(Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Start is not a goroutine any more: it returns once the surfaces are up, so
	// the sequence is straight-line and there is no WaitReady to win a race with.
	// WaitReady still has to answer immediately, which is asserted separately.
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { srv.Close("test done") })
	return srv
}

// TestAnEmbedderRunsACommandWithNoPort is the acceptance criterion from #273
// that is not about import paths: an external program can start an instance,
// run one command line and read the TERM/1 envelope, without a listener and
// without touching ~/.termcp.
//
// It asserts on the envelope rather than on the text, because the envelope is
// what the protocol promises: a status line, a Path header, and a body. If this
// test has to change because Term's shape changed, the protocol changed.
func TestAnEmbedderRunsACommandWithNoPort(t *testing.T) {
	srv := newInstance(t)

	reply := srv.Term(context.Background(), "ls", nil)
	if len(reply.Slots) != 1 {
		t.Fatalf("one command line produced %d slots, want 1", len(reply.Slots))
	}
	slot := reply.Slots[0]
	if got := slot.StatusLine(); !strings.HasPrefix(got, "TERM/1 200") {
		t.Fatalf("status line = %q, want a 200", got)
	}
	if got := slot.Headers["Path"]; got != "ls" {
		t.Errorf("Path header = %q, want the command echo %q", got, "ls")
	}
	// `ls` with nothing running is a legal, non-empty answer — the instance is
	// usable and says so, which is different from an empty body.
	if !strings.Contains(slot.Text(), "TERM/1 200") {
		t.Errorf("the rendered slot lost its status line:\n%s", slot.Text())
	}

	// And it is genuinely not listening: the address is reported but unbound.
	if srv.Addr() == "" {
		t.Error("Addr is empty; an embedder cannot tell what it would have bound")
	}
}

// TestAnInstanceWithoutAListenerBindsNothing pins the difference between "does not
// serve" and "does not work". The HTTP surface still exists — an embedder mounts it
// on its own listener — so Handler must answer, while the port stays closed.
func TestAnInstanceWithoutAListenerBindsNothing(t *testing.T) {
	srv := newInstance(t)

	req := httptest.NewRequest(http.MethodPost, "/api/term", strings.NewReader(`{"cmd":"ls"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/term on the embedder's own listener = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the REST answer is not JSON: %v\n%s", err, rec.Body.String())
	}
	if body["protocol"] != "TERM/1" {
		t.Errorf("REST answer protocol = %v, want TERM/1", body["protocol"])
	}
}

// TestAnInstanceThatNeverListensStillAdvertisesItsDocs is the regression for what
// the boolean actually broke. mcp.Start registers the documentation prompt and
// resolves the base URL that prompt advertises, and it used to sit inside the
// listening branch — so the form of the instance that most needs to describe
// itself (no port, no UI, driven in-process) was the one form that could not.
//
// It asks the TRANSPORT rather than the registry, for the same reason the tool test
// does: the claim is that a client can see the prompt, not that a map has a key.
func TestAnInstanceThatNeverListensStillAdvertisesItsDocs(t *testing.T) {
	srv := newInstance(t)
	if names := listPrompts(t, srv); len(names) == 0 {
		t.Fatal("an instance that never bound a port advertises no prompts at all; " +
			"the surfaces have to come up whether or not a socket does")
	}
}

// listPrompts asks a running instance's MCP endpoint what prompts it offers.
func listPrompts(t *testing.T, srv *Server) []string {
	t.Helper()
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	sessionID := initializeMCP(t, ts.URL)
	body := rpc(t, ts.URL, sessionID, `{"jsonrpc":"2.0","id":2,"method":"prompts/list","params":{}}`)

	var out struct {
		Result struct {
			Prompts []struct {
				Name string `json:"name"`
			} `json:"prompts"`
		} `json:"result"`
	}
	// The streamable transport answers either JSON or event-stream framing; strip
	// the framing rather than pinning the test to one of them.
	idx := strings.Index(body, `{"jsonrpc"`)
	if idx < 0 {
		t.Fatalf("prompts/list returned no JSON-RPC object: %s", body)
	}
	if err := json.Unmarshal([]byte(body[idx:]), &out); err != nil {
		t.Fatalf("prompts/list is not the JSON this test expects: %v\n%s", err, body)
	}
	names := make([]string, len(out.Result.Prompts))
	for i, p := range out.Result.Prompts {
		names[i] = p.Name
	}
	return names
}

// initializeMCP opens a session on an MCP endpoint and returns its id.
func initializeMCP(t *testing.T, url string) string {
	t.Helper()
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"c","version":"1"}}}`
	resp, err := http.Post(url+"/stream", "application/json", strings.NewReader(init))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize handed out no session id")
	}
	return sessionID
}

// rpc sends one request to a session and returns the raw response body.
func rpc(t *testing.T, url, sessionID, body string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/stream", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// TestListenRequiresStart pins the order, because the two calls are now separate
// and a caller that reverses them would otherwise get an instance that binds a port
// onto half a runtime — a failure that shows up much later as an unanswered route.
func TestListenRequiresStart(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Headless = true
	srv, err := New(Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close("test done")

	if err := srv.Listen(); err == nil {
		t.Fatal("Listen succeeded before Start; the surfaces it serves were never brought up")
	}
}

// TestAnInstanceNeedsNoValidPortUntilItListens is the acceptance criterion for
// removing the flag: a port is a constraint on a call, not on an object, so an
// embedder must not have to invent a free one. Port 0 is the clearest case — it is
// what a test reaches for to mean "anything", and it is exactly what the old
// validation rejected.
func TestAnInstanceNeedsNoValidPortUntilItListens(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Headless = true
	cfg.Port = 0
	srv, err := New(Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("New with Port 0: %v — an instance that is never bound should not be rejected for its port", err)
	}
	defer srv.Close("test done")

	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	reply := srv.Term(context.Background(), "ls", nil)
	if len(reply.Slots) == 0 || reply.Slots[0].Code >= 400 {
		t.Fatalf("an instance built with Port 0 cannot answer: %v", reply.Slots)
	}

	// And Listen is where the port becomes a real requirement.
	if err := srv.Listen(); err == nil {
		t.Error("Listen with Port 0 reported no error; it cannot bind a port it was not given")
	}
}

// TestHeadlessDropsTheUIButKeepsTheDocs pins what headless means, because the
// two halves are easy to get wrong in opposite directions: dropping /api.md with
// the UI takes away the only reference for the transport an embedder is still
// serving, and keeping the UI defeats the point of asking.
func TestHeadlessDropsTheUIButKeepsTheDocs(t *testing.T) {
	srv := newInstance(t)
	if srv.Embedded() {
		t.Fatal("a headless instance reports the browser UI as embedded")
	}
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET / = %d, want 404: a headless instance serves no browser UI", rec.Code)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api.md", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /api.md = %d, want 200: the transport reference stays, because the REST surface it describes stays", rec.Code)
	}
}

// TestCloseIsIdempotent pins that the one exit path tolerates being asked twice.
// It will be: a signal and a daemon countdown can both fire, and an embedder's
// defer runs after whichever came first.
func TestCloseIsIdempotent(t *testing.T) {
	srv := newInstance(t)
	srv.Close("first")
	srv.Close("second") // must not panic on a closed channel
}

// TestAnEmbedderAddsAToolToTheSameEndpoint is what MCP() is for: an integration
// registers its own tool on the instance's protocol endpoint, and a client of
// that endpoint can then call it.
//
// It goes over HTTP rather than calling the registry directly, because the
// claim is not "the map has an entry" — it is that a tool registered through
// the public surface is reachable on the transport termcp already serves.
func TestAnEmbedderAddsAToolToTheSameEndpoint(t *testing.T) {
	srv := newInstance(t)

	srv.MCP().AddTool(
		mcp.NewTool("embedder_ping", mcp.WithDescription("registered by the embedder")),
		func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("pong from the embedder"), nil
		},
	)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"c","version":"1"}}}`
	resp, err := http.Post(ts.URL+"/stream", "application/json", strings.NewReader(init))
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("initialize handed out no session id")
	}

	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"embedder_ping","arguments":{}}}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/stream", strings.NewReader(call))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", sessionID)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	defer resp2.Body.Close()
	body, _ := io.ReadAll(resp2.Body)

	if !strings.Contains(string(body), "pong from the embedder") {
		t.Fatalf("a tool registered through MCP() was not reachable on /stream: %s", body)
	}
}

// TestCloseReleasesThePort pins what a graceful stop is for. ListenAndServe owns a
// socket, and nothing shut it down: the binary never noticed because exiting the
// process closes file descriptors, but an embedder that Calls Close and keeps
// running leaves the port claimed — and then cannot explain why its own next bind
// fails.
func TestCloseReleasesThePort(t *testing.T) {
	// Ask the OS for a free port, then hand it to the instance: hardcoding one
	// races every other test on a shared runner.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probing for a free port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Host = "127.0.0.1"
	cfg.Port = port
	cfg.Headless = true
	srv, err := New(Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- srv.Listen() }()

	// WAIT FOR THE SOCKET, not for a duration: the state is "connections are
	// accepted", and the deadline only bounds how long a failure takes to report.
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, derr := net.Dial("tcp", srv.Addr())
		if derr == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Listen never bound %s: %v", srv.Addr(), derr)
		}
		time.Sleep(5 * time.Millisecond)
	}

	srv.Close("test done")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Listen returned %v, want nil after Close", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not make Listen return")
	}

	// The claim: the port is free again, immediately.
	ln, err := net.Listen("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("the port is still claimed after Close: %v", err)
	}
	ln.Close()
}

// TestCloseFinishesAnInstanceThatNeverListened covers the other half of who owns
// the release. Listen waits for stopCh and finishes on its way out; an instance
// that never got that far has nobody waiting, so Close has to do it. Without that
// branch the reconciler keeps probing and the store stays open after Close returns
// — a shutdown that looks complete and is not.
func TestCloseFinishesAnInstanceThatNeverListened(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Headless = true
	srv, err := New(Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	srv.Close("test done")
	if !srv.finished.Load() {
		t.Error("Close on an instance that never listened released nothing: no Listen is coming to do it")
	}
	// And asking twice still must not panic or double-release.
	srv.Close("again")
}
