package configstore

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"
)

var (
	// ErrNotFound reports that no committed object has the requested name.
	ErrNotFound = errors.New("configstore: object not found")
	// ErrExists reports a case-insensitive name collision.
	ErrExists = errors.New("configstore: object already exists")
	// ErrNotInitialized reports that no encrypted keyring has been created.
	ErrNotInitialized = errors.New("configstore: encrypted store is not initialized")
	// ErrAlreadyInitialized reports inconsistent initialization state.
	ErrAlreadyInitialized = errors.New("configstore: encrypted store is already initialized")
	// ErrClosed reports use of a repository after Close.
	ErrClosed = errors.New("configstore: repository is closed")
	// ErrKeyringMissing reports a keychain file that is gone or unreadable while
	// encrypted hosts still depend on it. The data is there, nobody can read it.
	ErrKeyringMissing = errors.New("configstore: the keyring for the encrypted profiles is missing")
)

var internalObjectID = repeatedID(0xcb)

/*
Repository owns one exclusive encrypted connection-profile directory.

	There is no index. A host is a directory holding one generation, and the
	directory plus the public envelope header are the whole record of what is
	committed. Saving publishes a new generation file and removes the old one;
	deleting removes the directory; renaming renames it.

	The old `store.enc` index is read once by `migrateLegacyIndex` and then
	deleted: it was a second copy of facts the filesystem already publishes, and
	sealing it with one key made that key a gate on every other key's hosts.
*/
type Repository struct {
	dataDir string
	lock    *directoryLock

	// Mutable state lives in the published snapshot (see store_state.go).
	state    atomic.Pointer[repoSnapshot]
	writeTok chan struct{}
}

// ErrKeyRequired reports a save or conversion with no key named. A host's key is a
// per-host choice, so there is no default to fall back on.
var ErrKeyRequired = errors.New("configstore: choose the key that should seal this host")

// Status is the non-secret administrative state of a repository.
type Status struct {
	StoreID    ID
	Profiles   int
	EncryptNew bool
	Masked     bool
	Closed     bool
	// LoadedKeys counts the keys whose material is in memory. Zero means every
	// encrypted profile is unreadable right now, which the panel reports as
	// "nothing loaded" rather than as an error.
	LoadedKeys int
	// Loaded reports whether the host list can be enumerated. With no index this
	// needs no key at all: it is true whenever the keychain parsed.
	Loaded    bool
	ErrorCode string
}

// InitializeRepository creates an empty encrypted repository holding one newly
// generated key.
func InitializeRepository(dataDir string, providers ...Provider) (*Repository, error) {
	root, err := prepareDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	lock, err := acquireDirectoryLock(root)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Repository, error) {
		_ = lock.Close()
		return nil, err
	}
	keysExist, err := pathExists(filepath.Join(root, "keys", "config.keys"))
	if err != nil {
		return fail(err)
	}
	if keysExist {
		return openExisting(root, lock, fail, providers...)
	}
	storeID, err := NewID()
	if err != nil {
		return fail(err)
	}
	phrase, main, err := NewKeyMaterial(storeID)
	if err != nil {
		return fail(err)
	}
	clear(main[:])
	return initializeWithMnemonic(root, lock, storeID, phrase, "", fail, providers...)
}

// InitializeRepositoryWithMnemonic creates an empty encrypted repository whose
// first key is built from mnemonic. The phrase itself is never stored.
func InitializeRepositoryWithMnemonic(dataDir, mnemonic, passphrase string, providers ...Provider) (*Repository, error) {
	root, err := prepareDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	lock, err := acquireDirectoryLock(root)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Repository, error) {
		_ = lock.Close()
		return nil, err
	}
	keysExist, err := pathExists(filepath.Join(root, "keys", "config.keys"))
	if err != nil {
		return fail(err)
	}
	if keysExist {
		_ = lock.Close()
		return OpenRepository(root, providers...)
	}
	storeID, err := NewID()
	if err != nil {
		return fail(err)
	}
	return initializeWithMnemonic(root, lock, storeID, mnemonic, passphrase, fail, providers...)
}

