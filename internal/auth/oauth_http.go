package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/ory/fosite"

	"github.com/open-mcp-ai/termcp/internal/oauthserver"
)

const (
	oauthBrowserCookie       = "termcp_oauth_browser"
	oauthTransactionLifetime = 10 * time.Minute
)

var oauthConsentTemplate = template.Must(template.New("oauth-consent").Parse(`<!doctype html>
<html lang="zh"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>确认接入 · Termcp</title>
<style>
:root{color-scheme:light dark;--bg:#f7f7f5;--paper:#fff;--ink:#17201d;--muted:#60706a;--line:#d9dfdc;--accent:#176b52;--accent-ink:#fff;--error:#a22b34}
@media(prefers-color-scheme:dark){:root{--bg:#111714;--paper:#171f1b;--ink:#eef3f0;--muted:#a0aea8;--line:#33403a;--accent:#62c7a2;--accent-ink:#0c1712;--error:#ff9198}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.55 system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif}
main{max-width:620px;margin:0 auto;padding:32px 20px 48px}header{display:flex;align-items:center;gap:12px;margin-bottom:28px}.mark{display:grid;place-items:center;width:36px;height:36px;border:1px solid var(--line);border-radius:8px;background:var(--paper);font-weight:750;color:var(--accent)}
h1{font-size:26px;line-height:1.2;margin:0 0 8px}.lead{margin:0 0 24px;color:var(--muted)}.card{background:var(--paper);border:1px solid var(--line);border-radius:8px;padding:22px}
.identity{display:flex;gap:14px;align-items:center;padding-bottom:18px;border-bottom:1px solid var(--line)}.avatar{width:48px;height:48px;border-radius:50%;object-fit:cover;background:var(--bg);border:1px solid var(--line)}.fallback{display:grid;place-items:center;font-weight:700;color:var(--accent)}
dl{display:grid;gap:12px;margin:18px 0}.fact{display:grid;grid-template-columns:120px minmax(0,1fr);gap:12px}.fact dt{color:var(--muted)}.fact dd{margin:0;overflow-wrap:anywhere}code{font:12px/1.5 ui-monospace,SFMono-Regular,monospace}
.actions{display:flex;gap:10px;justify-content:flex-end;margin-top:22px;flex-wrap:wrap}button{min-height:40px;padding:8px 16px;border:1px solid var(--line);border-radius:6px;background:transparent;color:var(--ink);font:inherit;cursor:pointer}button.primary{background:var(--accent);border-color:var(--accent);color:var(--accent-ink);font-weight:650}.note{color:var(--muted);font-size:13px;margin-top:16px}.error{color:var(--error)}
@media(max-width:520px){main{padding:22px 14px 32px}.card{padding:18px}.fact{grid-template-columns:1fr;gap:2px}.actions{flex-direction:column-reverse}.actions button{width:100%}}
</style></head><body><main><header><span class="mark">T</span><strong>Termcp</strong></header>
<h1>确认这次 MCP 接入</h1><p class="lead">登录已完成。确认后，Termcp 会为这个客户端创建一个独立 Agent，并按管理员设置分配初始权限。</p>
<section class="card"><div class="identity">{{if .AvatarURL}}<img class="avatar" src="{{.AvatarURL}}" alt="">{{else}}<span class="avatar fallback">{{.Initial}}</span>{{end}}<div><strong>{{.DisplayName}}</strong><div class="status">{{.Username}}</div></div></div>
<dl><div class="fact"><dt>申请客户端</dt><dd>{{.ClientName}}</dd></div><div class="fact"><dt>回调地址</dt><dd><code>{{.RedirectHost}}</code></dd></div><div class="fact"><dt>身份来源</dt><dd>{{.Issuer}}</dd></div><div class="fact"><dt>初始权限组</dt><dd>{{.GroupName}}</dd></div></dl>
<p class="note">登录身份只用于确认 Agent 归属，不会获得 Termcp 管理后台、REST 或管理员权限。</p>
{{if .Error}}<p class="error">{{.Error}}</p>{{end}}
<form method="post" action="{{.ContinueURL}}"><input type="hidden" name="state" value="{{.State}}"><input type="hidden" name="csrf" value="{{.CSRF}}"><div class="actions"><button type="submit" name="decision" value="deny">拒绝</button><button class="primary" type="submit" name="decision" value="approve">确认接入</button></div></form>
</section></main></body></html>`))

