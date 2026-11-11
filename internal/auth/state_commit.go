package auth

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

// commitState serializes persistent state changes, performs the vault write
// without holding the ordinary state mutex, then publishes the new snapshot.
// The after callback runs only after publication and after m.mu is released, so
// it may cancel work or notify transport code without lock inversion.
func (m *Managed) commitState(ctx context.Context, apply func(*managedState) (func(), error)) error {
	if m == nil || apply == nil {
		return errors.New("authentication state is unavailable")
	}
	if m.commit == nil {
		return errors.New("authentication state commit coordinator is unavailable")
	}
	select {
	case m.commit <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-m.commit }()

	m.mu.Lock()
	if m.state.Version == 0 || m.vault.AuthenticationLocked() {
		m.mu.Unlock()
		return errVaultLocked
	}
	next := cloneManagedState(m.state)
	baseEpoch := m.stateEpoch
	m.mu.Unlock()

	normalizeOAuthState(&next)
	after, err := apply(&next)
	if err != nil {
		return err
	}
	if err := validateState(next, false); err != nil {
		return err
	}
	persisted := next
	persisted.Sessions = map[string]sessionRecord{}
	raw, err := json.Marshal(persisted)
	if err != nil {
		return err
	}
	defer clear(raw)

	err = m.vault.WriteAuthentication(raw)
	if err != nil && !errors.Is(err, configstore.ErrDurability) {
		return err
	}

	m.mu.Lock()
	if m.stateEpoch != baseEpoch {
		m.mu.Unlock()
		return errors.New("authentication state changed while it was being saved")
	}
	// Browser sessions are memory-only and may change independently while the
	// encrypted snapshot is being written. Never replace the live session set.
	next.Sessions = cloneSessionMap(m.state.Sessions)
	m.state = next
	m.stateEpoch++
	m.applyOAuthSettingsLocked()
	m.sealed = true
	m.legacy = nil
	needMarker := m.markerDirty
	m.mu.Unlock()

	if needMarker {
		markerErr := os.Remove(m.path)
		if errors.Is(markerErr, os.ErrNotExist) {
			markerErr = nil
		}
		if markerErr == nil {
			m.mu.Lock()
			m.markerDirty = false
			m.mu.Unlock()
		}
		if markerErr != nil && err == nil {
			err = markerErr
		}
	}
	if after != nil {
		after()
	}
	if err != nil {
		return err
	}
	return nil
}

var errVaultLocked = errors.New("configuration vault is locked")

func cloneSessionMap(in map[string]sessionRecord) map[string]sessionRecord {
	out := make(map[string]sessionRecord, len(in))
	for id, session := range in {
		out[id] = session
	}
	return out
}
