package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type testOIDCProvider struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	clientID string

	mu          sync.Mutex
	codes       map[string]testOIDCLogin
	access      map[string]string
	redirectURI string
}

type testOIDCLogin struct {
	nonce       string
	challenge   string
	redirectURI string
	subject     string
}

func newTestOIDCProvider(t *testing.T) *testOIDCProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := &testOIDCProvider{
		key:      key,
		clientID: "termcp-web",
		codes:    map[string]testOIDCLogin{},
		access:   map[string]string{},
	}
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
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{rsaJWK(&provider.key.PublicKey, "oidc-test")}})
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("client_id") != provider.clientID || query.Get("response_type") != "code" {
			http.Error(w, "bad authorize", http.StatusBadRequest)
			return
		}
		code, _ := randomSecret()
		redirect := query.Get("redirect_uri")
		provider.mu.Lock()
		provider.codes[code] = testOIDCLogin{
			nonce:       query.Get("nonce"),
			challenge:   query.Get("code_challenge"),
			redirectURI: redirect,
			subject:     "subject-1",
		}
		provider.redirectURI = redirect
		provider.mu.Unlock()
		target, _ := url.Parse(redirect)
		values := target.Query()
		values.Set("code", code)
		values.Set("state", query.Get("state"))
		target.RawQuery = values.Encode()
		http.Redirect(w, r, target.String(), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		provider.mu.Lock()
		login, ok := provider.codes[r.Form.Get("code")]
		delete(provider.codes, r.Form.Get("code"))
		provider.mu.Unlock()
		clientID, _, _ := r.BasicAuth()
		if clientID == "" {
			clientID = r.Form.Get("client_id")
		}
		if !ok || clientID != provider.clientID ||
			r.Form.Get("redirect_uri") != login.redirectURI {
			http.Error(w, "invalid_grant", http.StatusBadRequest)
			return
		}
		hash := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(hash[:]) != login.challenge {
			http.Error(w, "invalid_verifier", http.StatusBadRequest)
			return
		}
		now := time.Now().UTC()
		claims := jwt.MapClaims{
			"iss":                provider.server.URL,
			"sub":                login.subject,
			"aud":                provider.clientID,
			"exp":                now.Add(time.Hour).Unix(),
			"iat":                now.Unix(),
			"nonce":              login.nonce,
			"name":               "Ada Example",
			"preferred_username": "ada",
		}
		idToken, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(provider.key)
		if err != nil {
			http.Error(w, "signing failed", http.StatusInternalServerError)
			return
		}
		access, _ := randomSecret()
		provider.mu.Lock()
		provider.access[access] = login.subject
		provider.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": access,
			"token_type":   "Bearer",
			"expires_in":   3600,
			"id_token":     idToken,
		})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		provider.mu.Lock()
		subject := provider.access[token]
		provider.mu.Unlock()
		if subject == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sub":                subject,
			"name":               "Ada Example",
			"preferred_username": "ada",
		})
	})
	provider.server = httptest.NewServer(mux)
	t.Cleanup(provider.server.Close)
	return provider
}

func rsaJWK(key *rsa.PublicKey, kid string) map[string]any {
	exponent := big.NewInt(int64(key.E)).Bytes()
	return map[string]any{
		"kty": "RSA",
		"kid": kid,
		"alg": "RS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(exponent),
	}
}

