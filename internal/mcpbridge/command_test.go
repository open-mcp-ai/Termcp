package mcpbridge

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/open-mcp-ai/termcp/internal/sshserver"
)

// ---------------------------------------------------------------------------
// argv parsing
// ---------------------------------------------------------------------------

// The argv is what a template's fields are rendered into, so it has to accept
// the shapes the template can produce: one URL, then any number of headers.
func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		argv    []string
		wantURL string
		wantSSE bool
		wantHdr map[string]string
		wantErr string
	}{
		{
			name:    "url only",
			argv:    []string{"https://mcp.example.com/mcp"},
			wantURL: "https://mcp.example.com/mcp",
		},
		{
			name:    "sse by path suffix",
			argv:    []string{"https://mcp.example.com/sse"},
			wantURL: "https://mcp.example.com/sse",
			wantSSE: true,
		},
		{
			name:    "sse with a query still reads the path",
			argv:    []string{"https://mcp.example.com/sse?tenant=1"},
			wantURL: "https://mcp.example.com/sse?tenant=1",
			wantSSE: true,
		},
		{
			name:    "a path that merely contains sse is not sse",
			argv:    []string{"https://mcp.example.com/asset"},
			wantURL: "https://mcp.example.com/asset",
		},
		{
			name:    "separate header flag",
			argv:    []string{"https://x/mcp", "--header", "X-Api-Key=sekrit"},
			wantURL: "https://x/mcp",
			wantHdr: map[string]string{"X-Api-Key": "sekrit"},
		},
		{
			name:    "joined header flag",
			argv:    []string{"--header=X-Api-Key=sekrit", "https://x/mcp"},
			wantURL: "https://x/mcp",
			wantHdr: map[string]string{"X-Api-Key": "sekrit"},
		},
		{
			name:    "several headers",
			argv:    []string{"https://x/mcp", "--header", "A=1", "--header", "B=2"},
			wantURL: "https://x/mcp",
			wantHdr: map[string]string{"A": "1", "B": "2"},
		},
		{
			name:    "a value may contain an equals",
			argv:    []string{"https://x/mcp", "--header", "A=b=c"},
			wantURL: "https://x/mcp",
			wantHdr: map[string]string{"A": "b=c"},
		},
		{
			name:    "a value keeps its own spaces",
			argv:    []string{"https://x/mcp", "--header", "A= padded "},
			wantURL: "https://x/mcp",
			wantHdr: map[string]string{"A": " padded "},
		},
		{name: "no url", argv: []string{"--header", "A=1"}, wantErr: "endpoint URL is required"},
		{name: "two urls", argv: []string{"https://x/mcp", "https://y/mcp"}, wantErr: "extra argument"},
		{name: "unknown flag", argv: []string{"https://x/mcp", "--verbose"}, wantErr: "unknown argument"},
		{name: "header with no value", argv: []string{"https://x/mcp", "--header", "novalue"}, wantErr: "Name=Value"},
		{name: "header flag with nothing after it", argv: []string{"https://x/mcp", "--header"}, wantErr: "Name=Value"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := ParseArgs(tc.argv)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.URL != tc.wantURL {
				t.Errorf("URL = %q, want %q", cfg.URL, tc.wantURL)
			}
			if cfg.SSE != tc.wantSSE {
				t.Errorf("SSE = %v, want %v", cfg.SSE, tc.wantSSE)
			}
			if len(cfg.Headers) != len(tc.wantHdr) {
				t.Fatalf("headers = %v, want %v", cfg.Headers, tc.wantHdr)
			}
			for name, want := range tc.wantHdr {
				if got := cfg.Headers[name]; got != want {
					t.Errorf("header %q = %q, want %q", name, got, want)
				}
			}
		})
	}
}

