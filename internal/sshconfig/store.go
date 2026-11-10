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

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/fsutil"
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
	dataDir string

	// temporary holds process-local profiles. A sync.Map: each entry is one name's
	// whole state and no rule spans two names.
	temporary sync.Map // name string -> []byte

	// nameTokens serialises the few operations that are a check-and-write on ONE
	// name (collision test, then file or temporary entry). The key is the lowercased
	// name, because uniqueness is case-insensitive — so `prod` and `PROD` share a
	// token while `prod` and `staging` do not. That is the whole point of splitting
	// what used to be one lock: unrelated profiles save in parallel.
	nameTokens sync.Map // lower(name) string -> chan struct{} (1-slot token)

	onChange atomic.Value // func(), set via SetOnChange, called after Save/Delete/Rename

	// state publishes the encryption side: the repository holding ciphertext and
	// the preference for how a brand new profile is first persisted. Readers load
	// the pointer; the operations that change it store a new one (store_state.go).
	state atomic.Pointer[storeState]

	// The two ciphertext writes are fields so a test can make publication fail
	// and observe what survives. That ordering — publish ciphertext first, remove
	// plaintext second — is the difference between a recoverable duplicate and a
	// lost profile, and it cannot be observed without an injected failure.
	// Production leaves both nil.
	writeCiphertext  func(*configstore.Repository, string, []byte) error
	importCiphertext func(*configstore.Repository, []configstore.Item) error
}

// saveCiphertext publishes one encrypted profile.
func (s *Store) saveCiphertext(repo *configstore.Repository, name string, data []byte) error {
	if s.writeCiphertext != nil {
		return s.writeCiphertext(repo, name, data)
	}
	return repo.Save(name, data)
}

// importCiphertexts publishes a converted selection as one catalog commit.
func (s *Store) importCiphertexts(repo *configstore.Repository, items []configstore.Item) error {
	if s.importCiphertext != nil {
		return s.importCiphertext(repo, items)
	}
	return repo.Import(items)
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
	// encryptNew is kept as given, even when repo is nil: a store told to
	// encrypt must refuse to write, not quietly downgrade to plaintext.
	store.setEncryption(repo, encryptNew)
	return store
}

