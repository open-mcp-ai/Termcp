package sshconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

// ErrNotFound reports that a named ssh config does not exist. Callers can
// branch on it with errors.Is instead of matching the message.
var ErrNotFound = errors.New("not found")

// ErrInvalid marks a profile whose CONTENT is unusable — a missing host, no
// credential, a port outside the range.
//
// It exists so the surface can answer 400 instead of 500. Those checks already
// refuse the write, but they returned bare fmt.Errorf values, so nothing upstream
// could tell "the caller wrote an incomplete profile" from "the write failed" and
// every one of them surfaced as operation_failed.
var ErrInvalid = errors.New("invalid ssh config")

// ErrReserved reports that a name denotes the built-in internal profile, which
// is virtual and cannot be deleted. It is a sentinel so a batch caller can turn
// it into its own per-name code without re-testing the name or restating the
// message that Delete already carries.
var ErrReserved = errors.New("reserved ssh config")

// Store manages dataDir/ssh_configs/<name>/config.toml for remote profiles.
// The built-in "internal" profile is virtual: it is never written to disk.
type Store struct {
	dataDir             string
	encrypted           *configstore.Repository
	encryptNew          bool
	legacyArtifacts     bool
	mu                  sync.Mutex
	temporary           map[string][]byte // process-local profiles; guarded by mu
	temporaryIdentities map[string]string
	identityByName      map[string]string
	nameByIdentity      map[string]string
	identityStamp       time.Time
	identitiesLoaded    bool
	onChange            atomic.Value // func(), set via SetOnChange, called after Save/Delete/Rename
}

// SetOnChange registers a callback fired after Save, Delete, or Rename succeeds.
func (s *Store) SetOnChange(fn func()) {
	s.onChange.Store(fn)
}

// Close releases the encrypted repository's directory lock and in-memory keys.
// Plaintext stores have no persistent resources to release.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	repo := s.repositorySnapshot()
	if repo == nil {
		return nil
	}
	return repo.Close()
}

func (s *Store) notifyChange() {
	fn, _ := s.onChange.Load().(func())
	if fn != nil {
		fn()
	}
}

// NewStore returns a Store rooted at dataDir (always absolute path when Abs succeeds).
func NewStore(dataDir string) *Store {
	d := filepath.Clean(strings.TrimSpace(dataDir))
	if d == "" {
		d = "."
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		abs = d
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &Store{dataDir: abs}
}

// NewEncryptedStore returns a Store whose persistent profiles live in repo.
// Temporary profiles remain process-local and never enter the encrypted store.
func NewEncryptedStore(dataDir string, repo *configstore.Repository) *Store {
	return NewHybridStore(dataDir, repo, true)
}

// NewHybridStore returns a Store that keeps plaintext profiles in their
// existing paths and encrypted profiles in repo. encryptNew controls only the
// mode for newly created persistent profiles.
func NewHybridStore(dataDir string, repo *configstore.Repository, encryptNew bool) *Store {
	store := NewStore(dataDir)
	store.encrypted = repo
	store.encryptNew = repo != nil && encryptNew
	return store
}

// OpenConfiguredStore opens an encrypted repository when one has been
// initialized and otherwise preserves the legacy plaintext layout.
func OpenConfiguredStore(dataDir string, providers ...configstore.Provider) (*Store, error) {
	repo, err := configstore.OpenRepositoryForRecovery(dataDir, providers...)
	if errors.Is(err, configstore.ErrNotInitialized) {
		hasCatalog, catalogErr := configstore.HasManagedCatalog(dataDir)
		if catalogErr != nil {
			return nil, catalogErr
		}
		if hasCatalog {
			return nil, fmt.Errorf("%w: encrypted catalog exists but config.keys is missing", configstore.ErrNotInitialized)
		}
		legacy, err := configstore.HasLegacyEncryptionArtifacts(dataDir)
		if err != nil {
			return nil, err
		}
		store := NewStore(dataDir)
		store.legacyArtifacts = legacy
		if err := store.RefreshIdentities(); err != nil {
			return nil, err
		}
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	dataDir = repo.DataDir()
	encryptNew := repo.EncryptNew()
	store := NewHybridStore(dataDir, repo, encryptNew)
	legacy, err := configstore.HasLegacyEncryptionArtifacts(dataDir)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	store.legacyArtifacts = legacy
	if err := store.RefreshIdentities(); err != nil {
		_ = store.Close()
		return nil, err
	}
	if repo.Status().Locked {
		return store, nil
	}
	names, err := store.List()
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	for _, name := range names {
		if _, err := store.Load(name); err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("verify profile %q: %w", name, err)
		}
	}
	return store, nil
}

// StartupWarning describes non-fatal encrypted data from an incompatible
// development layout. The current store never reads or modifies those files.
func (s *Store) StartupWarning() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.encrypted != nil && s.encrypted.Status().Locked {
		return "configuration key provider could not unlock the Main Key; open the Web UI and enter the encryption password"
	}
	if !s.legacyArtifacts {
		return ""
	}
	return "encryption-test data was detected under this data directory and is being ignored; current plaintext and managed ciphertext profiles are still available"
}

