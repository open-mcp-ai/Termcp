package auth

import (
	_ "embed"
	"encoding/json"
	"errors"
	"github.com/open-mcp-ai/termcp/internal/configstore"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

//go:embed login.html
var loginHTML string

//go:embed permissions.html
var permissionsHTML string

//go:embed page_theme.html
var pageThemeHTML string

var loginTemplate = template.Must(template.New("login").Parse(loginHTML + pageThemeHTML))
var permissionsTemplate = template.Must(template.New("permissions").Parse(permissionsHTML + pageThemeHTML))

const preloginCookie = "termcp_prelogin"

func authJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func authError(w http.ResponseWriter, status int, message string) {
	authJSON(w, status, map[string]string{"error": message})
}

func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site == "cross-site" {
		return false
	}
	source := r.Header.Get("Origin")
	if source == "" {
		source = r.Header.Get("Referer")
	}
	if source == "" {
		return true
	} // A valid custom CSRF header is still required for writes.
	u, err := url.Parse(source)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	// A TLS listener cannot accept an insecure origin. HTTPS origins also work
	// behind TLS termination; forwarded headers never determine authorization.
	return r.TLS == nil || u.Scheme == "https"
}

func secureCookie(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	for _, raw := range []string{r.Header.Get("Origin"), r.Header.Get("Referer")} {
		u, err := url.Parse(raw)
		if err == nil && u.Scheme == "https" && strings.EqualFold(u.Host, r.Host) {
			return true
		}
	}
	return false
}

func localSetup(r *http.Request) bool {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(peer)
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	hostIP := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() && (strings.EqualFold(host, "localhost") || hostIP != nil && hostIP.IsLoopback())
}

func uniqueCookie(r *http.Request, name string) (string, bool) {
	var value string
	count := 0
	for _, c := range r.Cookies() {
		if c.Name == name {
			count++
			value = c.Value
		}
	}
	return value, count == 1 && value != ""
}

func (m *Managed) requestSession(r *http.Request) (string, sessionRecord, error) {
	// Explicit credentials never fall back to a more privileged browser cookie.
	if len(r.Header.Values("Authorization")) > 0 {
		return "", sessionRecord{}, errors.New("browser sign-in requires a session cookie")
	}
	raw, ok := uniqueCookie(r, SessionCookie)
	if !ok {
		return "", sessionRecord{}, errors.New("sign in required")
	}
	return m.verify(raw)
}

func checkCSRF(r *http.Request, expected string) bool {
	return sameOrigin(r) && expected != "" && equalSecret(r.Header.Get("X-Termcp-CSRF"), expected)
}

func unsafeMethod(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

func requestSurface(r *http.Request) string {
	switch r.URL.Path {
	case "/stream", "/sse", "/message", "/auth/mcp":
		return "mcp"
	case "/api/daemon", "/api/daemon/stop":
		return "daemon"
	}
	if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/") {
		return "api"
	}
	return "web"
}

