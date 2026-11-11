package mcp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

type e2eOIDC struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	clientID string
	codes    map[string]e2eOIDCCode
	mu       sync.Mutex
}

type e2eOIDCCode struct {
	nonce     string
	challenge string
}

func newE2EOIDC(t *testing.T) *e2eOIDC {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := &e2eOIDC{key: key, clientID: "termcp-e2e", codes: map[string]e2eOIDCCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                provider.server.URL,
			"authorization_endpoint":                provider.server.URL + "/authorize",
			"token_endpoint":                        provider.server.URL + "/token",
			"userinfo_endpoint":                     provider.server.URL + "/userinfo",
			"jwks_uri":                              provider.server.URL + "/jwks",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		exponent := big.NewInt(int64(key.PublicKey.E)).Bytes()
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "e2e", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(exponent),
		}}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		code, _ := randomTestToken()
		provider.mu.Lock()
		provider.codes[code] = e2eOIDCCode{nonce: r.URL.Query().Get("nonce"), challenge: r.URL.Query().Get("code_challenge")}
		provider.mu.Unlock()
		target, _ := url.Parse(r.URL.Query().Get("redirect_uri"))
		values := target.Query()
		values.Set("code", code)
		values.Set("state", r.URL.Query().Get("state"))
		target.RawQuery = values.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		provider.mu.Lock()
		code, ok := provider.codes[r.Form.Get("code")]
		delete(provider.codes, r.Form.Get("code"))
		provider.mu.Unlock()
		hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(hash[:]) != code.challenge {
			http.Error(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		now := time.Now().UTC()
		idToken, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
			"iss": provider.server.URL, "sub": "e2e-user", "aud": provider.clientID,
			"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "nonce": code.nonce,
			"name": "E2E User", "preferred_username": "e2e",
		}).SignedString(provider.key)
		if err != nil {
			http.Error(w, "signing failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "upstream-access", "token_type": "Bearer", "id_token": idToken,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub": "e2e-user", "name": "E2E User", "preferred_username": "e2e",
		})
	})
	provider.server = httptest.NewServer(mux)
	t.Cleanup(provider.server.Close)
	return provider
}

