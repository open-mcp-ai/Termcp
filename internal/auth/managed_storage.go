package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

// Vault is the existing configuration repository, not a second keyring.
type Vault interface {
	PrepareAuthentication(password, previous string, automatic bool, providers ...configstore.Provider) error
	UnlockAuthentication(password string, providers ...configstore.Provider) error
	ReadAuthentication() ([]byte, error)
	WriteAuthentication([]byte) error
	AuthenticationLocked() bool
	AuthenticationInitialized() bool
	ChangeEncryptionPassword(current, password, confirmation string, providers ...configstore.Provider) error
}

var errInitialized = errors.New("admin is already configured")

func (m *Managed) Configured() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sealed || m.legacy != nil
}

func (m *Managed) NeedsUnlock() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.legacy != nil || m.sealed && m.state.Version == 0
}

func OpenManaged(dataDir string, vault Vault, providers ...configstore.Provider) (*Managed, error) {
	if strings.TrimSpace(dataDir) == "" || vault == nil {
		return nil, errors.New("authentication directory and shared vault are required")
	}
	boot, err := randomSecret()
	if err != nil {
		return nil, err
	}
	m := &Managed{background: make(map[string]map[*int]context.CancelFunc), path: filepath.Join(dataDir, "auth.json"), now: time.Now, requests: make(map[string]map[*int]context.CancelFunc), vault: vault, providers: providers, webBootID: boot}
	m.sealed, err = configstore.HasAuthentication(dataDir)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(m.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	defer clear(raw)
	var marker struct {
		Version int `json:"version"`
	}
	if len(raw) > 1<<20 {
		return nil, errors.New("authentication metadata too large")
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &marker); err != nil {
			return nil, errors.New("invalid authentication metadata")
		}
	}
	if m.sealed {
		m.markerDirty = marker.Version != 2
		if !vault.AuthenticationInitialized() {
			return nil, errors.New("authentication ciphertext exists without its shared keyring")
		}
		if !vault.AuthenticationLocked() {
			if err := m.loadLocked(); err != nil {
				return nil, err
			}
		}
		return m, nil
	}
	if len(raw) == 0 {
		return m, nil
	}
	if marker.Version != 1 {
		return nil, errors.New("encrypted authentication state is missing")
	}
	var legacy managedState
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return nil, errors.New("invalid legacy authentication")
	}
	if err := validateState(legacy, true); err != nil {
		return nil, err
	}
	m.legacy = &legacy
	m.markerDirty = true
	return m, nil
}

func validateState(s managedState, legacy bool) error {
	key, err := base64.RawURLEncoding.DecodeString(s.SigningKey)
	defer clear(key)
	version := managedStateVersion
	if legacy {
		version = 1
	}
	if s.Version != version || s.InstanceID == "" || s.Admin.ID == "" || s.Admin.Username == "" || err != nil || len(key) != 32 || s.Sessions == nil || s.Revision == 0 {
		return errors.New("incomplete authentication state")
	}
	if legacy && s.Admin.Password == "" || !legacy && s.Admin.Password != "" {
		return errors.New("invalid password storage")
	}
	if err := validatePolicy(s.Permissions); err != nil {
		return err
	}
	for id, session := range s.Sessions {
		if id == "" || session.CSRF == "" || session.Actor.ID != s.Admin.ID || session.Actor.Username != s.Admin.Username || session.Actor.Role != RoleAdmin || session.ExpiresAt.IsZero() {
			return errors.New("invalid saved session")
		}
	}
	return validateCredentials(s)
}

func (m *Managed) writeMarker() error {
	f, err := os.CreateTemp(filepath.Dir(m.path), ".auth-metadata-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	if _, err = f.WriteString("{\"version\":2}\n"); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), m.path); err != nil {
		return err
	}
	m.markerDirty = false
	return nil
}

func (m *Managed) loadLocked() error {
	raw, err := m.vault.ReadAuthentication()
	if err != nil {
		return err
	}
	defer clear(raw)
	var state managedState
	if err := json.Unmarshal(raw, &state); err != nil {
		return errors.New("invalid encrypted authentication state")
	}
	// Sessions in historical ciphertext are intentionally ignored. Preserve only
	// logins already created by this Managed instance while reloading the vault.
	state.Sessions = make(map[string]sessionRecord)
	if state.Admin.ID == m.state.Admin.ID && equalSecret(state.SigningKey, m.state.SigningKey) {
		for id, session := range m.state.Sessions {
			if m.now().Before(session.ExpiresAt) {
				state.Sessions[id] = session
			}
		}
	}
	migrated, err := migratePermissionGroups(&state, m.now())
	if err != nil {
		return err
	}
	repaired := repairAmbiguousAgentPolicies(&state)
	if err := validateState(state, false); err != nil {
		return err
	}
	if repaired || migrated {
		return m.save(state, false)
	}
	m.state = state
	if m.markerDirty {
		if err := m.writeMarker(); err != nil {
			return err
		}
	}
	return nil
}

