package auth

import (
	"context"
	"errors"
)

type backgroundWorkKey struct{}
type backgroundRegistrar func(context.CancelFunc) (func(), error)

// RegisterBackgroundWork binds work to the verified credential independently of
// the request lifetime. Normal response completion does not revoke background
// work; policy/binding changes and credential revocation do. No client flag can
// manufacture this registrar.
func RegisterBackgroundWork(ctx context.Context, cancel context.CancelFunc) (func(), error) {
	register, ok := ctx.Value(backgroundWorkKey{}).(backgroundRegistrar)
	if ok {
		return register(cancel)
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
	return context.WithValue(ctx, backgroundWorkKey{}, backgroundRegistrar(func(cancel context.CancelFunc) (func(), error) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a, ok := m.state.Agents[principal]
		token, exists := m.state.Tokens[credential]
		if !ok || !a.Enabled || !exists || token.PrincipalID != principal {
			return nil, errors.New("credential access has ended")
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
}
