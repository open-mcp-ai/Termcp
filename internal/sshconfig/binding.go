package sshconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

var ErrAuthorizationBindingChanged = errors.New("authorization binding changed")

// AuthorizationBinding is the immutable identity and content fact an Agent
// authorization decision was made against. It contains no credentials.
type AuthorizationBinding struct {
	Name     string `json:"name"`
	Identity string `json:"identity"`
	Version  string `json:"version"`
}

func rawVersion(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Binding captures the current canonical name, stable identity and config hash.
// A rename preserves the binding; deleting and recreating the name does not.
func (s *Store) Binding(name string) (AuthorizationBinding, error) {
	canonical, identity, err := s.ResolveReference(name, "")
	if err != nil {
		return AuthorizationBinding{}, err
	}
	raw, err := s.ReadRaw(canonical)
	if err != nil {
		return AuthorizationBinding{}, err
	}
	after, err := s.Identity(canonical)
	if err != nil {
		return AuthorizationBinding{}, err
	}
	if after != identity {
		return AuthorizationBinding{}, ErrAuthorizationBindingChanged
	}
	return AuthorizationBinding{Name: canonical, Identity: identity, Version: rawVersion(raw)}, nil
}

// LoadBound resolves the stable identity (not a reused display name), verifies
// content hasn't changed, and returns the exact entry used by execution.
func (s *Store) LoadBound(name string, expected AuthorizationBinding) (string, *Entry, error) {
	if expected.Identity == "" || expected.Version == "" {
		return "", nil, ErrAuthorizationBindingChanged
	}
	canonical, identity, err := s.ResolveReference(name, expected.Identity)
	if err != nil {
		return "", nil, err
	}
	if identity != expected.Identity {
		return "", nil, ErrAuthorizationBindingChanged
	}
	raw, err := s.ReadRaw(canonical)
	if err != nil {
		return "", nil, err
	}
	if rawVersion(raw) != expected.Version {
		return "", nil, fmt.Errorf("%w: profile content changed", ErrAuthorizationBindingChanged)
	}
	entry, err := s.Load(canonical)
	if err != nil {
		return "", nil, err
	}
	after, err := s.Identity(canonical)
	if err != nil {
		return "", nil, err
	}
	rawAfter, err := s.ReadRaw(canonical)
	if err != nil {
		return "", nil, err
	}
	if after != expected.Identity || rawVersion(rawAfter) != expected.Version {
		return "", nil, fmt.Errorf("%w: profile changed during load", ErrAuthorizationBindingChanged)
	}
	return canonical, entry, nil
}
