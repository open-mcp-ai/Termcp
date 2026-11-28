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
	"github.com/open-mcp-ai/termcp/internal/hostshare"
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
	importCiphertext func(*configstore.Repository, []configstore.Item, configstore.SaveOptions) error

	/* dirReads counts the directory listings this store performed. It exists
	   because "how many directories does one host cost" is the property that has
	   broken twice now (#353: one name rescanned every host; then a storage-mode
	   lookup read the whole host list to find one entry), and a timing test for it
	   would be the flaky kind. A counter makes it arithmetic: N hosts, N listings.

	   One atomic add per listing is noise next to the syscall it counts. */
	dirReads atomic.Int64
}

// saveCiphertext publishes one encrypted profile with this save's decisions: which
// key seals it and, when that key carries a passphrase, the passphrase that writes
// the host's `.adk`.
func (s *Store) saveCiphertext(repo *configstore.Repository, name string, data []byte, mode SaveMode) error {
	if s.writeCiphertext != nil {
		return s.writeCiphertext(repo, name, data)
	}
	return repo.SaveWithOptions(name, data, configstore.SaveOptions{
		KeyID:      mode.KeyID,
		Passphrase: mode.Passphrase,
	})
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
// initialized and otherwise preserves the plaintext layout. A keychain whose
// keys need a human comes back with nothing loaded rather than failing: the
// operator loads one through the panel, and until then writes are refused
// instead of silently falling back to plaintext.
//
// NOTHING IS READ HERE. Opening the store used to load every committed profile
// to prove they still parse, which made startup O(hosts) disk reads and — since
// one is enough — let a single host whose key is not loaded, whose host is not
// activated, or whose TOML no longer parses refuse to serve every other host.
// The panel that repairs those states is served by this same process, so a
// startup check is the one place the check cannot be acted on. Each profile is
// read when it is used, and reports its own state then.
func OpenConfiguredStore(dataDir string, providers ...configstore.Provider) (*Store, error) {
	repo, err := configstore.OpenRepository(dataDir, providers...)
	if errors.Is(err, configstore.ErrNotInitialized) {
		/* A catalog without its keychain does not land here: configstore opens that
		   state with the fault recorded, so the instance starts and each encrypted
		   profile fails on access instead of every plaintext profile being refused
		   at startup. What is left is a genuinely plaintext dir. */
		return NewStore(dataDir), nil
	}
	if err != nil {
		return nil, err
	}
	/* The preference is readable whether or not a key is loaded, so "no key is
	   active" no longer has to guess. Guessing true made the whole store read-only
	   in that state — including plaintext hosts, which need no key at all. */
	encryptNew := repo.EncryptNew()
	store := NewHybridStore(repo.DataDir(), repo, encryptNew)
	return store, nil
}

// StartupWarning names the one condition an operator has to act on before the
// store can persist anything: an initialized store with no key loaded.
func (s *Store) StartupWarning() string {
	repo := s.repositorySnapshot()
	if repo == nil {
		return ""
	}
	if !repo.Loaded() {
		return "configuration encryption is initialized but no key is plugged in; open the keychain panel and plug a key in before saving or converting hosts"
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

/* hostStorageError turns a repository read failure into the per-host vocabulary
 * the surface speaks, so one host's state is never reported as another's: "insert
 * the key" and "activate the host" are different actions, and a caller that gets
 * the wrong one has nothing to do next.
 */
func hostStorageError(err error, name string) error {
	switch {
	case errors.Is(err, configstore.ErrOrphaned):
		return orphanedCiphertextError{name: name}
	case errors.Is(err, configstore.ErrNotActivated):
		return notActivatedHostError{name: name}
	case errors.Is(err, configstore.ErrKeyNotLoaded):
		return unloadedHostError{name: name}
	default:
		return err
	}
}

// orphanedCiphertextError reports a profile whose key has been deleted: the
// file is there, nothing can decrypt it, and saying so is kinder than reporting
// the host as missing or letting the operator believe loading a key would help.
type orphanedCiphertextError struct{ name string }

func (e orphanedCiphertextError) Error() string {
	return fmt.Sprintf("host %q is encrypted and its key was deleted; this profile can never be read again", e.name)
}

func (e orphanedCiphertextError) Unwrap() error { return configstore.ErrNotInitialized }

// unloadedHostError names the one profile that cannot be read and what would fix
// it, while still answering errors.Is(err, configstore.ErrKeyNotLoaded) so
// callers keep one condition to handle.
type unloadedHostError struct{ name string }

func (e unloadedHostError) Error() string {
	return fmt.Sprintf("host %q is encrypted at rest and the key that seals it is not loaded; load that key in the keychain panel to use this host", e.name)
}

func (e unloadedHostError) Unwrap() error { return configstore.ErrKeyNotLoaded }

/* notActivatedHostError names the one profile whose second layer is off, and what
 * would fix it, while still answering errors.Is(err, configstore.ErrNotActivated)
 * so callers keep one condition to handle.
 */
type notActivatedHostError struct{ name string }

func (e notActivatedHostError) Error() string {
	return fmt.Sprintf("host %q is encrypted at rest and is not activated; activate it in the keychain panel (its key's passphrase may be needed) before using it", e.name)
}

func (e notActivatedHostError) Unwrap() error { return configstore.ErrNotActivated }

// IsNotActivated reports an error caused by a profile whose second layer is off.
func IsNotActivated(err error) bool {
	return errors.Is(err, configstore.ErrNotActivated)
}

// IsOrphanedCiphertext reports an error caused by a profile whose key is gone:
// the bytes are on disk, nothing can ever decrypt them again. Callers use it to
// answer "this profile is gone for good" instead of "bad request".
func IsOrphanedCiphertext(err error) bool {
	var orphaned orphanedCiphertextError
	return errors.As(err, &orphaned)
}

// Load reads and validates a named config.
// name "internal" is the built-in entry, optionally overridden by a stored
// internal/config.toml (see ParseInternalOverride).
func (s *Store) Load(name string) (*Entry, error) {
	ent, _, err := s.LoadWithMode(name)
	return ent, err
}

/* LoadWithMode loads a profile and reports, in the same pass, whether it is stored
 * as ciphertext.
 *
 * The storage mode is a fact this function establishes to answer "is there an
 * encrypted generation?" — asking `StorageMode` afterwards re-derived it with
 * another directory listing and a stat, per host, on every list. Measured on 1000
 * hosts: 600 µs of the 2400 µs a listing took were that second look.
 *
 * The mode is a STRING, not a private enum, because `StorageMode` is the public
 * vocabulary every surface already prints (StoragePlaintext and friends).
 */
func (s *Store) LoadWithMode(name string) (*Entry, string, error) {
	name = strings.TrimSpace(name)
	if IsInternalName(name) {
		base := InternalEntry()
		data, err := os.ReadFile(s.internalOverridePath())
		if err != nil {
			if os.IsNotExist(err) {
				return base, StorageBuiltin, nil // no override: the defaults ARE the profile
			}
			return nil, "", err
		}
		ent, err := ParseInternalOverride(data, base)
		if err != nil {
			// A broken override is reported, not silently ignored: falling back
			// to the defaults would run the session UNGATED after the user asked
			// for a gate, which is the one direction this must never fail.
			return nil, "", fmt.Errorf("internal profile override: %w", err)
		}
		return ent, StorageBuiltin, nil
	}
	/* ONE directory listing decides the two questions this function starts with:
	   does this name have a directory, and does that directory hold ciphertext.
	   They used to be two calls — a fold and then a `hasCiphertextOnDisk` in the
	   orphan check — and the fold itself re-read the directory. On a real
	   filesystem a ReadDir is the expensive part (~150 µs), so three of them per
	   host is 450 µs of the ~500 µs a list spends per host.

	   The catalog is consulted only when there is no directory to look at: that is
	   the case-fold fallback, for a sealed host whose spelling differs from the
	   name asked about. */
	repo := s.repositorySnapshot()
	unloadedRepo := repo != nil && !repo.Loaded()
	exists, sealed := s.ciphertextOnDisk(name)
	if !exists {
		if _, isSealed, err := s.encryptedFold(name); err == nil && isSealed {
			sealed = true
		}
	}
	/* Both no-repo and nothing-loaded mean "no readable ciphertext". The first
	   case is a deleted key: the ciphertext is still on disk and can never be
	   opened again, which is a different message from "load my key". */
	if sealed && (unloadedRepo || repo == nil) {
		if repo == nil || repo.KeyringMissing() {
			return nil, StorageEncrypted, orphanedCiphertextError{name: name}
		}
		return nil, StorageEncrypted, unloadedHostError{name: name}
	}
	p, err := s.configPath(name)
	if err != nil {
		return nil, "", err
	}
	if v, inMemory := s.temporary.Load(name); inMemory {
		ent, err := ParseAndValidate(v.([]byte))
		return ent, StorageTemporary, err
	}
	// Ciphertext is consulted before plaintext: an encrypted profile is the
	// current generation of that name, and a leftover plaintext copy is what a
	// half-finished conversion or an interrupted restore leaves behind.
	// A store with nothing loaded has no ciphertext it can read: asking would
	// fail the load with "no key loaded" and take every plaintext profile with it.
	if repo != nil && repo.Loaded() {
		data, err := repo.Read(name)
		if err != nil && !errors.Is(err, configstore.ErrNotFound) {
			return nil, StorageEncrypted, hostStorageError(err, name)
		}
		if err == nil {
			ent, err := ParseAndValidate(data)
			return ent, StorageEncrypted, err
		}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", fmt.Errorf("ssh config %q %w (use data-dir/ssh_configs/%s/config.toml)", name, ErrNotFound, name)
		}
		return nil, "", err
	}
	ent, err := ParseAndValidate(data)
	return ent, StoragePlaintext, err
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
	if repo := s.repositorySnapshot(); repo != nil && !repo.Loaded() {
		if _, encrypted, err := s.encryptedFold(name); err == nil && encrypted {
			/* The keychain file itself is gone: no key will ever open this, and
			   telling the operator to insert one sends them looking for something
			   that does not exist. */
			if repo.KeyringMissing() {
				return nil, orphanedCiphertextError{name: name}
			}
			/* Name the host and the action. A generic "no key loaded" leaves the AI
			   unable to tell which profile failed or what would fix it. */
			return nil, unloadedHostError{name: name}
		}
	}
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	if v, inMemory := s.temporary.Load(name); inMemory {
		return append([]byte(nil), v.([]byte)...), nil
	}
	// Same rule as Load: a store with nothing loaded has no ciphertext it can
	// read, so asking the repository would fail a plaintext profile too.
	if repo := s.repositorySnapshot(); repo != nil && repo.Loaded() {
		data, err := repo.Read(name)
		if err != nil && !errors.Is(err, configstore.ErrNotFound) {
			return nil, hostStorageError(err, name)
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
		/* No readable ciphertext and no plaintext, yet the directory holds
		   generations: the key is gone. Report that instead of a bare file error,
		   which the HTTP layer turns into "Not Found" for a profile that plainly
		   exists in the list. */
		if os.IsNotExist(err) && s.hasCiphertextOnDisk(name) {
			return nil, orphanedCiphertextError{name: name}
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
		// Nothing loaded: the catalog cannot be read, so the names below come from
		// the profile directories on disk. A host whose key is unavailable is still
		// a host the user can see and load a key for, not one that disappears.
		if err != nil && !errors.Is(err, configstore.ErrKeyNotLoaded) {
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
		dir := filepath.Join(s.root(), name)
		listed := false
		if st, err := os.Stat(filepath.Join(dir, "config.toml")); err == nil && !st.IsDir() {
			listed = true
		}
		if !listed {
			// Ciphertext on disk is still a known host: it stays in the list (and
			// fails per-profile) rather than vanishing when its key cannot open it.
			if entries, err := os.ReadDir(dir); err == nil {
				for _, f := range entries {
					if !f.IsDir() && strings.HasSuffix(f.Name(), ".enc") {
						listed = true
						break
					}
				}
			}
		}
		if listed {
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
// SaveMode is the caller's per-save storage decision. Encrypt is a triple state
// on purpose: nil keeps the store preference, true encrypts, false stores
// plaintext. The connection form sends it because whether one profile is
// encrypted is a property of that profile, not a global setting — and "nil"
// keeps older callers working unchanged.
type SaveMode struct {
	Temporary bool
	Encrypt   *bool
	// KeyID seals this profile with one named Main Key. It implies Encrypt: a
	// caller that chose a key asked for ciphertext, and the choice is per profile
	// because which key protects a host is a property of that host.
	KeyID configstore.ID
	/* Passphrase is the sealing key's passphrase, present only when this save
	   creates or re-saves a host under a key that carries one. It is never stored:
	   it derives the host's activation key, which writes the `.adk` beside the new
	   generation (§4.5, §8.2 step 2). */
	Passphrase string
}

// SaveWithOptions stores a profile under the store's preference.
func (s *Store) SaveWithOptions(name string, data []byte, temporary bool) error {
	return s.SaveWithMode(name, data, SaveMode{Temporary: temporary})
}

// SaveWithMode stores a profile with an explicit per-save storage decision.
func (s *Store) SaveWithMode(name string, data []byte, mode SaveMode) error {
	name = strings.TrimSpace(name)
	if IsInternalName(name) {
		if mode.Temporary {
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

	if mode.Temporary {
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

	// The explicit request wins over the store preference; nil keeps the old
	// rule (existing plaintext profiles stay plaintext until converted). A named
	// key is an even more explicit request: it names the ciphertext to use.
	encryptNew := s.stateSnapshot().encryptNew
	if mode.Encrypt != nil {
		encryptNew = *mode.Encrypt
	}
	if !mode.KeyID.IsZero() {
		encryptNew = true
	}
	// Unchecking "encrypt" on an existing encrypted profile restores it: the
	// operator asked for plaintext, and leaving ciphertext behind would make the
	// checkbox a lie.
	if encryptedExists && mode.Encrypt != nil && !*mode.Encrypt {
		repo := s.repositorySnapshot()
		if repo == nil {
			return errEncryptionUnavailable
		}
		data, err := repo.Read(name)
		if err != nil {
			return err
		}
		p := s.plaintextPath(name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(p, data); err != nil {
			return err
		}
		if err := repo.Delete(name); err != nil {
			return fmt.Errorf("plaintext copy written but the encrypted one could not be removed: %w", err)
		}
		s.notifyChange()
		return nil
	}
	if !encryptedExists && (!encryptNew || (plaintextExists && mode.Encrypt == nil)) {
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
	if mode.KeyID.IsZero() {
		/* No implicit choice: a host's key is a property of that host (§8.2). The one
		   exception is a keychain with exactly one key, where there is nothing to
		   choose between and making the caller name it would be ceremony. */
		keys, err := repo.Keys()
		if err != nil {
			return err
		}
		if len(keys) != 1 {
			return configstore.ErrKeyRequired
		}
		wanted, err := parseKeyID(keys[0].ID)
		if err != nil {
			return err
		}
		mode.KeyID = wanted
	}
	// The ciphertext is published first. Only after it succeeds is the plaintext
	// copy removed: a crash here leaves both copies, which is recoverable, while
	// the opposite order would lose the profile.
	if err := s.saveCiphertext(repo, name, data, mode); err != nil {
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

// errEncryptionUnavailable is the refusal that keeps a store with no key loaded
// from silently writing plaintext where the operator expects ciphertext.
var errEncryptionUnavailable = fmt.Errorf("configuration encryption is enabled but no key is loaded")

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
	encryptedName, inCatalog, err := s.encryptedFold(name)
	/* A loaded catalog only answers for names it still lists, so a leftover
	   generation from a deleted key looks absent. Ask the disk as well: without
	   this the delete returned success and removed nothing. */
	onDisk := s.hasCiphertextOnDisk(name)
	if err == nil && (inCatalog || onDisk) {
		switch {
		case repo == nil:
			/* No catalog and no keyring: nothing can ever decrypt these files, so
			   removing them IS the delete. Refusing made an unreadable profile
			   impossible to clean up — the exact profile the panel tells the
			   operator to delete and recreate. */
			err = s.removeCiphertextFiles(name)
		case repo.KeyringMissing():
			/* The keychain file itself is gone: nothing can ever open these bytes,
			   so removing them IS the delete. Refusing left the one profile the
			   panel tells the operator to delete and recreate impossible to clean
			   up. */
			err = s.removeCiphertextFiles(name)
		case !repo.Loaded():
			/* A keychain exists but nothing is plugged in: what looks like an
			   orphan might still open once its key is loaded, so do not destroy it
			   from a state that cannot prove entitlement. Say "load the key",
			   which is actionable. */
			err = configstore.ErrKeyNotLoaded
		default:
			if inCatalog {
				err = repo.Delete(encryptedName)
			}
			if err == nil && s.hasCiphertextOnDisk(name) {
				/* The catalog did not know this name — its entry was lost with an
				   older key — so repo.Delete had nothing to remove. Clear the
				   leftover generations, which nothing can read. */
				err = s.removeCiphertextFiles(name)
			}
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
	/* The name's own directory answers the common case in one readdir. The catalog
	   listing below is O(hosts), and asking it once per host — which is what a
	   listing does — made painting the host list quadratic. */
	if exists, sealed := s.ciphertextOnDisk(name); sealed {
		return name, true, nil
	} else if exists {
		/* The directory is there and holds no `.enc`: this name is plaintext (or a
		   temporary profile), whatever the catalog says. The catalog listing below is
		   O(hosts), and asking it here — which is what one call per host did — made a
		   list of N hosts cost N catalog scans. It is still needed for the case it
		   exists for: a sealed host whose spelling differs from the name asked
		   about, where there is no directory to look at. */
		return "", false, nil
	}
	repo := s.repositorySnapshot()
	unloaded := repo == nil
	var names []string
	var err error
	if repo != nil {
		names, err = repo.List()
		unloaded = errors.Is(err, configstore.ErrKeyNotLoaded)
	}
	if unloaded {
		/* Nothing loaded: the catalog is unreadable, but the ciphertext's directory
		   still says this name is encrypted. Answering from disk keeps the storage
		   mode — and therefore the badge and the "key not loaded" state — visible
		   instead of degrading into "not found". */
		dir := filepath.Join(s.root(), name)
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return "", false, nil
		}
		for _, f := range entries {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".enc") {
				return name, true, nil
			}
		}
		return "", false, nil
	}
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

/* ciphertextOnDisk answers both questions one directory listing can answer: does
 * this name have a directory at all, and does it hold encrypted generations.
 *
 * They are asked together because the listing is the expensive part — on a real
 * filesystem a ReadDir costs ~150 µs while the loop over its entries costs nothing
 * — and the two callers each used to pay for it separately, plus a stat to learn
 * what the listing had already told them.
 */
func (s *Store) ciphertextOnDisk(name string) (exists, sealed bool) {
	s.dirReads.Add(1)
	entries, err := os.ReadDir(filepath.Join(s.root(), name))
	if err != nil {
		return false, false
	}
	for _, f := range entries {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".enc") {
			return true, true
		}
	}
	return true, false
}

// hasCiphertextOnDisk reports whether the profile directory holds encrypted
// generations, regardless of whether any catalog currently lists the name.
func (s *Store) hasCiphertextOnDisk(name string) bool {
	_, sealed := s.ciphertextOnDisk(name)
	return sealed
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
	/* The name as written answers the common case in ONE stat. The scan below reads
	   every host directory to find a name whose spelling differs, and a listing asks
	   this question once per host: 1000 hosts meant 1000 full directory reads, which
	   is what made a storage-mode lookup cost 1.4 ms (measured) instead of a few
	   microseconds. */
	if exists, err := regularFileExists(filepath.Join(s.root(), name, "config.toml")); err != nil {
		return "", false, err
	} else if exists {
		return name, true, nil
	}
	s.dirReads.Add(1)
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

// removeCiphertextFiles deletes every encrypted generation stored under a
// profile directory, plus the directory itself. Only callers that have already
// established the bytes are unreadable (no catalog, no keyring) use this.
func (s *Store) removeCiphertextFiles(name string) error {
	dir := filepath.Join(s.root(), name)
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".enc") && !strings.HasSuffix(entry.Name(), ".adk")) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	_ = os.Remove(dir)
	return nil
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

/* Sharing a profile happens between two people, so it cannot use the store's own
 * key: the recipient has a different one. Each store derives a stable X25519
 * identity from its Main Key, and a profile is sealed to the recipient's public
 * half with an ephemeral exchange. Nothing new is stored, nothing new can be
 * lost: a store with a loaded key can receive, one with nothing loaded cannot.
 */
const shareIdentityInfo = "termcp/hostshare/identity/v1"

/* SharePublicKey returns the base64 public half of ONE key's sharing identity.
 *
 * It used to be the store's identity, derived from whichever key seals the index,
 * and that was the wrong unit: a keychain with ten keys answered to one address, so
 * whoever sent a host could not say which of them should hold it, and the receiving
 * side had to guess. A key is the thing that holds hosts, so a key is the thing that
 * has an address. Handing one out is how someone else sends you a host it will
 * protect — and it is public by design, useless to anyone who does not already have
 * the blob.
 */
func (s *Store) SharePublicKey(keyID configstore.ID) (string, error) {
	secret, err := s.shareSecretForKey(keyID)
	if err != nil {
		return "", err
	}
	defer clear(secret[:])
	return hostshare.PublicKey(secret), nil
}

// shareSecretForKey derives the X25519 secret of one key. It requires that key to
// be inserted, which is the same rule the rest of the panel follows: an address you
// cannot answer at is worse than no address.
func (s *Store) shareSecretForKey(keyID configstore.ID) ([32]byte, error) {
	var out [32]byte
	repo := s.repositorySnapshot()
	if repo == nil {
		return out, configstore.ErrNotInitialized
	}
	return repo.DeriveSubkeyFor(keyID, shareIdentityInfo)
}

// shareSecretLegacy is the pre-existing store-wide identity. Blobs sealed against
// it are still in chat windows, so import keeps answering them; nothing new is
// ever sealed with it.
func (s *Store) shareSecretLegacy() ([32]byte, error) {
	var out [32]byte
	repo := s.repositorySnapshot()
	if repo == nil {
		return out, configstore.ErrNotInitialized
	}
	return repo.DeriveSubkey(shareIdentityInfo)
}

// ExportSharedProfile seals one profile's plaintext to recipientPublicKey and
// returns the chat-sized blob. The profile keeps its own name as a suggestion;
// the recipient decides where it lands.
func (s *Store) ExportSharedProfile(name, recipientPublicKey string) (string, error) {
	recipient, err := hostshare.ParsePublicKey(recipientPublicKey)
	if err != nil {
		return "", err
	}
	data, err := s.ReadRaw(name)
	if err != nil {
		return "", err
	}
	defer clear(data)
	/* The sender's own address in the card is a label, and the honest one is the key
	   that seals the profile being sent: a person with several keys who sends from
	   different ones should be able to tell the two apart in the reply. */
	secret, err := s.senderIdentity(name)
	if err != nil {
		return "", err
	}
	defer clear(secret[:])
	return hostshare.Seal(recipient, name, data, hostshare.PublicKey(secret))
}

// senderIdentity derives the sharing identity of the key that seals name, falling
// back to the store-wide identity for a plaintext profile, which no key seals.
func (s *Store) senderIdentity(name string) ([32]byte, error) {
	status, err := s.EncryptionStatus()
	if err == nil {
		for _, host := range status.Hosts {
			if host.Name != name || host.KeyID == "" {
				continue
			}
			var keyID configstore.ID
			if err := keyID.UnmarshalText([]byte(host.KeyID)); err == nil {
				if secret, err := s.shareSecretForKey(keyID); err == nil {
					return secret, nil
				}
			}
			break
		}
	}
	return s.shareSecretLegacy()
}

/* openingShareKey finds the key a blob was addressed to by trying each key this
 * store is currently holding. The legacy store-wide identity is tried last, so a
 * blob shared before keys had addresses still imports.
 */
func (s *Store) openingShareKey(blob string) (configstore.ID, [32]byte, error) {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ID{}, [32]byte{}, configstore.ErrNotInitialized
	}
	keys, err := repo.Keys()
	if err != nil {
		return configstore.ID{}, [32]byte{}, err
	}
	for _, key := range keys {
		if !key.Plugged {
			/* A key that is not loaded cannot answer at this address, which is the
			   same rule the panel shows. Skipping it is not a limitation: the sender
			   was given the address of a key, and that key has to be there. */
			continue
		}
		var keyID configstore.ID
		if err := keyID.UnmarshalText([]byte(key.ID)); err != nil {
			continue
		}
		secret, err := s.shareSecretForKey(keyID)
		if err != nil {
			continue
		}
		if _, _, err := hostshare.Open(secret, blob); err == nil {
			return keyID, secret, nil
		}
		clear(secret[:])
	}
	legacy, err := s.shareSecretLegacy()
	if err != nil {
		return configstore.ID{}, [32]byte{}, hostshare.ErrDecrypt
	}
	if _, _, err := hostshare.Open(legacy, blob); err == nil {
		return configstore.ID{}, legacy, nil
	}
	clear(legacy[:])
	return configstore.ID{}, [32]byte{}, hostshare.ErrDecrypt
}

/* ImportSharedProfile opens a blob and saves it under the name the sender used,
 * suffixed if that name is taken. Importing is deliberately plain: the profile is
 * validated like any other paste, and the caller decides whether to keep it.
 *
 * Which key opens it is not a question the user is asked. The blob names no key —
 * it was sealed to one public key — so the store tries the keys it is holding and
 * the one that opens it is the one that seals the result. That is the whole point
 * of an address per key: the sender already chose, by choosing which address to
 * write to.
 */
func (s *Store) ImportSharedProfile(blob string) (string, error) {
	keyID, secret, err := s.openingShareKey(blob)
	if err != nil {
		return "", err
	}
	defer clear(secret[:])
	name, data, err := hostshare.Open(secret, blob)
	if err != nil {
		return "", err
	}
	defer clear(data)
	if _, err := ParseAndValidate(data); err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	if err := ValidateName(name); err != nil || IsInternalName(name) {
		return "", fmt.Errorf("%w: the shared profile name %q is not usable here", ErrInvalid, name)
	}
	for i := 0; i < 50; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", name, i+1)
		}
		if err := s.checkNameCollision(candidate, ""); err != nil {
			continue
		}
		/* Sealed by the key that opened it, not by a store preference: which key
		   protects a host is a property of that host, and this one arrived addressed
		   to a particular key. */
		if err := s.SaveWithMode(candidate, data, SaveMode{KeyID: keyID}); err != nil {
			return "", err
		}
		return candidate, nil
	}
	return "", fmt.Errorf("%w: too many profiles named %q", ErrInvalid, name)
}