func initializeWithMnemonic(root string, lock *directoryLock, storeID ID, phrase, passphrase string, fail func(error) (*Repository, error), providers ...Provider) (*Repository, error) {
	keyData, keyring, err := CreateKeyring(storeID, phrase, passphrase, providers...)
	if err != nil {
		return fail(err)
	}
	if err := writeFileAtomic(filepath.Join(root, "keys", "config.keys"), keyData); err != nil {
		keyring.Destroy()
		return fail(err)
	}
	repository := newRepository(root)
	repository.lock = lock
	repository.storeKeyring(keyring)
	if err := repository.loadRefs(); err != nil {
		keyring.Destroy()
		return fail(err)
	}
	return repository, nil
}

// openExisting finishes a repository that already has a keychain file.
func openExisting(root string, lock *directoryLock, fail func(error) (*Repository, error), providers ...Provider) (*Repository, error) {
	keyData, err := readBoundedFile(filepath.Join(root, "keys", "config.keys"), maxKeyringFile)
	if err != nil {
		return fail(err)
	}
	keyring, options, loadErr := LoadKeyringWithOptions(keyData, providers...)
	clear(keyData)
	if keyring == nil {
		if errors.Is(loadErr, ErrFormat) {
			return damagedRepository(root, lock), nil
		}
		return fail(loadErr)
	}
	repository := newRepository(root)
	repository.lock = lock
	repository.mutate(func(next *repoSnapshot) {
		next.keyring = keyring
		next.loadErr = loadErr
		next.encryptNew = options.EncryptNew
		next.masked = options.Masked
	})
	if err := repository.migrateLegacyIndex(); err != nil {
		keyring.Destroy()
		return fail(err)
	}
	if err := repository.loadRefs(); err != nil {
		keyring.Destroy()
		return fail(err)
	}
	return repository, nil
}

// damagedRepository opens with the fault recorded and every encrypted profile
// failing on access, so plaintext profiles and the panel still work.
func damagedRepository(root string, lock *directoryLock) *Repository {
	damaged := newRepository(root)
	damaged.lock = lock
	damaged.mutate(func(next *repoSnapshot) {
		next.loadErr = ErrKeyringMissing
		next.encryptNew = true
	})
	return damaged
}

/*
OpenRepository loads every key this host can unwrap by itself.

	A keychain whose keys need a human opens with nothing loaded instead of
	failing: hosts stay listed, each encrypted one reports which key it needs,
	and plugging that key in brings it back. Refusing to open would take every
	plaintext profile down with the encrypted ones.
*/
func OpenRepository(dataDir string, providers ...Provider) (*Repository, error) {
	root, err := prepareDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	lock, err := acquireDirectoryLock(root)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Repository, error) {
		_ = lock.Close()
		return nil, err
	}
	keyData, err := readBoundedFile(filepath.Join(root, "keys", "config.keys"), maxKeyringFile)
	if errors.Is(err, os.ErrNotExist) {
		/* Ciphertext with no keychain is damaged, not uninitialized: the hosts are
		   on disk and nothing can ever open them. Reporting "no encryption" here
		   would invite a new keychain over profiles that are still there. */
		if hasCiphertext(root) {
			return damagedRepository(root, lock), nil
		}
		return fail(ErrNotInitialized)
	}
	if err != nil {
		return fail(err)
	}
	keyring, options, loadErr := LoadKeyringWithOptions(keyData, providers...)
	clear(keyData)
	if keyring == nil {
		if errors.Is(loadErr, ErrFormat) {
			return damagedRepository(root, lock), nil
		}
		return fail(loadErr)
	}
	repository := newRepository(root)
	repository.lock = lock
	repository.mutate(func(next *repoSnapshot) {
		next.keyring = keyring
		next.loadErr = loadErr
		next.encryptNew = options.EncryptNew
		next.masked = options.Masked
	})
	if err := repository.migrateLegacyIndex(); err != nil {
		keyring.Destroy()
		return fail(err)
	}
	if err := repository.loadRefs(); err != nil {
		keyring.Destroy()
		return fail(err)
	}
	return repository, nil
}

// List returns committed names in lexical order, with no key required.
func (r *Repository) List() ([]string, error) {
	if err := r.usable(); err != nil {
		return nil, err
	}
	return r.listRefs()
}

