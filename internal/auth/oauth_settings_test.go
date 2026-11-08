package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func oauthSettingsBody(t *testing.T, settings OAuthSettings) string {
	t.Helper()
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestOAuthSettingsSavedThroughAdminPageAndRestored(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	initial := managedRequest(t, h, http.MethodGet, "/auth/oauth", "", cookie, "")
	var result struct {
		Settings OAuthSettings `json:"settings"`
	}
	if initial.Code != http.StatusOK || json.Unmarshal(initial.Body.Bytes(), &result) != nil {
		t.Fatalf("read initial settings: %d %s", initial.Code, initial.Body)
	}
	if result.Settings.Enabled || result.Settings.AgentClaim != "termcp_agent_id" || result.Settings.Scope != "mcp" {
		t.Fatalf("unexpected initial settings: %+v", result.Settings)
	}
	settings := result.Settings
	settings.Enabled = true
	settings.Issuer = " https://issuer.example.test "
	settings.Resource = "https://termcp.example.test/proxy/"
	settings.JWKSURL = "https://issuer.example.test/jwks"
	saved := managedRequest(t, h, http.MethodPut, "/auth/oauth", oauthSettingsBody(t, settings), cookie, session.CSRF)
	if saved.Code != http.StatusOK || json.Unmarshal(saved.Body.Bytes(), &result) != nil {
		t.Fatalf("save settings: %d %s", saved.Code, saved.Body)
	}
	if result.Settings.Revision != 1 || result.Settings.Resource != "https://termcp.example.test/proxy" || result.Settings.Issuer != "https://issuer.example.test" {
		t.Fatalf("configuration was not normalized and versioned: %+v", result.Settings)
	}
	if stale := managedRequest(t, h, http.MethodPut, "/auth/oauth", oauthSettingsBody(t, settings), cookie, session.CSRF); stale.Code != http.StatusConflict {
		t.Fatalf("stale page overwrote configuration: %d %s", stale.Code, stale.Body)
	}
	// A normal account save must keep OAuth enabled and preserve its key cache.
	verifier := m.oauth
	if w := managedRequest(t, h, http.MethodPost, "/auth/principals", `{"name":"saved-after-oauth"}`, cookie, session.CSRF); w.Code != http.StatusOK {
		t.Fatalf("create Agent: %d %s", w.Code, w.Body)
	}
	if m.oauth != verifier {
		t.Fatal("unrelated account save discarded the OAuth verifier")
	}
	if page := managedRequest(t, h, http.MethodGet, "/oauth", "", cookie, ""); page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `id="oauth-form"`) {
		t.Fatalf("OAuth settings page unavailable: %d", page.Code)
	}
	restarted, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if !restarted.OAuthEnabled() || restarted.currentOAuthSettings() != result.Settings {
		t.Fatal("restart lost the saved OAuth configuration")
	}
	metadata := managedRequest(t, restarted.Wrap(http.NotFoundHandler()), http.MethodGet, "/.well-known/oauth-protected-resource/stream", "", "", "")
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), "https://termcp.example.test/proxy/stream") {
		t.Fatalf("saved public URL missing from discovery: %d %s", metadata.Code, metadata.Body)
	}
}

func TestOAuthSettingsRejectInvalidURLsAndRequests(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	valid := OAuthSettings{Enabled: true, OAuthConfig: OAuthConfig{
		Issuer: "https://issuer.example.test", Resource: "https://termcp.example.test", JWKSURL: "https://issuer.example.test/jwks", Scope: "mcp",
	}}
	for name, change := range map[string]func(*OAuthSettings){
		"missing issuer": func(s *OAuthSettings) { s.Issuer = "" },
		"missing all":    func(s *OAuthSettings) { s.OAuthConfig = OAuthConfig{} },
		"remote http":    func(s *OAuthSettings) { s.Issuer = "http://issuer.example.test" },
		"query":          func(s *OAuthSettings) { s.Resource += "?key=value" },
		"empty query":    func(s *OAuthSettings) { s.Resource += "?" },
		"fragment":       func(s *OAuthSettings) { s.Resource += "#fragment" },
		"userinfo":       func(s *OAuthSettings) { s.JWKSURL = "https://example:sample@issuer.example.test/jwks" },
	} {
		t.Run(name, func(t *testing.T) {
			settings := valid
			change(&settings)
			w := managedRequest(t, h, http.MethodPut, "/auth/oauth", oauthSettingsBody(t, settings), cookie, session.CSRF)
			if w.Code != http.StatusBadRequest || m.OAuthEnabled() {
				t.Fatalf("invalid configuration applied: %d %s", w.Code, w.Body)
			}
		})
	}
	for _, body := range []string{`{"unknown":true}`, `{} {}`, strings.Repeat("x", 8193)} {
		if w := managedRequest(t, h, http.MethodPut, "/auth/oauth", body, cookie, session.CSRF); w.Code != http.StatusBadRequest {
			t.Fatalf("malformed settings accepted: %d %s", w.Code, w.Body)
		}
	}
	wrongMethod := managedRequest(t, h, http.MethodPost, "/auth/oauth", "{}", cookie, session.CSRF)
	if wrongMethod.Code != http.StatusMethodNotAllowed || wrongMethod.Header().Get("Allow") != "GET, PUT" {
		t.Fatalf("unsupported method: %d %s", wrongMethod.Code, wrongMethod.Body)
	}
}