// Wrap owns only authentication routes. Existing business handlers and the
// legacy middleware remain unchanged, and never run before managed admission.
func (m *Managed) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		m.mu.Lock()
		auditSink := m.auditSink
		m.mu.Unlock()
		r = r.WithContext(withAuditSink(r.Context(), auditSink))
		if r.URL.Path == "/auth/agents" || strings.HasPrefix(r.URL.Path, "/auth/agents/") {
			http.NotFound(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, pageAssetsPrefix) {
			m.servePageAsset(w, r)
			return
		}
		if isPublicDoc(r) {
			next.ServeHTTP(w, r)
			return
		}
		if (r.URL.Path == "/login" || r.URL.Path == "/setup") && (r.Method == "GET" || r.Method == "HEAD") {
			m.page(w, r)
			return
		}
		if r.URL.Path == "/auth/status" && r.Method == "GET" {
			m.status(w, r)
			return
		}
		if (r.URL.Path == "/auth/setup" || r.URL.Path == "/auth/login") && r.Method == "POST" {
			m.signIn(w, r)
			return
		}
		if len(r.Header.Values("Authorization")) > 0 {
			ctx, done, status := m.admitToken(r)
			if status != 0 {
				if status == http.StatusUnauthorized {
					w.Header().Set("WWW-Authenticate", `Bearer realm="termcp-mcp"`)
				}
				authError(w, status, "MCP credential is invalid, disabled, or not allowed at this entrance")
				return
			}
			defer done()
			if !sameOrigin(r) {
				authError(w, 403, "request origin is invalid")
				return
			}
			if ctx.Err() != nil {
				authError(w, 401, "credential was revoked")
				return
			}
			r = r.WithContext(ctx)
			if r.URL.Path == "/auth/mcp" {
				m.mcpProbe(w, r)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		id, session, err := m.requestSession(r)
		if err != nil {
			if _, present := uniqueCookie(r, SessionCookie); present {
				http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: secureCookie(r), SameSite: http.SameSiteLaxMode})
			}
			if r.Method == "GET" && !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/auth/") && requestSurface(r) != "mcp" {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				authError(w, 401, "sign in or supply an MCP bearer token")
			}
			return
		}
		if !sameOrigin(r) || unsafeMethod(r.Method) && !checkCSRF(r, session.CSRF) {
			authError(w, 403, "request origin or CSRF token is invalid")
			return
		}
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && r.Header.Get("Origin") == "" {
			authError(w, 403, "WebSocket origin is required")
			return
		}
		minimal := r.URL.Path == "/auth" || r.URL.Path == "/auth/unlock" || r.URL.Path == "/auth/logout" || r.URL.Path == "/auth/password"
		if m.NeedsUnlock() && !minimal {
			if r.Method == "GET" && requestSurface(r) == "web" {
				http.Redirect(w, r, "/auth", http.StatusSeeOther)
			} else {
				authError(w, 423, "vault is locked; sign in and unlock configuration encryption at /auth")
			}
			return
		}
		if !minimal && !m.allows(session.Actor.Role, requestSurface(r)) {
			authError(w, 403, "role does not allow this entrance")
			return
		}
		ctx, done := m.track(r.Context(), id, session)
		defer done()
		if ctx.Err() != nil {
			authError(w, 401, "session has ended")
			return
		}
		r = r.WithContext(ctx)
		switch {
		case r.URL.Path == "/auth/unlock" && r.Method == "POST":
			m.unlock(w, r)
		case r.URL.Path == "/auth/mcp":
			m.mcpProbe(w, r)
		case r.URL.Path == "/auth/access":
			if session.Actor.Role != RoleAdmin {
				authError(w, 403, "admin required")
				return
			}
			m.accessCatalog(w, r)
		case r.URL.Path == "/auth/access/preview":
			if session.Actor.Role != RoleAdmin {
				authError(w, 403, "admin required")
				return
			}
			m.accessPreview(w, r)
		case isGroupPath(r.URL.Path):
			if session.Actor.Role != RoleAdmin {
				authError(w, 403, "admin required")
				return
			}
			if unsafeMethod(r.Method) {
				m.audited(w, r, auditAction(r), m.groupManagement)
			} else {
				m.groupManagement(w, r)
			}
		case r.URL.Path == "/auth/principals" || r.URL.Path == "/auth/tokens" || strings.HasPrefix(r.URL.Path, "/auth/principals/") || strings.HasPrefix(r.URL.Path, "/auth/tokens/"):
			if session.Actor.Role != RoleAdmin {
				authError(w, 403, "admin required")
				return
			}
			if unsafeMethod(r.Method) {
				m.audited(w, r, auditAction(r), m.credentials)
			} else {
				m.credentials(w, r)
			}
		case r.URL.Path == "/auth/logout" && r.Method == "POST":
			if err := m.revoke(id); err != nil {
				authError(w, 500, "could not save logout; try again")
				return
			}
			http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: secureCookie(r), SameSite: http.SameSiteLaxMode})
			authJSON(w, 200, map[string]bool{"ok": true})
		case r.URL.Path == "/auth/permissions" && (r.Method == "GET" || r.Method == "PUT"):
			if session.Actor.Role != RoleAdmin {
				authError(w, 403, "admin required")
				return
			}
			if r.Method == "PUT" {
				m.audited(w, r, auditAction(r), m.permissions)
			} else {
				m.permissions(w, r)
			}
		case r.URL.Path == "/auth/password" && r.Method == "POST":
			if session.Actor.Role != RoleAdmin {
				authError(w, 403, "admin required")
				return
			}
			m.audited(w, r, auditAction(r), m.password)
		case r.URL.Path == "/permissions" && (r.Method == "GET" || r.Method == "HEAD"):
			m.page(w, r)
		case r.URL.Path == "/auth" && r.Method == "GET":
			m.page(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (m *Managed) page(w http.ResponseWriter, r *http.Request) {
	nonce, err := randomSecret()
	if err != nil {
		authError(w, 500, "could not prepare login")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; connect-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'nonce-"+nonce+"'; font-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
	if r.Method != "HEAD" {
		page := loginTemplate
		if r.URL.Path == "/permissions" {
			page = permissionsTemplate
		}
		_ = page.Execute(w, struct {
			Nonce  string
			Themes bool
		}{nonce, m.pageAssets != nil})
	}
}

func (m *Managed) status(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		authError(w, 403, "request origin is invalid")
		return
	}
	m.mu.Lock()
	configured := m.sealed || m.legacy != nil
	migration := m.credential == nil && (m.legacy != nil || m.sealed)
	locked := m.sealed && (m.state.Version == 0 || m.vault.AuthenticationLocked())
	existing := m.vault.AuthenticationInitialized()
	initialShared := m.credential != nil && m.credential.InitialPasswordsShared
	m.mu.Unlock()
	id, session, err := m.requestSession(r)
	if err == nil {
		_ = id
		authJSON(w, 200, map[string]any{"mode": "managed", "configured": configured, "authenticated": true, "actor": session.Actor, "csrf": session.CSRF, "expires_at": session.ExpiresAt, "locked": locked, "vault_ready": !locked, "initial_passwords_shared": initialShared})
		return
	}
	if len(r.Header.Values("Authorization")) > 0 {
		authError(w, 401, "explicit credentials are invalid")
		return
	}
	csrf, err := randomSecret()
	if err != nil {
		authError(w, 500, "could not prepare login")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: preloginCookie, Value: csrf, Path: "/auth/", HttpOnly: true, Secure: secureCookie(r), SameSite: http.SameSiteStrictMode, MaxAge: 600})
	authJSON(w, 200, map[string]any{"mode": "managed", "configured": configured, "authenticated": false, "can_setup": localSetup(r), "csrf": csrf, "migration_required": migration, "locked": locked, "encryption_initialized": existing})
}