type oauthConsentView struct {
	State        string
	CSRF         string
	ContinueURL  string
	AvatarURL    string
	Initial      string
	DisplayName  string
	Username     string
	ClientName   string
	RedirectHost string
	Issuer       string
	GroupName    string
	Error        string
}

func (m *Managed) protectedOAuthProtocol() *oauthserver.Provider {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.oauthProtocol
}

// ServeOAuthMetadata serves protected-resource and authorization-server metadata
// without requiring a browser session. The advertisement is public but contains
// no OIDC secret, user profile, or token material.
func (m *Managed) ServeOAuthMetadata(w http.ResponseWriter, r *http.Request) bool {
	if m == nil || r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	provider := m.protectedOAuthProtocol()
	if provider == nil {
		return false
	}
	path := r.URL.Path
	if metadataPathMatches(path, "/.well-known/oauth-protected-resource", provider.Config.Issuer, "/stream", "/sse") {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		if r.Method != http.MethodHead {
			_ = json.NewEncoder(w).Encode(provider.ProtectedResourceMetadata())
		}
		return true
	}
	if metadataPathMatches(path, "/.well-known/oauth-authorization-server", provider.Config.Issuer) ||
		metadataPathMatches(path, "/.well-known/openid-configuration", provider.Config.Issuer) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		if r.Method != http.MethodHead {
			_ = json.NewEncoder(w).Encode(provider.AuthorizationMetadata())
		}
		return true
	}
	return false
}

func metadataPathMatches(path, wellKnown, issuer string, suffixes ...string) bool {
	index := strings.Index(path, wellKnown)
	if index < 0 {
		return false
	}
	suffix := strings.TrimPrefix(path[index+len(wellKnown):], "/")
	issuerURL, err := url.Parse(issuer)
	issuerPath := ""
	if err == nil {
		issuerPath = strings.Trim(issuerURL.Path, "/")
	}
	suffix = strings.Trim(suffix, "/")
	if suffix == "" || suffix == issuerPath {
		return true
	}
	for _, allowed := range suffixes {
		allowed = strings.Trim(allowed, "/")
		if suffix == allowed || issuerPath != "" && suffix == issuerPath+"/"+allowed {
			return true
		}
	}
	return false
}

func (m *Managed) oauthEndpoint(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/oauth/authorize":
		m.oauthAuthorize(w, r)
		return true
	case "/oauth/oidc/callback":
		m.oauthOIDCCallback(w, r)
		return true
	case "/oauth/continue":
		m.oauthContinue(w, r)
		return true
	case "/oauth/token":
		m.oauthToken(w, r)
		return true
	case "/oauth/register":
		m.oauthRegister(w, r)
		return true
	case "/oauth/revoke":
		m.oauthRevoke(w, r)
		return true
	default:
		return false
	}
}