// Read returns a caller-owned plaintext copy for the committed name.
func (r *Repository) Read(name string) ([]byte, error) {
	if err := r.usable(); err != nil {
		return nil, err
	}
	if err := validateCatalogName(name); err != nil {
		return nil, err
	}
	ref, err := r.currentRef(name)
	if err != nil {
		return nil, err
	}
	return r.readObject(ref)
}

// currentRef resolves one name against the disk view.
func (r *Repository) currentRef(name string) (catalogRef, error) {
	if err := r.loadRefs(); err != nil {
		return catalogRef{}, err
	}
	ref, ok := r.snap().refs[name]
	if !ok {
		return catalogRef{}, fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	return ref, nil
}

// Save creates a profile or commits a new generation under the existing stable
// object ID. The host is born ACTIVATED: a person is creating it right now.
func (r *Repository) Save(name string, data []byte) error {
	return r.put([]Item{{Name: name, Data: data}}, true, SaveOptions{})
}

// SaveWithKey seals the profile with one named Main Key, which must be plugged.
func (r *Repository) SaveWithKey(name string, data []byte, keyID ID) error {
	return r.put([]Item{{Name: name, Data: data}}, true, SaveOptions{KeyID: keyID})
}

// SaveWithOptions commits a profile with explicit per-save decisions.
func (r *Repository) SaveWithOptions(name string, data []byte, options SaveOptions) error {
	return r.put([]Item{{Name: name, Data: data}}, true, options)
}

// ImportWithOptions commits profiles that must not overwrite an existing name.
func (r *Repository) ImportWithOptions(items []Item, options SaveOptions) error {
	return r.put(items, false, options)
}

// Import commits profiles that must not overwrite an existing name.
func (r *Repository) Import(items []Item) error {
	return r.put(items, false, SaveOptions{})
}

// ImportWithKey commits profiles sealed with one named Main Key.
func (r *Repository) ImportWithKey(items []Item, keyID ID) error {
	return r.put(items, false, SaveOptions{KeyID: keyID})
}

// Item is one opaque profile body for an import transaction.
type Item struct {
	Name string
	Data []byte
}

// SaveOptions is one save request's per-host decisions, all of them optional.
type SaveOptions struct {
	// KeyID seals with that key. There is no default: a host's key is a choice.
	KeyID ID
	// Passphrase is the key's passphrase, present only when the caller is
	// creating or re-saving a host under a passphrase-bearing key. It never
	// reaches disk: it derives the activation key, which writes a fresh `.adk`.
	Passphrase string
	// ReuseActivation keeps the activation key this host already has in memory
	// instead of deriving a new one from Passphrase.
	ReuseActivation bool
}

/* put writes each host's new generation, then publishes it.
 *
 * There is no shared commit point any more: each host's directory is its own
 * transaction. A new generation is written and verified, then the old one is
 * removed. A crash between those two leaves two generations in that directory,
 * which the next open reports as ambiguous rather than silently choosing one.
 */
func (r *Repository) put(items []Item, overwrite bool, options SaveOptions) error {
	if len(items) == 0 {
		return nil
	}
	defer r.acquireWrite()()
	if err := r.usable(); err != nil {
		return err
	}
	if err := r.loadRefs(); err != nil {
		return err
	}
	cur := r.snap()
	next := cloneRefs(cur.refs)
	seen := make(map[string]struct{}, len(items))
	type pending struct {
		ref           catalogRef
		previous      catalogRef
		hadOld        bool
		keyID         ID
		activationKey [32]byte
		data          []byte
		record        activationRecord
		writeADK      bool
	}
	var batch []pending
	for _, item := range items {
		if err := validateCatalogName(item.Name); err != nil {
			return err
		}
		if len(item.Data) > MaxProfilePlaintext {
			return ErrTooLarge
		}
		folded := strings.ToLower(item.Name)
		if _, duplicate := seen[folded]; duplicate {
			return fmt.Errorf("%q: %w", item.Name, ErrExists)
		}
		seen[folded] = struct{}{}
		existing, collision := findRef(next, item.Name)
		if collision && (existing.name != item.Name || !overwrite) {
			return fmt.Errorf("%s: %w", existing.name, ErrExists)
		}
		ref, exists := next[item.Name]
		previous := ref
		if !exists {
			objectID := internalObjectID
			if !strings.EqualFold(item.Name, "internal") {
				var err error
				objectID, err = NewID()
				if err != nil {
					return err
				}
				if objectID == internalObjectID {
					return ErrFormat
				}
			}
			ref = catalogRef{name: item.Name, objectID: objectID}
		}
		generationID, err := NewID()
		if err != nil {
			return err
		}
		ref.generationID = generationID
		saveKeyID := options.KeyID
		if saveKeyID.IsZero() {
			/* No implicit choice: a host's key is a property of that host. The one
			   exception is a keychain with exactly one key in memory, where there is
			   nothing to choose between and demanding a name would be ceremony. */
			only, ok := cur.keyring.anySealableKey()
			if !ok || cur.keyring.keysCount() != 1 {
				return ErrKeyRequired
			}
			saveKeyID = only
		}
		ref.keyID = saveKeyID
		activationKey, activated, record, err := r.activationForSave(ref, saveKeyID, storedKeyForKey(r, saveKeyID), options)
		if err != nil {
			return err
		}
		ref.activated = activated
		batch = append(batch, pending{
			ref: ref, previous: previous, hadOld: exists, keyID: saveKeyID,
			activationKey: activationKey, data: item.Data, record: record,
			writeADK: activated && options.Passphrase != "",
		})
	}
	for i := range batch {
		entry := &batch[i]
		if err := r.commitGeneration(entry.ref, entry.data, entry.activationKey); err != nil {
			return err
		}
		if entry.writeADK {
			if err := r.writeActivation(entry.ref, entry.record, entry.activationKey); err != nil {
				return err
			}
		}
		if entry.ref.activated && entry.activationKey == noActivationKey {
			if err := r.writeActiveMarker(entry.ref); err != nil {
				return err
			}
		}
		if entry.hadOld {
			r.removeObject(entry.previous)
		}
		if entry.ref.activated && entry.activationKey != noActivationKey {
			cur.keyring.withActivationKey(entry.keyID, entry.ref.objectID, entry.activationKey)
		}
		next[entry.ref.name] = entry.ref
	}
	r.mutate(func(s *repoSnapshot) { s.refs = next })
	return nil
}

// Rename moves the whole host directory; the ciphertext is untouched.
func (r *Repository) Rename(oldName, newName string) error {
	defer r.acquireWrite()()
	if err := r.usable(); err != nil {
		return err
	}
	if oldName == newName {
		return nil
	}
	if err := validateCatalogName(oldName); err != nil {
		return err
	}
	if err := validateCatalogName(newName); err != nil {
		return err
	}
	if err := r.loadRefs(); err != nil {
		return err
	}
	cur := r.snap()
	ref, ok := cur.refs[oldName]
	if !ok {
		return fmt.Errorf("%q: %w", oldName, ErrNotFound)
	}
	if existing, collision := findRef(cur.refs, newName); collision {
		return fmt.Errorf("%s: %w", existing.name, ErrExists)
	}
	oldDir, newDir := hostDir(r.dataDir, oldName), hostDir(r.dataDir, newName)
	if err := requireRealDirectory(oldDir); err != nil {
		return err
	}
	if exists, err := pathExists(newDir); err != nil {
		return err
	} else if exists {
		return ErrExists
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}
	if err := syncDir(hostsRoot(r.dataDir)); err != nil {
		return fmt.Errorf("%w: sync renamed profile directory: %v", ErrDurability, err)
	}
	next := cloneRefs(cur.refs)
	delete(next, oldName)
	newRef := ref
	newRef.name = newName
	next[newName] = newRef
	r.mutate(func(s *repoSnapshot) { s.refs = next })
	return nil
}

// Delete removes the host's directory and its activation state.
func (r *Repository) Delete(name string) error {
	defer r.acquireWrite()()
	if err := r.usable(); err != nil {
		return err
	}
	if err := r.loadRefs(); err != nil {
		return err
	}
	cur := r.snap()
	ref, ok := cur.refs[name]
	if !ok {
		return nil
	}
	r.removeObject(ref)
	_ = removeIfExists(r.activeMarkerPath(ref))
	_ = os.Remove(hostDir(r.dataDir, name))
	if err := syncDir(hostsRoot(r.dataDir)); err != nil {
		return err
	}
	next := cloneRefs(cur.refs)
	delete(next, name)
	r.mutate(func(s *repoSnapshot) { s.refs = next })
	cur.keyring.withoutActivationKeys(map[ID]struct{}{ref.objectID: {}})
	return nil
}

// Status returns non-secret state for diagnostics.
func (r *Repository) Status() Status {
	if r == nil {
		return Status{}
	}
	st := r.snap()
	status := Status{
		Profiles:   len(st.refs),
		EncryptNew: st.encryptNew,
		Masked:     st.masked,
		Closed:     st.closed,
		Loaded:     r.Loaded(),
	}
	if st.keyring != nil {
		status.StoreID = st.keyring.StoreID()
		status.LoadedKeys = st.keyring.keysCount()
	}
	status.ErrorCode = keyLoadErrorCode(st.loadErr)
	return status
}

// keyLoadErrorCode names why the keychain could not be used.
func keyLoadErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrKeyringMissing):
		return "keyring_missing"
	case errors.Is(err, ErrProviderUnavailable):
		return "key_provider_unavailable"
	case errors.Is(err, ErrAuthentication):
		return "keyring_authentication_failed"
	default:
		return "key_load_failed"
	}
}