func TestOAuthSettingsPreserveAdvancedOptionsBeforeEnabling(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	h := m.Wrap(http.NotFoundHandler())
	settings := OAuthSettings{OAuthConfig: OAuthConfig{AgentClaim: "agent_id", Scope: "mcp shell"}}
	w := managedRequest(t, h, http.MethodPut, "/auth/oauth", oauthSettingsBody(t, settings), cookie, session.CSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("save advanced options: %d %s", w.Code, w.Body)
	}
	saved := m.currentOAuthSettings()
	if saved.Enabled || saved.AgentClaim != settings.AgentClaim || saved.Scope != settings.Scope || saved.Revision != 1 {
		t.Fatalf("disabled configuration discarded advanced options: %+v", saved)
	}
}

func TestOAuthSettingsChangeEndsOldAuthority(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	static := issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"oauth-settings-agent"}`)
	key, err := newOAuthTestKey("settings-key")
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwksFor(key)}})
	}))
	t.Cleanup(jwks.Close)
	cfg := OAuthConfig{Issuer: "https://issuer.example.test", Resource: "https://termcp.example.test", JWKSURL: jwks.URL, Scope: "mcp"}
	if err := m.SetOAuthConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var agentID string
	for id := range m.state.Agents {
		agentID = id
	}
	raw := signedOAuthToken(t, key, cfg.Issuer, cfg.Resource+"/stream", agentID, "mcp", time.Now().Add(time.Hour))
	r := httptest.NewRequest(http.MethodPost, cfg.Resource+"/stream", nil)
	ctx, done, status := m.admitOAuthToken(r, raw)
	if status != 0 {
		t.Fatalf("initial OAuth admission: %d", status)
	}
	t.Cleanup(done)
	stale := context.WithoutCancel(ctx)
	backgroundEnded := make(chan struct{})
	unregister, err := RegisterBackgroundWork(ctx, func() {
		_ = m.OAuthEnabled() // callback must run outside m.mu
		close(backgroundEnded)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unregister)
	invalidated := make(chan string, 1)
	m.SetMCPConnectionInvalidator(func(principal, credential string) {
		_ = m.OAuthEnabled() // registry callback may re-enter Managed
		if principal != agentID {
			t.Errorf("invalidated wrong Agent: %s", principal)
		}
		invalidated <- credential
	})
	settings := m.currentOAuthSettings()
	settings.Enabled = false
	h := m.Wrap(http.NotFoundHandler())
	w := managedRequest(t, h, http.MethodPut, "/auth/oauth", oauthSettingsBody(t, settings), cookie, session.CSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("disable OAuth: %d %s", w.Code, w.Body)
	}
	if ctx.Err() == nil {
		t.Fatal("configuration change left an old OAuth request active")
	}
	select {
	case <-backgroundEnded:
	default:
		t.Fatal("configuration change left old background work active")
	}
	select {
	case id := <-invalidated:
		if "token:"+id != CredentialIDFromContext(ctx) {
			t.Fatal("invalidated wrong OAuth credential")
		}
	default:
		t.Fatal("old MCP connection was not invalidated")
	}
	if _, _, err := PolicyFromContext(stale); err == nil {
		t.Fatal("stale OAuth authority still resolved a policy")
	}
	if _, err := RegisterBackgroundWork(stale, func() {}); err == nil {
		t.Fatal("stale OAuth authority registered new work")
	}
	if w := bearerRequest(h, http.MethodPost, "/stream", static.Token, ""); w.Code != http.StatusNotFound {
		t.Fatalf("OAuth change disrupted a static token: %d %s", w.Code, w.Body)
	}
	if w := bearerRequest(h, http.MethodPost, "/stream", raw, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("disabled OAuth still admitted a token: %d", w.Code)
	}
	settings = m.currentOAuthSettings()
	if settings.Enabled || settings.Issuer != cfg.Issuer || settings.Revision != 2 {
		t.Fatalf("disable should retain editable configuration: %+v", settings)
	}
}

func TestOAuthSettingsChangeDuringKeyFetchRejectsOldVerifier(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	_ = issueTestCredential(t, m, cookie, session, "/auth/principals", `{"name":"inflight-oauth-agent"}`)
	key, err := newOAuthTestKey("inflight-key")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{jwksFor(key)}})
	}))
	t.Cleanup(jwks.Close)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	cfg := OAuthConfig{Issuer: "https://issuer.example.test", Resource: "https://termcp.example.test", JWKSURL: jwks.URL, Scope: "mcp"}
	if err := m.SetOAuthConfig(cfg); err != nil {
		t.Fatal(err)
	}
	var agentID string
	for id := range m.state.Agents {
		agentID = id
	}
	raw := signedOAuthToken(t, key, cfg.Issuer, cfg.Resource+"/stream", agentID, "mcp", time.Now().Add(time.Hour))
	result := make(chan int, 1)
	go func() {
		_, done, status := m.admitOAuthToken(httptest.NewRequest(http.MethodPost, cfg.Resource+"/stream", nil), raw)
		if done != nil {
			done()
		}
		result <- status
	}()
	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		t.Fatal("OAuth verification did not reach JWKS")
	}
	settings := m.currentOAuthSettings()
	settings.Enabled = false
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPut, "/auth/oauth", oauthSettingsBody(t, settings), cookie, session.CSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("could not change settings during verification: %d %s", w.Code, w.Body)
	}
	once.Do(func() { close(release) })
	select {
	case status := <-result:
		if status != http.StatusUnauthorized {
			t.Fatalf("old verifier admitted an in-flight request: %d", status)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("OAuth verification did not finish")
	}
}