func (m *Managed) oauthChallenge(r *http.Request, invalid bool) string {
	provider := m.protectedOAuthProtocol()
	if provider == nil {
		return ""
	}
	metadata := oauthserver.ProtectedResourceMetadataURL(provider.ResourceURL())
	if metadata == "" {
		return ""
	}
	parts := []string{`Bearer resource_metadata=` + strconvQuote(metadata), `scope="mcp"`}
	if invalid {
		parts = append(parts, `error="invalid_token"`)
	}
	return strings.Join(parts, ", ")
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func (m *Managed) oauthAuthorize(w http.ResponseWriter, r *http.Request) {
	oauthNoStore(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.allowOAuthAttempt(r, "authorize", 60) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many OAuth authorization attempts", http.StatusTooManyRequests)
		return
	}
	provider := m.protectedOAuthProtocol()
	settings := m.currentOAuthServerSettings()
	m.mu.Lock()
	locked := m.vault.AuthenticationLocked()
	m.mu.Unlock()
	if provider == nil || !settings.Enabled {
		http.Error(w, "OAuth is not configured", http.StatusServiceUnavailable)
		return
	}
	if locked {
		http.Error(w, "configuration vault is locked", http.StatusServiceUnavailable)
		return
	}
	resource, err := requestedOAuthResource(r.URL.Query(), provider.ResourceURL())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	clientID := r.URL.Query().Get("client_id")
	if clientID == "" {
		http.Error(w, "client_id is required", http.StatusBadRequest)
		return
	}
	client, err := m.resolveOAuthClient(r.Context(), clientID)
	if err != nil {
		slog.WarnContext(r.Context(), "OAuth client resolution failed", "client_id", clientID, "err", err)
		http.Error(w, "unknown or invalid OAuth client", http.StatusBadRequest)
		return
	}
	authorizeRequest := r.Clone(r.Context())
	authorizeRequest.Form = cloneURLValues(r.URL.Query())
	authorizeRequest.PostForm = nil
	authorizeRequest.Form.Set("resource", resource)
	authorizeRequest.Form.Set("audience", resource)
	authorizeContext := withOAuthRequestedRedirect(r.Context(), r.URL.Query().Get("redirect_uri"))
	ar, err := provider.NewAuthorizeRequest(authorizeContext, authorizeRequest)
	if err != nil {
		m.writeOAuthAuthorizeError(w, r, ar, err)
		return
	}
	oidc, err := oauthserver.NewOIDC(r.Context(), oauthserver.OIDCConfig{
		Issuer:       settings.OIDCIssuer,
		ClientID:     settings.OIDCClientID,
		ClientSecret: settings.OIDCClientSecret,
		RedirectURL:  settings.PublicURL + "/oauth/oidc/callback",
	})
	if err != nil {
		http.Error(w, "OIDC identity source is unavailable", http.StatusBadGateway)
		return
	}
	browser, err := randomSecret()
	if err != nil {
		http.Error(w, "could not start OAuth login", http.StatusInternalServerError)
		return
	}
	nonce, err := randomSecret()
	if err != nil {
		http.Error(w, "could not start OAuth login", http.StatusInternalServerError)
		return
	}
	verifier := oauth2Verifier()
	tx := &oauthAuthorizationTransaction{
		Browser:        browser,
		Nonce:          nonce,
		Verifier:       verifier,
		ExpiresAt:      m.now().UTC().Add(oauthTransactionLifetime),
		ConfigRevision: settings.Revision,
		Client:         client,
		OIDC:           oidc,
		Authorize:      ar,
	}
	state, err := m.rememberOAuthTransaction(tx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusTooManyRequests)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     oauthBrowserCookieName(state),
		Value:    browser,
		Path:     "/",
		HttpOnly: true,
		Secure:   strings.HasPrefix(settings.PublicURL, "https://"),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthTransactionLifetime.Seconds()),
	})
	http.Redirect(w, r, oidc.AuthCodeURL(state, nonce, verifier), http.StatusFound)
}

func oauth2Verifier() string {
	verifier, _ := randomSecret()
	if len(verifier) > 96 {
		verifier = verifier[:96]
	}
	return verifier
}

func requestedOAuthResource(values url.Values, expected string) (string, error) {
	resources := values["resource"]
	if len(resources) == 0 {
		return expected, nil
	}
	if len(resources) != 1 || resources[0] != expected {
		return "", errors.New("resource must identify this exact Termcp MCP endpoint")
	}
	return resources[0], nil
}

func oauthBrowserCookieName(state string) string {
	sum := sha256.Sum256([]byte(state))
	return oauthBrowserCookie + "_" + hex.EncodeToString(sum[:8])
}