func (m *Managed) save(state managedState, initial bool) error {
	if err := validateState(state, false); err != nil {
		return err
	}
	// Keep the legacy field readable, but never persist an active Web session.
	persisted := state
	persisted.Sessions = make(map[string]sessionRecord)
	raw, err := json.Marshal(persisted)
	if err != nil {
		return err
	}
	defer clear(raw)
	err = m.vault.WriteAuthentication(raw)
	if err != nil && !errors.Is(err, configstore.ErrDurability) {
		return err
	}
	m.state = state
	m.sealed = true
	m.legacy = nil
	if err != nil {
		return err
	}
	if initial || m.markerDirty {
		return m.writeMarker()
	}
	return nil
}

func (m *Managed) setup(username, password string) error {
	return m.setupWithOptions(username, password, "", false)
}

func (m *Managed) setupWithOptions(username, password, previous string, automatic bool) error {
	m.passwordMu.Lock()
	defer m.passwordMu.Unlock()
	m.mu.Lock()
	configured := m.sealed || m.legacy != nil
	m.mu.Unlock()
	if configured {
		return errInitialized
	}
	if username != strings.TrimSpace(username) || username == "" || len(username) > 128 || strings.ContainsAny(username, "\r\n\x00") {
		return errors.New("a valid username is required")
	}
	if err := m.vault.PrepareAuthentication(password, previous, automatic, m.providers...); err != nil {
		return err
	}
	key, err := randomSecret()
	if err != nil {
		return err
	}
	id, err := randomSecret()
	if err != nil {
		return err
	}
	instance, err := randomSecret()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sealed || m.legacy != nil {
		return errInitialized
	}
	state := managedState{Version: managedStateVersion, InstanceID: instance, SigningKey: key, Admin: adminRecord{ID: id, Username: username}, Permissions: defaultPolicy(), Revision: 1, Sessions: make(map[string]sessionRecord)}
	seedPermissionGroups(&state, m.now().UTC())
	m.markerDirty = true
	return m.save(state, true)
}

// unlockForLogin holds passwordMu, but releases the ordinary policy mutex while
// the shared vault derives/verifies keys. Reload only after verification so a
// concurrent token/permission save cannot be overwritten by an old snapshot.
func (m *Managed) unlockForLogin(username, password, previous string, automatic bool) error {
	m.mu.Lock()
	legacy, sealed := m.legacy, m.sealed
	m.mu.Unlock()
	if legacy != nil && !sealed {
		old := credentialState(*legacy)
		if !equalSecret(username, old.Admin.Username) || !equalSecret(password, old.Admin.Password) {
			return configstore.ErrAuthentication
		}
		if err := m.vault.PrepareAuthentication(password, previous, automatic, m.providers...); err != nil {
			return err
		}
		key, err := randomSecret()
		if err != nil {
			return err
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.legacy != legacy || m.sealed {
			return errInitialized
		}
		old.Version = 2
		if _, err := migratePermissionGroups(&old, m.now().UTC()); err != nil {
			return err
		}
		old.Admin.Password = ""
		old.Sessions = make(map[string]sessionRecord)
		old.SigningKey = key
		return m.save(old, true)
	}
	if err := m.vault.UnlockAuthentication(password, m.providers...); err != nil {
		return err
	}
	m.mu.Lock()
	err := m.loadLocked()
	if err == nil && !equalSecret(username, m.state.Admin.Username) {
		err = configstore.ErrAuthentication
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	if automatic {
		return m.vault.PrepareAuthentication(password, "", true, m.providers...)
	}
	return nil
}

// ChangePassword revokes runtime browser sessions before the resumable key
// transition. Password work does not hold the ordinary authentication mutex.
func (m *Managed) ChangePassword(current, password, confirmation string) error {
	m.passwordMu.Lock()
	defer m.passwordMu.Unlock()
	m.mu.Lock()
	ready := m.sealed && m.state.Version == managedStateVersion
	m.mu.Unlock()
	if !ready {
		return configstore.ErrLocked
	}
	if err := configstore.ValidatePassword(password, confirmation); err != nil {
		return err
	}
	if err := m.vault.UnlockAuthentication(current, m.providers...); err != nil {
		return err
	}
	m.mu.Lock()
	state := m.state
	state.Sessions = make(map[string]sessionRecord)
	err := m.save(state, false)
	var cancels []context.CancelFunc
	if err == nil || errors.Is(err, configstore.ErrDurability) {
		for id, requests := range m.requests {
			if strings.HasPrefix(id, "token:") {
				continue
			}
			for _, cancel := range requests {
				cancels = append(cancels, cancel)
			}
		}
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	if err != nil {
		return err
	}
	if err := m.vault.ChangeEncryptionPassword(current, password, confirmation, m.providers...); err != nil {
		return fmt.Errorf("change shared master password: %w", err)
	}
	return nil
}
