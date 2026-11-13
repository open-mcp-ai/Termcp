package auth

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ory/fosite"

	"github.com/open-mcp-ai/termcp/internal/oauthserver"
)

const (
	agentOriginManual = "manual"
	agentOriginOAuth  = "oauth"
	agentOriginDaemon = "daemon_stdio"

	tokenKindStatic = "static"
	tokenKindDaemon = "daemon"

	localDaemonRegistrationID = "local-daemon"

	maxLoginUsers    = 4096
	maxOAuthClients  = 4096
	maxOAuthGrants   = 8192
	maxOAuthCodes    = 4096
	maxOAuthAccess   = 16384
	maxOAuthRefresh  = 16384
	maxGrantsPerUser = 256
)

type OAuthServerSettings struct {
	Enabled          bool   `json:"enabled"`
	Revision         uint64 `json:"revision"`
	PublicURL        string `json:"public_url,omitempty"`
	OIDCIssuer       string `json:"oidc_issuer,omitempty"`
	OIDCClientID     string `json:"oidc_client_id,omitempty"`
	OIDCClientSecret string `json:"oidc_client_secret,omitempty"`
}

type oauthServerView struct {
	Enabled       bool   `json:"enabled"`
	Revision      uint64 `json:"revision"`
	PublicURL     string `json:"public_url"`
	OIDCIssuer    string `json:"oidc_issuer"`
	OIDCClientID  string `json:"oidc_client_id"`
	OIDCSecretSet bool   `json:"oidc_client_secret_set"`
	CallbackURL   string `json:"callback_url"`
	ResourceURL   string `json:"resource_url"`
}

type loginUser struct {
	ID          string    `json:"id"`
	Issuer      string    `json:"issuer"`
	Subject     string    `json:"subject"`
	DisplayName string    `json:"display_name"`
	Username    string    `json:"username,omitempty"`
	AvatarURL   string    `json:"avatar_url,omitempty"`
	AvatarFile  string    `json:"avatar_file,omitempty"`
	AvatarType  string    `json:"avatar_type,omitempty"`
	Enabled     bool      `json:"enabled"`
	Revision    uint64    `json:"revision"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	LastLoginAt time.Time `json:"last_login_at"`
}

type oauthGrant struct {
	ID             string     `json:"id"`
	UserID         string     `json:"user_id"`
	AgentID        string     `json:"agent_id"`
	ClientID       string     `json:"client_id"`
	Resource       string     `json:"resource"`
	Scopes         []string   `json:"scopes"`
	ConfigRevision uint64     `json:"config_revision"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	LastUsedAt     *time.Time `json:"last_used_at,omitempty"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	RevokeReason   string     `json:"revoke_reason,omitempty"`
}

type oauthStoredSession struct {
	ExpiresAt map[string]time.Time `json:"expires_at"`
	Username  string               `json:"username,omitempty"`
	Subject   string               `json:"subject,omitempty"`
	Extra     map[string]any       `json:"extra,omitempty"`
}

type oauthStoredRequest struct {
	ID                string              `json:"id"`
	RequestedAt       time.Time           `json:"requested_at"`
	ClientID          string              `json:"client_id"`
	RequestedScopes   []string            `json:"requested_scopes,omitempty"`
	GrantedScopes     []string            `json:"granted_scopes,omitempty"`
	RequestedAudience []string            `json:"requested_audience,omitempty"`
	GrantedAudience   []string            `json:"granted_audience,omitempty"`
	Form              map[string][]string `json:"form,omitempty"`
	Session           oauthStoredSession  `json:"session"`
}

type oauthCodeRecord struct {
	Request       oauthStoredRequest `json:"request"`
	ExpiresAt     time.Time          `json:"expires_at"`
	ConsumedAt    *time.Time         `json:"consumed_at,omitempty"`
	PKCEChallenge string             `json:"pkce_challenge,omitempty"`
	PKCEMethod    string             `json:"pkce_method,omitempty"`
}

type oauthCredentialRecord struct {
	ID         string             `json:"id"`
	GrantID    string             `json:"grant_id"`
	ClientID   string             `json:"client_id"`
	Resource   string             `json:"resource"`
	Scopes     []string           `json:"scopes"`
	FamilyID   string             `json:"family_id"`
	Request    oauthStoredRequest `json:"request"`
	CreatedAt  time.Time          `json:"created_at"`
	ExpiresAt  time.Time          `json:"expires_at"`
	LastUsedAt *time.Time         `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time         `json:"revoked_at,omitempty"`
	ReplacedBy string             `json:"replaced_by,omitempty"`
}

type pendingOAuthGrant struct {
	UserID         string `json:"user_id"`
	Issuer         string `json:"issuer"`
	Subject        string `json:"subject"`
	DisplayName    string `json:"display_name"`
	Username       string `json:"username,omitempty"`
	AvatarURL      string `json:"avatar_url,omitempty"`
	AvatarFile     string `json:"avatar_file,omitempty"`
	AvatarType     string `json:"avatar_type,omitempty"`
	ClientID       string `json:"client_id"`
	Resource       string `json:"resource"`
	ConfigRevision uint64 `json:"config_revision"`
}

