package oauthserver

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/ory/fosite/handler/oauth2"
	"github.com/ory/fosite/handler/pkce"
	"github.com/ory/fosite/storage"
)

const (
	DefaultAccessTokenLifetime   = 10 * time.Minute
	DefaultRefreshTokenLifetime  = 30 * 24 * time.Hour
	DefaultAuthorizeCodeLifetime = 60 * time.Second
)

// Config is the protocol-facing configuration derived from public URLs. Secrets
// and OIDC settings stay in the auth state and never enter this package's
// metadata.
type Config struct {
	Issuer                string
	Resource              string
	AccessTokenLifetime   time.Duration
	RefreshTokenLifetime  time.Duration
	AuthorizeCodeLifetime time.Duration
}

// Storage is the transaction-aware Fosite adapter implemented by internal/auth.
// Every mutation must be visible through the supplied context transaction; the
// adapter publishes all changes with Commit.
type Storage interface {
	fosite.ClientManager
	oauth2.CoreStorage
	oauth2.TokenRevocationStorage
	pkce.PKCERequestStorage
	storage.Transactional
}

// Provider owns the mature OAuth protocol engine and its opaque token strategy.
type Provider struct {
	Config Config
	Engine fosite.OAuth2Provider

	config  *fosite.Config
	storage Storage
}

func NewProvider(config Config, storage Storage) (*Provider, error) {
	if storage == nil {
		return nil, errors.New("OAuth storage is required")
	}
	config.Issuer = trimURL(config.Issuer)
	config.Resource = trimURL(config.Resource)
	if config.Issuer == "" || config.Resource == "" {
		return nil, errors.New("OAuth issuer and resource are required")
	}
	if config.AccessTokenLifetime <= 0 {
		config.AccessTokenLifetime = DefaultAccessTokenLifetime
	}
	if config.RefreshTokenLifetime <= 0 {
		config.RefreshTokenLifetime = DefaultRefreshTokenLifetime
	}
	if config.AuthorizeCodeLifetime <= 0 {
		config.AuthorizeCodeLifetime = DefaultAuthorizeCodeLifetime
	}

	engineConfig := &fosite.Config{
		AccessTokenLifespan:            config.AccessTokenLifetime,
		RefreshTokenLifespan:           config.RefreshTokenLifetime,
		AuthorizeCodeLifespan:          config.AuthorizeCodeLifetime,
		GlobalSecret:                   []byte("opaque-storage-backed-tokens"),
		TokenURL:                       config.Issuer + "/oauth/token",
		AccessTokenIssuer:              config.Issuer,
		EnforcePKCE:                    true,
		EnforcePKCEForPublicClients:    true,
		EnablePKCEPlainChallengeMethod: false,
		ScopeStrategy:                  fosite.ExactScopeStrategy,
		AudienceMatchingStrategy:       fosite.ExactAudienceMatchingStrategy,
		RefreshTokenScopes:             []string{},
		SanitationWhiteList:            []string{"redirect_uri", "resource", "audience", "code_challenge", "code_challenge_method"},
		RedirectSecureChecker:          SecureRedirect,
		MinParameterEntropy:            8,
	}
	strategy := newOpaqueStrategy(engineConfig)
	engine := compose.Compose(
		engineConfig,
		storage,
		strategy,
		compose.OAuth2AuthorizeExplicitFactory,
		compose.OAuth2RefreshTokenGrantFactory,
		compose.OAuth2TokenRevocationFactory,
		compose.OAuth2PKCEFactory,
	)
	return &Provider{Config: config, Engine: engine, config: engineConfig, storage: storage}, nil
}

func trimURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func (p *Provider) NewAuthorizeRequest(ctx context.Context, r *http.Request) (fosite.AuthorizeRequester, error) {
	return p.Engine.NewAuthorizeRequest(ctx, r)
}

func (p *Provider) NewAuthorizeResponse(ctx context.Context, request fosite.AuthorizeRequester, session fosite.Session) (fosite.AuthorizeResponder, error) {
	return p.Engine.NewAuthorizeResponse(ctx, request, session)
}

func (p *Provider) NewAccessRequest(ctx context.Context, r *http.Request, session fosite.Session) (fosite.AccessRequester, error) {
	return p.Engine.NewAccessRequest(ctx, r, session)
}

func (p *Provider) NewAccessResponse(ctx context.Context, request fosite.AccessRequester) (fosite.AccessResponder, error) {
	return p.Engine.NewAccessResponse(ctx, request)
}

func (p *Provider) NewRevocationRequest(ctx context.Context, r *http.Request) error {
	return p.Engine.NewRevocationRequest(ctx, r)
}

// ResourceURL returns the exact MCP endpoint that owns OAuth credentials.
func (p *Provider) ResourceURL() string {
	if p == nil {
		return ""
	}
	return p.Config.Resource
}

func (p *Provider) Issuer() string {
	if p == nil {
		return ""
	}
	return p.Config.Issuer
}

func (p *Provider) AuthorizationMetadata() map[string]any {
	if p == nil {
		return nil
	}
	issuer := p.Config.Issuer
	return map[string]any{
		"issuer":                                         issuer,
		"authorization_endpoint":                         issuer + "/oauth/authorize",
		"token_endpoint":                                 issuer + "/oauth/token",
		"registration_endpoint":                          issuer + "/oauth/register",
		"revocation_endpoint":                            issuer + "/oauth/revoke",
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"scopes_supported":                               []string{"mcp", "offline_access"},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	}
}

func (p *Provider) ProtectedResourceMetadata() map[string]any {
	if p == nil {
		return nil
	}
	return map[string]any{
		"resource":                 p.Config.Resource,
		"authorization_servers":    []string{p.Config.Issuer},
		"scopes_supported":         []string{"mcp"},
		"bearer_methods_supported": []string{"header"},
	}
}

func MetadataURL(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	path := u.EscapedPath()
	u.RawQuery = ""
	u.Fragment = ""
	if path == "" || path == "/" {
		u.Path = "/.well-known/oauth-authorization-server"
	} else {
		u.Path = "/.well-known/oauth-authorization-server" + path
	}
	return u.String()
}

func ProtectedResourceMetadataURL(resource string) string {
	u, err := url.Parse(resource)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = "/.well-known/oauth-protected-resource" + u.EscapedPath()
	return u.String()
}
