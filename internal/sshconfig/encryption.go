package sshconfig

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/fsutil"
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
	MainKeyID                 string           `json:"main_key_id,omitempty"`
	RotationPending           bool             `json:"rotation_pending"`
	Locked                    bool             `json:"locked"`
	ErrorCode                 string           `json:"error_code,omitempty"`
	PasswordConfigured        bool             `json:"password_configured"`
	AutomaticUnlock           bool             `json:"automatic_unlock"`
	// SystemAvailable reports whether this OS offers a native credential store.
	// The setup form uses it to offer (or refuse) machine binding up front,
	// instead of letting the user choose a mode that then fails.
	SystemAvailable bool `json:"system_available"`
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
	st := s.stateSnapshot()
	repo := st.repo
	if enabled && repo == nil {
		var err error
		repo, err = configstore.OpenRepository(s.dataDir, providers...)
		if errors.Is(err, configstore.ErrNotInitialized) {
			repo, err = configstore.InitializeRepository(s.dataDir, providers...)
		}
		if err != nil {
			return err
		}
	}
	if repo != nil {
		if err := repo.SetEncryptNew(enabled); err != nil {
			if repo != st.repo {
				_ = repo.Close()
			}
			return err
		}
	}
	s.setEncryption(repo, enabled && repo != nil)
	s.notifyChange()
	return nil
}

// EncryptionStatus reports which hosts currently use plaintext or encrypted
// storage. Temporary profiles and the stateless built-in profile are included
// so the page can explain why they cannot be converted.
func (s *Store) EncryptionStatus() (EncryptionStatus, error) {
	// Reported on every response, including the locked one: the page decides
	// whether "bind to this machine" is even offered, and it must not have to
	// guess or fail after the user picks it.
	status := EncryptionStatus{Hosts: []EncryptionHost{}, SystemAvailable: configstore.SystemAvailable()}
	st := s.stateSnapshot()
	status.Initialized = st.repo != nil
	status.EncryptNew = st.encryptNew
	if st.repo != nil {
		repoStatus := st.repo.Status()
		if !repoStatus.MainKeyID.IsZero() {
			status.MainKeyID = hex.EncodeToString(repoStatus.MainKeyID[:])
		}
		status.RotationPending = repoStatus.RotationPending
		status.Locked = repoStatus.Locked
		status.ErrorCode = repoStatus.ErrorCode
		status.PasswordConfigured = repoStatus.PasswordConfigured
		status.AutomaticUnlock = repoStatus.AutomaticUnlock
	}
	if status.Locked {
		return status, nil
	}
	names, err := s.List()
	if err != nil {
		return EncryptionStatus{}, err
	}
	for _, name := range names {
		mode, err := s.StorageMode(name)
		if err != nil {
			return EncryptionStatus{}, err
		}
		status.Hosts = append(status.Hosts, EncryptionHost{Name: name, Mode: mode})
	}
	return status, nil
}

// InitializeEncryption creates the first keyring. An empty password asks for a
// machine-bound keyring instead: the Main Key is wrapped only by the providers,
// so the operating system's credential store can open it on every later start
// with no human in the loop, and a copy of the data directory cannot be opened
// on another machine at all. A non-empty password keeps the old behaviour and
// adds a password wrapper beside the provider's.
func (s *Store) InitializeEncryption(password, confirmation string, providers ...configstore.Provider) error {
	if s.repositorySnapshot() != nil {
		return configstore.ErrAlreadyInitialized
	}
	var repo *configstore.Repository
	var err error
	if password == "" && confirmation == "" {
		repo, err = configstore.InitializeRepository(s.dataDir, providers...)
	} else {
		repo, err = configstore.InitializeRepositoryWithPassword(s.dataDir, password, confirmation, providers...)
	}
	if err == nil {
		err = repo.SetEncryptNew(true)
		if err != nil {
			_ = repo.Close()
		}
	}
	if err == nil {
		s.setEncryption(repo, true)
		s.notifyChange()
	}
	return err
}

// UnlockEncryption opens a locked store. Unlocking is the only way to make
// encrypted profiles readable, and it requires the credential — that is what
// makes 上锁 worth doing.
//
// bindMachine is the caller's choice, never a default: adding a machine wrapper
// changes key material, and key material must only change because someone asked.
// (An older build did this implicitly to migrate password-only keyrings; the
// migration is now a checkbox in the unlock form.)
func (s *Store) UnlockEncryption(password string, bindMachine bool, providers ...configstore.Provider) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	var err error
	if password == "" {
		err = repo.UnlockProviders(providers...)
	} else {
		err = repo.UnlockPassword(password)
		if err == nil && bindMachine {
			err = repo.RebindPasswordProvider(providers...)
		}
	}
	if !repo.Status().Locked {
		s.setEncryptPreference(repo.EncryptNew())
	}
	s.notifyChange()
	return err
}

func (s *Store) ChangeEncryptionPassword(current, password, confirmation string, providers ...configstore.Provider) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	err := repo.ChangePassword(current, password, confirmation, providers...)
	s.notifyChange()
	return err
}

// RotateMainKey preserves the compatibility rotation used by old
// provider-only stores. Password-derived stores must use ChangeEncryptionPassword.
func (s *Store) RotateMainKey(providers ...configstore.Provider) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	err := repo.RotateMainKey(providers...)
	// A failed publication can still leave a persisted, resumable rotation.
	s.notifyChange()
	return err
}