func (s *Store) root() string {
	return filepath.Join(s.dataDir, "ssh_configs")
}

// ConfigDir returns the directory containing config.toml for a named profile.
// For the virtual internal profile this path is not used for I/O.
func (s *Store) ConfigDir(name string) string {
	return filepath.Join(s.root(), name)
}

func (s *Store) configPath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.root(), name, "config.toml"), nil
}

// Load reads and validates a named config.
// name "internal" is the built-in entry, optionally overridden by a stored
// internal/config.toml (see ParseInternalOverride).
func (s *Store) Load(name string) (*Entry, error) {
	name = strings.TrimSpace(name)
	if s.IsTemporary(name) {
		data, err := s.temporarySnapshot(name)
		if err != nil {
			return nil, err
		}
		if IsInternalName(name) {
			return ParseInternalOverride(data, InternalEntry())
		}
		return ParseAndValidate(data)
	}
	if repo := s.repositorySnapshot(); repo != nil {
		data, err := repo.Read(name)
		if err != nil && !errors.Is(err, configstore.ErrNotFound) {
			return nil, err
		}
		if err == nil {
			if IsInternalName(name) {
				return ParseInternalOverride(data, InternalEntry())
			}
			return ParseAndValidate(data)
		}
	}
	return s.loadPlaintext(name)
}

func (s *Store) loadPlaintext(name string) (*Entry, error) {
	if IsInternalName(name) {
		base := InternalEntry()
		data, err := os.ReadFile(s.internalOverridePath())
		if err != nil {
			if os.IsNotExist(err) {
				return base, nil // no override: the defaults ARE the profile
			}
			return nil, err
		}
		ent, err := ParseInternalOverride(data, base)
		if err != nil {
			// A broken override is reported, not silently ignored: falling back
			// to the defaults would run the session UNGATED after the user asked
			// for a gate, which is the one direction this must never fail.
			return nil, fmt.Errorf("internal profile override: %w", err)
		}
		return ent, nil
	}
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ssh config %q %w (use data-dir/ssh_configs/%s/config.toml)", name, ErrNotFound, name)
		}
		return nil, err
	}
	return ParseAndValidate(data)
}

// ReadRaw returns the raw config.toml bytes for a name.
//
// For the internal profile it returns the stored override when one exists and
// the built-in template otherwise, so the editor round-trips what is actually
// in effect rather than a constant it cannot change.
func (s *Store) ReadRaw(name string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if s.IsTemporary(name) {
		return s.temporarySnapshot(name)
	}
	if repo := s.repositorySnapshot(); repo != nil {
		data, err := repo.Read(name)
		if err != nil && !errors.Is(err, configstore.ErrNotFound) {
			return nil, err
		}
		if err == nil {
			if !IsInternalName(name) {
				if _, err := ParseAndValidate(data); err != nil {
					return nil, err
				}
			}
			return data, nil
		}
	}
	return s.readPlaintextRaw(name)
}

