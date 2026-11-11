package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/open-mcp-ai/termcp/internal/oauthserver"
)

type oauthAuthority struct {
	GrantID  string
	UserID   string
	ClientID string
	Resource string
}

type oauthAuthorityKey struct{}
type oauthCredentialKey struct{}

func OAuthGrantIDFromContext(ctx context.Context) string {
	authority, _ := ctx.Value(oauthAuthorityKey{}).(oauthAuthority)
	return authority.GrantID
}

func OAuthClientIDFromContext(ctx context.Context) string {
	authority, _ := ctx.Value(oauthAuthorityKey{}).(oauthAuthority)
	return authority.ClientID
}

func OAuthResourceFromContext(ctx context.Context) string {
	authority, _ := ctx.Value(oauthAuthorityKey{}).(oauthAuthority)
	return authority.Resource
}

func IsOAuthCredential(ctx context.Context) bool {
	value, _ := ctx.Value(oauthCredentialKey{}).(bool)
	return value
}

// WithVerifiedOAuthAuthority attaches authority that a caller has already
// verified. Production admission uses admitOAuthToken; this constructor exists
// for transport-level composition and tests that do not re-run admission.
func WithVerifiedOAuthAuthority(ctx context.Context, grantID, clientID, resource string) context.Context {
	ctx = context.WithValue(ctx, oauthCredentialKey{}, true)
	return context.WithValue(ctx, oauthAuthorityKey{}, oauthAuthority{
		GrantID:  grantID,
		ClientID: clientID,
		Resource: resource,
	})
}

func (m *Managed) admitOAuthToken(r *http.Request, raw string) (context.Context, context.CancelFunc, int) {
	if !stringsHasPrefix(raw, "toa_") || len(raw) > 8192 {
		return nil, nil, http.StatusUnauthorized
	}
	now := m.now().UTC()
	signature := oauthserver.TokenSignature(raw)
	expectedResource := m.oauthResource()
	m.mu.Lock()
	if m.vault.AuthenticationLocked() {
		m.mu.Unlock()
		return nil, nil, http.StatusLocked
	}
	if !m.state.OAuthServer.Enabled || m.oauthProtocol == nil {
		m.mu.Unlock()
		return nil, nil, http.StatusUnauthorized
	}
	record, ok := m.state.OAuthAccess[signature]
	if !ok || record.RevokedAt != nil || !now.Before(record.ExpiresAt) {
		m.mu.Unlock()
		return nil, nil, http.StatusUnauthorized
	}
	grant, ok := m.state.OAuthGrants[record.GrantID]
	if !ok || !grantActive(grant, now) || grant.ConfigRevision != m.state.OAuthServer.Revision ||
		record.ClientID != grant.ClientID || grant.Resource != expectedResource || record.Resource != expectedResource ||
		!slices.Contains(record.Scopes, "mcp") {
		m.mu.Unlock()
		return nil, nil, http.StatusUnauthorized
	}
	user, ok := m.state.LoginUsers[grant.UserID]
	if !ok || !user.Enabled {
		m.mu.Unlock()
		return nil, nil, http.StatusUnauthorized
	}
	client, ok := m.state.OAuthClients[grant.ClientID]
	if !ok || !client.Enabled {
		m.mu.Unlock()
		return nil, nil, http.StatusUnauthorized
	}
	agent, ok := m.state.Agents[grant.AgentID]
	if !ok || !agent.Enabled || agent.OwnerUserID != user.ID || agent.Origin != agentOriginOAuth {
		m.mu.Unlock()
		return nil, nil, http.StatusUnauthorized
	}
	if _, err := resolvedAgentPolicy(m.state, agent); err != nil {
		m.mu.Unlock()
		return nil, nil, http.StatusUnauthorized
	}
	if !slices.Contains(m.state.Permissions[RoleAgent], "mcp") || requestSurface(r) != "mcp" {
		m.mu.Unlock()
		return nil, nil, http.StatusForbidden
	}
	switch r.URL.Path {
	case "/stream", "/sse", "/message", "/auth/mcp":
	default:
		m.mu.Unlock()
		return nil, nil, http.StatusForbidden
	}
	record.LastUsedAt = &now
	m.state.OAuthAccess[signature] = record
	grant.LastUsedAt = &now
	m.state.OAuthGrants[grant.ID] = grant
	actor := Actor{ID: agent.ID, Username: agent.Name, Role: RoleAgent}
	baseCtx, baseCancel := context.WithDeadline(r.Context(), record.ExpiresAt)
	ctx := baseCtx
	ctx = m.withOAuthAgentPolicy(ctx, signature, record.ID, agent.ID, grant.ID)
	ctx = m.withOAuthBackgroundWork(ctx, signature, record.ID, agent.ID, grant.ID, record.ExpiresAt)
	ctx = context.WithValue(ctx, actorKey{}, actor)
	ctx = context.WithValue(ctx, credentialKey{}, "token:"+record.ID)
	ctx = context.WithValue(ctx, oauthCredentialKey{}, true)
	ctx = context.WithValue(ctx, oauthAuthorityKey{}, oauthAuthority{
		GrantID:  grant.ID,
		UserID:   user.ID,
		ClientID: client.ID,
		Resource: grant.Resource,
	})
	ref := new(int)
	key := "token:" + record.ID
	if m.requests[key] == nil {
		m.requests[key] = map[*int]context.CancelFunc{}
	}
	m.requests[key][ref] = baseCancel
	m.mu.Unlock()
	return ctx, func() {
		baseCancel()
		m.mu.Lock()
		delete(m.requests[key], ref)
		if len(m.requests[key]) == 0 {
			delete(m.requests, key)
		}
		m.mu.Unlock()
	}, 0
}