func (m *Managed) oauthOIDCCallback(w http.ResponseWriter, r *http.Request) {
	oauthNoStore(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.allowOAuthAttempt(r, "oidc_callback", 120) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many OAuth callback attempts", http.StatusTooManyRequests)
		return
	}
	state := r.URL.Query().Get("state")
	tx := m.oauthTransaction(state)
	if tx == nil {
		http.Error(w, "OAuth transaction is missing or expired", http.StatusBadRequest)
		return
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if !oauthBrowserMatches(r, state, tx.Browser) {
		http.Error(w, "OAuth browser binding is invalid", http.StatusForbidden)
		return
	}
	settings := m.currentOAuthServerSettings()
	if !settings.Enabled || settings.Revision != tx.ConfigRevision {
		http.Error(w, "OAuth configuration changed; restart the connection", http.StatusConflict)
		return
	}
	if oidcError := r.URL.Query().Get("error"); oidcError != "" {
		m.redirectOAuthError(w, r, tx, "access_denied", "identity login was not completed")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "OIDC callback has no authorization code", http.StatusBadRequest)
		return
	}
	identity, err := tx.OIDC.Exchange(r.Context(), code, tx.Verifier, tx.Nonce)
	if err != nil {
		slog.WarnContext(r.Context(), "OIDC identity verification failed", "err", err)
		http.Error(w, "OIDC identity verification failed", http.StatusUnauthorized)
		return
	}
	avatarFile, avatarType := "", ""
	if identity.AvatarURL != "" {
		if data, contentType, err := m.oauthHTTPFetcher().Avatar(r.Context(), identity.AvatarURL); err == nil {
			avatarFile, avatarType, _ = m.saveOAuthAvatar(identity, data, contentType)
		}
	}
	csrf, err := randomSecret()
	if err != nil {
		http.Error(w, "could not prepare consent", http.StatusInternalServerError)
		return
	}
	tx.Identity = &identity
	tx.AvatarFile = avatarFile
	tx.AvatarType = avatarType
	tx.CSRF = csrf
	m.renderOAuthConsent(w, r, tx, state, "")
}

func oauthBrowserMatches(r *http.Request, state, browser string) bool {
	cookie, err := r.Cookie(oauthBrowserCookieName(state))
	return err == nil && cookie.Value != "" && equalSecret(cookie.Value, browser)
}

func (m *Managed) saveOAuthAvatar(identity oauthserver.Identity, data []byte, contentType string) (string, string, error) {
	sum := sha256.Sum256([]byte(identity.Issuer + "\x00" + identity.Subject))
	name := hex.EncodeToString(sum[:]) + ".png"
	dir := filepath.Join(filepath.Dir(m.path), "oauth-avatars")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	target := filepath.Join(dir, name)
	temp, err := os.CreateTemp(dir, ".avatar-*")
	if err != nil {
		return "", "", err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return "", "", err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return "", "", err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return "", "", err
	}
	if err := temp.Close(); err != nil {
		return "", "", err
	}
	if err := os.Rename(tempName, target); err != nil {
		return "", "", err
	}
	return name, contentType, nil
}

