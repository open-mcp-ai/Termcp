package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/ns"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func TestAgentFunctionsAndHostScopeEnforceBothEntrances(t *testing.T) {
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
	s, _, mgr, _ := newTestServerWithHistory(t)
	s.sshConfigs = vault
	mgr.SetSSHConfigResolver(vault.ResolveSessionReference)
	h := account.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Tool string         `json:"tool"`
			Args map[string]any `json:"args"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		result, err := s.call(withRequestContext(r.Context(), r), input.Tool, input.Args)
		if err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	var cookie *http.Cookie
	var preCookie *http.Cookie
	var csrf string
	admin := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		r := httptest.NewRequest(method, "http://127.0.0.1:18765"+path, bytes.NewReader(raw))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("X-Termcp-CSRF", csrf)
		if preCookie != nil {
			r.AddCookie(preCookie)
		}
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		return w
	}
	var status struct {
		CSRF string `json:"csrf"`
	}
	w := admin("GET", "/auth/status", nil)
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	csrf = status.CSRF
	preCookie = w.Result().Cookies()[0]
	w = admin("POST", "/auth/setup", map[string]any{"username": "test-admin", "password": "synthetic-policy-password"})
	_ = json.Unmarshal(w.Body.Bytes(), &status)
	csrf = status.CSRF
	for _, c := range w.Result().Cookies() {
		if c.Name == auth.SessionCookie {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("missing admin cookie")
	}
	if err = vault.Save("other-host", []byte("kind = \"remote\"\nhost = \"host.example\"\nuser = \"test\"\npassword = \"synthetic\"\n")); err != nil {
		t.Fatal(err)
	}
	policy := auth.AgentPolicy{Functions: []string{"profile_read", "file_read", "terminal_read", "session_read"}, HostMode: "selected", Hosts: []string{sshconfig.InternalIdentity}}
	w = admin("POST", "/auth/agents", map[string]any{"name": "reader", "policy": policy})
	var created struct {
		Token string `json:"token"`
		Agent struct {
			ID string `json:"id"`
		} `json:"agent"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	call := func(tool string, args map[string]any) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"tool": tool, "args": args})
		r := httptest.NewRequest("POST", "http://127.0.0.1:18765/stream", bytes.NewReader(raw))
		r.Header.Set("Authorization", "Bearer "+created.Token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("MCP admission: %d", w.Code)
		}
		var result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		for _, c := range result.Content {
			out.WriteString(c.Text)
		}
		return out.String()
	}
	term := func(cmd string) string { return call("term", map[string]any{"cmd": cmd, "timeout": 0}) }
	if text := term("ls host"); !strings.Contains(text, "internal") || strings.Contains(text, "other-host") {
		t.Fatalf("host listing leaked scope: %s", text)
	}
	for _, cmd := range []string{"connect internal", "get host other-host", "create alias forbidden run x:1 echo", "open internal cdp http://127.0.0.1:1"} {
		if text := term(cmd); !strings.Contains(text, "403") {
			t.Fatalf("operation escaped grant: %s\n%s", cmd, text)
		}
	}
	if text := call("session_start", map[string]any{"ssh_config": "internal"}); !strings.Contains(text, "forbidden") {
		t.Fatal("specialized tool bypassed function grant", text)
	}
	aliases.set("policy_test_alias", "connect internal")
	t.Cleanup(func() { aliases.drop("policy_test_alias") })
	if text := term("policy_test_alias"); !strings.Contains(text, "403") {
		t.Fatal("alias bypassed normalized permission check", text)
	}
	sess, err := mgr.Create(session.Config{Command: testShell(), Mode: api.ModePipe, SSHConfig: "internal", Namespace: ns.ForCaller("agent-" + created.Agent.ID)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Delete(sess.ID) })
	path := filepath.Join(t.TempDir(), "sample.txt")
	if err = os.WriteFile(path, []byte("original content"), 0600); err != nil {
		t.Fatal(err)
	}
	if text := call("file_read", map[string]any{"session_id": sess.ID, "remote_path": path}); !strings.Contains(text, "original content") {
		t.Fatal("allowed read failed", text)
	}
	if text := call("file_write", map[string]any{"session_id": sess.ID, "remote_path": path, "data": "changed"}); !strings.Contains(text, "forbidden") {
		t.Fatal("read permission allowed writing", text)
	}
	if text := term("get " + sess.ID + " file '" + path + "' --op truncate"); !strings.Contains(text, "403") {
		t.Fatal("get --op bypassed write grant", text)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "original content" {
		t.Fatal("denied write had a side effect")
	}
	policy.Functions = append(policy.Functions, "file_write")
	admin("PATCH", "/auth/agents/"+created.Agent.ID, map[string]any{"revision": 1, "policy": policy})
	other, err := mgr.Create(session.Config{Command: testShell(), Mode: api.ModePipe, SSHConfig: "other-host", Namespace: ns.ForCaller("agent-" + created.Agent.ID)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mgr.Delete(other.ID) })
	if text := term("read " + sess.ID + " file '" + path + "' | write " + other.ID + " file '" + path + "'"); !strings.Contains(text, "403") {
		t.Fatal("copy ignored destination host scope", text)
	}
	if text := term("ls --all"); strings.Contains(text, other.ID) {
		t.Fatal("--all bypassed host listing filter", text)
	}
	policy.HostMode = "none"
	policy.Hosts = []string{}
	admin("PATCH", "/auth/agents/"+created.Agent.ID, map[string]any{"revision": 2, "policy": policy})
	if text := term("get " + sess.ID); !strings.Contains(text, "403") {
		t.Fatal("old session retained removed host grant", text)
	}
	if text := call("file_read", map[string]any{"session_id": sess.ID, "remote_path": path}); !strings.Contains(text, "forbidden") {
		t.Fatal("old tool target retained removed grant", text)
	}
	// Trusted operator calls retain their existing rights.
	if bad, held := s.checkAdmission(context.Background(), "file_read", makeRequest(map[string]any{"session_id": sess.ID, "remote_path": path})); bad != nil || held {
		t.Fatal("Agent policy affected operator")
	}
}