// OpenConfiguredStore opens an encrypted repository when one has been
// initialized and otherwise preserves the plaintext layout. A store whose
// keyring needs a password comes back locked rather than failing: the operator
// unlocks it through the UI, and until then writes are refused instead of
// silently falling back to plaintext.
func OpenConfiguredStore(dataDir string, providers ...configstore.Provider) (*Store, error) {
	repo, err := configstore.OpenRepositoryForRecovery(dataDir, providers...)
	if errors.Is(err, configstore.ErrNotInitialized) {
		// A catalog without its keyring is not a plaintext store: something was
		// initialized and then lost its keys. Falling back to plaintext here would
		// write cleartext where the operator expects ciphertext.
		hasCatalog, catalogErr := configstore.HasManagedCatalog(dataDir)
		if catalogErr != nil {
			return nil, catalogErr
		}
		if hasCatalog {
			return nil, fmt.Errorf("%w: encrypted catalog exists but config.keys is missing", configstore.ErrNotInitialized)
		}
		return NewStore(dataDir), nil
	}
	if err != nil {
		return nil, err
	}
	store := NewHybridStore(repo.DataDir(), repo, repo.EncryptNew())
	if repo.Status().Locked {
		return store, nil
	}
	// Opening verifies every committed profile: a catalog entry that no longer
	// parses must fail startup rather than surface later as a broken session.
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

// StartupWarning names the one condition an operator has to act on before the
// store can persist anything: an initialized store whose keyring is locked.
func (s *Store) StartupWarning() string {
	repo := s.repositorySnapshot()
	if repo == nil {
		return ""
	}
	if status := repo.Status(); status.Locked {
		return "configuration encryption is initialized but locked; open the settings page and enter the password before saving or converting hosts"
	}
	return ""
}

// Close releases the repository this store opened, if any.
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

// lockName takes the token for one name and returns its release. Tokens are 1-slot
// channels rather than mutexes so the acquisition is a plain send, and a caller that
// already holds another name's token can never deadlock by ordering.
func (s *Store) lockName(name string) func() {
	key := strings.ToLower(name)
	tok := make(chan struct{}, 1)
	tok <- struct{}{}
	actual, _ := s.nameTokens.LoadOrStore(key, tok)
	ch := actual.(chan struct{})
	<-ch
	return func() { ch <- struct{}{} }
}

// lockNames takes several tokens in sorted order, so two callers that need the same
// pair (a rename) cannot each hold what the other wants.
func (s *Store) lockNames(names ...string) func() {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	releases := make([]func(), 0, len(sorted))
	for _, n := range sorted {
		releases = append(releases, s.lockName(n))
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
}

// SetOnChange registers a callback fired after Save, Delete, or Rename succeeds.
func (s *Store) SetOnChange(fn func()) {
	s.onChange.Store(fn)
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
	return &Store{dataDir: abs}
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
	if v, inMemory := s.temporary.Load(name); inMemory {
		return ParseAndValidate(v.([]byte))
	}
	// Ciphertext is consulted before plaintext: an encrypted profile is the
	// current generation of that name, and a leftover plaintext copy is what a
	// half-finished conversion or an interrupted restore leaves behind.
	if repo := s.repositorySnapshot(); repo != nil {
		data, err := repo.Read(name)
		if err != nil && !errors.Is(err, configstore.ErrNotFound) {
			return nil, err
		}
		if err == nil {
			return ParseAndValidate(data)
		}
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
	if v, inMemory := s.temporary.Load(name); inMemory {
		return append([]byte(nil), v.([]byte)...), nil
	}
	if repo := s.repositorySnapshot(); repo != nil {
		data, err := repo.Read(name)
		if err != nil && !errors.Is(err, configstore.ErrNotFound) {
			return nil, err
		}
		if err == nil {
			if _, err := ParseAndValidate(data); err != nil {
				return nil, err
			}
			return data, nil
		}
	}
	data, err := os.ReadFile(p)
	if err != nil {
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
	entries, err := os.ReadDir(s.root())
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	names := []string{"internal"}
	seen := map[string]bool{"internal": true}
	s.temporary.Range(func(k, _ any) bool {
		name := k.(string)
		folded := strings.ToLower(name)
		if !seen[folded] {
			names = append(names, name)
			seen[folded] = true
		}
		return true
	})
	if repo := s.repositorySnapshot(); repo != nil {
		stored, err := repo.List()
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
	name = strings.TrimSpace(name)
	if IsInternalName(name) {
		if temporary {
			return fmt.Errorf("internal profile cannot be temporary")
		}
		if _, err := ParseInternalOverride(data, InternalEntry()); err != nil {
			return err
		}
		p := s.internalOverridePath()
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(p, data); err != nil {
			return err
		}
		s.notifyChange()
		return nil
	}
	if _, err := ParseAndValidate(data); err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}

	// ONE NAME, ONE TOKEN. Collision tests and the write that follows them are a
	// check-and-write on this name only, so unrelated profiles still save in
	// parallel while `prod` and `PROD` serialize (uniqueness is case-insensitive).
	unlock := s.lockName(name)
	defer unlock()

	if err := s.checkNameCollision(name, name); err != nil {
		return err
	}
	repo := s.repositorySnapshot()
	encryptedName, encryptedExists, err := s.encryptedFold(name)
	if err != nil {
		return err
	}
	if encryptedExists && encryptedName != name {
		return fmt.Errorf("config already exists: %s", encryptedName)
	}
	_, plaintextExists, err := s.plaintextFold(name)
	if err != nil {
		return err
	}

	if temporary {
		if encryptedExists {
			if repo == nil {
				return errEncryptionUnavailable
			}
			if err := repo.Delete(name); err != nil {
				return err
			}
		}
		if plaintextExists {
			if err := removePlaintextProfile(s.plaintextPath(name)); err != nil {
				return err
			}
		}
		s.temporary.Store(name, append([]byte(nil), data...))
		s.notifyChange()
		return nil
	}

	// Existing plaintext profiles keep that mode until the operator explicitly
	// selects them on the encryption page. The preference only chooses how a
	// brand-new profile is first persisted.
	encryptNew := s.stateSnapshot().encryptNew
	if !encryptedExists && (!encryptNew || plaintextExists) {
		p := s.plaintextPath(name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(p, data); err != nil {
			return err
		}
		s.temporary.Delete(name)
		s.notifyChange()
		return nil
	}

	if repo == nil {
		return errEncryptionUnavailable
	}
	// The ciphertext is published first. Only after it succeeds is the plaintext
	// copy removed: a crash here leaves both copies, which is recoverable, while
	// the opposite order would lose the profile.
	if err := s.saveCiphertext(repo, name, data); err != nil {
		return err
	}
	if plaintextExists {
		if err := removePlaintextProfile(s.plaintextPath(name)); err != nil {
			return fmt.Errorf("profile was encrypted but its plaintext copy could not be removed: %w", err)
		}
	}
	s.temporary.Delete(name)
	s.notifyChange()
	return nil
}

// errEncryptionUnavailable is the refusal that keeps a locked store from
// silently writing plaintext where the operator expects ciphertext.
var errEncryptionUnavailable = fmt.Errorf("configuration encryption is enabled but its keyring is unavailable")

// IsTemporary reports whether the profile exists only in this process.
func (s *Store) IsTemporary(name string) bool {
	_, ok := s.temporary.Load(name)
	return ok
}

// checkNameCollision reports whether the name is taken, case-insensitively, in
// any of the three places a profile can live: process memory, the encrypted
// catalog, or the plaintext tree. The caller holds that name's token, which is
// what makes the answer usable: without it another saver could claim the name
// between this check and the write.
func (s *Store) checkNameCollision(name, exclude string) error {
	var clash string
	s.temporary.Range(func(k, _ any) bool {
		existing := k.(string)
		if existing != exclude && strings.EqualFold(existing, name) {
			clash = existing
			return false
		}
		return true
	})
	if clash != "" {
		return fmt.Errorf("config already exists: %s", clash)
	}
	if existing, exists, err := s.encryptedFold(name); err != nil {
		return err
	} else if exists && existing != exclude {
		return fmt.Errorf("config already exists: %s", existing)
	}
	entries, err := os.ReadDir(s.root())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != exclude && strings.EqualFold(entry.Name(), name) {
			return fmt.Errorf("config already exists: %s", entry.Name())
		}
	}
	return nil
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
// Rename moves a remote config directory to a new validated name. An encrypted
// profile is renamed inside the catalog, so its object identity and generation
// survive the move.
func (s *Store) Rename(oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == newName {
		return nil
	}
	if IsInternalName(oldName) || IsInternalName(newName) {
		return fmt.Errorf("cannot rename reserved ssh config %q", "internal")
	}
	if err := ValidateName(oldName); err != nil {
		return err
	}
	if err := ValidateName(newName); err != nil {
		return err
	}

	// BOTH NAMES, TAKEN IN SORTED ORDER. A rename moves two names at once, so it is
	// the one operation here that needs two tokens; sorting is what stops a concurrent
	// rename of the pair in the other direction from deadlocking against it.
	unlock := s.lockNames(oldName, newName)
	defer unlock()

	if err := s.checkNameCollision(newName, oldName); err != nil {
		return err
	}

	if v, ok := s.temporary.Load(oldName); ok {
		s.temporary.Store(newName, v)
		s.temporary.Delete(oldName)
		s.notifyChange()
		return nil
	}

	repo := s.repositorySnapshot()
	encryptedName, encryptedExists, err := s.encryptedFold(oldName)
	if err != nil {
		return err
	}
	if encryptedExists {
		if repo == nil {
			return errEncryptionUnavailable
		}
		renameErr := repo.Rename(encryptedName, newName)
		if renameErr != nil && !errors.Is(renameErr, configstore.ErrDurability) {
			return renameErr
		}
		// A stale plaintext copy belongs to the same logical profile and must not
		// become visible under the new name after the catalog rename.
		if plaintextName, exists, err := s.plaintextFold(newName); err != nil {
			return err
		} else if exists {
			if err := removePlaintextProfile(s.plaintextPath(plaintextName)); err != nil {
				return err
			}
		}
		s.notifyChange()
		return renameErr
	}

	plaintextName, plaintextExists, err := s.plaintextFold(oldName)
	if err != nil {
		return err
	}
	if !plaintextExists {
		return fmt.Errorf("ssh config %q %w", oldName, ErrNotFound)
	}
	if err := os.Rename(filepath.Dir(s.plaintextPath(plaintextName)), filepath.Dir(s.plaintextPath(newName))); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// Delete removes a remote config; the virtual internal profile cannot be deleted.
func (s *Store) Delete(name string) error {
	if IsInternalName(name) {
		return fmt.Errorf("cannot delete reserved ssh config %q: %w", name, ErrReserved)
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	unlock := s.lockName(name)
	defer unlock()
	s.temporary.Delete(name)
	repo := s.repositorySnapshot()
	encryptedName, encryptedExists, err := s.encryptedFold(name)
	if err == nil && encryptedExists {
		if repo == nil {
			err = errEncryptionUnavailable
		} else {
			err = repo.Delete(encryptedName)
		}
	}
	plaintextName, plaintextExists, plaintextErr := s.plaintextFold(name)
	if err == nil {
		err = plaintextErr
	}
	if err == nil && plaintextExists {
		err = removePlaintextProfile(s.plaintextPath(plaintextName))
	}
	if err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// encryptedFold reports whether the repository holds a name that equals name
// case-insensitively, returning the stored spelling.
func (s *Store) encryptedFold(name string) (string, bool, error) {
	repo := s.repositorySnapshot()
	if repo == nil {
		return "", false, nil
	}
	names, err := repo.List()
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

// plaintextFold reports whether a config.toml exists for name, case-insensitively.
func (s *Store) plaintextFold(name string) (string, bool, error) {
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

func removePlaintextProfile(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Removing the now-empty per-profile directory is best effort: a leftover
	// directory without config.toml is already invisible to List.
	_ = os.Remove(filepath.Dir(path))
	return nil
}

func regularFileExists(path string) (bool, error) {
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !st.IsDir(), nil
}