func TestBuiltinOAuthURLLoginAgentProvisionRefreshAndReuseRevocation(t *testing.T) {
	m, cookie, session := configuredManaged(t)
	m.enableOAuthTestMode()
	oidc := newTestOIDCProvider(t)
	public := httptest.NewServer(m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := ActorFromContext(r.Context())
		if !ok || actor.Role != RoleAgent {
			t.Error("OAuth request did not reach the business handler with an Agent")
		}
		if OAuthGrantIDFromContext(r.Context()) == "" {
			t.Error("OAuth request has no grant identity")
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	t.Cleanup(public.Close)

	settings := fmt.Sprintf(`{"enabled":true,"revision":0,"public_url":%q,"oidc_issuer":%q,"oidc_client_id":%q,"oidc_client_secret":"test-secret"}`, public.URL, oidc.server.URL, oidc.clientID)
	w := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPut, "/auth/oauth", settings, cookie, session.CSRF)
	if w.Code != http.StatusOK {
		t.Fatalf("enable OAuth: %d %s", w.Code, w.Body)
	}

	metadata := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodGet, "/.well-known/oauth-authorization-server", "", "", "")
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"client_id_metadata_document_supported":true`) {
		t.Fatalf("authorization metadata: %d %s", metadata.Code, metadata.Body)
	}
	protected := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodGet, "/.well-known/oauth-protected-resource", "", "", "")
	if protected.Code != http.StatusOK || !strings.Contains(protected.Body.String(), public.URL+"/stream") {
		t.Fatalf("protected resource metadata: %d %s", protected.Code, protected.Body)
	}

	registeredRedirectURI := "http://127.0.0.1:45678/callback"
	redirectURI := "http://127.0.0.1:45679/callback"
	registration := fmt.Sprintf(`{"redirect_uris":[%q],"client_name":"Test MCP client","token_endpoint_auth_method":"none","grant_types":["authorization_code","refresh_token"],"response_types":["code"],"application_type":"native"}`, registeredRedirectURI)
	registered := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPost, "/oauth/register", registration, "", "")
	if registered.Code != http.StatusCreated {
		t.Fatalf("DCR: %d %s", registered.Code, registered.Body)
	}
	var clientResponse struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(registered.Body.Bytes(), &clientResponse); err != nil || clientResponse.ClientID == "" {
		t.Fatalf("DCR response: %s", registered.Body)
	}

	denialVerifier := strings.Repeat("d", 64)
	denialHash := sha256.Sum256([]byte(denialVerifier))
	denialURL := public.URL + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientResponse.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"mcp offline_access"},
		"state":                 {"denied-state-123"},
		"resource":              {public.URL + "/stream"},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(denialHash[:])},
		"code_challenge_method": {"S256"},
	}.Encode()
	deniedCookies, deniedState, deniedCSRF := oauthConsentForURL(t, m, denialURL)
	denialForm := url.Values{"state": {deniedState}, "csrf": {deniedCSRF}, "decision": {"deny"}}
	denialRequest := httptest.NewRequest(http.MethodPost, public.URL+"/oauth/continue", strings.NewReader(denialForm.Encode()))
	denialRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	denialRequest.Header.Set("Origin", public.URL)
	for _, c := range deniedCookies {
		if c.Name == oauthBrowserCookieName(deniedState) {
			denialRequest.AddCookie(c)
		}
	}
	denialResponse := httptest.NewRecorder()
	m.Wrap(http.NotFoundHandler()).ServeHTTP(denialResponse, denialRequest)
	if denialResponse.Code != http.StatusFound || !strings.Contains(denialResponse.Header().Get("Location"), "error=access_denied") {
		t.Fatalf("deny consent: %d %s", denialResponse.Code, denialResponse.Header().Get("Location"))
	}
	m.mu.Lock()
	if len(m.state.Agents) != 0 || len(m.state.OAuthGrants) != 0 || len(m.state.LoginUsers) != 0 {
		t.Fatalf("denied consent provisioned state: agents=%d grants=%d users=%d", len(m.state.Agents), len(m.state.OAuthGrants), len(m.state.LoginUsers))
	}
	m.mu.Unlock()

	verifier := strings.Repeat("v", 64)
	challengeHash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])
	authorizeURL := public.URL + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientResponse.ClientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"mcp offline_access"},
		"state":                 {"client-state-123"},
		"resource":              {public.URL + "/stream"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	recorder := httptest.NewRecorder()
	m.Wrap(http.NotFoundHandler()).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, authorizeURL, nil))
	if recorder.Code != http.StatusFound {
		t.Fatalf("authorize recorder: %d %s", recorder.Code, recorder.Body)
	}
	cookies := recorder.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("authorize returned no browser-binding cookie")
	}
	upstreamAuth, _ := url.Parse(recorder.Header().Get("Location"))
	upstreamResponse, err := noRedirect.Get(upstreamAuth.String())
	if err != nil {
		t.Fatal(err)
	}
	upstreamResponse.Body.Close()
	callback, _ := url.Parse(upstreamResponse.Header.Get("Location"))
	callbackState := callback.Query().Get("state")
	type callbackOutcome struct {
		status int
		body   string
	}
	callbackResults := make(chan callbackOutcome, 2)
	for range 2 {
		go func() {
			callbackRequest := httptest.NewRequest(http.MethodGet, callback.String(), nil)
			for _, c := range cookies {
				if c.Name == oauthBrowserCookieName(callbackState) {
					callbackRequest.AddCookie(c)
				}
			}
			recorder := httptest.NewRecorder()
			m.Wrap(http.NotFoundHandler()).ServeHTTP(recorder, callbackRequest)
			callbackResults <- callbackOutcome{status: recorder.Code, body: recorder.Body.String()}
		}()
	}
	var consentBody string
	callbackSuccesses := 0
	for range 2 {
		result := <-callbackResults
		if result.status == http.StatusOK && strings.Contains(result.body, "Ada Example") {
			callbackSuccesses++
			consentBody = result.body
		}
	}
	if callbackSuccesses != 1 {
		t.Fatalf("duplicate OIDC callback successes = %d", callbackSuccesses)
	}
	csrf := hiddenInputValue(t, consentBody, "csrf")

	form := url.Values{
		"state":    {callbackState},
		"csrf":     {csrf},
		"decision": {"approve"},
	}
	continueRequest := httptest.NewRequest(http.MethodPost, public.URL+"/oauth/continue", strings.NewReader(form.Encode()))
	continueRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	continueRequest.Header.Set("Origin", public.URL)
	for _, c := range cookies {
		if c.Name == oauthBrowserCookieName(callbackState) {
			continueRequest.AddCookie(c)
		}
	}
	continued := httptest.NewRecorder()
	m.Wrap(http.NotFoundHandler()).ServeHTTP(continued, continueRequest)
	if continued.Code != http.StatusFound {
		t.Fatalf("consent approval: %d %s", continued.Code, continued.Body)
	}
	clientCallback, _ := url.Parse(continued.Header().Get("Location"))
	if clientCallback.Query().Get("iss") != public.URL || clientCallback.Query().Get("state") != "client-state-123" {
		t.Fatalf("authorization response: %s", clientCallback)
	}
	code := clientCallback.Query().Get("code")
	if code == "" {
		t.Fatal("authorization response has no code")
	}
	m.mu.Lock()
	codeState, _ := json.Marshal(m.state)
	m.mu.Unlock()
	if strings.Contains(string(codeState), code) {
		t.Fatal("authorization code was persisted in plaintext")
	}

	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientResponse.ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		"resource":      {public.URL + "/stream"},
	}
	wrongResource := cloneValues(tokenForm)
	wrongResource.Set("resource", public.URL+"/other")
	if status := exchangeOAuthTokenStatus(t, m, wrongResource); status != http.StatusBadRequest {
		t.Fatalf("wrong token resource status = %d, want 400", status)
	}
	wrongPKCE := cloneValues(tokenForm)
	wrongPKCE.Set("code_verifier", strings.Repeat("x", 64))
	if status := exchangeOAuthTokenStatus(t, m, wrongPKCE); status != http.StatusBadRequest {
		t.Fatalf("wrong PKCE status = %d, want 400", status)
	}
	tokens := concurrentOAuthCodeExchange(t, m, tokenForm)
	if tokens["access_token"] == "" || tokens["refresh_token"] == "" {
		t.Fatalf("token response: %#v", tokens)
	}
	accessToken, _ := tokens["access_token"].(string)
	refreshToken, _ := tokens["refresh_token"].(string)
	m.mu.Lock()
	persisted, err := json.Marshal(m.state)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(persisted), accessToken) || strings.Contains(string(persisted), refreshToken) {
		t.Fatal("encrypted auth state persisted a raw OAuth credential")
	}

	m.mu.Lock()
	if len(m.state.LoginUsers) != 1 || len(m.state.Agents) != 1 || len(m.state.OAuthGrants) != 1 {
		t.Fatalf("provisioned state users=%d agents=%d grants=%d", len(m.state.LoginUsers), len(m.state.Agents), len(m.state.OAuthGrants))
	}
	var agent agentRecord
	for _, candidate := range m.state.Agents {
		agent = candidate
	}
	if agent.OwnerUserID == "" || agent.Origin != agentOriginOAuth || agent.GroupID != m.state.DefaultGroupID {
		t.Fatalf("provisioned Agent = %+v", agent)
	}
	m.mu.Unlock()
	principalResponse := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodGet, "/auth/principals", "", cookie, session.CSRF)
	if principalResponse.Code != http.StatusOK {
		t.Fatalf("list OAuth Agent: %d %s", principalResponse.Code, principalResponse.Body)
	}
	var principalList struct {
		Principals []struct {
			ID                          string   `json:"id"`
			Origin                      string   `json:"origin"`
			OwnerIssuer                 string   `json:"owner_issuer"`
			ActiveOAuthGrantCount       int      `json:"active_oauth_grant_count"`
			OAuthClients                []string `json:"oauth_clients"`
			StaticCredentialCount       int      `json:"static_credential_count"`
			ActiveStaticCredentialCount int      `json:"active_static_credential_count"`
		} `json:"principals"`
	}
	if err := json.Unmarshal(principalResponse.Body.Bytes(), &principalList); err != nil {
		t.Fatal(err)
	}
	if len(principalList.Principals) != 1 {
		t.Fatalf("OAuth principal list = %+v", principalList.Principals)
	}
	oauthView := principalList.Principals[0]
	if oauthView.ID != agent.ID || oauthView.Origin != agentOriginOAuth || oauthView.OwnerIssuer == "" ||
		oauthView.ActiveOAuthGrantCount != 1 || len(oauthView.OAuthClients) != 1 ||
		oauthView.StaticCredentialCount != 0 || oauthView.ActiveStaticCredentialCount != 0 {
		t.Fatalf("OAuth principal view conflates origin and credentials: %+v", oauthView)
	}

	accessRequest := httptest.NewRequest(http.MethodPost, public.URL+"/stream", nil)
	accessRequest.Header.Set("Authorization", "Bearer "+accessToken)
	accessResponse := httptest.NewRecorder()
	m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(accessResponse, accessRequest)
	if accessResponse.Code != http.StatusNoContent {
		t.Fatalf("OAuth MCP request: %d %s", accessResponse.Code, accessResponse.Body)
	}
	baseVault := m.vault
	m.vault = lockOverrideVault{Vault: baseVault, locked: true}
	assertOAuthBearerStatus(t, m, public.URL, accessToken, http.StatusLocked)
	m.vault = baseVault

	// Permission changes take effect immediately without rotating credentials or
	// ending the authenticated identity. Admission remains valid; the business
	// policy is consulted by the MCP operation path.
	m.mu.Lock()
	initialGroup := m.state.Groups[agent.GroupID]
	accessRevision := m.state.AccessRevision
	m.mu.Unlock()
	disableGroup := fmt.Sprintf(`{"revision":%d,"enabled":false}`, initialGroup.Revision)
	if response := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPatch, "/auth/groups/"+initialGroup.ID, disableGroup, cookie, session.CSRF); response.Code != http.StatusOK {
		t.Fatalf("disable OAuth Agent group: %d %s", response.Code, response.Body)
	}
	assertOAuthBearerStatus(t, m, public.URL, accessToken, http.StatusNoContent)
	enableGroup := fmt.Sprintf(`{"revision":%d,"enabled":true}`, initialGroup.Revision+1)
	if response := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPatch, "/auth/groups/"+initialGroup.ID, enableGroup, cookie, session.CSRF); response.Code != http.StatusOK {
		t.Fatalf("re-enable OAuth Agent group: %d %s", response.Code, response.Body)
	}
	assertOAuthBearerStatus(t, m, public.URL, accessToken, http.StatusNoContent)
	setDefault := fmt.Sprintf(`{"group_id":"preset-viewer","revision":%d}`, accessRevision+2)
	if response := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodPut, "/auth/default-group", setDefault, cookie, session.CSRF); response.Code != http.StatusOK {
		t.Fatalf("change default group: %d %s", response.Code, response.Body)
	}
	m.mu.Lock()
	if got := m.state.Agents[agent.ID].GroupID; got != initialGroup.ID {
		t.Errorf("default-group change reset existing OAuth Agent group: got %s want %s", got, initialGroup.ID)
	}
	m.mu.Unlock()
	assertOAuthBearerStatus(t, m, public.URL, accessToken, http.StatusNoContent)

	refreshForm := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientResponse.ClientID},
		"refresh_token": {refreshToken},
		"resource":      {public.URL + "/stream"},
	}
	refreshed := exchangeOAuthToken(t, m, refreshForm)
	if refreshed["access_token"] == "" || refreshed["refresh_token"] == "" || refreshed["refresh_token"] == refreshToken {
		t.Fatalf("refresh did not rotate: %#v", refreshed)
	}
	refreshedAccess, _ := refreshed["access_token"].(string)
	restarted, err := reopenManaged(t, m)
	if err != nil {
		t.Fatal(err)
	}
	restartedRequest := httptest.NewRequest(http.MethodPost, public.URL+"/stream", nil)
	restartedRequest.Header.Set("Authorization", "Bearer "+refreshedAccess)
	restartedResponse := httptest.NewRecorder()
	restarted.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(restartedResponse, restartedRequest)
	if restartedResponse.Code != http.StatusNoContent {
		t.Fatalf("restart rejected the rotated OAuth token: %d %s", restartedResponse.Code, restartedResponse.Body)
	}
	reused := exchangeOAuthTokenStatus(t, restarted, refreshForm)
	if reused != http.StatusBadRequest {
		t.Fatalf("refresh reuse status = %d, want 400", reused)
	}
	restarted.mu.Lock()
	for _, grant := range restarted.state.OAuthGrants {
		if grant.RevokedAt == nil {
			t.Error("refresh reuse did not revoke the grant")
		}
	}
	restarted.mu.Unlock()
	afterReuse, err := reopenManaged(t, restarted)
	if err != nil {
		t.Fatal(err)
	}
	afterReuse.mu.Lock()
	var revokeGrantID, disableUserID string
	disableRevision := uint64(0)
	for _, grant := range afterReuse.state.OAuthGrants {
		if grant.RevokedAt == nil {
			t.Error("restart forgot refresh-family revocation")
		}
		revokeGrantID = grant.ID
		user := afterReuse.state.LoginUsers[grant.UserID]
		disableUserID = user.ID
		disableRevision = user.Revision
	}
	afterReuse.mu.Unlock()
	adminToken, adminSession, err := afterReuse.login("admin", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if response := managedRequest(t, afterReuse.Wrap(http.NotFoundHandler()), http.MethodDelete, "/auth/oauth/grants/"+revokeGrantID, "", adminToken, adminSession.CSRF); response.Code != http.StatusOK {
		t.Fatalf("admin revoke grant: %d %s", response.Code, response.Body)
	}
	disableUser := fmt.Sprintf(`{"enabled":false,"revision":%d}`, disableRevision)
	if response := managedRequest(t, afterReuse.Wrap(http.NotFoundHandler()), http.MethodPatch, "/auth/oauth/users/"+disableUserID, disableUser, adminToken, adminSession.CSRF); response.Code != http.StatusOK {
		t.Fatalf("admin disable user: %d %s", response.Code, response.Body)
	}
	finalState, err := reopenManaged(t, afterReuse)
	if err != nil {
		t.Fatal(err)
	}
	finalState.mu.Lock()
	defer finalState.mu.Unlock()
	if finalState.state.LoginUsers[disableUserID].Enabled {
		t.Error("login-user disable was not persisted")
	}
}

func oauthConsentForURL(t *testing.T, m *Managed, authorizeURL string) ([]*http.Cookie, string, string) {
	t.Helper()
	authorizeRecorder := httptest.NewRecorder()
	m.Wrap(http.NotFoundHandler()).ServeHTTP(authorizeRecorder, httptest.NewRequest(http.MethodGet, authorizeURL, nil))
	if authorizeRecorder.Code != http.StatusFound || !strings.HasPrefix(authorizeRecorder.Header().Get("Location"), "http") {
		t.Fatalf("start OAuth authorization: %d %s", authorizeRecorder.Code, authorizeRecorder.Body)
	}
	cookies := authorizeRecorder.Result().Cookies()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	upstream, err := noRedirect.Get(authorizeRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	upstream.Body.Close()
	if upstream.StatusCode != http.StatusFound {
		t.Fatalf("OIDC authorize: %d", upstream.StatusCode)
	}
	callback, _ := url.Parse(upstream.Header.Get("Location"))
	state := callback.Query().Get("state")
	callbackRequest := httptest.NewRequest(http.MethodGet, callback.String(), nil)
	for _, c := range cookies {
		if c.Name == oauthBrowserCookieName(state) {
			callbackRequest.AddCookie(c)
		}
	}
	consent := httptest.NewRecorder()
	m.Wrap(http.NotFoundHandler()).ServeHTTP(consent, callbackRequest)
	if consent.Code != http.StatusOK || !strings.Contains(consent.Body.String(), "Ada Example") {
		t.Fatalf("OIDC callback consent: %d %s", consent.Code, consent.Body)
	}
	return cookies, state, hiddenInputValue(t, consent.Body.String(), "csrf")
}

func hiddenInputValue(t *testing.T, body, name string) string {
	t.Helper()
	needle := `name="` + name + `" value="`
	index := strings.Index(body, needle)
	if index < 0 {
		t.Fatalf("hidden input %s not found in %s", name, body)
	}
	value := body[index+len(needle):]
	end := strings.IndexByte(value, '"')
	if end < 0 {
		t.Fatal("unterminated hidden input")
	}
	return value[:end]
}