/* KeyringMissing reports ciphertext whose keychain file is gone.
 *
 * It is the state the panel states as "the keychain cannot be read", and the one
 * where nothing that is encrypted can ever be opened again — as opposed to "no key
 * is plugged in", which the operator fixes by inserting one.
 */
func (r *Repository) KeyringMissing() bool {
	return errors.Is(r.snap().loadErr, ErrKeyringMissing)
}

// Loaded reports whether hosts can be listed. With no index this needs no key:
// names are directory names, and each host says which key it needs.
func (r *Repository) Loaded() bool {
	st := r.snap()
	return !st.closed && st.keyring != nil && st.loadErr == nil
}

// DeriveSubkey exposes a purpose-separated subkey of a plugged-in Main Key.
func (r *Repository) DeriveSubkey(info string) ([32]byte, error) {
	if err := r.usable(); err != nil {
		return [32]byte{}, err
	}
	st := r.snap()
	if st.keyring == nil {
		return [32]byte{}, ErrNotInitialized
	}
	return st.keyring.DeriveSubkey(info)
}

// DeriveSubkeyFor derives a purpose-separated subkey of ONE key.
func (r *Repository) DeriveSubkeyFor(keyID ID, info string) ([32]byte, error) {
	if r == nil || r.snap().closed {
		return [32]byte{}, ErrClosed
	}
	st := r.snap()
	if st.keyring == nil {
		return [32]byte{}, ErrNotInitialized
	}
	return st.keyring.DeriveSubkeyFor(keyID, info)
}