// StorageMode returns where one profile is currently persisted.
func (s *Store) StorageMode(name string) (string, error) {
	name = strings.TrimSpace(name)
	if s.IsTemporary(name) {
		return StorageTemporary, nil
	}
	if _, encrypted, err := s.encryptedFold(name); err != nil {
		return "", err
	} else if encrypted {
		return StorageEncrypted, nil
	}
	if _, plaintext, err := s.plaintextFold(name); err != nil {
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
	repo := s.repositorySnapshot()
	if repo == nil {
		return nil, fmt.Errorf("configuration encryption has not been enabled")
	}
	if repo.Status().RotationPending {
		return nil, configstore.ErrRotationPending
	}

	// Serialise all selected names so concurrent saves cannot race with this batch.
	unlock := s.lockNames(names...)
	defer unlock()

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
		if s.IsTemporary(name) {
			results = append(results, EncryptionResult{Name: name, Code: "temporary_profile", Error: "temporary profiles are not persisted"})
			continue
		}
		if _, encrypted, err := s.encryptedFold(name); err != nil {
			return nil, err
		} else if encrypted {
			results = append(results, EncryptionResult{Name: name, OK: true, Code: "already_encrypted"})
			continue
		}
		plaintextName, plaintext, err := s.plaintextFold(name)
		if err != nil {
			return nil, err
		}
		if !plaintext {
			results = append(results, EncryptionResult{Name: name, Code: "connection_not_found", Error: "host has no persisted plaintext profile"})
			continue
		}
		data, err := os.ReadFile(s.plaintextPath(plaintextName))
		if err != nil {
			return nil, err
		}
		if IsInternalName(plaintextName) {
			_, err = ParseInternalOverride(data, InternalEntry())
		} else {
			_, err = ParseAndValidate(data)
		}
		if err != nil {
			clear(data)
			return nil, fmt.Errorf("profile %q: %w", plaintextName, err)
		}
		items = append(items, configstore.Item{Name: plaintextName, Data: data})
		sources = append(sources, plaintextName)
	}
	if len(items) == 0 {
		return results, nil
	}
	if err := s.importCiphertexts(repo, items); err != nil {
		for _, item := range items {
			clear(item.Data)
		}
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
	repo := s.repositorySnapshot()
	if repo != nil && repo.Status().RotationPending {
		return nil, configstore.ErrRotationPending
	}

	unlock := s.lockNames(names...)
	defer unlock()

	results := make([]EncryptionResult, 0, len(names))
	seen := make(map[string]bool, len(names))
	restored := 0
	for _, name := range names {
		folded := strings.ToLower(name)
		if seen[folded] {
			continue
		}
		seen[folded] = true
		if repo == nil {
			results = append(results, EncryptionResult{Name: name, Code: "not_encrypted", Error: "host is not stored in ciphertext"})
			continue
		}
		encryptedName, encrypted, err := s.encryptedFold(name)
		if err != nil {
			return nil, err
		}
		if !encrypted {
			results = append(results, EncryptionResult{Name: name, Code: "not_encrypted", Error: "host is not stored in ciphertext"})
			continue
		}
		data, err := repo.Read(encryptedName)
		if err != nil {
			return nil, err
		}
		path := s.plaintextPath(encryptedName)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			clear(data)
			return nil, err
		}
		if err := fsutil.WriteFileAtomic(path, data); err != nil {
			clear(data)
			return nil, err
		}
		clear(data)
		if err := repo.Delete(encryptedName); err != nil {
			results = append(results, EncryptionResult{Name: name, Code: "ciphertext_cleanup_failed", Error: err.Error()})
			continue
		}
		restored++
		results = append(results, EncryptionResult{Name: name, OK: true})
	}
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

// DestroyEncryption 删除密钥: it deletes the keyring and the catalog index and
// nothing else. Encrypted profiles stay on disk, unreadable forever — that is
// the stated consequence, not an accident, and it is what a user asking to
// "delete the key" actually asked for. Removing host data is a separate,
// per-host action; folding it in here would destroy more than the request.
//
// It requires an unlocked store, so the caller has already presented the
// credential. Deleting a key you cannot use would be a way to destroy data
// without ever proving you were entitled to it.
func (s *Store) DestroyEncryption() error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return fmt.Errorf("configuration encryption has not been enabled")
	}
	if repo.Status().Locked {
		return configstore.ErrPasswordRequired
	}
	if repo.Status().RotationPending {
		return fmt.Errorf("a password change is unfinished; finish it before deleting the key")
	}
	if err := configstore.DiscardDataDirKeys(s.dataDir); err != nil {
		return err
	}
	if err := repo.Close(); err != nil {
		return err
	}
	s.setEncryption(nil, false)
	s.notifyChange()
	return nil
}

// LockEncryption 上锁: the unwrapped key leaves memory, the keyring file stays.
// Encrypted hosts stop being readable; plaintext ones are untouched. Locking
// asks for no credential — it is the safe direction, and being able to do it
// deliberately (而不是只能靠重启) is the reason the lock exists.
func (s *Store) LockEncryption() error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	err := repo.Lock()
	s.notifyChange()
	return err
}