func (m *Managed) signIn(w http.ResponseWriter, r *http.Request) {
	csrf, ok := uniqueCookie(r, preloginCookie)
	if !ok || !checkCSRF(r, csrf) {
		authError(w, 403, "reload the login page and try again")
		return
	}
	if len(r.Header.Values("Authorization")) > 0 {
		authError(w, 401, "use the login form without Authorization")
		return
	}
	if r.URL.Path == "/auth/setup" && !localSetup(r) {
		authError(w, 403, "first-run setup requires a local connection; use a local browser or SSH port forwarding")
		return
	}
	// Keep concurrent Argon2 work bounded without queuing a burst of login
	// attempts ahead of already authenticated requests.
	if !m.loginGate.TryLock() {
		w.Header().Set("Retry-After", "1")
		authError(w, 429, "Another sign-in is in progress; try again shortly.")
		return
	}
	defer m.loginGate.Unlock()
	m.mu.Lock()
	now := m.now()
	if m.window.IsZero() || now.Sub(m.window) >= time.Minute {
		m.window = now
		m.attempts = 0
	}
	m.attempts++
	limited := m.attempts > 30
	m.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", "60")
		authError(w, 429, "too many login attempts; try again in a minute")
		return
	}
	var input struct {
		Username        string `json:"username"`
		Password        string `json:"password"`
		ConfigPassword  string `json:"config_password"`
		AutomaticUnlock bool   `json:"automatic_unlock"`
	}
	if err := decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input); err != nil {
		authError(w, 400, "invalid login request")
		return
	}
	if r.URL.Path == "/auth/setup" {
		err := m.setupWithOptions(input.Username, input.Password, input.ConfigPassword, input.AutomaticUnlock)
		if errors.Is(err, errInitialized) {
			authError(w, 409, err.Error())
			return
		}
		if err != nil {
			if errors.Is(err, configstore.ErrAuthentication) || errors.Is(err, configstore.ErrPasswordRequired) {
				authError(w, 400, "Enter the existing configuration encryption password; it remains independent of the login password.")
				return
			}
			// Filesystem errors must never expose the configuration path or contents.
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				authError(w, 500, "could not save admin configuration")
				return
			}
			authError(w, 400, err.Error())
			return
		}
	}
	token, session, err := m.loginWithOptions(input.Username, input.Password, input.ConfigPassword, input.AutomaticUnlock)
	if err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			authError(w, 503, "Legacy account cleanup or login metadata access failed. Check local file permissions and retry.")
			return
		}
		authError(w, 401, "Could not sign in. Check the username and login password; legacy migration may also require the configuration password.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: secureCookie(r), SameSite: http.SameSiteLaxMode, MaxAge: int(SessionLifetime.Seconds()), Expires: session.ExpiresAt})
	http.SetCookie(w, &http.Cookie{Name: preloginCookie, Path: "/auth/", MaxAge: -1, HttpOnly: true, Secure: secureCookie(r), SameSite: http.SameSiteStrictMode})
	m.mu.Lock()
	initialShared := m.credential != nil && m.credential.InitialPasswordsShared
	m.mu.Unlock()
	authJSON(w, 200, map[string]any{"actor": session.Actor, "csrf": session.CSRF, "expires_at": session.ExpiresAt, "locked": m.NeedsUnlock(), "initial_passwords_shared": r.URL.Path == "/auth/setup" && initialShared})
}