func (s *Store) readPlaintextRaw(name string) ([]byte, error) {
	if IsInternalName(name) {
		data, err := os.ReadFile(s.internalOverridePath())
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		return InternalTemplate(), nil
	}
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ssh config %q %w", name, ErrNotFound)
		}
		return nil, err
	}
	if _, err := ParseAndValidate(data); err != nil {
		return nil, err
	}
	return data, nil
}

// List returns sorted config names: virtual "internal", temporary profiles, and profiles on disk.
// Leftover ssh_configs/internal/ directories are ignored.
func (s *Store) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshIdentitiesLocked(false); err != nil {
		return nil, err
	}
	names := []string{"internal"}
	seen := map[string]bool{"internal": true}
	for name := range s.temporary {
		folded := strings.ToLower(name)
		if !seen[folded] {
			names = append(names, name)
			seen[folded] = true
		}
	}
	if s.encrypted != nil {
		stored, err := s.encrypted.List()
		if err != nil {
			return nil, err
		}
		for _, name := range stored {
			folded := strings.ToLower(name)
			if IsInternalName(name) || seen[folded] {
				continue
			}
			names = append(names, name)
			seen[folded] = true
		}
	}
	entries, err := os.ReadDir(s.root())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		folded := strings.ToLower(name)
		if IsInternalName(name) || seen[folded] {
			continue // disk leftover; virtual entry already listed
		}
		cfg := filepath.Join(s.root(), name, "config.toml")
		if st, err := os.Stat(cfg); err == nil && !st.IsDir() {
			names = append(names, name)
			seen[folded] = true
		}
	}
	sort.Strings(names)
	return names, nil
}

func (s *Store) repositorySnapshot() *configstore.Repository {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encrypted
}

// Save writes config for a name (validates first).
//
// The internal profile is saved as an override file that Load layers over the
// built-in defaults, so setting one field does not mean restating the rest.
func (s *Store) Save(name string, data []byte) error {
	return s.SaveWithOptions(name, data, false)
}

// SaveWithOptions stores a remote profile only in memory when temporary is true.
// Switching an existing profile between modes removes its previous copy.
func (s *Store) SaveWithOptions(name string, data []byte, temporary bool) error {
	return s.saveHybrid(name, data, temporary)
}