// EncryptNew reports the preference for newly created profiles.
func (r *Repository) EncryptNew() bool {
	if r == nil {
		return false
	}
	return r.snap().encryptNew
}

// Masked reports the global address/username masking switch.
func (r *Repository) Masked() bool {
	if r == nil {
		return false
	}
	return r.snap().masked
}

// SetMasked records the global masking switch. Display layer only.
func (r *Repository) SetMasked(masked bool) error {
	defer r.acquireWrite()()
	if err := r.usable(); err != nil {
		return err
	}
	if r.snap().masked == masked {
		return nil
	}
	stored, err := r.readKeyringFile()
	if err != nil {
		return err
	}
	stored.Masked = masked
	if err := r.writeKeyringFile(stored); err != nil {
		return err
	}
	r.mutate(func(next *repoSnapshot) { next.masked = masked })
	return nil
}

// SetEncryptNew records whether new profiles are sealed by default. It works
// with every key unloaded, which is when someone decides where new hosts go.
func (r *Repository) SetEncryptNew(enabled bool) error {
	defer r.acquireWrite()()
	if err := r.usable(); err != nil {
		return err
	}
	cur := r.snap()
	if cur.encryptNew == enabled {
		return nil
	}
	stored, err := r.readKeyringFile()
	if err != nil {
		return err
	}
	stored.EncryptNew = &enabled
	if err := r.writeKeyringFile(stored); err != nil {
		return err
	}
	r.mutate(func(next *repoSnapshot) { next.encryptNew = enabled })
	return nil
}

// DataDir returns the resolved directory owned by this repository.
func (r *Repository) DataDir() string {
	if r == nil {
		return ""
	}
	return r.dataDir
}

