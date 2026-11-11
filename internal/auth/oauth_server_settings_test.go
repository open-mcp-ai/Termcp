package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/oauthserver"
)

func TestOAuthServerSettingsAreWriteOnlyAndReplaceLegacyOAuthKey(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	handler := m.Wrap(http.NotFoundHandler())
	input := `{"enabled":true,"revision":0,"public_url":"https://termcp.example.test/termcp","oidc_issuer":"https://id.example.test","oidc_client_id":"termcp","oidc_client_secret":"top-secret"}`
	response := managedRequest(t, handler, http.MethodPut, "/auth/oauth", input, cookie, session.CSRF)
	if response.Code != http.StatusOK {
		t.Fatalf("save settings: %d %s", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), "top-secret") {
		t.Fatal("settings response exposed the OIDC secret")
	}
	var view struct {
		Settings oauthServerView `json:"settings"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if !view.Settings.OIDCSecretSet || view.Settings.CallbackURL != "https://termcp.example.test/termcp/oauth/oidc/callback" {
		t.Fatalf("unexpected settings view: %+v", view.Settings)
	}

	legacyInput := `{"enabled":true,"revision":1,"issuer":"https://issuer.example.test","resource":"https://termcp.example.test","jwks_url":"https://issuer.example.test/jwks"}`
	if response := managedRequest(t, handler, http.MethodPut, "/auth/oauth", legacyInput, cookie, session.CSRF); response.Code != http.StatusBadRequest {
		t.Fatalf("legacy OAuth settings were accepted: %d %s", response.Code, response.Body)
	}

	raw, err := m.vault.ReadAuthentication()
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if _, exists := stored["oauth"]; exists {
		t.Fatal("normal save wrote the retired oauth key")
	}
	stored["oauth"] = json.RawMessage(`{"enabled":true,"issuer":"https://old.example.test"}`)
	legacyState, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.vault.WriteAuthentication(legacyState); err != nil {
		t.Fatal(err)
	}
	reopened, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	reopened.mu.Lock()
	defer reopened.mu.Unlock()
	if reopened.state.OAuthServer != view.Settings.serverSettings() {
		t.Fatalf("OAuth settings were not preserved across legacy key removal: %+v", reopened.state.OAuthServer)
	}
}

func (v oauthServerView) serverSettings() OAuthServerSettings {
	return OAuthServerSettings{
		Enabled:      v.Enabled,
		Revision:     v.Revision,
		PublicURL:    v.PublicURL,
		OIDCIssuer:   v.OIDCIssuer,
		OIDCClientID: v.OIDCClientID,
		OIDCClientSecret: func() string {
			if v.OIDCSecretSet {
				return "top-secret"
			}
			return ""
		}(),
	}
}

func TestCIMDClientMetadataFetchValidationAndCache(t *testing.T) {
	var fetches int
	var metadataURL string
	metadataServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches++
		w.Header().Set("Cache-Control", "max-age=60")
		_ = json.NewEncoder(w).Encode(oauthserver.ClientMetadata{
			ClientID:                metadataURL,
			ClientName:              "CIMD client",
			RedirectURIs:            []string{"http://127.0.0.1:9876/callback"},
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		})
	}))
	t.Cleanup(metadataServer.Close)
	metadataURL = metadataServer.URL + "/client.json"
	fetcher := oauthserver.NewFetcher()
	fetcher.AllowPrivate = true
	fetcher.Client = metadataServer.Client()
	fetcher.Client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	m, _, _ := configuredManaged(t)
	m.setOAuthFetcher(fetcher)
	client, err := m.resolveOAuthClient(t.Context(), metadataURL)
	if err != nil {
		t.Fatal(err)
	}
	if client.Name != "CIMD client" || client.Registration != oauthserver.ClientRegistrationCIMD {
		t.Fatalf("resolved CIMD client = %+v", client)
	}
	if _, err := m.resolveOAuthClient(t.Context(), metadataURL); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("CIMD metadata fetched %d times, want cache hit", fetches)
	}
}

func TestCIMDRejectsMismatchedClientID(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(oauthserver.ClientMetadata{
			ClientID:     "https://other.example/client.json",
			ClientName:   "bad",
			RedirectURIs: []string{"https://client.example/callback"},
		})
	}))
	t.Cleanup(server.Close)
	clientURL := server.URL + "/client.json"
	fetcher := oauthserver.NewFetcher()
	fetcher.AllowPrivate = true
	fetcher.Client = server.Client()
	_, err := fetcher.ClientMetadata(t.Context(), clientURL)
	if err == nil {
		t.Fatal("mismatched CIMD client_id was accepted")
	}
}
