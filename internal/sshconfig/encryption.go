package sshconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

const (
	StorageBuiltin   = "builtin"
	StoragePlaintext = "plaintext"
	StorageEncrypted = "encrypted"
	StorageTemporary = "temporary"
)

// EncryptionHost is one host's persistent storage mode.
type EncryptionHost struct {
	Name string `json:"name"`
	Mode string `json:"mode"`
}

// EncryptionStatus is the page's non-secret configuration-encryption state.
type EncryptionStatus struct {
	Initialized               bool             `json:"initialized"`
	EncryptNew                bool             `json:"encrypt_new"`
	LegacyEncryptionArtifacts bool             `json:"legacy_encryption_artifacts"`
	Hosts                     []EncryptionHost `json:"hosts"`
}

// EncryptionResult is one selected host's outcome.
type EncryptionResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// SetNewProfileEncryption enables or disables encryption for newly created
// persistent profiles. Enabling creates a keyring on first use.
func (s *Store) SetNewProfileEncryption(enabled bool, providers ...configstore.Provider) error {
	s.mu.Lock()
	repo := s.encrypted
	if enabled && repo == nil {
		var err error
		repo, err = configstore.OpenRepository(s.dataDir, providers...)
		if errors.Is(err, configstore.ErrNotInitialized) {
			repo, err = configstore.InitializeRepository(s.dataDir, providers...)
		}
		if err != nil {
			s.mu.Unlock()
			return err
		}
	}
	if err := configstore.SaveEncryptNew(s.dataDir, enabled); err != nil {
		if repo != nil && repo != s.encrypted {
			_ = repo.Close()
		}
		s.mu.Unlock()
		return err
	}
	s.encrypted = repo
	s.encryptNew = enabled && repo != nil
	s.mu.Unlock()
	s.notifyChange()
	return nil
}

// EncryptionStatus reports which hosts currently use plaintext or encrypted
// storage. Temporary profiles and the stateless built-in profile are included
// so the page can explain why they cannot be converted.
func (s *Store) EncryptionStatus() (EncryptionStatus, error) {
	names, err := s.List()
	if err != nil {
		return EncryptionStatus{}, err
	}
	status := EncryptionStatus{}
	s.mu.Lock()
	status.Initialized = s.encrypted != nil
	status.EncryptNew = s.encryptNew
	status.LegacyEncryptionArtifacts = s.legacyArtifacts
	s.mu.Unlock()
	for _, name := range names {
		mode, err := s.StorageMode(name)
		if err != nil {
			return EncryptionStatus{}, err
		}
		status.Hosts = append(status.Hosts, EncryptionHost{Name: name, Mode: mode})
	}
	return status, nil
}

