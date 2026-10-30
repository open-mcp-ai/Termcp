package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

const testPassword = "sample-password-for-tests"

func openManaged(t *testing.T) *Managed {
	t.Helper()
	m := openManagedAt(t, t.TempDir())
	return m
}

func openManagedAt(t *testing.T, dir string) *Managed {
	t.Helper()
	t.Setenv("TERMCP_TEST_AUTH_KEK", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	provider := configstore.NewEnvProvider("TERMCP_TEST_AUTH_KEK")
	store, err := sshconfig.OpenConfiguredStore(dir, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	m, err := OpenManaged(dir, store, provider)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func reopenManaged(t *testing.T, m *Managed) (*Managed, error) {
	t.Helper()
	if err := m.vault.(*sshconfig.Store).Close(); err != nil {
		return nil, err
	}
	provider := configstore.NewEnvProvider("TERMCP_TEST_AUTH_KEK")
	store, err := sshconfig.OpenConfiguredStore(filepath.Dir(m.path), provider)
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = store.Close() })
	return OpenManaged(filepath.Dir(m.path), store, provider)
}

func configuredManaged(t *testing.T) (*Managed, string, sessionRecord) {
	t.Helper()
	m := openManaged(t)
	if err := m.setupWithOptions("admin", testPassword, "", true); err != nil {
		t.Fatal(err)
	}
	token, session, err := m.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	return m, token, session
}

func managedRequest(t *testing.T, handler http.Handler, method, path, body, token, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "http://127.0.0.1:18765"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	if token != "" {
		r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
	}
	if csrf != "" {
		r.Header.Set("X-Termcp-CSRF", csrf)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestManagedFirstRunLoginRestartLogout(t *testing.T) {
	m := openManaged(t)
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := ActorFromContext(r.Context())
		if !ok || a.Role != RoleAdmin || a.Username != "admin" {
			t.Error("missing trusted actor")
		}
		w.WriteHeader(204)
	})
	h := m.Wrap(next)
	if w := managedRequest(t, h, "GET", "/", "", "", ""); w.Code != 303 {
		t.Fatalf("anonymous workspace: %d", w.Code)
	}
	if w := managedRequest(t, h, "GET", "/api/sessions", "", "", ""); w.Code != 401 {
		t.Fatalf("anonymous API: %d", w.Code)
	}
	status := managedRequest(t, h, "GET", "/auth/status", "", "", "")
	var pre struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(status.Body.Bytes(), &pre); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:18765/auth/setup", strings.NewReader(`{"username":"admin","password":"`+testPassword+`"}`))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Origin", "http://127.0.0.1:18765")
	r.Header.Set("X-Termcp-CSRF", pre.CSRF)
	for _, c := range status.Result().Cookies() {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == SessionCookie {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.MaxAge <= 0 {
		t.Fatal("missing protected session cookie")
	}
	if strings.Contains(w.Body.String(), cookie.Value) || strings.Contains(w.Body.String(), testPassword) {
		t.Fatal("credentials leaked in response")
	}
	_, session, err := m.verify(cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	if w := managedRequest(t, h, "POST", "/api/action", "", cookie.Value, session.CSRF); w.Code != 204 {
		t.Fatalf("authenticated write: %d", w.Code)
	}
	loaded, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := loaded.verify(cookie.Value); err == nil {
		t.Fatal("password-only restart must stay locked")
	}
	fresh, freshSession, err := loaded.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := loaded.verify(cookie.Value); err == nil {
		t.Fatal("unlock restored a cookie from the previous run")
	}
	cookie.Value, session = fresh, freshSession
	b, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(testPassword)) || bytes.Contains(b, []byte("signing_key")) {
		t.Fatal("plaintext credentials persisted")
	}
	cipher, err := os.ReadFile(filepath.Join(filepath.Dir(m.path), "auth.enc"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, []byte(testPassword)) || bytes.Contains(cipher, []byte(loaded.state.SigningKey)) {
		t.Fatal("authentication secrets are not encrypted")
	}
	logout := managedRequest(t, loaded.Wrap(next), "POST", "/auth/logout", "", cookie.Value, session.CSRF)
	if logout.Code != 200 {
		t.Fatalf("logout: %d", logout.Code)
	}
	if _, _, err := loaded.verify(cookie.Value); err == nil {
		t.Fatal("copied JWT survived logout")
	}
	again, err := reopenManaged(t, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := again.verify(cookie.Value); err == nil {
		t.Fatal("restart resurrected logged-out session")
	}
}

func TestManagedRejectsForgedAndWrongPurposeJWT(t *testing.T) {
	m, raw, _ := configuredManaged(t)
	var original sessionClaims
	_, _, err := jwt.NewParser().ParseUnverified(raw, &original)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := base64.RawURLEncoding.DecodeString(m.state.SigningKey)
	tests := []struct {
		name   string
		edit   func(*sessionClaims)
		method jwt.SigningMethod
		key    any
	}{
		{"role", func(c *sessionClaims) { c.Role = RoleAgent }, jwt.SigningMethodHS256, key},
		{"subject", func(c *sessionClaims) { c.Subject = "other" }, jwt.SigningMethodHS256, key},
		{"audience", func(c *sessionClaims) { c.Audience = jwt.ClaimStrings{"termcp-mcp"} }, jwt.SigningMethodHS256, key},
		{"issuer", func(c *sessionClaims) { c.Issuer = "other-instance" }, jwt.SigningMethodHS256, key},
		{"unknown-session", func(c *sessionClaims) { c.ID = "unknown" }, jwt.SigningMethodHS256, key},
		{"expiry-required", func(c *sessionClaims) { c.ExpiresAt = nil }, jwt.SigningMethodHS256, key},
		{"expired", func(c *sessionClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Hour)) }, jwt.SigningMethodHS256, key},
		{"wrong-key", func(*sessionClaims) {}, jwt.SigningMethodHS256, []byte("synthetic-wrong-signing-key-for-test")},
		{"wrong-algorithm", func(*sessionClaims) {}, jwt.SigningMethodHS384, key},
		{"unsigned", func(*sessionClaims) {}, jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := original
			test.edit(&claims)
			token, err := jwt.NewWithClaims(test.method, claims).SignedString(test.key)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := m.verify(token); err == nil {
				t.Fatal("accepted invalid JWT")
			}
		})
	}
	parts := strings.Split(raw, ".")
	parts[1] = "e30"
	if _, _, err := m.verify(strings.Join(parts, ".")); err == nil {
		t.Fatal("accepted edited payload")
	}
}

func TestManagedCSRFAndCredentialPrecedence(t *testing.T) {
	m, token, session := configuredManaged(t)
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, tc := range []struct {
		name, method, path, origin, csrf, authz string
		duplicate                               bool
		status                                  int
	}{
		{"read", "GET", "/api/sessions", "", "", "", false, 204},
		{"write", "POST", "/api/sessions", "http://127.0.0.1:18765", session.CSRF, "", false, 204},
		{"csrf-required", "POST", "/api/sessions", "http://127.0.0.1:18765", "", "", false, 403},
		{"foreign-origin", "POST", "/api/sessions", "https://other.example", session.CSRF, "", false, 403},
		{"foreign-ws", "GET", "/api/ui/ws", "https://other.example", "", "", false, 403},
		{"invalid-explicit", "GET", "/api/sessions", "", "", "Bearer invalid", false, 401},
		{"duplicate-cookie", "GET", "/api/sessions", "", "", "", true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://127.0.0.1:18765"+tc.path, nil)
			r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
			if tc.duplicate {
				r.AddCookie(&http.Cookie{Name: SessionCookie, Value: token})
			}
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("X-Termcp-CSRF", tc.csrf)
			if tc.authz != "" {
				r.Header.Set("Authorization", tc.authz)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
		})
	}
	if w := managedRequest(t, h, "POST", "/auth/login", `{"username":"admin","password":"`+testPassword+`","role":"admin"}`, "", ""); w.Code != 403 {
		t.Fatalf("login without prelogin CSRF: %d", w.Code)
	}
}

func TestManagedPoliciesAreLiveAndPersistent(t *testing.T) {
	m, token, session := configuredManaged(t)
	h := m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	if w := managedRequest(t, h, "POST", "/stream", "{}", token, session.CSRF); w.Code != 204 {
		t.Fatalf("initial MCP: %d", w.Code)
	}
	body := `{"revision":1,"permissions":{"admin":["web","api","daemon"],"agent":[]}}`
	if w := managedRequest(t, h, "PUT", "/auth/permissions", body, token, session.CSRF); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	if w := managedRequest(t, h, "POST", "/stream", "{}", token, session.CSRF); w.Code != 403 {
		t.Fatalf("policy was not applied to existing session: %d", w.Code)
	}
	if w := managedRequest(t, h, "PUT", "/auth/permissions", body, token, session.CSRF); w.Code != 409 {
		t.Fatalf("stale save: %d", w.Code)
	}
	bad := `{"revision":2,"permissions":{"admin":["web","api"],"agent":["api"]}}`
	if w := managedRequest(t, h, "PUT", "/auth/permissions", bad, token, session.CSRF); w.Code != 400 {
		t.Fatalf("Agent API escalation: %d", w.Code)
	}
	reopened, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.allows(RoleAdmin, "mcp") || reopened.allows(RoleAgent, "mcp") {
		t.Fatal("restart lost permissions")
	}
	if !reopened.allows(RoleAdmin, "api") {
		t.Fatal("admin lost management access")
	}
}

func TestManagedSetupLocalAndExclusive(t *testing.T) {
	m := openManaged(t)
	h := m.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unexpected business access") }))
	status := managedRequest(t, h, "GET", "/auth/status", "", "", "")
	var pre struct {
		CSRF string `json:"csrf"`
	}
	_ = json.Unmarshal(status.Body.Bytes(), &pre)
	post := func(remote, host string, body string) int {
		r := httptest.NewRequest("POST", "http://"+host+"/auth/setup", strings.NewReader(body))
		r.RemoteAddr = remote
		r.Header.Set("X-Termcp-CSRF", pre.CSRF)
		for _, c := range status.Result().Cookies() {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	body := `{"username":"admin","password":"` + testPassword + `"}`
	if code := post("192.0.2.1:1234", "127.0.0.1:18765", body); code != 403 {
		t.Fatalf("remote setup: %d", code)
	}
	if code := post("127.0.0.1:1234", "other.example:18765", body); code != 403 {
		t.Fatalf("rebound setup host: %d", code)
	}
	if code := post("127.0.0.1:1234", "127.0.0.1:18765", `{"username":"admin","password":"`+testPassword+`","role":"agent"}`); code != 400 {
		t.Fatalf("client-selected role: %d", code)
	}
	var wg sync.WaitGroup
	codes := make(chan int, 4)
	for range 4 {
		wg.Go(func() { codes <- post("127.0.0.1:1234", "127.0.0.1:18765", body) })
	}
	wg.Wait()
	close(codes)
	success := 0
	for code := range codes {
		if code == 200 {
			success++
		} else if code != 409 && code != 429 {
			t.Fatalf("concurrent setup: %d", code)
		}
	}
	if success != 1 {
		t.Fatalf("successful setups: %d", success)
	}
	second, err := reopenManaged(t, m)
	if err != nil || !second.Configured() {
		t.Fatalf("persisted admin: %v", err)
	}
	if err := second.setup("replacement", testPassword); !errors.Is(err, errInitialized) {
		t.Fatal("overwrote admin", err)
	}
}

func TestManagedCorruptionFailsClosedAndLogoutDoesNotNeedStorage(t *testing.T) {
	m, token, _ := configuredManaged(t)
	cipher := filepath.Join(filepath.Dir(m.path), "auth.enc")
	if err := os.WriteFile(cipher, []byte("damaged ciphertext"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenManaged(filepath.Dir(m.path), m.vault, m.providers...); err == nil {
		t.Fatal("corrupt ciphertext became setup")
	}
	if err := os.Remove(cipher); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cipher, 0700); err != nil {
		t.Fatal(err)
	}
	id, _, err := m.verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.revoke(id); err != nil {
		t.Fatal("in-memory logout depended on storage", err)
	}
	if _, _, err := m.verify(token); err == nil {
		t.Fatal("storage error kept a logged-out browser session alive")
	}
}

func TestManagedLogoutCancelsOnlyItsOwnConnections(t *testing.T) {
	m, token, _ := configuredManaged(t)
	id, s, err := m.verify(token)
	if err != nil {
		t.Fatal(err)
	}
	ctx, done := m.track(context.Background(), id, s)
	defer done()
	other, _, err := m.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	id2, s2, err := m.verify(other)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, done2 := m.track(context.Background(), id2, s2)
	defer done2()
	if err := m.revoke(id); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("revoked connection survived")
	}
	if ctx2.Err() != nil {
		t.Fatal("revocation canceled an independent session")
	}
}
