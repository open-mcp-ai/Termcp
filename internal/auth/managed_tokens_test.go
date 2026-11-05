package auth

import (
	"context"
	"encoding/json"
	"github.com/open-mcp-ai/termcp/internal/configstore"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type issuedCredential struct {
	Agent      agentRecord `json:"principal"`
	Token      string      `json:"token"`
	Credential tokenInfo   `json:"credential"`
}

func issueTestCredential(t *testing.T, m *Managed, cookie string, session sessionRecord, path, body string) issuedCredential {
	t.Helper()
	if path == "/auth/principals" {
		var input map[string]any
		if err := json.Unmarshal([]byte(body), &input); err != nil {
			t.Fatal(err)
		}
		if policy, ok := input["policy"]; ok {
			groupName, _ := randomSecret()
			payload, _ := json.Marshal(map[string]any{"name": "fixture-" + groupName, "policy": policy})
			response := managedRequest(t, m.Wrap(http.NotFoundHandler()), "POST", "/auth/groups", string(payload), cookie, session.CSRF)
			if response.Code != 200 {
				t.Fatalf("create fixture group: %d %s", response.Code, response.Body)
			}
			var created struct {
				Group groupWire `json:"group"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
				t.Fatal(err)
			}
			input["group_id"] = created.Group.ID
			delete(input, "policy")
		}
		input["issue_token"] = true
		payload, _ := json.Marshal(input)
		body = string(payload)
	}
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "POST", path, body, cookie, session.CSRF)
	if w.Code != 200 {
		t.Fatalf("issue: %d %s", w.Code, w.Body.String())
	}
	var out issuedCredential
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatal("missing one-time token")
	}
	return out
}

func rotateTestCredential(t *testing.T, m *Managed, cookie string, session sessionRecord, current issuedCredential) issuedCredential {
	t.Helper()
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), "POST", "/auth/tokens/"+current.Credential.ID+"/rotate", `{}`, cookie, session.CSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", w.Code, w.Body.String())
	}
	var out issuedCredential
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" || out.Agent.ID != "" || out.Credential.PrincipalID != current.Credential.PrincipalID {
		t.Fatalf("unexpected rotation response: %+v", out)
	}
	return out
}

func bearerRequest(h http.Handler, method, path, token, cookie string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:18765"+path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestMCPCredentialsIdentityPersistenceAndPurpose(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"Build agent"}`)
	second := rotateTestCredential(t, m, cookie, session, a)
	admin := issueTestCredential(t, m, cookie, session, "/auth/tokens", `{"label":"admin client"}`)
	check := func(token string, want Actor) {
		t.Helper()
		h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			actor, ok := ActorFromContext(r.Context())
			if !ok || actor != want {
				t.Errorf("actor: %#v", actor)
			}
			w.WriteHeader(204)
		}))
		for _, path := range []string{"/stream", "/sse", "/message"} {
			if w := bearerRequest(h, "POST", path, token, ""); w.Code != 204 {
				t.Fatalf("MCP: %d %s", w.Code, w.Body)
			}
		}
	}
	agent := Actor{ID: a.Agent.ID, Username: a.Agent.Name, Role: RoleAgent}
	check(second.Token, agent)
	if w := bearerRequest(m.Wrap(http.NotFoundHandler()), "GET", "/auth/mcp", a.Token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("rotated token accepted: %d", w.Code)
	}
	check(admin.Token, Actor{ID: m.state.Admin.ID, Username: "admin", Role: RoleAdmin})
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, token := range []string{second.Token, admin.Token} {
		for _, path := range []string{"/api/daemon", "/api/daemon/stop", "/auth/principals", "/auth/tokens", "/auth/permissions", "/auth/password", "/api/sessions", "/api/approvals", "/api/approvals/fake/approve", "/"} {
			if w := bearerRequest(h, "POST", path, token, cookie); w.Code != 403 {
				t.Fatalf("bearer escaped MCP: %s %d", path, w.Code)
			}
		}
	}
	if w := bearerRequest(h, "POST", "/stream", a.Token+"x", cookie); w.Code != 401 {
		t.Fatal("invalid bearer fell back to Cookie")
	}
	for _, path := range []string{"/auth/principals", "/auth/tokens"} {
		w := managedRequest(t, h, "GET", path, "", cookie, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), a.Token) || strings.Contains(w.Body.String(), "digest") {
			t.Fatalf("list leaks credentials: %d", w.Code)
		}
	}
	m2, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if w := bearerRequest(m2.Wrap(http.NotFoundHandler()), "GET", "/auth/mcp", second.Token, ""); w.Code != 200 {
		t.Fatalf("restart lost token: %d", w.Code)
	}
	if w := bearerRequest(m2.Wrap(http.NotFoundHandler()), "GET", "/auth/mcp", a.Token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("restart revived rotated token: %d", w.Code)
	}
	raw, err := os.ReadFile(configstore.LocalStatePath(filepath.Dir(m.path)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), a.Token) {
		t.Fatal("raw token stored")
	}
	cleartext, err := m2.vault.ReadAuthentication()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(cleartext)
	if strings.Contains(string(cleartext), a.Token) {
		t.Fatal("vault contains raw token instead of digest")
	}
}

