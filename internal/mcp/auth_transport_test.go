package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/mcpbridge"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestManagedMCPTransportCredentials(t *testing.T) {
	dir := t.TempDir()
	vault, err := sshconfig.OpenConfiguredStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vault.Close() })
	account, err := auth.OpenManaged(dir, vault)
	if err != nil {
		t.Fatal(err)
	}
	s, _, _, _ := newTestServerWithHistory(t)
	mux := http.NewServeMux()
	mux.Handle("/stream", s.StreamableHTTPHandler())
	mux.Handle("/sse", s.SSEHandler())
	mux.Handle("/message", s.MessageHandler())
	ts := httptest.NewServer(account.Wrap(mux))
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	admin := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	var csrf string
	call := func(method, path, body string) map[string]any {
		t.Helper()
		r, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
		r.Header.Set("X-Termcp-CSRF", csrf)
		resp, err := admin.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var data map[string]any
		if err = json.NewDecoder(resp.Body).Decode(&data); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, data)
		}
		return data
	}
	csrf = call("GET", "/auth/status", "")["csrf"].(string)
	csrf = call("POST", "/auth/setup", `{"username":"test-admin","password":"synthetic-transport-password"}`)["csrf"].(string)
	first := call("POST", "/auth/agents", `{"name":"agent A"}`)
	token := first["token"].(string)
	principal := first["agent"].(map[string]any)["id"].(string)
	second := call("POST", "/auth/agents", `{"name":"agent B"}`)
	other := second["token"].(string)
	networkCtx, networkCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer networkCancel()
	request := func(method, path, body, credential, session string) *http.Response {
		t.Helper()
		r, _ := http.NewRequestWithContext(networkCtx, method, ts.URL+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("Mcp-Session-Id", session)
		r.Header.Set(NamespaceHeader, "pretend-to-be-another-agent")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"auth-test","version":"1"}}}`
	resp := request("POST", "/stream", init, token, "")
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d", resp.StatusCode)
	}
	ping := `{"jsonrpc":"2.0","id":2,"method":"ping"}`
	resp = request("POST", "/stream", ping, other, sid)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("foreign Agent adopted stream session")
	}
	resp = request("POST", "/stream", ping, token, sid)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("owner lost stream session")
	}
	if got := s.Connections(); len(got) != 1 || got[0].Namespace != "ns:agent-"+principal {
		t.Fatalf("identity namespace: %+v", got)
	}
	// The real stdio bridge uses the same Bearer credential on every request.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := range 3 {
		wg.Go(func() {
			var out bytes.Buffer
			err := mcpbridge.Run(ctx, mcpbridge.Config{URL: ts.URL + "/stream", Token: token, In: strings.NewReader(init + "\n" + ping + "\n"), Out: &out})
			if err == nil && !strings.Contains(out.String(), `"result"`) {
				err = fmt.Errorf("bridge %d got no result", i)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	// SSE messages are tied to the credential that opened their event stream.
	resp = request("GET", "/sse", "", token, "")
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	var endpoint string
	for endpoint == "" {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, "data: ") {
			endpoint = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
		}
	}
	msg := request("POST", endpoint, init, other, "")
	msg.Body.Close()
	if msg.StatusCode != 403 {
		t.Fatal("foreign Agent adopted SSE session")
	}
	msg = request("POST", endpoint, init, token, "")
	msg.Body.Close()
	if msg.StatusCode != 202 {
		t.Fatalf("SSE initialize: %d", msg.StatusCode)
	}
	credential := first["credential"].(map[string]any)["id"].(string)
	call("DELETE", "/auth/tokens/"+credential, "")
	finished := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, reader); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("revoked SSE connection survived")
	}
	resp = request("POST", "/stream", ping, other, "")
	resp.Body.Close()
	if resp.StatusCode == 401 {
		t.Fatal("other Agent was revoked")
	}
}