// A header the bridge sends has to reach the server on the requests the relay
// makes, not just the first one: a credential that stopped after initialize
// would leave the rest of the conversation unauthenticated.
func TestCommandHandlerSendsHeaders(t *testing.T) {
	baseURL, _ := newMCPEndpoint(t)
	seen := make(chan map[string]string, 16)

	// The endpoint, wrapped so each request reports the headers it carried.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := map[string]string{":method": r.Method}
		for name, values := range r.Header {
			if len(values) > 0 {
				hdr[name] = values[0]
			}
		}
		select {
		case seen <- hdr:
		default:
		}
		forwardTo(t, baseURL, w, r)
	})
	proxy := httptest.NewServer(inner)
	t.Cleanup(proxy.Close)

	out := &syncBuffer{}
	errOut := &syncBuffer{}
	in := strings.NewReader(initMessage + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")

	code := CommandHandler(context.Background(), in, out, errOut,
		[]string{proxy.URL + "/stream", "--header", "X-Api-Key=sekrit"}, nil)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"tools"`) {
		t.Errorf("tools/list reply missing from stdout: %s", out.String())
	}

	// Every recorded request carried the header, whichever method it used.
	deadline := time.After(2 * time.Second)
	requests := 0
	for {
		select {
		case hdr := <-seen:
			requests++
			if hdr["X-Api-Key"] != "sekrit" {
				t.Fatalf("request without the configured header (method %s): %v", hdr[":method"], hdr)
			}
		case <-deadline:
			if requests == 0 {
				t.Fatal("the relay made no request through the proxy")
			}
			return
		}
	}
}

// forwardTo relays one proxied request to the real endpoint. The test only
// needs the headers, but the relay still needs a working server, so the request
// is replayed verbatim.
func forwardTo(t *testing.T, baseURL string, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, baseURL+r.URL.Path, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for name, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// ---------------------------------------------------------------------------
// the built-in command over the internal SSH server
// ---------------------------------------------------------------------------

// The whole point of the built-in command: a session that asks for it is served
// in-process, so no program is looked up on the host and the relay's stdio is
// the SSH channel's.
func TestBuiltinCommandOverInternalSSH(t *testing.T) {
	mcpURL, _ := newMCPEndpoint(t)

	srv := sshserver.New()
	RegisterBuiltin(srv, nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })

	sess := startBuiltinSession(t, srv)
	defer sess.Close()

	stdin, err := sess.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	// The name is not a program on this host, and that is the point: the internal
	// server intercepts it before any exec, so the only thing that can answer is
	// the handler registered above.
	if err := sess.Start(BuiltinCommand + " " + mcpURL); err != nil {
		t.Fatalf("start builtin: %v", err)
	}

	replies := make(chan string, 8)
	go func() {
		br := bufio.NewReader(stdout)
		for {
			line, err := br.ReadString('\n')
			if strings.TrimSpace(line) != "" {
				replies <- strings.TrimSpace(line)
			}
			if err != nil {
				close(replies)
				return
			}
		}
	}()

	for _, msg := range []string{
		initMessage,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"in-process"}}}`,
	} {
		if _, err := io.WriteString(stdin, msg+"\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	var callReply string
	deadline := time.After(15 * time.Second)
	for callReply == "" {
		select {
		case line, ok := <-replies:
			if !ok {
				t.Fatal("the built-in bridge closed stdout before answering tools/call")
			}
			if strings.Contains(line, `"id":2`) {
				callReply = line
			}
		case <-deadline:
			t.Fatal("the built-in bridge produced no tools/call reply")
		}
	}
	if !strings.Contains(callReply, "echo:in-process") {
		t.Fatalf("tool result = %s", callReply)
	}
}

// A built-in that cannot make sense of its argv must say so and exit, not hold
// the channel open: the reconciler reads the process's stderr into the failure
// reason, and a session that hangs produces no reason at all.
func TestBuiltinCommandRejectsBadArgs(t *testing.T) {
	srv := sshserver.New()
	RegisterBuiltin(srv, nil)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Stop() })

	sess := startBuiltinSession(t, srv)
	defer sess.Close()

	var stderr strings.Builder
	sess.Stderr = &stderr
	if err := sess.Run(BuiltinCommand); err == nil {
		t.Fatal("a built-in with no endpoint should fail")
	}
	if !strings.Contains(stderr.String(), "endpoint URL is required") {
		t.Errorf("stderr = %q, want the parse error", stderr.String())
	}
}

// Registering a command is what makes it built in; a nil handler or a blank
// name must not take the name away from the host's PATH.
func TestRegisterCommandIgnoresEmpty(t *testing.T) {
	srv := sshserver.New()
	RegisterBuiltin(nil, nil)
	if h := srv.BuiltinForTest(""); h != nil {
		t.Error("an empty name must not be registered")
	}
	if h := srv.BuiltinForTest(BuiltinCommand); h != nil {
		t.Error("RegisterBuiltin(nil, nil) must register nothing")
	}
}

// startBuiltinSession dials the internal server and opens one session channel,
// the way a session manager does.
func startBuiltinSession(t *testing.T, srv *sshserver.Server) *ssh.Session {
	t.Helper()
	cfg, err := srv.MintClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := srv.Dial()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(conn, "inmem", cfg)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	client := ssh.NewClient(c, chans, reqs)
	t.Cleanup(func() { client.Close() })
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	return sess
}
