package mcp

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

func TestOAuthTokenInitializesAttributedMCPConnection(t *testing.T) {
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
	account.SetMCPConnectionInvalidator(func(principalID, credentialID string) {
		go s.InvalidateConnections(principalID, credentialID)
	})
	mux := http.NewServeMux()
	mux.Handle("/stream", s.StreamableHTTPHandler())
	server := httptest.NewServer(account.Wrap(mux))
	t.Cleanup(server.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{testRSAJWK(key, "test-key")}})
	}))
	t.Cleanup(jwks.Close)

	const issuer = "https://issuer.example.test"
	jar, _ := cookiejar.New(nil)
	admin := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	status, err := admin.Get(server.URL + "/auth/status")
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		CSRF string `json:"csrf"`
	}
	if err := json.NewDecoder(status.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	status.Body.Close()
	do := func(method, path, body string) map[string]any {
		request, _ := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		request.Header.Set("Origin", server.URL)
		request.Header.Set("X-Termcp-CSRF", state.CSRF)
		request.Header.Set("Content-Type", "application/json")
		response, err := admin.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var result map[string]any
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %#v", path, response.StatusCode, result)
		}
		if value, ok := result["csrf"].(string); ok && value != "" {
			state.CSRF = value
		}
		return result
	}
	do(http.MethodPost, "/auth/setup", `{"password":"oauth-transport-password","config_password":"oauth-transport-password"}`)
	settings := auth.OAuthSettings{Enabled: true, OAuthConfig: auth.OAuthConfig{
		Issuer: issuer, Resource: server.URL, JWKSURL: jwks.URL,
		AgentClaim: "termcp_agent_id", Scope: "mcp",
	}}
	saveOAuth := func() {
		body, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		result := do(http.MethodPut, "/auth/oauth", string(body))
		settings.Revision = uint64(result["settings"].(map[string]any)["revision"].(float64))
	}
	saveOAuth()
	created := do(http.MethodPost, "/auth/principals", `{"name":"oauth-agent"}`)
	principalID := created["principal"].(map[string]any)["id"].(string)

	resource := server.URL + "/stream"
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": issuer, "aud": resource, "resource": resource,
		"scope": "mcp", "termcp_agent_id": principalID,
		"iat": time.Now().Add(-time.Minute).Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	token.Header["kid"] = "test-key"
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"oauth-test","version":"1"}}}`
	request, _ := http.NewRequest(http.MethodPost, resource, strings.NewReader(init))
	request.Header.Set("Authorization", "Bearer "+signed)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("OAuth initialize: %d", response.StatusCode)
	}

	connection := waitForConnection(t, s, func(connection Connection) bool {
		return connection.PrincipalID == principalID
	})
	if connection.AgentName != "oauth-agent" || connection.Transport != ConnectionTransportStreamableHTTP ||
		!strings.HasPrefix(connection.CredentialID, "oauth-") {
		t.Fatalf("OAuth connection attribution = %+v", connection)
	}

	// Saving through the settings entrance must revoke the real transport, not
	// only flip discovery or prevent the next HTTP request from authenticating.
	settings.Enabled = false
	saveOAuth()
	waitForCredentialAbsent(t, s, connection.CredentialID)
	settings.Enabled = true
	saveOAuth()
	request, _ = http.NewRequest(http.MethodPost, resource, strings.NewReader(init))
	request.Header.Set("Authorization", "Bearer "+signed)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("re-enabled OAuth initialize: %d", response.StatusCode)
	}
	waitForConnection(t, s, func(connection Connection) bool { return connection.PrincipalID == principalID })

	disable, _ := http.NewRequest(http.MethodPatch, server.URL+"/auth/principals/"+principalID,
		strings.NewReader(`{"revision":1,"enabled":false}`))
	disable.Header.Set("Origin", server.URL)
	disable.Header.Set("X-Termcp-CSRF", state.CSRF)
	disable.Header.Set("Content-Type", "application/json")
	response, err = admin.Do(disable)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("disable OAuth Agent: %d", response.StatusCode)
	}
	waitForCredentialAbsent(t, s, connection.CredentialID)
}

func testRSAJWK(key *rsa.PrivateKey, kid string) map[string]any {
	exponent := big.NewInt(int64(key.PublicKey.E)).Bytes()
	return map[string]any{
		"kty": "RSA", "kid": kid, "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(exponent),
	}
}