func (m *Managed) renderOAuthConsent(w http.ResponseWriter, r *http.Request, tx *oauthAuthorizationTransaction, state, message string) {
	settings := m.currentOAuthServerSettings()
	m.mu.Lock()
	group := m.state.Groups[m.state.DefaultGroupID]
	groupName := group.Name
	if groupName == "" {
		groupName = "不可用"
	}
	m.mu.Unlock()
	// The fetched avatar is shown in authenticated administration views. The
	// consent document intentionally uses a same-origin fallback rather than
	// exposing a guessable pre-consent avatar URL.
	avatarURL := ""
	initial := "U"
	displayName, username := "", ""
	if tx.Identity != nil {
		displayName = tx.Identity.DisplayName
		username = tx.Identity.Username
		if runes := []rune(displayName); len(runes) > 0 {
			initial = string(runes[0])
		}
	}
	redirectHost := ""
	if tx.Authorize.GetRedirectURI() != nil {
		redirectHost = tx.Authorize.GetRedirectURI().Host
		if redirectHost == "" {
			redirectHost = tx.Authorize.GetRedirectURI().String()
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	_ = oauthConsentTemplate.Execute(w, oauthConsentView{
		State:        state,
		CSRF:         tx.CSRF,
		ContinueURL:  settings.PublicURL + "/oauth/continue",
		AvatarURL:    avatarURL,
		Initial:      initial,
		DisplayName:  displayName,
		Username:     username,
		ClientName:   tx.Client.Name,
		RedirectHost: redirectHost,
		Issuer:       tx.Identity.Issuer,
		GroupName:    groupName,
		Error:        message,
	})
}

func (m *Managed) oauthContinue(w http.ResponseWriter, r *http.Request) {
	oauthNoStore(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "GET does not approve OAuth access; submit the confirmation form", http.StatusMethodNotAllowed)
		return
	}
	if !m.allowOAuthAttempt(r, "continue", 120) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many OAuth confirmation attempts", http.StatusTooManyRequests)
		return
	}
	if err := parseOAuthForm(w, r, 16384); err != nil {
		http.Error(w, "invalid confirmation request", http.StatusBadRequest)
		return
	}
	state := r.Form.Get("state")
	tx := m.oauthTransaction(state)
	if tx == nil {
		http.Error(w, "OAuth transaction is missing or expired", http.StatusBadRequest)
		return
	}
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.Identity == nil || tx.Authorize == nil {
		http.Error(w, "OAuth transaction is missing or expired", http.StatusBadRequest)
		return
	}
	if !oauthBrowserMatches(r, state, tx.Browser) || tx.CSRF == "" ||
		!equalSecret(tx.CSRF, r.Form.Get("csrf")) || !sameOrigin(r) {
		http.Error(w, "OAuth confirmation binding is invalid", http.StatusForbidden)
		return
	}
	settings := m.currentOAuthServerSettings()
	if !settings.Enabled || settings.Revision != tx.ConfigRevision {
		http.Error(w, "OAuth configuration changed; restart the connection", http.StatusConflict)
		return
	}
	if r.Form.Get("decision") != "approve" {
		m.consumeOAuthTransaction(state)
		m.redirectOAuthError(w, r, tx, "access_denied", "the user denied this connection")
		return
	}
	if err := m.validateOAuthProvisioning(tx); err != nil {
		m.renderOAuthConsent(w, r, tx, state, err.Error())
		return
	}

	provider := m.protectedOAuthProtocol()
	if provider == nil {
		http.Error(w, "OAuth is unavailable", http.StatusServiceUnavailable)
		return
	}
	storage := &oauthFositeStorage{managed: m}
	ctx, err := storage.BeginTX(r.Context())
	if err != nil {
		http.Error(w, "OAuth state is unavailable", http.StatusServiceUnavailable)
		return
	}
	txStored := transactionFromContext(ctx)
	txStored.candidate.OAuthClients[tx.Client.ID] = tx.Client
	for _, scope := range requestedScopes(tx.Authorize.GetRequestedScopes()) {
		tx.Authorize.GrantScope(scope)
	}
	for _, audience := range tx.Authorize.GetRequestedAudience() {
		if audience != provider.ResourceURL() {
			http.Error(w, "requested resource is invalid", http.StatusBadRequest)
			_ = storage.Rollback(ctx)
			return
		}
		tx.Authorize.GrantAudience(audience)
	}
	if len(tx.Authorize.GetGrantedAudience()) == 0 {
		tx.Authorize.GrantAudience(provider.ResourceURL())
	}
	session := &fosite.DefaultSession{
		Subject:  tx.Identity.Subject,
		Username: tx.Identity.DisplayName,
		Extra:    map[string]any{},
	}
	userID, err := m.oauthUserID(*tx.Identity)
	if err != nil {
		_ = storage.Rollback(ctx)
		http.Error(w, "could not prepare OAuth user", http.StatusInternalServerError)
		return
	}
	pending := pendingOAuthGrant{
		UserID:         userID,
		Issuer:         tx.Identity.Issuer,
		Subject:        tx.Identity.Subject,
		DisplayName:    tx.Identity.DisplayName,
		Username:       tx.Identity.Username,
		AvatarURL:      tx.Identity.AvatarURL,
		AvatarFile:     tx.AvatarFile,
		AvatarType:     tx.AvatarType,
		ClientID:       tx.Client.ID,
		Resource:       provider.ResourceURL(),
		ConfigRevision: settings.Revision,
	}
	session.Extra["termcp_pending_grant"] = pending
	response, err := provider.NewAuthorizeResponse(ctx, tx.Authorize, session)
	if err != nil {
		_ = storage.Rollback(ctx)
		m.writeOAuthAuthorizeError(w, r, tx.Authorize, err)
		return
	}
	if err := storage.Commit(ctx); err != nil {
		_ = storage.Rollback(ctx)
		http.Error(w, "could not persist OAuth authorization", http.StatusInternalServerError)
		return
	}
	m.consumeOAuthTransaction(state)
	m.redirectOAuthSuccess(w, r, tx, response)
}