func TestMCPCredentialRevocationDisablesOnlyItsConnections(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"agent"}`)
	b := rotateTestCredential(t, m, cookie, session, a)
	other := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"other agent"}`)
	contexts := map[string]context.Context{}
	for _, issued := range []issuedCredential{b, other} {
		r := httptest.NewRequest("GET", "http://127.0.0.1:18765/sse", nil)
		r.Header.Set("Authorization", "Bearer "+issued.Token)
		ctx, done, code := m.admitToken(r)
		if code != 0 {
			t.Fatal(code)
		}
		defer done()
		contexts[issued.Token] = ctx
	}
	h := m.Wrap(http.NotFoundHandler())
	if w := managedRequest(t, h, "DELETE", "/auth/tokens/"+b.Credential.ID, "", cookie, session.CSRF); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if contexts[b.Token].Err() == nil || contexts[other.Token].Err() != nil {
		t.Fatal("revocation affected wrong connection")
	}
	if w := bearerRequest(h, "POST", "/stream", b.Token, ""); w.Code != 401 {
		t.Fatal("revoked token accepted")
	}
	d := issueTestCredential(t, m, cookie, session, "/auth/tokens", `{"principal_id":"`+a.Agent.ID+`"}`)
	r := httptest.NewRequest("GET", "http://127.0.0.1:18765/sse", nil)
	r.Header.Set("Authorization", "Bearer "+d.Token)
	ctx, done, code := m.admitToken(r)
	if code != 0 {
		t.Fatal(code)
	}
	defer done()
	if w := managedRequest(t, h, "PATCH", "/auth/principals/"+a.Agent.ID, `{"revision":1,"enabled":false}`, cookie, session.CSRF); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if ctx.Err() == nil || contexts[other.Token].Err() != nil {
		t.Fatal("disabled Agent connection survived")
	}
	if w := bearerRequest(h, "POST", "/stream", d.Token, ""); w.Code != 401 {
		t.Fatal("disabled Agent accepted")
	}
	if w := managedRequest(t, h, "PATCH", "/auth/principals/"+a.Agent.ID, `{"revision":2,"enabled":true}`, cookie, session.CSRF); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := bearerRequest(h, "GET", "/auth/mcp", d.Token, ""); w.Code != 200 {
		t.Fatal("enable changed identity or token")
	}
	// Password changes invalidate web sessions, while independent MCP credentials survive.
	if err := m.ChangePassword(testPassword, "next-synthetic-password", "next-synthetic-password"); err != nil {
		t.Fatal(err)
	}
	if w := bearerRequest(h, "GET", "/auth/mcp", d.Token, ""); w.Code != 200 {
		t.Fatal("master password change invalidated MCP token")
	}
	reopened, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if w := bearerRequest(reopened.Wrap(http.NotFoundHandler()), "GET", "/auth/mcp", a.Token, ""); w.Code != 401 {
		t.Fatal("restart resurrected revoked token")
	}
	if w := bearerRequest(reopened.Wrap(http.NotFoundHandler()), "GET", "/auth/mcp", d.Token, ""); w.Code != 200 {
		t.Fatal("restart lost replacement token")
	}

}

func TestMCPPolicyChangeCancelsLiveTokenAndDoesNotAcceptAmbiguousCredentials(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"agent"}`)
	r := httptest.NewRequest("GET", "http://127.0.0.1:18765/sse", nil)
	r.Header.Set("Authorization", "Bearer "+a.Token)
	ctx, done, code := m.admitToken(r)
	if code != 0 {
		t.Fatal(code)
	}
	defer done()
	h := m.Wrap(http.NotFoundHandler())
	body := `{"revision":1,"permissions":{"admin":["web","api","mcp","daemon"],"agent":[]}}`
	if w := managedRequest(t, h, "PUT", "/auth/permissions", body, cookie, session.CSRF); w.Code != 200 {
		t.Fatal(w.Code)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("policy did not cancel stream")
	}
	if w := bearerRequest(h, "GET", "/auth/mcp", a.Token, ""); w.Code != 403 {
		t.Fatal("role restriction ignored")
	}
	r.Header.Add("Authorization", "Bearer "+a.Token)
	if _, _, status := m.admitToken(r); status != 401 {
		t.Fatal("duplicate credentials accepted")
	}
}

func TestMCPCredentialIssueFailureAndLockedRestart(t *testing.T) {
	m := openManaged(t)
	if err := m.setup("admin", testPassword); err != nil {
		t.Fatal(err)
	}
	cookie, session, err := m.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	a := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"agent"}`)
	m2, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if w := bearerRequest(m2.Wrap(http.NotFoundHandler()), "GET", "/auth/mcp", a.Token, ""); w.Code != 423 {
		t.Fatal("token unlocked encrypted vault")
	}
	cookie, session, err = m2.loginWithOptions("admin", testPassword, testPassword, false)
	if err != nil {
		t.Fatal(err)
	}
	if w := bearerRequest(m2.Wrap(http.NotFoundHandler()), "GET", "/auth/mcp", a.Token, ""); w.Code != 200 {
		t.Fatal("unlock lost token")
	}
	path := configstore.LocalStatePath(filepath.Dir(m2.path))
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	before := len(m2.state.Tokens)
	w := managedRequest(t, m2.Wrap(http.NotFoundHandler()), "POST", "/auth/tokens", `{"label":"unpublished"}`, cookie, session.CSRF)
	if w.Code != 500 || strings.Contains(w.Body.String(), "tmcp_") || len(m2.state.Tokens) != before {
		t.Fatal("failed save published a credential")
	}
}