// StorageMode returns where one profile is currently persisted.
func (s *Store) StorageMode(name string) (string, error) {
	name = strings.TrimSpace(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	for existing := range s.temporary {
		if strings.EqualFold(existing, name) {
			return StorageTemporary, nil
		}
	}
	if _, encrypted, err := s.encryptedFoldLocked(name); err != nil {
		return "", err
	} else if encrypted {
		return StorageEncrypted, nil
	}
	if _, plaintext, err := s.plaintextFoldLocked(name); err != nil {
		return "", err
	} else if plaintext {
		return StoragePlaintext, nil
	}
	if IsInternalName(name) {
		return StorageBuiltin, nil
	}
	return "", fmt.Errorf("ssh config %q %w", name, ErrNotFound)
}

// EncryptProfiles moves selected persistent plaintext profiles into the
// encrypted repository. The catalog commits the whole selection before any
// selected plaintext source is removed.
func (s *Store) EncryptProfiles(names []string) ([]EncryptionResult, error) {
	names = cleanProfileSelection(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: select at least one host", ErrInvalid)
	}
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	if s.encrypted == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("configuration encryption has not been enabled")
	}
	results := make([]EncryptionResult, 0, len(names))
	items := make([]configstore.Item, 0, len(names))
	sources := make([]string, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		folded := strings.ToLower(name)
		if seen[folded] {
			continue
		}
		seen[folded] = true
		temporary := false
		for existing := range s.temporary {
			if strings.EqualFold(existing, name) {
				temporary = true
				break
			}
		}
		if temporary {
			results = append(results, EncryptionResult{Name: name, Code: "temporary_profile", Error: "temporary profiles are not persisted"})
			continue
		}
		if _, encrypted, err := s.encryptedFoldLocked(name); err != nil {
			s.mu.Unlock()
			return nil, err
		} else if encrypted {
			results = append(results, EncryptionResult{Name: name, OK: true, Code: "already_encrypted"})
			continue
		}
		plaintextName, plaintext, err := s.plaintextFoldLocked(name)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if !plaintext {
			results = append(results, EncryptionResult{Name: name, Code: "connection_not_found", Error: "host has no persisted plaintext profile"})
			continue
		}
		data, err := os.ReadFile(s.plaintextPath(plaintextName))
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if IsInternalName(plaintextName) {
			_, err = ParseInternalOverride(data, InternalEntry())
		} else {
			_, err = ParseAndValidate(data)
		}
		if err != nil {
			clear(data)
			s.mu.Unlock()
			return nil, fmt.Errorf("profile %q: %w", plaintextName, err)
		}
		items = append(items, configstore.Item{Name: plaintextName, Data: data})
		sources = append(sources, plaintextName)
	}
	if len(items) == 0 {
		s.mu.Unlock()
		return results, nil
	}
	if err := s.encrypted.Import(items); err != nil {
		for _, item := range items {
			clear(item.Data)
		}
		s.mu.Unlock()
		return nil, err
	}
	for _, name := range sources {
		if err := removePlaintextProfile(s.plaintextPath(name)); err != nil {
			results = append(results, EncryptionResult{Name: name, Code: "plaintext_cleanup_failed", Error: err.Error()})
			continue
		}
		results = append(results, EncryptionResult{Name: name, OK: true})
	}
	for _, item := range items {
		clear(item.Data)
	}
	s.mu.Unlock()
	s.notifyChange()
	return results, nil
}

// RestoreProfiles writes selected encrypted profiles back to plaintext before
// removing their catalog references.
func (s *Store) RestoreProfiles(names []string) ([]EncryptionResult, error) {
	names = cleanProfileSelection(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: select at least one host", ErrInvalid)
	}
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	results := make([]EncryptionResult, 0, len(names))
	seen := make(map[string]bool, len(names))
	restored := 0
	for _, name := range names {
		folded := strings.ToLower(name)
		if seen[folded] {
			continue
		}
		seen[folded] = true
		if s.encrypted == nil {
			results = append(results, EncryptionResult{Name: name, Code: "not_encrypted", Error: "host is not stored in ciphertext"})
			continue
		}
		encryptedName, encrypted, err := s.encryptedFoldLocked(name)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		if !encrypted {
			results = append(results, EncryptionResult{Name: name, Code: "not_encrypted", Error: "host is not stored in ciphertext"})
			continue
		}
		data, err := s.encrypted.Read(encryptedName)
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
		path := s.plaintextPath(encryptedName)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			clear(data)
			s.mu.Unlock()
			return nil, err
		}
		if err := writeFileAtomic(path, data); err != nil {
			clear(data)
			s.mu.Unlock()
			return nil, err
		}
		clear(data)
		if err := s.encrypted.Delete(encryptedName); err != nil {
			results = append(results, EncryptionResult{Name: name, Code: "ciphertext_cleanup_failed", Error: err.Error()})
			continue
		}
		restored++
		results = append(results, EncryptionResult{Name: name, OK: true})
	}
	s.mu.Unlock()
	if restored > 0 {
		s.notifyChange()
	}
	return results, nil
}

func cleanProfileSelection(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}