func (s *Store) saveHybrid(name string, data []byte, temporary bool) error {
	name = strings.TrimSpace(name)
	if IsInternalName(name) {
		if temporary {
			return fmt.Errorf("internal profile cannot be temporary")
		}
		if _, err := ParseInternalOverride(data, InternalEntry()); err != nil {
			return err
		}
	} else {
		if _, err := ParseAndValidate(data); err != nil {
			return err
		}
		if err := ValidateName(name); err != nil {
			return err
		}
	}

	s.mu.Lock()
	if err := s.refreshIdentitiesLocked(false); err != nil {
		s.mu.Unlock()
		return err
	}
	encryptedName, encryptedExists, err := s.encryptedFoldLocked(name)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	plaintextName, plaintextExists, err := s.plaintextFoldLocked(name)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if encryptedExists && encryptedName != name {
		s.mu.Unlock()
		return fmt.Errorf("config already exists: %s", encryptedName)
	}
	if plaintextExists && plaintextName != name {
		s.mu.Unlock()
		return fmt.Errorf("config already exists: %s", plaintextName)
	}
	for existing := range s.temporary {
		if strings.EqualFold(existing, name) && existing != name {
			s.mu.Unlock()
			return fmt.Errorf("config already exists: %s", existing)
		}
	}
	if temporary {
		id := s.identityByName[strings.ToLower(name)]
		if id == "" {
			id, err = newProfileIdentity()
			if err != nil {
				s.mu.Unlock()
				return err
			}
		}
		if encryptedExists {
			if s.encrypted == nil {
				s.mu.Unlock()
				return fmt.Errorf("configuration encryption is enabled but its keyring is unavailable")
			}
			if err := s.encrypted.Delete(name); err != nil {
				s.mu.Unlock()
				return err
			}
		}
		if plaintextExists {
			if err := removePlaintextProfile(s.plaintextPath(name)); err != nil {
				s.mu.Unlock()
				return err
			}
		}
		if encryptedExists || plaintextExists {
			if err := removeProfileIdentity(filepath.Join(s.root(), name)); err != nil {
				s.mu.Unlock()
				return err
			}
		}
		if s.temporary == nil {
			s.temporary = make(map[string][]byte)
		}
		if s.temporaryIdentities == nil {
			s.temporaryIdentities = make(map[string]string)
		}
		s.temporaryIdentities[name] = id
		s.temporary[name] = append([]byte(nil), data...)
		s.invalidateIdentitiesLocked()
		s.mu.Unlock()
		s.notifyChange()
		return nil
	}
	// Existing plaintext profiles keep that mode until the operator explicitly
	// selects them on the encryption page. The global switch only chooses how a
	// brand-new profile is first persisted.
	if !encryptedExists && (!s.encryptNew || plaintextExists) {
		path := s.plaintextPath(name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			s.mu.Unlock()
			return err
		}
		expected := s.temporaryIdentities[name]
		if IsInternalName(name) {
			expected = InternalIdentity
		}
		if _, err := ensureProfileIdentity(filepath.Dir(path), expected); err != nil {
			s.mu.Unlock()
			return err
		}
		if err := writeFileAtomic(path, data); err != nil {
			s.mu.Unlock()
			return err
		}
		delete(s.temporary, name)
		delete(s.temporaryIdentities, name)
		s.invalidateIdentitiesLocked()
		s.mu.Unlock()
		s.notifyChange()
		return nil
	}
	if s.encrypted == nil {
		s.mu.Unlock()
		return fmt.Errorf("configuration encryption is enabled but its keyring is unavailable")
	}
	expected := s.temporaryIdentities[name]
	if IsInternalName(name) {
		expected = InternalIdentity
	}
	if _, err := ensureProfileIdentity(filepath.Join(s.root(), name), expected); err != nil {
		s.mu.Unlock()
		return err
	}
	if err := s.encrypted.Save(name, data); err != nil {
		s.mu.Unlock()
		return err
	}
	if plaintextExists {
		if err := removePlaintextProfile(s.plaintextPath(name)); err != nil {
			s.mu.Unlock()
			return fmt.Errorf("profile was encrypted but its plaintext copy could not be removed: %w", err)
		}
	}
	delete(s.temporary, name)
	delete(s.temporaryIdentities, name)
	s.invalidateIdentitiesLocked()
	s.mu.Unlock()
	s.notifyChange()
	return nil
}

func (s *Store) encryptedFoldLocked(name string) (string, bool, error) {
	if s.encrypted == nil {
		return "", false, nil
	}
	names, err := s.encrypted.List()
	if err != nil {
		return "", false, err
	}
	for _, existing := range names {
		if strings.EqualFold(existing, name) {
			return existing, true, nil
		}
	}
	return "", false, nil
}

func (s *Store) plaintextFoldLocked(name string) (string, bool, error) {
	if IsInternalName(name) {
		exists, err := regularFileExists(s.internalOverridePath())
		if err != nil || !exists {
			return "", false, err
		}
		return "internal", true, nil
	}
	entries, err := os.ReadDir(s.root())
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") ||
			!strings.EqualFold(entry.Name(), name) {
			continue
		}
		exists, err := regularFileExists(filepath.Join(s.root(), entry.Name(), "config.toml"))
		if err != nil {
			return "", false, err
		}
		if exists {
			return entry.Name(), true, nil
		}
	}
	return "", false, nil
}

