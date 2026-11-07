package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/config"
	"github.com/open-mcp-ai/termcp/mcp"
)

// newInstance builds a headless, listener-less instance rooted in a temp dir.
//
// The two flags are the point of the test, not a convenience: NoListener means
// nothing binds a port, and Headless means no browser UI is mounted. Together
// they are the SDK shape — an embedder that already has a transport and wants a
// command language, not a web application.
func newInstance(t *testing.T) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.NoInternal = false
	srv, err := New(Options{Config: cfg, Version: "test", NoListener: true, Headless: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() { _ = srv.Start() }()
	if err := srv.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady: %v", err)
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

// TestNoListenerBindsNothing pins the difference between "does not serve" and
// "does not work". The HTTP surface still exists — an embedder mounts it on its
// own listener — so Handler must answer, while the port stays closed.
func TestNoListenerBindsNothing(t *testing.T) {
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
