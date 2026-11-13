package auth

import (
	"context"
	"errors"
)

type backgroundWorkKey struct{}
type backgroundWork struct {
	cancel    context.CancelFunc
	authority BackgroundWorkAuthority
}

// BackgroundWorkAuthority identifies the capabilities and resources whose
// revocation can invalidate detached work. Empty authority is conservatively
// canceled on any policy change for the same Agent.
type BackgroundWorkAuthority struct {
	Operations []string
	Hosts      []string
}

type backgroundRegistrar func(context.Context, context.CancelFunc, BackgroundWorkAuthority) (func(), error)

// RegisterBackgroundWork binds work to the verified credential independently of
// the request lifetime. Normal response completion does not revoke background
// work; policy/binding changes and credential revocation do. No client flag can
// manufacture this registrar.
func RegisterBackgroundWork(ctx context.Context, cancel context.CancelFunc) (func(), error) {
	return RegisterBackgroundWorkFor(ctx, cancel, BackgroundWorkAuthority{})
}

// RegisterBackgroundWorkFor binds detached work to the exact operations and
// host identities it continues to exercise. A policy change only cancels work
// whose authority is actually narrowed or removed.
func RegisterBackgroundWorkFor(ctx context.Context, cancel context.CancelFunc, authority BackgroundWorkAuthority) (func(), error) {
	authority.Operations = append([]string(nil), authority.Operations...)
	authority.Hosts = append([]string(nil), authority.Hosts...)
	register, ok := ctx.Value(backgroundWorkKey{}).(backgroundRegistrar)
	if ok {
		return register(ctx, cancel, authority)
	}
	if actor, present := ActorFromContext(ctx); present && actor.Role == RoleAgent {
		return nil, errors.New("Agent background-work authority is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() {}, nil
}
func (m *Managed) withBackgroundWork(ctx context.Context, principal, credential string) context.Context {
	// Called by admitToken with m.mu held. Bind the request's authority, not the
	// HTTP response's lifetime: SSE handles a message after returning HTTP 202.
	revision := agentRevision(m.state.Agents[principal])
	roleRevision := m.state.Revision
	return context.WithValue(ctx, backgroundWorkKey{}, backgroundRegistrar(func(message context.Context, cancel context.CancelFunc, authority BackgroundWorkAuthority) (func(), error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := message.Err(); err != nil {
			return nil, err
		}
		a, ok := m.state.Agents[principal]
		token, exists := m.state.Tokens[credential]
		if !ok || !a.Enabled || !exists || token.PrincipalID != principal || !tokenLive(token, m.now().UTC()) {
			return nil, errors.New("credential access has ended")
		}
		if agentRevision(a) != revision || m.state.Revision != roleRevision {
			return nil, errors.New("credential authority changed; start a new request")
		}
		if _, err := resolvedAgentPolicy(m.state, a); err != nil {
			return nil, err
		}
		permitted := false
		for _, surface := range m.state.Permissions[RoleAgent] {
			if surface == "mcp" {
				permitted = true
			}
		}
		if !permitted {
			return nil, errors.New("Agent MCP access has ended")
		}
		ref := new(int)
		if m.background[credential] == nil {
			m.background[credential] = map[*int]backgroundWork{}
		}
		m.background[credential][ref] = backgroundWork{cancel: cancel, authority: authority}
		return func() {
			m.mu.Lock()
			defer m.mu.Unlock()
			delete(m.background[credential], ref)
			if len(m.background[credential]) == 0 {
				delete(m.background, credential)
			}
		}, nil
	}))
}
func (m *Managed) cancelCredentialLocked(id string) {
	for _, cancel := range m.requests["token:"+id] {
		cancel()
	}
	for _, work := range m.background[id] {
		work.cancel()
	}
	if m.mcpConnectionInvalidator != nil {
		principalID := ""
		if token, ok := m.state.Tokens[id]; ok {
			principalID = token.PrincipalID
		} else {
			for _, record := range m.state.OAuthAccess {
				if record.ID == id {
					principalID = m.state.OAuthGrants[record.GrantID].AgentID
					break
				}
			}
		}
		m.mcpConnectionInvalidator(principalID, id)
	}
}