func (m *Managed) password(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Current      string `json:"current_password"`
		Password     string `json:"new_password"`
		Confirmation string `json:"confirmation"`
	}
	if err := decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input); err != nil {
		authError(w, 400, "invalid password request")
		return
	}
	if err := m.ChangePassword(input.Current, input.Password, input.Confirmation); err != nil {
		if errors.Is(err, configstore.ErrPasswordInvalid) || errors.Is(err, configstore.ErrPasswordConfirmation) || errors.Is(err, configstore.ErrAuthentication) {
			authError(w, 400, "Check the current password; the new passwords must match and contain at least 8 characters.")
			return
		}
		authError(w, 503, "Login password replacement did not complete. Reload and try the current or new login password.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Path: "/", MaxAge: -1, HttpOnly: true, Secure: secureCookie(r), SameSite: http.SameSiteLaxMode})
	authJSON(w, 200, map[string]bool{"ok": true, "sign_in_required": true})
}

func (m *Managed) permissions(w http.ResponseWriter, r *http.Request) {
	if r.Method == "PUT" {
		var input struct {
			Revision    uint64     `json:"revision"`
			Permissions RolePolicy `json:"permissions"`
		}
		if err := decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input); err != nil {
			authError(w, 400, "invalid permissions request")
			return
		}
		if err := validatePolicy(input.Permissions); err != nil {
			authError(w, 400, err.Error())
			return
		}
		m.mu.Lock()
		beforeRevision := m.state.Revision
		if input.Revision != m.state.Revision {
			m.mu.Unlock()
			authError(w, 409, "permissions changed; reload before saving")
			return
		}
		s := m.state
		s.Revision++
		s.Permissions = input.Permissions
		err := m.save(s, false)
		if err == nil || errors.Is(err, configstore.ErrDurability) {
			m.cancelDeniedTokensLocked()
		}
		m.mu.Unlock()
		if err != nil {
			authError(w, 500, "could not save permissions")
			return
		}
		auditDetail(r.Context(), func(draft *auditDraft) {
			draft.beforeRevision = beforeRevision
			draft.afterRevision = s.Revision
		})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	authJSON(w, 200, map[string]any{"revision": m.state.Revision, "permissions": m.state.Permissions})
}

// mcpProbe reveals only the keepalive interval, never daemon control details.
func (m *Managed) mcpProbe(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		authError(w, 405, "method not allowed")
		return
	}
	authJSON(w, 200, map[string]any{"ok": true, "idle_timeout_ms": m.MCPIdleTimeout.Milliseconds()})
}

func (m *Managed) unlock(w http.ResponseWriter, r *http.Request) {
	if !m.loginGate.TryLock() {
		authError(w, 429, "another password operation is running")
		return
	}
	defer m.loginGate.Unlock()
	var input struct {
		Password  string `json:"password"`
		Automatic bool   `json:"automatic_unlock"`
	}
	if decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input) != nil {
		authError(w, 400, "invalid unlock request")
		return
	}
	if err := m.UnlockVault(input.Password, input.Automatic); err != nil {
		authError(w, 400, "Configuration unlock failed. Check the configuration password and encrypted data.")
		return
	}
	authJSON(w, 200, map[string]bool{"ok": true, "locked": false})
}