func (s *Store) plaintextPath(name string) string {
	if IsInternalName(name) {
		return s.internalOverridePath()
	}
	return filepath.Join(s.root(), name, "config.toml")
}

func (s *Store) temporarySnapshot(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.temporary[name]
	if !ok {
		return nil, fmt.Errorf("ssh config %q %w", name, ErrNotFound)
	}
	return append([]byte(nil), data...), nil
}

// IsTemporary reports whether the profile exists only in this process.
func (s *Store) IsTemporary(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.temporary[name]
	return ok
}

// internalOverridePath is where a stored internal-profile override lives. The
// name is dot-prefixed so the directory scan that builds the profile list skips
// it (that scan treats any dot-directory as private).
func (s *Store) internalOverridePath() string {
	return filepath.Join(s.root(), ".internal", "config.toml")
}

// writeFileAtomic writes data through a temp file and renames it into place, so
// a crash cannot leave a half-written config behind. The file is 0600: profiles
// hold credentials.
func writeFileAtomic(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-cfg-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, p); err != nil {
		return err
	}
	return nil
}

func removePlaintextProfile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(filepath.Dir(path))
	return nil
}

func regularFileExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

// Rename moves a remote config directory to a new validated name.
func (s *Store) Rename(oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == newName {
		return nil
	}
	if IsInternalName(oldName) || IsInternalName(newName) {
		return fmt.Errorf("cannot rename reserved ssh config %q", "internal")
	}
	return s.renameHybrid(oldName, newName)
}

func (s *Store) renameHybrid(oldName, newName string) error {
	if err := ValidateName(oldName); err != nil {
		return err
	}
	if err := ValidateName(newName); err != nil {
		return err
	}
	s.mu.Lock()
	if err := s.refreshIdentitiesLocked(false); err != nil {
		s.mu.Unlock()
		return err
	}
	if data, ok := s.temporary[oldName]; ok {
		if existing, err := s.repositoryNameConflictLocked(newName, oldName); err != nil {
			s.mu.Unlock()
			return err
		} else if existing != "" {
			s.mu.Unlock()
			return fmt.Errorf("config already exists: %s", existing)
		}
		if existing, err := s.plaintextNameConflictLocked(newName, oldName); err != nil {
			s.mu.Unlock()
			return err
		} else if existing != "" {
			s.mu.Unlock()
			return fmt.Errorf("config already exists: %s", existing)
		}
		if _, exists := s.temporary[newName]; exists {
			s.mu.Unlock()
			return fmt.Errorf("config already exists: %s", newName)
		}
		for existing := range s.temporary {
			if strings.EqualFold(existing, newName) {
				s.mu.Unlock()
				return fmt.Errorf("config already exists: %s", existing)
			}
		}
		s.temporary[newName] = data
		delete(s.temporary, oldName)
		if id := s.temporaryIdentities[oldName]; id != "" {
			s.temporaryIdentities[newName] = id
			delete(s.temporaryIdentities, oldName)
		}
		s.invalidateIdentitiesLocked()
		s.mu.Unlock()
		s.notifyChange()
		return nil
	}
	if existing, err := s.repositoryNameConflictLocked(newName, oldName); err != nil {
		s.mu.Unlock()
		return err
	} else if existing != "" {
		s.mu.Unlock()
		return fmt.Errorf("config already exists: %s", existing)
	}
	if existing, err := s.plaintextNameConflictLocked(newName, oldName); err != nil {
		s.mu.Unlock()
		return err
	} else if existing != "" {
		s.mu.Unlock()
		return fmt.Errorf("config already exists: %s", existing)
	}
	for existing := range s.temporary {
		if strings.EqualFold(existing, newName) {
			s.mu.Unlock()
			return fmt.Errorf("config already exists: %s", existing)
		}
	}
	encryptedName, encryptedExists, err := s.encryptedFoldLocked(oldName)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if encryptedExists {
		if s.encrypted == nil {
			s.mu.Unlock()
			return fmt.Errorf("configuration encryption is enabled but its keyring is unavailable")
		}
		renameErr := s.encrypted.Rename(encryptedName, newName)
		if renameErr != nil && !errors.Is(renameErr, configstore.ErrDurability) {
			s.mu.Unlock()
			return renameErr
		}
		// A stale plaintext copy belongs to the same logical profile and must not
		// become visible under the old name after the catalog rename.
		if plaintextName, exists, err := s.plaintextFoldLocked(newName); err != nil {
			s.mu.Unlock()
			return err
		} else if exists {
			if err := removePlaintextProfile(s.plaintextPath(plaintextName)); err != nil {
				s.mu.Unlock()
				return err
			}
		}
		if err := removeProfileIdentity(filepath.Join(s.root(), encryptedName)); err != nil {
			s.mu.Unlock()
			return err
		}
		s.invalidateIdentitiesLocked()
		s.mu.Unlock()
		s.notifyChange()
		return renameErr
	}
	plaintextName, plaintextExists, err := s.plaintextFoldLocked(oldName)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	if !plaintextExists {
		s.mu.Unlock()
		return fmt.Errorf("ssh config %q %w", oldName, ErrNotFound)
	}
	if err := os.Rename(filepath.Dir(s.plaintextPath(plaintextName)), filepath.Dir(s.plaintextPath(newName))); err != nil {
		s.mu.Unlock()
		return err
	}
	s.invalidateIdentitiesLocked()
	s.mu.Unlock()
	s.notifyChange()
	return nil
}