func requestedScopes(input []string) []string {
	if len(input) == 0 {
		return []string{"mcp"}
	}
	out := make([]string, 0, len(input))
	for _, scope := range input {
		switch scope {
		case "mcp", "offline_access":
			if !slices.Contains(out, scope) {
				out = append(out, scope)
			}
		}
	}
	if !slices.Contains(out, "mcp") {
		out = append(out, "mcp")
	}
	return out
}

func (m *Managed) oauthUserID(identity oauthserver.Identity) (string, error) {
	m.mu.Lock()
	id := findLoginUserBySubject(m.state, identity.Issuer, identity.Subject)
	m.mu.Unlock()
	if id != "" {
		return id, nil
	}
	id, err := randomSecret()
	return id, err
}

func (m *Managed) validateOAuthProvisioning(tx *oauthAuthorizationTransaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.state.OAuthServer.Enabled {
		return errors.New("OAuth is disabled")
	}
	group := m.state.Groups[m.state.DefaultGroupID]
	if m.state.DefaultGroupID == "" || !group.Enabled {
		return errors.New("The default permission group is unavailable; ask the administrator to repair it")
	}
	if len(m.state.Agents) >= maxAgents {
		return errors.New("The Agent limit has been reached; ask the administrator to clean up unused Agents")
	}
	if id := findLoginUserBySubject(m.state, tx.Identity.Issuer, tx.Identity.Subject); id != "" {
		user := m.state.LoginUsers[id]
		if !user.Enabled {
			return errors.New("This login user is disabled")
		}
		if countGrantsForUser(m.state, id, m.now().UTC()) >= maxGrantsPerUser {
			return errors.New("This login user has reached the OAuth grant limit")
		}
	}
	return nil
}

