package auth

import (
	"context"
	"errors"
	"net/http"
)

// OAuthSettings lives in the shared encrypted account state. Revision prevents
// two open settings pages from silently replacing each other's configuration.
type OAuthSettings struct {
	Enabled  bool   `json:"enabled"`
	Revision uint64 `json:"revision"`
	OAuthConfig
}

func normalizeOAuthSettings(settings OAuthSettings) (OAuthSettings, error) {
	cfg, err := normalizeOAuthConfig(settings.OAuthConfig)
	if err != nil {
		return OAuthSettings{}, err
	}
	if settings.Enabled && !cfg.enabled() {
		return OAuthSettings{}, errors.New("oauth issuer, resource, and JWKS URL are required together")
	}
	settings.OAuthConfig = cfg
	return settings, nil
}

// applyOAuthSettingsLocked only changes in-memory configuration. Network key
// fetching happens later, outside m.mu, in the verifier. Preserve the key cache
// when unrelated account changes save the same OAuth configuration.
func (m *Managed) applyOAuthSettingsLocked() {
	settings := m.state.OAuth
	if !settings.Enabled {
		m.oauth = nil
		return
	}
	if m.oauth != nil && m.oauth.cfg == settings.OAuthConfig {
		return
	}
	m.oauth = &oauthVerifier{
		cfg:    settings.OAuthConfig,
		client: &http.Client{Timeout: oauthHTTPTimeout},
	}
}

func (m *Managed) currentOAuthSettings() OAuthSettings {
	m.mu.Lock()
	defer m.mu.Unlock()
	settings := m.state.OAuth
	if settings.Revision == 0 {
		settings.AgentClaim = "termcp_agent_id"
		settings.Scope = "mcp"
	}
	return settings
}

func (m *Managed) updateOAuthSettings(settings OAuthSettings, expected *uint64) (OAuthSettings, int, error) {
	if m == nil {
		return OAuthSettings{}, http.StatusServiceUnavailable, errors.New("managed auth is unavailable")
	}
	normalized, err := normalizeOAuthSettings(settings)
	if err != nil {
		return OAuthSettings{}, http.StatusBadRequest, err
	}
	m.mu.Lock()
	if m.state.Version == 0 || m.vault.AuthenticationLocked() {
		m.mu.Unlock()
		return OAuthSettings{}, http.StatusLocked, errors.New("unlock configuration encryption before changing OAuth settings")
	}
	previous := m.state.OAuth
	if expected != nil && previous.Revision != *expected {
		m.mu.Unlock()
		return OAuthSettings{}, http.StatusConflict, errors.New("OAuth settings changed; reload before saving")
	}
	if previous.Enabled == normalized.Enabled && previous.OAuthConfig == normalized.OAuthConfig {
		m.mu.Unlock()
		return previous, http.StatusOK, nil
	}
	normalized.Revision = previous.Revision + 1
	next := m.state
	next.OAuth = normalized
	err = m.save(next, false)
	// A durability warning can mean the state was published but its directory
	// sync failed. End old authority whenever the new state actually took effect.
	var cancel []context.CancelFunc
	type connection struct{ principal, credential string }
	var connections []connection
	if m.state.OAuth == normalized {
		for id, session := range m.oauthSessions {
			connections = append(connections, connection{session.principal, id})
			for _, stop := range m.requests["token:"+id] {
				cancel = append(cancel, stop)
			}
			for _, stop := range m.background[id] {
				cancel = append(cancel, stop)
			}
		}
		clear(m.oauthSessions)
	}
	invalidate := m.mcpConnectionInvalidator
	m.mu.Unlock()
	// Cancellation and registry callbacks may call back into Managed. The lock
	// protects the authority transition, never external work or notification.
	for _, stop := range cancel {
		stop()
	}
	if invalidate != nil {
		for _, c := range connections {
			invalidate(c.principal, c.credential)
		}
	}
	if err != nil {
		return OAuthSettings{}, http.StatusInternalServerError, errors.New("could not save OAuth settings; reload to check the result")
	}
	return normalized, http.StatusOK, nil
}

func (m *Managed) oauthSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		authJSON(w, http.StatusOK, map[string]any{"settings": m.currentOAuthSettings()})
	case http.MethodPut:
		var input OAuthSettings
		if err := decodeJSON(http.MaxBytesReader(w, r.Body, 8192), &input); err != nil {
			authError(w, http.StatusBadRequest, "invalid OAuth settings request")
			return
		}
		settings, status, err := m.updateOAuthSettings(input, &input.Revision)
		if err != nil {
			authError(w, status, err.Error())
			return
		}
		authJSON(w, http.StatusOK, map[string]any{"settings": settings})
	default:
		w.Header().Set("Allow", "GET, PUT")
		authError(w, http.StatusMethodNotAllowed, "use GET to read or PUT to save OAuth settings")
	}
}
