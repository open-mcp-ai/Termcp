package auth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/oauthserver"
)

var errOAuthIncomplete = errors.New("OAuth public URL, OIDC issuer, and OIDC client ID are required")

type oauthSettingsInput struct {
	Enabled          bool   `json:"enabled"`
	Revision         uint64 `json:"revision"`
	PublicURL        string `json:"public_url"`
	OIDCIssuer       string `json:"oidc_issuer"`
	OIDCClientID     string `json:"oidc_client_id"`
	OIDCClientSecret string `json:"oidc_client_secret,omitempty"`
	ClearOIDCSecret  bool   `json:"clear_oidc_client_secret,omitempty"`
}

func validatePublicURL(raw, name string) error {
	return validateHTTPURL(raw, name, true)
}

func validateHTTPURL(raw, name string, allowPath bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New(name + " must be an absolute HTTP(S) URL")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New(name + " must not contain a query or fragment")
	}
	if !allowPath && u.Path != "" && u.Path != "/" {
		return errors.New(name + " must not contain a path")
	}
	if u.Scheme == "http" {
		host := strings.Trim(u.Hostname(), "[]")
		ip := net.ParseIP(host)
		if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
			return errors.New(name + " must use HTTPS unless it is loopback")
		}
	}
	return nil
}

func (m *Managed) OAuthEnabled() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.OAuthServer.Enabled && m.oauthProtocol != nil
}

// applyOAuthSettingsLocked rebuilds only in-memory protocol objects. Discovery,
// OIDC network calls, and file writes happen outside m.mu.
func (m *Managed) applyOAuthSettingsLocked() {
	settings := m.state.OAuthServer
	if !settings.Enabled {
		m.oauthProtocol = nil
		return
	}
	config := oauthserver.Config{
		Issuer:   settings.PublicURL,
		Resource: settings.PublicURL + "/stream",
	}
	if m.oauthProtocol != nil &&
		m.oauthProtocol.Config.Issuer == config.Issuer &&
		m.oauthProtocol.Config.Resource == config.Resource {
		return
	}
	provider, err := oauthserver.NewProvider(config, &oauthFositeStorage{managed: m})
	if err != nil {
		m.oauthProtocol = nil
		return
	}
	m.oauthProtocol = provider
}

func (m *Managed) currentOAuthServerSettings() OAuthServerSettings {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.OAuthServer
}

func (m *Managed) updateOAuthServerSettings(ctx context.Context, input oauthSettingsInput, expected *uint64) (OAuthServerSettings, int, error) {
	if m == nil {
		return OAuthServerSettings{}, http.StatusServiceUnavailable, errors.New("managed auth is unavailable")
	}
	var published OAuthServerSettings
	err := m.commitState(ctx, func(next *managedState) (func(), error) {
		if expected != nil && next.OAuthServer.Revision != *expected {
			return nil, &statusError{status: http.StatusConflict, err: errors.New("OAuth settings changed; reload before saving")}
		}
		previous := next.OAuthServer
		settings := OAuthServerSettings{
			Enabled:          input.Enabled,
			Revision:         previous.Revision,
			PublicURL:        input.PublicURL,
			OIDCIssuer:       input.OIDCIssuer,
			OIDCClientID:     input.OIDCClientID,
			OIDCClientSecret: previous.OIDCClientSecret,
		}
		if input.ClearOIDCSecret {
			settings.OIDCClientSecret = ""
		} else if strings.TrimSpace(input.OIDCClientSecret) != "" {
			settings.OIDCClientSecret = input.OIDCClientSecret
		}
		normalized, err := normalizeOAuthServerSettings(settings)
		if err != nil {
			return nil, &statusError{status: http.StatusBadRequest, err: err}
		}
		if normalized == previous {
			published = previous
			return nil, nil
		}
		normalized.Revision = previous.Revision + 1
		published = normalized
		next.OAuthServer = normalized

		_, affected := revokeAllOAuth(next, m.now().UTC(), "oauth_configuration_changed")
		return func() {
			m.clearOAuthTransactions()
			m.cancelOAuthCredentials(affected)
		}, nil
	})
	if err != nil {
		if status, ok := err.(*statusError); ok {
			return OAuthServerSettings{}, status.status, status.err
		}
		if errors.Is(err, errVaultLocked) {
			return OAuthServerSettings{}, http.StatusLocked, errors.New("unlock configuration encryption before changing OAuth settings")
		}
		return OAuthServerSettings{}, http.StatusInternalServerError, errors.New("could not save OAuth settings; reload to check the result")
	}
	return published, http.StatusOK, nil
}

type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string { return e.err.Error() }
func (e *statusError) Unwrap() error { return e.err }

func (m *Managed) oauthSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		settings := m.currentOAuthServerSettings()
		authJSON(w, http.StatusOK, map[string]any{"settings": oauthView(settings)})
	case http.MethodPut:
		var input oauthSettingsInput
		if err := decodeJSON(http.MaxBytesReader(w, r.Body, 16384), &input); err != nil {
			authError(w, http.StatusBadRequest, "invalid OAuth settings request")
			return
		}
		settings, status, err := m.updateOAuthServerSettings(r.Context(), input, &input.Revision)
		if err != nil {
			authError(w, status, err.Error())
			return
		}
		authJSON(w, http.StatusOK, map[string]any{"settings": oauthView(settings)})
	default:
		w.Header().Set("Allow", "GET, PUT")
		authError(w, http.StatusMethodNotAllowed, "use GET to read or PUT to save OAuth settings")
	}
}

func (m *Managed) oauthSettingsTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		authError(w, http.StatusMethodNotAllowed, "use POST to test OIDC discovery")
		return
	}
	var input oauthSettingsInput
	if err := decodeJSON(http.MaxBytesReader(w, r.Body, 16384), &input); err != nil {
		authError(w, http.StatusBadRequest, "invalid OIDC test request")
		return
	}
	settings := OAuthServerSettings{
		PublicURL:    input.PublicURL,
		OIDCIssuer:   input.OIDCIssuer,
		OIDCClientID: input.OIDCClientID,
	}
	if input.ClearOIDCSecret {
		settings.OIDCClientSecret = ""
	} else if input.OIDCClientSecret == "" {
		settings.OIDCClientSecret = m.currentOAuthServerSettings().OIDCClientSecret
	} else {
		settings.OIDCClientSecret = input.OIDCClientSecret
	}
	normalized, err := normalizeOAuthServerSettings(settings)
	if err != nil {
		authError(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	_, err = oauthserver.NewOIDC(ctx, oauthserver.OIDCConfig{
		Issuer:       normalized.OIDCIssuer,
		ClientID:     normalized.OIDCClientID,
		ClientSecret: normalized.OIDCClientSecret,
		RedirectURL:  normalized.PublicURL + "/oauth/oidc/callback",
	})
	if err != nil {
		authError(w, http.StatusBadGateway, "OIDC discovery or verifier setup failed")
		return
	}
	authJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"issuer":       normalized.OIDCIssuer,
		"callback_url": normalized.PublicURL + "/oauth/oidc/callback",
	})
}