// daemonRegistration is the durable trust record for the local stdio bridge.
// The raw bridge credential is intentionally kept outside the encrypted auth
// JSON in a 0600 data-directory file; the encrypted state contains only its
// digest-backed token record and the ownership/registration relationship.
type daemonRegistration struct {
	ID            string     `json:"id"`
	AgentID       string     `json:"agent_id"`
	TokenID       string     `json:"token_id"`
	CreatedAt     time.Time  `json:"created_at"`
	ProvisionedAt time.Time  `json:"provisioned_at"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokeReason  string     `json:"revoke_reason,omitempty"`
}

func normalizeOAuthServerSettings(settings OAuthServerSettings) (OAuthServerSettings, error) {
	settings.PublicURL = strings.TrimRight(strings.TrimSpace(settings.PublicURL), "/")
	settings.OIDCIssuer = strings.TrimSpace(settings.OIDCIssuer)
	settings.OIDCClientID = strings.TrimSpace(settings.OIDCClientID)
	settings.OIDCClientSecret = strings.TrimSpace(settings.OIDCClientSecret)
	if settings.PublicURL == "" && settings.OIDCIssuer == "" && settings.OIDCClientID == "" && settings.OIDCClientSecret == "" {
		return settings, nil
	}
	if err := validatePublicURL(settings.PublicURL, "public URL"); err != nil {
		return OAuthServerSettings{}, err
	}
	if settings.OIDCIssuer == "" || settings.OIDCClientID == "" {
		return OAuthServerSettings{}, errOAuthIncomplete
	}
	if err := validateHTTPURL(settings.OIDCIssuer, "OIDC issuer", true); err != nil {
		return OAuthServerSettings{}, err
	}
	if settings.Enabled && settings.OIDCClientSecret == "" {
		// Public OIDC clients are valid. The callback and client ID still bind
		// the login; no secret is required merely to enable the flow.
	}
	return settings, nil
}

func normalizeOAuthState(s *managedState) bool {
	changed := false
	if s.LoginUsers == nil {
		s.LoginUsers = map[string]loginUser{}
		changed = true
	}
	if s.OAuthClients == nil {
		s.OAuthClients = map[string]oauthserver.Client{}
		changed = true
	}
	if s.OAuthGrants == nil {
		s.OAuthGrants = map[string]oauthGrant{}
		changed = true
	}
	if s.OAuthCodes == nil {
		s.OAuthCodes = map[string]oauthCodeRecord{}
		changed = true
	}
	if s.OAuthAccess == nil {
		s.OAuthAccess = map[string]oauthCredentialRecord{}
		changed = true
	}
	if s.OAuthRefresh == nil {
		s.OAuthRefresh = map[string]oauthCredentialRecord{}
		changed = true
	}
	for id, token := range s.Tokens {
		if token.Kind == "" {
			token.Kind = tokenKindStatic
			s.Tokens[id] = token
			changed = true
		}
	}
	if s.OAuthRevision == 0 {
		s.OAuthRevision = 1
		changed = true
	}
	for id, agent := range s.Agents {
		if agent.Origin == "" {
			agent.Origin = agentOriginManual
			s.Agents[id] = agent
			changed = true
		}
	}
	return changed
}

func cloneManagedState(s managedState) managedState {
	s.Sessions = maps.Clone(s.Sessions)
	if s.Sessions == nil {
		s.Sessions = map[string]sessionRecord{}
	}
	s.Permissions = cloneRolePolicy(s.Permissions)
	s.Agents = maps.Clone(s.Agents)
	if s.Agents == nil {
		s.Agents = map[string]agentRecord{}
	}
	s.Tokens = maps.Clone(s.Tokens)
	if s.Tokens == nil {
		s.Tokens = map[string]tokenRecord{}
	}
	s.Groups = cloneGroups(s.Groups)
	s.LoginUsers = maps.Clone(s.LoginUsers)
	if s.LoginUsers == nil {
		s.LoginUsers = map[string]loginUser{}
	}
	s.OAuthClients = maps.Clone(s.OAuthClients)
	if s.OAuthClients == nil {
		s.OAuthClients = map[string]oauthserver.Client{}
	}
	for id, client := range s.OAuthClients {
		client.RedirectURIs = slices.Clone(client.RedirectURIs)
		s.OAuthClients[id] = client
	}
	s.OAuthGrants = maps.Clone(s.OAuthGrants)
	if s.OAuthGrants == nil {
		s.OAuthGrants = map[string]oauthGrant{}
	}
	for id, grant := range s.OAuthGrants {
		grant.Scopes = slices.Clone(grant.Scopes)
		s.OAuthGrants[id] = grant
	}
	s.OAuthCodes = maps.Clone(s.OAuthCodes)
	if s.OAuthCodes == nil {
		s.OAuthCodes = map[string]oauthCodeRecord{}
	}
	for id, code := range s.OAuthCodes {
		code.Request = cloneStoredRequest(code.Request)
		s.OAuthCodes[id] = code
	}
	s.OAuthAccess = maps.Clone(s.OAuthAccess)
	if s.OAuthAccess == nil {
		s.OAuthAccess = map[string]oauthCredentialRecord{}
	}
	for id, credential := range s.OAuthAccess {
		credential.Scopes = slices.Clone(credential.Scopes)
		credential.Request = cloneStoredRequest(credential.Request)
		s.OAuthAccess[id] = credential
	}
	s.OAuthRefresh = maps.Clone(s.OAuthRefresh)
	if s.OAuthRefresh == nil {
		s.OAuthRefresh = map[string]oauthCredentialRecord{}
	}
	for id, credential := range s.OAuthRefresh {
		credential.Scopes = slices.Clone(credential.Scopes)
		credential.Request = cloneStoredRequest(credential.Request)
		s.OAuthRefresh[id] = credential
	}
	if s.DaemonRegistration != nil {
		registration := *s.DaemonRegistration
		s.DaemonRegistration = &registration
	}
	return s
}

func cloneRolePolicy(in RolePolicy) RolePolicy {
	out := make(RolePolicy, len(in))
	for role, surfaces := range in {
		out[role] = slices.Clone(surfaces)
	}
	return out
}

func cloneStoredRequest(in oauthStoredRequest) oauthStoredRequest {
	in.RequestedScopes = slices.Clone(in.RequestedScopes)
	in.GrantedScopes = slices.Clone(in.GrantedScopes)
	in.RequestedAudience = slices.Clone(in.RequestedAudience)
	in.GrantedAudience = slices.Clone(in.GrantedAudience)
	if in.Form != nil {
		form := make(map[string][]string, len(in.Form))
		for key, values := range in.Form {
			form[key] = slices.Clone(values)
		}
		in.Form = form
	}
	if in.Session.ExpiresAt != nil {
		in.Session.ExpiresAt = maps.Clone(in.Session.ExpiresAt)
	}
	in.Session.Extra = cloneAnyMap(in.Session.Extra)
	return in
}

func cloneAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := maps.Clone(in)
	for key, value := range out {
		if nested, ok := value.(map[string]any); ok {
			out[key] = cloneAnyMap(nested)
		}
	}
	return out
}

func (m *Managed) currentOAuthServerView() oauthServerView {
	m.mu.Lock()
	defer m.mu.Unlock()
	settings := m.state.OAuthServer
	return oauthView(settings)
}

func oauthView(settings OAuthServerSettings) oauthServerView {
	view := oauthServerView{
		Enabled:       settings.Enabled,
		Revision:      settings.Revision,
		PublicURL:     settings.PublicURL,
		OIDCIssuer:    settings.OIDCIssuer,
		OIDCClientID:  settings.OIDCClientID,
		OIDCSecretSet: settings.OIDCClientSecret != "",
	}
	if settings.PublicURL != "" {
		view.CallbackURL = settings.PublicURL + "/oauth/oidc/callback"
		view.ResourceURL = settings.PublicURL + "/stream"
	}
	return view
}

func storedSession(session fosite.Session) oauthStoredSession {
	stored := oauthStoredSession{ExpiresAt: map[string]time.Time{}}
	if session == nil {
		return stored
	}
	stored.Username = session.GetUsername()
	stored.Subject = session.GetSubject()
	for _, kind := range []fosite.TokenType{
		fosite.AuthorizeCode,
		fosite.AccessToken,
		fosite.RefreshToken,
		fosite.IDToken,
	} {
		if expiry := session.GetExpiresAt(kind); !expiry.IsZero() {
			stored.ExpiresAt[string(kind)] = expiry
		}
	}
	if extra, ok := session.(interface{ GetExtraClaims() map[string]any }); ok {
		stored.Extra = cloneAnyMap(extra.GetExtraClaims())
	}
	return stored
}

func hydrateSession(target fosite.Session, stored oauthStoredSession) fosite.Session {
	session := target
	if session == nil {
		session = new(fosite.DefaultSession)
	}
	if value, ok := session.(*fosite.DefaultSession); ok {
		value.Username = stored.Username
		value.Subject = stored.Subject
		if value.ExpiresAt == nil {
			value.ExpiresAt = map[fosite.TokenType]time.Time{}
		}
		for kind, expiry := range stored.ExpiresAt {
			if _, exists := value.ExpiresAt[fosite.TokenType(kind)]; !exists {
				value.ExpiresAt[fosite.TokenType(kind)] = expiry
			}
		}
		if value.Extra == nil {
			value.Extra = cloneAnyMap(stored.Extra)
		} else {
			for key, item := range stored.Extra {
				if _, exists := value.Extra[key]; !exists {
					value.Extra[key] = item
				}
			}
		}
		return value
	}
	return session
}