func exchangeOAuthToken(t *testing.T, m *Managed, form url.Values) map[string]any {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://termcp.example/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	m.Wrap(http.NotFoundHandler()).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("token exchange: %d %s", response.Code, response.Body)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func cloneValues(in url.Values) url.Values {
	out := make(url.Values, len(in))
	for key, values := range in {
		out[key] = append([]string(nil), values...)
	}
	return out
}

func concurrentOAuthCodeExchange(t *testing.T, m *Managed, form url.Values) map[string]any {
	t.Helper()
	type outcome struct {
		status int
		body   map[string]any
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			request := httptest.NewRequest(http.MethodPost, "http://termcp.example/oauth/token", strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			response := httptest.NewRecorder()
			m.Wrap(http.NotFoundHandler()).ServeHTTP(response, request)
			var body map[string]any
			_ = json.Unmarshal(response.Body.Bytes(), &body)
			results <- outcome{status: response.Code, body: body}
		}()
	}
	close(start)
	var success map[string]any
	failures := 0
	for range 2 {
		result := <-results
		if result.status == http.StatusOK {
			if success != nil {
				t.Fatal("concurrent authorization code exchange succeeded twice")
			}
			success = result.body
		} else {
			failures++
		}
	}
	if success == nil || failures != 1 {
		t.Fatalf("concurrent exchange success=%#v failures=%d", success, failures)
	}
	return success
}

