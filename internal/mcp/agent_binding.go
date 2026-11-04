package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/auth"
	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

var errAgentBindingMissing = errors.New("Agent resource binding is unavailable")

type agentResourceBindings struct {
	profiles   map[string]sshconfig.AuthorizationBinding
	extensions map[string]extensions.AuthorizationBinding
}

type agentResourceBindingsKey struct{}

func extensionBindingKey(name, target string) string {
	return strings.TrimSpace(name) + "\x00" + strings.TrimSpace(target)
}

func withAgentResourceBindings(ctx context.Context, bindings agentResourceBindings) context.Context {
	return context.WithValue(ctx, agentResourceBindingsKey{}, bindings)
}

func agentBindingsFromContext(ctx context.Context) (agentResourceBindings, bool) {
	bindings, ok := ctx.Value(agentResourceBindingsKey{}).(agentResourceBindings)
	return bindings, ok
}

func profileReferenceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if looksLikeResourceLocator(name) {
		p, err := parseResourceURL(name)
		if err != nil {
			return "", err
		}
		if p.Kind != resourceURLEntry {
			return "", errInvalidResourceLocator
		}
		name = p.Entry
	}
	return name, nil
}

func (s *Server) profileAuthorizationBinding(name string) (sshconfig.AuthorizationBinding, error) {
	if s.sshConfigs == nil {
		return sshconfig.AuthorizationBinding{}, errors.New("ssh config store not configured")
	}
	canonical, err := profileReferenceName(name)
	if err != nil {
		return sshconfig.AuthorizationBinding{}, err
	}
	if canonical == "" {
		return sshconfig.AuthorizationBinding{}, errMissingSSHConfig
	}
	return s.sshConfigs.Binding(canonical)
}

func (s *Server) extensionAuthorizationBinding(name, target string) (extensions.AuthorizationBinding, error) {
	if s.extensions == nil {
		return extensions.AuthorizationBinding{}, errors.New("extensions are not configured")
	}
	return s.extensions.Binding(strings.TrimSpace(name), strings.TrimSpace(target))
}

func (s *Server) loadAgentProfile(ctx context.Context, name string) (string, *sshconfig.Entry, error) {
	bindings, _ := agentBindingsFromContext(ctx)
	if expected, ok := bindings.profiles[name]; ok {
		return s.sshConfigs.LoadBound(name, expected)
	}
	if actor, ok := auth.ActorFromContext(ctx); ok && actor.Role == auth.RoleAgent {
		return "", nil, errAgentBindingMissing
	}
	canonical, err := profileReferenceName(name)
	if err != nil {
		return "", nil, err
	}
	ent, err := s.sshConfigs.Load(canonical)
	return canonical, ent, err
}

func (s *Server) readAgentProfileRaw(ctx context.Context, name string) (string, []byte, error) {
	bindings, _ := agentBindingsFromContext(ctx)
	if expected, ok := bindings.profiles[name]; ok {
		canonical, _, err := s.sshConfigs.LoadBound(name, expected)
		if err != nil {
			return "", nil, err
		}
		raw, err := s.sshConfigs.ReadRaw(canonical)
		if err != nil {
			return "", nil, err
		}
		current, err := s.sshConfigs.Binding(canonical)
		if err != nil {
			return "", nil, err
		}
		if current.Identity != expected.Identity || current.Version != expected.Version {
			return "", nil, sshconfig.ErrAuthorizationBindingChanged
		}
		return canonical, raw, nil
	}
	if actor, ok := auth.ActorFromContext(ctx); ok && actor.Role == auth.RoleAgent {
		return "", nil, errAgentBindingMissing
	}
	canonical, err := profileReferenceName(name)
	if err != nil {
		return "", nil, err
	}
	raw, err := s.sshConfigs.ReadRaw(canonical)
	return canonical, raw, err
}

func (s *Server) loadAgentExtension(ctx context.Context, name, target string) (*extensions.Extension, error) {
	bindings, _ := agentBindingsFromContext(ctx)
	if expected, ok := bindings.extensions[extensionBindingKey(name, target)]; ok {
		return s.extensions.LoadBound(expected)
	}
	if actor, ok := auth.ActorFromContext(ctx); ok && actor.Role == auth.RoleAgent {
		return nil, errAgentBindingMissing
	}
	return s.extensions.Store().Load(strings.TrimSpace(name))
}

func (s *Server) acquireAgentExtension(ctx context.Context, name, target string, wait time.Duration) (extensions.Status, error) {
	bindings, _ := agentBindingsFromContext(ctx)
	if expected, ok := bindings.extensions[extensionBindingKey(name, target)]; ok {
		return s.extensions.AcquireBound(expected, wait)
	}
	if actor, ok := auth.ActorFromContext(ctx); ok && actor.Role == auth.RoleAgent {
		return extensions.Status{}, errAgentBindingMissing
	}
	return s.extensions.AcquireTarget(name, target, wait)
}

func (s *Server) retryAgentExtension(ctx context.Context, name, target string) error {
	bindings, _ := agentBindingsFromContext(ctx)
	if expected, ok := bindings.extensions[extensionBindingKey(name, target)]; ok {
		return s.extensions.RetryBound(expected)
	}
	if actor, ok := auth.ActorFromContext(ctx); ok && actor.Role == auth.RoleAgent {
		return errAgentBindingMissing
	}
	return s.extensions.RetryTarget(name, target)
}

func (s *Server) validateAgentExtensionTarget(ctx context.Context, target string) error {
	if s.sshConfigs == nil {
		return errors.New("ssh config store not configured")
	}
	bindings, _ := agentBindingsFromContext(ctx)
	if expected, ok := bindings.profiles[target]; ok {
		_, _, err := s.sshConfigs.LoadBound(target, expected)
		return err
	}
	if actor, ok := auth.ActorFromContext(ctx); ok && actor.Role == auth.RoleAgent {
		return errAgentBindingMissing
	}
	return nil
}

func bindingError(name string, err error) error {
	return fmt.Errorf("authorized %s changed before execution: %w", name, err)
}