func TestBuiltinOAuthStreamableHTTPInitializesAndRefreshes(t *testing.T) {
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

	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, nil)
	t.Cleanup(func() {
		sessMgr.MarkAllDead()
		_ = store.Close()
	})
	mcpServer := New(sessMgr, msgMgr, vault, forward.NewForwardManager(), "test")
	account.SetMCPConnectionInvalidator(func(principalID, credentialID string) {
		go mcpServer.InvalidateConnections(principalID, credentialID)
	})
	mux := http.NewServeMux()
	mux.Handle("/stream", mcpServer.StreamableHTTPHandler())
	server := httptest.NewServer(account.Wrap(mux))
	t.Cleanup(server.Close)

	idp := newE2EOIDC(t)
	jar, _ := cookiejar.New(nil)
	admin := &http.Client{Jar: jar, Timeout: 20 * time.Second}
	statusResponse, err := admin.Get(server.URL + "/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		CSRF string `json:"csrf"`
	}
	if err := json.NewDecoder(statusResponse.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	statusResponse.Body.Close()
	adminDo := func(method, path, body string) *http.Response {
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", server.URL)
		request.Header.Set("X-Termcp-CSRF", status.CSRF)
		response, err := admin.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		_ = json.NewDecoder(response.Body).Decode(&decoded)
		response.Body.Close()
		if csrf, _ := decoded["csrf"].(string); csrf != "" {
			status.CSRF = csrf
		}
		if response.StatusCode >= 300 {
			t.Fatalf("%s %s: %d %#v", method, path, response.StatusCode, decoded)
		}
		return response
	}
	adminDo(http.MethodPost, "/auth/setup", `{"password":"oauth-e2e-password","config_password":"oauth-e2e-password"}`)
	settings := `{"enabled":true,"revision":0,"public_url":"` + server.URL + `","oidc_issuer":"` + idp.server.URL + `","oidc_client_id":"` + idp.clientID + `","oidc_client_secret":"secret"}`
	adminDo(http.MethodPut, "/auth/oauth", settings)

	registration := `{"redirect_uris":["http://127.0.0.1:45678/callback"],"client_name":"MCP E2E","token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"],"application_type":"native"}`
	registerRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/oauth/register", strings.NewReader(registration))
	registerRequest.Header.Set("Content-Type", "application/json")
	registerResponse, err := http.DefaultClient.Do(registerRequest)
	if err != nil {
		t.Fatal(err)
	}
	var registered struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(registerResponse.Body).Decode(&registered); err != nil {
		t.Fatal(err)
	}
	registerResponse.Body.Close()

	verifier := strings.Repeat("v", 64)
	hash := sha256.Sum256([]byte(verifier))
	redirectURI := "http://127.0.0.1:45679/callback"
	authorizeURL := server.URL + "/oauth/authorize?" + url.Values{
		"response_type": {"code"}, "client_id": {registered.ClientID}, "redirect_uri": {redirectURI},
		"scope": {"mcp offline_access"}, "state": {"mcp-state-123"},
		"resource": {server.URL + "/stream"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])},
		"code_challenge_method": {"S256"},
	}.Encode()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	authorizeResponse, err := noRedirect.Get(authorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	authorizeResponse.Body.Close()
	if authorizeResponse.StatusCode != http.StatusFound {
		t.Fatalf("authorize status = %d", authorizeResponse.StatusCode)
	}
	upstreamResponse, err := noRedirect.Get(authorizeResponse.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	upstreamResponse.Body.Close()
	callback, _ := url.Parse(upstreamResponse.Header.Get("Location"))
	state := callback.Query().Get("state")
	var browserCookie *http.Cookie
	for _, cookie := range authorizeResponse.Cookies() {
		if strings.HasPrefix(cookie.Name, "termcp_oauth_browser_") {
			browserCookie = cookie
		}
	}
	if browserCookie == nil {
		t.Fatal("authorize response had no browser binding cookie")
	}
	callbackRequest, _ := http.NewRequest(http.MethodGet, callback.String(), nil)
	callbackRequest.AddCookie(browserCookie)
	callbackResponse, err := http.DefaultClient.Do(callbackRequest)
	if err != nil {
		t.Fatal(err)
	}
	consentBody, _ := io.ReadAll(callbackResponse.Body)
	callbackResponse.Body.Close()
	if callbackResponse.StatusCode != http.StatusOK || !strings.Contains(string(consentBody), "E2E User") {
		t.Fatalf("consent: %d %s", callbackResponse.StatusCode, consentBody)
	}
	csrf := e2eHiddenValue(string(consentBody), "csrf")
	form := url.Values{"state": {state}, "csrf": {csrf}, "decision": {"approve"}}
	continueRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/oauth/continue", strings.NewReader(form.Encode()))
	continueRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	continueRequest.Header.Set("Origin", server.URL)
	continueRequest.AddCookie(browserCookie)
	continueResponse, err := noRedirect.Do(continueRequest)
	if err != nil {
		t.Fatal(err)
	}
	continueResponse.Body.Close()
	if continueResponse.StatusCode != http.StatusFound {
		t.Fatalf("continue status = %d", continueResponse.StatusCode)
	}
	clientCallback, _ := url.Parse(continueResponse.Header.Get("Location"))
	code := clientCallback.Query().Get("code")
	tokenForm := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {registered.ClientID}, "code": {code},
		"redirect_uri": {redirectURI}, "code_verifier": {verifier}, "resource": {server.URL + "/stream"},
	}
	tokens := e2eExchangeToken(t, server.URL, tokenForm)
	accessToken, _ := tokens["access_token"].(string)
	refreshToken, _ := tokens["refresh_token"].(string)

	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"oauth-e2e","version":"1"}}}`
	initRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/stream", strings.NewReader(init))
	initRequest.Header.Set("Authorization", "Bearer "+accessToken)
	initRequest.Header.Set("Content-Type", "application/json")
	initRequest.Header.Set("Accept", "application/json, text/event-stream")
	initResponse, err := http.DefaultClient.Do(initRequest)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, initResponse.Body)
	initResponse.Body.Close()
	if initResponse.StatusCode != http.StatusOK {
		t.Fatalf("MCP initialize = %d", initResponse.StatusCode)
	}
	sessionID := initResponse.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("MCP initialize returned no session id")
	}
	connection := waitForConnection(t, mcpServer, func(connection Connection) bool {
		return strings.HasPrefix(connection.CredentialID, "oauth-")
	})
	if connection.Transport != ConnectionTransportStreamableHTTP {
		t.Fatalf("OAuth connection transport = %+v", connection)
	}

	refreshed := e2eExchangeToken(t, server.URL, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {registered.ClientID},
		"refresh_token": {refreshToken}, "resource": {server.URL + "/stream"},
	})
	newAccess, _ := refreshed["access_token"].(string)
	pingRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/stream", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	pingRequest.Header.Set("Authorization", "Bearer "+newAccess)
	pingRequest.Header.Set("Content-Type", "application/json")
	pingRequest.Header.Set("Accept", "application/json, text/event-stream")
	pingRequest.Header.Set("Mcp-Session-Id", sessionID)
	pingResponse, err := http.DefaultClient.Do(pingRequest)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, pingResponse.Body)
	pingResponse.Body.Close()
	if pingResponse.StatusCode != http.StatusOK {
		t.Fatalf("refreshed OAuth token could not continue MCP session: %d", pingResponse.StatusCode)
	}
	refreshedConnection := waitForConnection(t, mcpServer, func(connection Connection) bool {
		return connection.SessionID == sessionID
	})
	if refreshedConnection.CredentialID == "" || refreshedConnection.CredentialID == connection.CredentialID {
		t.Fatalf("MCP connection did not follow refreshed credential: before=%+v after=%+v", connection, refreshedConnection)
	}
	adminDo(http.MethodPatch, "/auth/principals/"+refreshedConnection.PrincipalID, `{"revision":1,"enabled":false}`)
	waitForCredentialAbsent(t, mcpServer, refreshedConnection.CredentialID)
	disabledRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/stream", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"ping"}`))
	disabledRequest.Header.Set("Authorization", "Bearer "+newAccess)
	disabledRequest.Header.Set("Content-Type", "application/json")
	disabledRequest.Header.Set("Accept", "application/json, text/event-stream")
	disabledRequest.Header.Set("Mcp-Session-Id", sessionID)
	disabledResponse, err := http.DefaultClient.Do(disabledRequest)
	if err != nil {
		t.Fatal(err)
	}
	disabledResponse.Body.Close()
	if disabledResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("disabled OAuth Agent request status = %d, want 401", disabledResponse.StatusCode)
	}
}

func randomTestToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func e2eExchangeToken(t *testing.T, base string, form url.Values) map[string]any {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, base+"/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("token endpoint: %d %#v", response.StatusCode, result)
	}
	return result
}

func e2eHiddenValue(body, name string) string {
	needle := `name="` + name + `" value="`
	index := strings.Index(body, needle)
	if index < 0 {
		return ""
	}
	value := body[index+len(needle):]
	end := strings.IndexByte(value, '"')
	if end < 0 {
		return ""
	}
	return value[:end]
}
