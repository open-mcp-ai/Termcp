package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"golang.org/x/crypto/argon2"
)

type loginCredential struct {
	Version                int    `json:"version"`
	InstanceID             string `json:"instance_id"`
	Actor                  Actor  `json:"actor"`
	PasswordHash           string `json:"password_hash"`
	Revision               uint64 `json:"revision"`
	InitialPasswordsShared bool   `json:"initial_passwords_shared,omitempty"`
}

func passwordHash(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 1, 32)
	defer clear(hash)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(hash)), nil
}

func parsePasswordHash(encoded string) (salt, hash []byte, memory, rounds uint32, threads uint8, err error) {
	bad := func() { err = errors.New("invalid login verifier") }
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" {
		bad()
		return
	}
	params := strings.Split(parts[3], ",")
	if len(params) != 3 {
		bad()
		return
	}
	values := make([]uint64, 3)
	for i, prefix := range []string{"m=", "t=", "p="} {
		if !strings.HasPrefix(params[i], prefix) {
			bad()
			return
		}
		n, e := strconv.ParseUint(strings.TrimPrefix(params[i], prefix), 10, 32)
		if e != nil {
			bad()
			return
		}
		values[i] = n
	}
	if values[0] < 8192 || values[0] > 256*1024 || values[1] < 1 || values[1] > 10 || values[2] < 1 || values[2] > 8 {
		bad()
		return
	}
	salt, err = base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		bad()
		return
	}
	hash, err = base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(hash) != 32 {
		bad()
		return
	}
	memory, rounds, threads = uint32(values[0]), uint32(values[1]), uint8(values[2])
	return
}

func verifyPasswordHash(encoded, password string) bool {
	salt, want, m, t, p, err := parsePasswordHash(encoded)
	if err != nil || len(password) > 1024 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, 32)
	defer clear(got)
	return subtle.ConstantTimeCompare(got, want) == 1
}

func decodeLoginCredential(raw []byte) (*loginCredential, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var c loginCredential
	if err := decodeJSON(bytes.NewReader(raw), &c); err != nil {
		return nil, err
	}
	if c.Version != 1 || c.Revision == 0 || c.InstanceID == "" || c.Actor.ID == "" || c.Actor.Role != RoleAdmin || !validLabel(c.Actor.Username) {
		return nil, errors.New("invalid login bootstrap")
	}
	if _, _, _, _, _, err := parsePasswordHash(c.PasswordHash); err != nil {
		return nil, err
	}
	return &c, nil
}

func (m *Managed) readLoginCredential() (*loginCredential, []byte, error) {
	raw, err := m.vault.ReadLoginCredential()
	if err != nil {
		return nil, nil, err
	}
	c, err := decodeLoginCredential(raw)
	return c, raw, err
}

func (m *Managed) newLoginCredential(state managedState, password string, shared bool) (*loginCredential, error) {
	hash, err := passwordHash(password)
	if err != nil {
		return nil, err
	}
	return &loginCredential{Version: 1, InstanceID: state.InstanceID, Actor: Actor{ID: state.Admin.ID, Username: state.Admin.Username, Role: RoleAdmin}, PasswordHash: hash, Revision: 1, InitialPasswordsShared: shared}, nil
}

func (m *Managed) saveWithLogin(state managedState, c *loginCredential) error {
	if err := validateState(state, false); err != nil {
		return err
	}
	persisted := state
	persisted.Sessions = map[string]sessionRecord{}
	raw, err := json.Marshal(persisted)
	if err != nil {
		return err
	}
	defer clear(raw)
	credential, err := json.Marshal(c)
	if err != nil {
		return err
	}
	err = m.vault.WriteAuthenticationAndLogin(raw, credential)
	if errors.Is(err, configstore.ErrAlreadyInitialized) {
		return errInitialized
	}
	if err != nil && !errors.Is(err, configstore.ErrDurability) {
		return err
	}
	m.state = state
	m.credential = c
	m.sealed = true
	m.legacy = nil
	if err != nil {
		return err
	}
	return m.writeMarker()
}

// UnlockVault proves the independent configuration password. It neither changes
// the login verifier nor invalidates current browser identities.
func (m *Managed) UnlockVault(password string, automatic bool) error {
	m.passwordMu.Lock()
	defer m.passwordMu.Unlock()
	return m.unlockVault(password, automatic)
}
func (m *Managed) unlockVault(password string, automatic bool) error {
	if err := m.vault.UnlockAuthentication(password, m.providers...); err != nil {
		return err
	}
	if automatic {
		if err := m.vault.PrepareAuthentication(password, "", true, m.providers...); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loadLocked()
}

// ChangePassword changes only the account password. Config password derivation,
// MK, EDKs, provider slots and encrypted bodies are deliberately untouched.
func (m *Managed) ChangePassword(current, password, confirmation string) error {
	m.passwordMu.Lock()
	defer m.passwordMu.Unlock()
	if err := configstore.ValidatePassword(password, confirmation); err != nil {
		return err
	}
	c, raw, err := m.readLoginCredential()
	if err != nil {
		return err
	}
	if c == nil || !m.verifyLoginPassword(c.PasswordHash, current) {
		return configstore.ErrAuthentication
	}
	hash, err := passwordHash(password)
	if err != nil {
		return err
	}
	next := *c
	next.PasswordHash = hash
	next.Revision++
	next.InitialPasswordsShared = false
	encoded, err := json.Marshal(next)
	if err != nil {
		return err
	}
	err = m.vault.WriteLoginCredential(raw, encoded)
	if err != nil && !errors.Is(err, configstore.ErrDurability) {
		return err
	}
	m.mu.Lock()
	m.credential = &next
	m.state.Sessions = map[string]sessionRecord{}
	var cancels []context.CancelFunc
	for id, requests := range m.requests {
		if !strings.HasPrefix(id, "token:") {
			for _, cancel := range requests {
				cancels = append(cancels, cancel)
			}
		}
	}
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	return err
}

func (m *Managed) verifyLoginPassword(hash, password string) bool {
	if m.verifyLoginHash != nil {
		return m.verifyLoginHash(hash, password)
	}
	return verifyPasswordHash(hash, password)
}