func exchangeOAuthTokenStatus(t *testing.T, m *Managed, form url.Values) int {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "http://termcp.example/oauth/token", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	m.Wrap(http.NotFoundHandler()).ServeHTTP(response, request)
	return response.Code
}

func assertOAuthBearerStatus(t *testing.T, m *Managed, base, token string, want int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, base+"/stream", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	m.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("OAuth bearer status = %d, want %d: %s", response.Code, want, response.Body)
	}
}

func TestOAuthMetadataDoesNotExposeOIDCSecret(t *testing.T) {
	m, _, _ := configuredManaged(t)
	m.mu.Lock()
	m.state.OAuthServer = OAuthServerSettings{
		Enabled:          true,
		Revision:         1,
		PublicURL:        "https://termcp.example.test/termcp",
		OIDCIssuer:       "https://id.example.test",
		OIDCClientID:     "client",
		OIDCClientSecret: "do-not-expose",
	}
	m.applyOAuthSettingsLocked()
	m.mu.Unlock()
	response := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodGet, "/.well-known/oauth-authorization-server", "", "", "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "do-not-expose") {
		t.Fatalf("metadata leaked secret: %d %s", response.Code, response.Body)
	}
	pathResponse := managedRequest(t, m.Wrap(http.NotFoundHandler()), http.MethodGet, "/.well-known/oauth-authorization-server/termcp", "", "", "")
	if pathResponse.Code != http.StatusOK || !strings.Contains(pathResponse.Body.String(), "https://termcp.example.test") {
		t.Fatalf("path-specific metadata: %d %s", pathResponse.Code, pathResponse.Body)
	}
}