// Close clears Main Keys in this process and releases the directory lock.
func (r *Repository) Close() error {
	if r == nil {
		return nil
	}
	defer r.acquireWrite()()
	cur := r.snap()
	if cur.closed {
		return nil
	}
	r.mutate(func(next *repoSnapshot) {
		*next = repoSnapshot{closed: true}
	})
	if cur.keyring != nil {
		cur.keyring.Destroy()
	}
	return r.lock.Close()
}

// usable reports whether the repository can serve reads and writes. It needs a
// parsed keychain, not a plugged key: which key each host needs is a fact about
// that host, and the panel has to be able to show it with nothing plugged in.
func (r *Repository) usable() error {
	st := r.snap()
	if st.closed {
		return ErrClosed
	}
	if st.keyring == nil && st.loadErr == nil {
		return ErrNotInitialized
	}
	if st.loadErr != nil {
		return ErrKeyNotLoaded
	}
	return nil
}

func (r *Repository) writeObject(ref catalogRef, data []byte, keyID ID, activationKey [32]byte) error {
	keyring := r.snap().keyring
	if keyring == nil {
		return ErrNotInitialized
	}
	identity := Identity{
		StoreID:      keyring.StoreID(),
		ObjectID:     ref.objectID,
		GenerationID: ref.generationID,
		Kind:         KindProfile,
	}
	envelope, err := keyring.SealWith(data, identity, keyID, activationKey)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(r.objectPath(ref), envelope.Bytes()); err != nil {
		return err
	}
	parsed, err := Parse(envelope.Bytes(), identity)
	if err != nil {
		return err
	}
	check, err := keyring.Open(parsed, activationKey)
	if err != nil {
		return err
	}
	defer clear(check)
	if !bytes.Equal(check, data) {
		return ErrAuthentication
	}
	return nil
}

/* commitGeneration publishes one host's generation with no shared index.
 *
 * Write the new file, verify it reads back, and only then is it committed. The
 * caller removes the previous generation afterwards, so a failure here leaves
 * the old generation intact rather than a half-written host.
 */
func (r *Repository) commitGeneration(ref catalogRef, data []byte, activationKey [32]byte) error {
	return r.writeObject(ref, data, ref.keyID, activationKey)
}

func (r *Repository) readObject(ref catalogRef) ([]byte, error) {
	raw, err := r.readObjectRaw(ref)
	if err != nil {
		return nil, err
	}
	return r.openObjectRaw(ref, raw)
}

