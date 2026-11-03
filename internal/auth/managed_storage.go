package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	ReadLoginCredential() ([]byte, error)
	WriteLoginCredential(expected, next []byte) error
	WriteAuthenticationAndLogin(data, login []byte) error
}

var errInitialized = errors.New("admin is already configured")

func (m *Managed) Configured() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.credential != nil || m.sealed || m.legacy != nil
}

func (m *Managed) NeedsUnlock() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.legacy != nil || m.sealed && (m.state.Version == 0 || m.vault.AuthenticationLocked())
}

func OpenManaged(dataDir string, vault Vault, providers ...configstore.Provider) (*Managed, error) {
	if strings.TrimSpace(dataDir) == "" || vault == nil {
		return nil, errors.New("authentication directory and shared vault are required")
	}
	boot, err := randomSecret()
	if err != nil {
		return nil, err
	}
	signing, err := randomSecret()
	if err != nil {
		return nil, err
	}
	m := &Managed{webSigningKey: signing, background: make(map[string]map[*int]context.CancelFunc), path: filepath.Join(dataDir, "auth.json"), now: time.Now, requests: make(map[string]map[*int]context.CancelFunc), vault: vault, providers: providers, webBootID: boot, auditSink: SlogAuditSink{}}
	m.credential, _, err = m.readLoginCredential()
	if err != nil {
		return nil, err
	}
	if m.state.Sessions == nil {
		m.state.Sessions = map[string]sessionRecord{}
	}
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
	if m.credential != nil && !m.sealed {
		return nil, errors.New("login bootstrap exists without encrypted account")
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
	if legacy {
		if len(s.Agents) > 0 || len(s.Groups) > 0 {
			return errors.New("unsupported legacy permission state")
		}
		return nil
	}
	return validateCredentials(s)
}

func (m *Managed) writeMarker() error {
	if err := os.Remove(m.path); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	if m.credential != nil && (state.Admin.ID != m.credential.Actor.ID || state.Admin.Username != m.credential.Actor.Username || state.InstanceID != m.credential.InstanceID) {
		return errors.New("account bootstrap does not match encrypted account")
	}
	if state.Admin.ID == m.state.Admin.ID || m.credential != nil && state.Admin.ID == m.credential.Actor.ID {
		for id, session := range m.state.Sessions {
			if m.now().Before(session.ExpiresAt) {
				state.Sessions[id] = session
			}
		}
	}
	if state.Version != managedStateVersion {
		return errors.New("unsupported permission state version")
	}
	changed := repairAmbiguousAgentPolicies(&state)
	if upgradePresetGroups(&state) {
		changed = true
	}
	if err := validateState(state, false); err != nil {
		return err
	}
	if changed {
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
	if err := configstore.ValidatePassword(password, password); err != nil {
		return err
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
	credential, err := m.newLoginCredential(state, password, previous == "" || equalSecret(password, previous))
	if err != nil {
		return err
	}
	return m.saveWithLogin(state, credential)
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
		// Login/storage migration never imports retired business permissions.
		if len(old.Agents) > 0 {
			return errors.New("unsupported legacy permission state")
		}
		old.Version = managedStateVersion
		seedPermissionGroups(&old, m.now().UTC())
		old.Admin.Password = ""
		old.Sessions = make(map[string]sessionRecord)
		old.SigningKey = key
		credential, err := m.newLoginCredential(old, password, false)
		if err != nil {
			return err
		}
		return m.saveWithLogin(old, credential)
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
		if err := m.vault.PrepareAuthentication(password, "", true, m.providers...); err != nil {
			return err
		}
	}
	m.mu.Lock()
	snapshot := m.state
	m.mu.Unlock()
	credential, err := m.newLoginCredential(snapshot, password, false)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveWithLogin(m.state, credential)
}