func (s *Store) repositoryNameConflictLocked(name, exclude string) (string, error) {
	if s.encrypted == nil {
		return "", nil
	}
	names, err := s.encrypted.List()
	if err != nil {
		return "", err
	}
	for _, existing := range names {
		if !strings.EqualFold(existing, exclude) && strings.EqualFold(existing, name) {
			return existing, nil
		}
	}
	return "", nil
}

func (s *Store) plaintextNameConflictLocked(name, exclude string) (string, error) {
	if IsInternalName(name) && !IsInternalName(exclude) {
		exists, err := regularFileExists(s.internalOverridePath())
		if err != nil || !exists {
			return "", err
		}
		return "internal", nil
	}
	entries, err := os.ReadDir(s.root())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") ||
			strings.EqualFold(entry.Name(), exclude) || !strings.EqualFold(entry.Name(), name) {
			continue
		}
		exists, err := regularFileExists(filepath.Join(s.root(), entry.Name(), "config.toml"))
		if err != nil {
			return "", err
		}
		if exists {
			return entry.Name(), nil
		}
	}
	return "", nil
}

// Delete removes a remote config; the virtual internal profile cannot be deleted.
func (s *Store) Delete(name string) error {
	if IsInternalName(name) {
		return fmt.Errorf("cannot delete reserved ssh config %q: %w", name, ErrReserved)
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.temporary, name)
	delete(s.temporaryIdentities, name)
	encryptedName, encryptedExists, err := s.encryptedFoldLocked(name)
	if err == nil && encryptedExists {
		if s.encrypted == nil {
			err = fmt.Errorf("configuration encryption is enabled but its keyring is unavailable")
		} else {
			err = s.encrypted.Delete(encryptedName)
		}
	}
	plaintextName, plaintextExists, plaintextErr := s.plaintextFoldLocked(name)
	if err == nil {
		err = plaintextErr
	}
	if err == nil && plaintextExists {
		err = removePlaintextProfile(s.plaintextPath(plaintextName))
	}
	if err == nil {
		err = removeProfileIdentity(filepath.Join(s.root(), name))
	}
	s.invalidateIdentitiesLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.notifyChange()
	return nil
}
