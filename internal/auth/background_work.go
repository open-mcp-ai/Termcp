package auth

import (
	"context"
	"errors"
)

type backgroundWorkKey struct{}
type backgroundRegistrar func(context.Context, context.CancelFunc) (func(), error)

// RegisterBackgroundWork binds work to the verified credential independently of
// the request lifetime. Normal response completion does not revoke background
// work; policy/binding changes and credential revocation do. No client flag can
// manufacture this registrar.
func RegisterBackgroundWork(ctx context.Context, cancel context.CancelFunc) (func(), error) {
	register, ok := ctx.Value(backgroundWorkKey{}).(backgroundRegistrar)
	if ok {
		return register(ctx, cancel)
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
	return context.WithValue(ctx, backgroundWorkKey{}, backgroundRegistrar(func(message context.Context, cancel context.CancelFunc) (func(), error) {
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
			m.background[credential] = map[*int]context.CancelFunc{}
		}
		m.background[credential][ref] = cancel
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
	for _, cancel := range m.background[id] {
		cancel()
	}
	if m.mcpConnectionInvalidator != nil {
		principalID := ""
		if token, ok := m.state.Tokens[id]; ok {
			principalID = token.PrincipalID
		}
		m.mcpConnectionInvalidator(principalID, id)
	}
}