func (m *Managed) redirectOAuthSuccess(w http.ResponseWriter, r *http.Request, tx *oauthAuthorizationTransaction, response fosite.AuthorizeResponder) {
	target := *tx.Authorize.GetRedirectURI()
	query := target.Query()
	for key, values := range response.GetParameters() {
		for _, value := range values {
			query.Set(key, value)
		}
	}
	query.Set("iss", m.currentOAuthServerSettings().PublicURL)
	target.RawQuery = query.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (m *Managed) redirectOAuthError(w http.ResponseWriter, r *http.Request, tx *oauthAuthorizationTransaction, code, description string) {
	if tx.Authorize == nil || tx.Authorize.GetRedirectURI() == nil {
		http.Error(w, description, http.StatusBadRequest)
		return
	}
	target := *tx.Authorize.GetRedirectURI()
	query := target.Query()
	query.Set("error", code)
	query.Set("error_description", description)
	query.Set("state", tx.Authorize.GetState())
	query.Set("iss", m.currentOAuthServerSettings().PublicURL)
	target.RawQuery = query.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (m *Managed) writeOAuthAuthorizeError(w http.ResponseWriter, r *http.Request, request fosite.AuthorizeRequester, err error) {
	if request == nil || request.GetRedirectURI() == nil || !request.IsRedirectURIValid() {
		http.Error(w, "invalid OAuth authorization request", http.StatusBadRequest)
		return
	}
	code := "invalid_request"
	description := "the authorization request could not be completed"
	var oauthErr *fosite.RFC6749Error
	if errors.As(err, &oauthErr) {
		if oauthErr.ErrorField != "" {
			code = oauthErr.ErrorField
		}
		if oauthErr.DescriptionField != "" {
			description = oauthErr.DescriptionField
		}
	}
	target := *request.GetRedirectURI()
	query := target.Query()
	query.Set("error", code)
	query.Set("error_description", description)
	if state := request.GetState(); state != "" {
		query.Set("state", state)
	}
	query.Set("iss", m.currentOAuthServerSettings().PublicURL)
	target.RawQuery = query.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (m *Managed) oauthToken(w http.ResponseWriter, r *http.Request) {
	oauthNoStore(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.allowOAuthAttempt(r, "token", 300) {
		w.Header().Set("Retry-After", "60")
		authJSON(w, http.StatusTooManyRequests, map[string]any{"error": "temporarily_unavailable"})
		return
	}
	if err := parseOAuthForm(w, r, 1<<20); err != nil {
		http.Error(w, "invalid token request", http.StatusBadRequest)
		return
	}
	provider := m.protectedOAuthProtocol()
	if provider == nil {
		http.Error(w, "OAuth is unavailable", http.StatusServiceUnavailable)
		return
	}
	resource, err := requestedOAuthResource(r.Form, provider.ResourceURL())
	if err != nil {
		authJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_request", "error_description": err.Error()})
		return
	}
	r.Form.Set("resource", resource)
	r.Form.Set("audience", resource)

	storage := &oauthFositeStorage{managed: m}
	ctx, err := storage.BeginTX(r.Context())
	if err != nil {
		authJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "temporarily_unavailable"})
		return
	}
	request, err := provider.NewAccessRequest(ctx, r, new(fosite.DefaultSession))
	if err != nil {
		slog.WarnContext(r.Context(), "OAuth token request rejected", "err", err)
		_ = storage.Rollback(ctx)
		provider.Engine.WriteAccessError(ctx, w, request, err)
		return
	}
	response, err := provider.NewAccessResponse(ctx, request)
	if err != nil {
		slog.WarnContext(r.Context(), "OAuth token response failed", "err", err)
		_ = storage.Rollback(ctx)
		provider.Engine.WriteAccessError(ctx, w, request, err)
		return
	}
	if tx := transactionFromContext(ctx); tx != nil && !tx.committed {
		if err := storage.Commit(ctx); err != nil {
			_ = storage.Rollback(ctx)
			provider.Engine.WriteAccessError(ctx, w, request, fosite.ErrServerError.WithWrap(err))
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(response.ToMap())
}

type oauthRegistrationInput struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	LogoURI                 string   `json:"logo_uri"`
	ApplicationType         string   `json:"application_type"`
	Scope                   string   `json:"scope"`
}

func (m *Managed) oauthRegister(w http.ResponseWriter, r *http.Request) {
	oauthNoStore(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.allowOAuthAttempt(r, "register", 20) {
		w.Header().Set("Retry-After", "60")
		authJSON(w, http.StatusTooManyRequests, map[string]any{"error": "temporarily_unavailable"})
		return
	}
	var input oauthRegistrationInput
	if err := decodeJSONAllowUnknown(http.MaxBytesReader(w, r.Body, 32768), &input); err != nil {
		authJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client_metadata"})
		return
	}
	if len(input.RedirectURIs) == 0 || len(input.RedirectURIs) > 16 ||
		len(input.ClientName) > 256 || strings.TrimSpace(input.ClientName) == "" ||
		strings.ContainsFunc(input.ClientName, unicode.IsControl) {
		authJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client_metadata"})
		return
	}
	if input.TokenEndpointAuthMethod == "" {
		input.TokenEndpointAuthMethod = "none"
	}
	if input.TokenEndpointAuthMethod != "none" {
		authJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client_metadata", "error_description": "only public clients are supported"})
		return
	}
	if input.ApplicationType != "" && input.ApplicationType != "native" && input.ApplicationType != "web" {
		authJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_redirect_uri"})
		return
	}
	for _, redirect := range input.RedirectURIs {
		if !oauthserver.ValidRedirect(redirect) {
			authJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_redirect_uri"})
			return
		}
	}
	if len(input.GrantTypes) > 0 && !slices.Contains(input.GrantTypes, "authorization_code") {
		authJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client_metadata"})
		return
	}
	if len(input.ResponseTypes) > 0 && !slices.Contains(input.ResponseTypes, "code") {
		authJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid_client_metadata"})
		return
	}
	id, err := randomSecret()
	if err != nil {
		authJSON(w, http.StatusInternalServerError, map[string]any{"error": "server_error"})
		return
	}
	clientID := "mcpc_" + id
	client := oauthserver.Client{
		ID:           clientID,
		Name:         strings.TrimSpace(input.ClientName),
		URI:          strings.TrimSpace(input.ClientURI),
		LogoURI:      strings.TrimSpace(input.LogoURI),
		Registration: oauthserver.ClientRegistrationDCR,
		RedirectURIs: append([]string(nil), input.RedirectURIs...),
		CreatedAt:    m.now().UTC(),
		Enabled:      true,
	}
	err = m.commitState(r.Context(), func(next *managedState) (func(), error) {
		if !next.OAuthServer.Enabled {
			return nil, errors.New("OAuth is disabled")
		}
		if len(next.OAuthClients) >= maxOAuthClients {
			return nil, errors.New("OAuth client limit reached")
		}
		next.OAuthClients[clientID] = client
		next.OAuthRevision++
		return nil, nil
	})
	if err != nil {
		authJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "temporarily_unavailable"})
		return
	}
	authJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"client_name":                client.Name,
		"redirect_uris":              client.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

func (m *Managed) oauthRevoke(w http.ResponseWriter, r *http.Request) {
	oauthNoStore(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !m.allowOAuthAttempt(r, "revoke", 120) {
		w.Header().Set("Retry-After", "60")
		http.Error(w, "too many revocation attempts", http.StatusTooManyRequests)
		return
	}
	if err := parseOAuthForm(w, r, 32768); err != nil {
		http.Error(w, "invalid revocation request", http.StatusBadRequest)
		return
	}
	raw := r.Form.Get("token")
	clientID := r.Form.Get("client_id")
	if raw == "" || clientID == "" {
		http.Error(w, "token and client_id are required", http.StatusBadRequest)
		return
	}
	client, err := m.resolveOAuthClient(r.Context(), clientID)
	if err != nil {
		http.Error(w, "unknown OAuth client", http.StatusBadRequest)
		return
	}
	var cancel map[string]string
	err = m.commitState(r.Context(), func(next *managedState) (func(), error) {
		now := m.now().UTC()
		cancel = map[string]string{}
		if strings.HasPrefix(raw, "toa_") {
			signature := oauthserver.TokenSignature(raw)
			record, ok := next.OAuthAccess[signature]
			if !ok || record.ClientID != client.ID {
				return nil, nil
			}
			if record.RevokedAt == nil {
				record.RevokedAt = &now
				next.OAuthAccess[signature] = record
				grant := next.OAuthGrants[record.GrantID]
				cancel[record.ID] = grant.AgentID
				next.OAuthRevision++
			}
			return func() { m.cancelOAuthCredentials(cancel) }, nil
		}
		if strings.HasPrefix(raw, "tor_") {
			signature := oauthserver.TokenSignature(raw)
			record, ok := next.OAuthRefresh[signature]
			if !ok || record.ClientID != client.ID {
				return nil, nil
			}
			grant, ok := next.OAuthGrants[record.GrantID]
			if !ok {
				return nil, nil
			}
			grant.RevokedAt = &now
			grant.RevokeReason = "token_revoked"
			next.OAuthGrants[grant.ID] = grant
			for accessSig, access := range next.OAuthAccess {
				if access.GrantID == grant.ID && access.RevokedAt == nil {
					access.RevokedAt = &now
					next.OAuthAccess[accessSig] = access
					cancel[access.ID] = grant.AgentID
				}
			}
			for refreshSig, refresh := range next.OAuthRefresh {
				if refresh.GrantID == grant.ID && refresh.RevokedAt == nil {
					refresh.RevokedAt = &now
					next.OAuthRefresh[refreshSig] = refresh
				}
			}
			next.OAuthRevision++
			return func() { m.cancelOAuthCredentials(cancel) }, nil
		}
		return nil, nil
	})
	if err != nil {
		authJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "temporarily_unavailable"})
		return
	}
	w.WriteHeader(http.StatusOK)
}

func parseOAuthForm(w http.ResponseWriter, r *http.Request, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseForm(); err != nil {
		return err
	}
	return nil
}

func decodeJSONAllowUnknown(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("invalid JSON document")
	}
	return nil
}

func (m *Managed) oauthAvatar(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/auth/oauth/avatar/") {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	name := strings.TrimPrefix(r.URL.Path, "/auth/oauth/avatar/")
	if name == "" || filepath.Base(name) != name || !strings.HasSuffix(name, ".png") {
		http.NotFound(w, r)
		return true
	}
	path := filepath.Join(filepath.Dir(m.path), "oauth-avatars", name)
	file, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return true
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > maxAvatarHTTPBytes {
		http.NotFound(w, r)
		return true
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, file)
	}
	return true
}

const maxAvatarHTTPBytes = 1 << 20

func oauthStateHash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func oauthTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

func oauthDebug(summary string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", summary, err)
}
