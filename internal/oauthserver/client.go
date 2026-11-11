package oauthserver

import (
	"context"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/ory/fosite"
)

const (
	ClientRegistrationDCR  = "dcr"
	ClientRegistrationCIMD = "cimd"
)

// Client is the encrypted registration record. It grants only the ability to
// begin a browser authorization; business authority comes from the resulting
// Agent and group.
type Client struct {
	ID           string     `json:"id"`
	Name         string     `json:"name,omitempty"`
	URI          string     `json:"uri,omitempty"`
	LogoURI      string     `json:"logo_uri,omitempty"`
	Registration string     `json:"registration"`
	RedirectURIs []string   `json:"redirect_uris"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	Enabled      bool       `json:"enabled"`
}

func (c Client) FositeClient(resource string) fosite.Client {
	public := &fosite.DefaultClient{
		ID:            c.ID,
		RedirectURIs:  append([]string(nil), c.RedirectURIs...),
		GrantTypes:    []string{"authorization_code", "refresh_token"},
		ResponseTypes: []string{"code"},
		Scopes:        []string{"mcp", "offline_access"},
		Audience:      []string{resource},
		Public:        true,
	}
	return &fosite.DefaultOpenIDConnectClient{
		DefaultClient:           public,
		TokenEndpointAuthMethod: "none",
	}
}

func SecureRedirect(_ context.Context, target *url.URL) bool {
	if target == nil || target.User != nil || target.Fragment != "" {
		return false
	}
	if target.Scheme == "https" && target.Host != "" {
		return true
	}
	return target.Scheme == "http" && isLoopbackHost(target.Hostname()) && target.Path != ""
}

func ValidRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.String() != raw {
		return false
	}
	return SecureRedirect(nil, u)
}

func RedirectAllowed(registered []string, requested *url.URL) bool {
	if requested == nil {
		return false
	}
	if !SecureRedirect(context.Background(), requested) {
		return false
	}
	for _, raw := range registered {
		allowed, err := url.Parse(raw)
		if err != nil || !SecureRedirect(context.Background(), allowed) {
			continue
		}
		if sameRedirect(allowed, requested) {
			return true
		}
	}
	return false
}

func sameRedirect(allowed, requested *url.URL) bool {
	if allowed == nil || requested == nil || allowed.Scheme != requested.Scheme ||
		allowed.User != nil || requested.User != nil ||
		allowed.Fragment != "" || requested.Fragment != "" ||
		allowed.EscapedPath() != requested.EscapedPath() ||
		allowed.RawQuery != requested.RawQuery {
		return false
	}
	if strings.EqualFold(allowed.Host, requested.Host) {
		return true
	}
	if allowed.Scheme != "http" || !isLoopbackHost(allowed.Hostname()) || !isLoopbackHost(requested.Hostname()) {
		return false
	}
	return allowed.Hostname() == requested.Hostname()
}

func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