func stringsHasPrefix(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}

func (m *Managed) withOAuthAgentPolicy(ctx context.Context, signature, credentialID, principalID, grantID string) context.Context {
	return context.WithValue(ctx, agentPolicyKey{}, policyResolver(func() (AgentPolicy, error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := m.oauthAuthorityLocked(signature, credentialID, principalID, grantID); err != nil {
			return AgentPolicy{}, err
		}
		agent := m.state.Agents[principalID]
		return resolvedAgentPolicy(m.state, agent)
	}))
}

func (m *Managed) withOAuthBackgroundWork(ctx context.Context, signature, credentialID, principalID, grantID string, expires time.Time) context.Context {
	return context.WithValue(ctx, backgroundWorkKey{}, backgroundRegistrar(func(message context.Context, cancel context.CancelFunc) (func(), error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := message.Err(); err != nil {
			return nil, err
		}
		if !expires.After(m.now().UTC()) {
			return nil, errors.New("credential access has ended")
		}
		if err := m.oauthAuthorityLocked(signature, credentialID, principalID, grantID); err != nil {
			return nil, err
		}
		ref := new(int)
		if m.background[credentialID] == nil {
			m.background[credentialID] = map[*int]context.CancelFunc{}
		}
		m.background[credentialID][ref] = cancel
		return func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			delete(m.background[credentialID], ref)
			if len(m.background[credentialID]) == 0 {
				delete(m.background, credentialID)
			}
		}, nil
	}))
}

func (m *Managed) oauthAuthorityLocked(signature, credentialID, principalID, grantID string) error {
	now := m.now().UTC()
	if m.vault.AuthenticationLocked() || !m.state.OAuthServer.Enabled {
		return errors.New("OAuth authority is unavailable")
	}
	record, found := m.state.OAuthAccess[signature]
	if !found || record.GrantID != grantID || record.RevokedAt != nil || !now.Before(record.ExpiresAt) ||
		record.ID != credentialID || record.Resource != resourceForState(m.state) || !slices.Contains(record.Scopes, "mcp") {
		return errors.New("OAuth credential has ended")
	}
	grant, ok := m.state.OAuthGrants[grantID]
	if !ok || !grantActive(grant, now) || grant.AgentID != principalID ||
		grant.ConfigRevision != m.state.OAuthServer.Revision {
		return errors.New("OAuth grant has ended")
	}
	user, ok := m.state.LoginUsers[grant.UserID]
	if !ok || !user.Enabled {
		return errors.New("login user is disabled")
	}
	client, ok := m.state.OAuthClients[grant.ClientID]
	if !ok || !client.Enabled || record.ClientID != client.ID || grant.Resource != record.Resource {
		return errors.New("OAuth client is disabled")
	}
	agent, ok := m.state.Agents[principalID]
	if !ok || !agent.Enabled || agent.OwnerUserID != user.ID {
		return errors.New("Agent access has ended")
	}
	if !slices.Contains(m.state.Permissions[RoleAgent], "mcp") {
		return errors.New("Agent MCP access is disabled")
	}
	return nil
}