func (r *Repository) readObjectRaw(ref catalogRef) ([]byte, error) {
	max := int64(envelopeHeaderSize + MaxProfilePlaintext + chacha20poly1305.Overhead)
	raw, err := readBoundedFile(r.objectPath(ref), max)
	if err == nil {
		return raw, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	raw, err = readBoundedFile(r.legacyObjectPath(ref), max)
	if err == nil {
		return raw, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return nil, fmt.Errorf("%w: missing object %s", ErrNotFound, hex.EncodeToString(ref.objectID[:]))
}

func (r *Repository) openObjectRaw(ref catalogRef, raw []byte) ([]byte, error) {
	keyring := r.snap().keyring
	if keyring == nil {
		return nil, ErrNotInitialized
	}
	identity := Identity{
		StoreID:      keyring.StoreID(),
		ObjectID:     ref.objectID,
		GenerationID: ref.generationID,
		Kind:         KindProfile,
	}
	envelope, err := Parse(raw, identity)
	if err != nil {
		return nil, err
	}
	activationKey, err := r.activationKeyFor(ref, envelope, ref.activated)
	if err != nil {
		return nil, err
	}
	return keyring.Open(envelope, activationKey)
}

/*
activationKeyFor resolves a host's second layer before any body decryption.

  - the key that sealed it is not loaded -> ErrKeyNotLoaded
  - the key carries a passphrase and its activation key is not in memory
    -> ErrNotActivated, without deriving anything
  - otherwise the activation key, or the all-zero stand-in, in which case the
    per-host `active` marker is the gate.
*/
func (r *Repository) activationKeyFor(ref catalogRef, envelope *Envelope, activated bool) ([32]byte, error) {
	keyring := r.snap().keyring
	if _, plugged := keyring.key(envelope.keyID); !plugged {
		/* "Not plugged in" and "this key was deleted" are different answers, and
		   the keychain file is what tells them apart: the first is fixed by
		   inserting the key, the second never is. */
		if stored, err := r.readKeyringFile(); err == nil {
			if _, known := storedKeyByID(stored, envelope.keyID); !known {
				return noActivationKey, ErrOrphaned
			}
		}
		return noActivationKey, ErrKeyNotLoaded
	}
	if key, ok := keyring.activationKey(envelope.keyID, envelope.identity.ObjectID); ok {
		return key, nil
	}
	stored, err := r.readKeyringFile()
	if err != nil {
		return noActivationKey, err
	}
	if key, ok := storedKeyByID(stored, envelope.keyID); ok && keyHasPassphrase(key) {
		return noActivationKey, ErrNotActivated
	}
	if !activated {
		return noActivationKey, ErrNotActivated
	}
	return noActivationKey, nil
}

/* removeObject clears one generation: its ciphertext and the `.adk` that belongs
 * to it. They are one unit — an activation record for a body that is gone can only
 * confuse the next generation.
 *
 * The `active` marker is NOT removed here. It is a fact about the host, and a save
 * writes the new generation's marker before the previous generation is cleaned up;
 * deleting it here would erase the state the save just recorded. Host deletion
 * removes it explicitly.
 */
func (r *Repository) removeObject(ref catalogRef) {
	_ = removeIfExists(r.activationPath(ref))
	for _, path := range []string{r.objectPath(ref), r.legacyObjectPath(ref)} {
		_ = removeIfExists(path)
		_ = os.Remove(filepath.Dir(path))
	}
}

func cloneRefs(refs map[string]catalogRef) map[string]catalogRef {
	out := make(map[string]catalogRef, len(refs))
	for name, ref := range refs {
		out[name] = ref
	}
	return out
}

func findRef(refs map[string]catalogRef, name string) (catalogRef, bool) {
	for existing, ref := range refs {
		if strings.EqualFold(existing, name) {
			return ref, true
		}
	}
	return catalogRef{}, false
}

func validateCatalogName(name string) error {
	if name == "" || name != strings.TrimSpace(name) || len(name) > 64 {
		return ErrFormat
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return ErrFormat
	}
	return nil
}

func prepareDataDir(dataDir string) (string, error) {
	clean := filepath.Clean(strings.TrimSpace(dataDir))
	if clean == "" {
		clean = "."
	}
	absolute, err := filepath.Abs(clean)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(absolute, privateDirMode); err != nil {
		return "", fmt.Errorf("create data directory: %w", err)
	}
	absolute, err = filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("resolve data directory: %w", err)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("data directory is not a directory")
	}
	return absolute, nil
}

func hasIncompatibleLayout(dataDir string) (bool, error) {
	for _, path := range []string{
		filepath.Join(dataDir, "keys", "master.key"),
		filepath.Join(dataDir, "config", "hosts.enc"),
	} {
		exists, err := pathExists(path)
		if err != nil || exists {
			return exists, err
		}
	}
	return false, nil
}

// HasLegacyEncryptionArtifacts reports files from the unreleased
// encryption-test branch. They are ignored by the current mixed store.
func HasLegacyEncryptionArtifacts(dataDir string) (bool, error) {
	root, err := filepath.Abs(filepath.Clean(dataDir))
	if err != nil {
		return false, err
	}
	root, err = filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return hasIncompatibleLayout(root)
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func readBoundedFile(path string, max int64) ([]byte, error) {
	if err := requireRealDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !linkInfo.Mode().IsRegular() {
		return nil, ErrFormat
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(linkInfo, info) {
		return nil, ErrFormat
	}
	if info.Size() > max {
		return nil, ErrTooLarge
	}
	data, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		clear(data)
		return nil, ErrTooLarge
	}
	return data, nil
}

func repeatedID(value byte) (id ID) {
	for i := range id {
		id[i] = value
	}
	return id
}

func randomBytes(n int) ([]byte, error) {
	id, err := NewID()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, id[:]...)
		if len(out) < n {
			id, err = NewID()
			if err != nil {
				return nil, err
			}
		}
	}
	return out[:n], nil
}

// sortedRefNames gives deterministic iteration for reports and tests.
func sortedRefNames(refs map[string]catalogRef) []string {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
