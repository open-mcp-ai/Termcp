package configstore

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	repositoryVersion = uint16(1)
	maxCatalogEntries = 10_000
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
)

var (
	catalogObjectID  = repeatedID(0xca)
	internalObjectID = repeatedID(0xcb)
)

type catalogDocument struct {
	Version  uint16           `json:"version"`
	StoreID  []byte           `json:"store_id"`
	Revision uint64           `json:"revision"`
	Profiles []catalogProfile `json:"profiles"`
}

type catalogProfile struct {
	Name         string `json:"name"`
	ObjectID     []byte `json:"object_id"`
	GenerationID []byte `json:"generation_id"`
}

type catalogRef struct {
	name         string
	objectID     ID
	generationID ID
}

// Item is one opaque profile body for an import transaction.
type Item struct {
	Name string
	Data []byte
}

// Status is the non-secret administrative state of a repository.
type Status struct {
	StoreID  ID
	Revision uint64
	Profiles int
	Closed   bool
}

// Repository owns one exclusive encrypted connection-profile directory. The
// catalog commits name-to-object mappings; profile bodies remain independent
// generations so one damaged item cannot silently replace another.
type Repository struct {
	dataDir string
	lock    *directoryLock
	keyring *Keyring

	mu                sync.Mutex
	catalogGeneration ID
	revision          uint64
	refs              map[string]catalogRef
	closed            bool
}

// InitializeRepository creates an empty encrypted repository. If key creation
// succeeded but catalog publication was interrupted, a retry with the same
// provider resumes at the missing catalog.
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

	keysPath := filepath.Join(root, "keys", "config.keys")
	catalogPath := repositoryCatalogPath(root)
	keysExist, err := pathExists(keysPath)
	if err != nil {
		return fail(err)
	}
	catalogExists, err := pathExists(catalogPath)
	if err != nil {
		return fail(err)
	}
	if catalogExists && !keysExist {
		return fail(fmt.Errorf("%w: catalog exists without its keyring", ErrAlreadyInitialized))
	}
	if !keysExist {
		storeID, err := NewID()
		if err != nil {
			return fail(err)
		}
		keyData, keyring, err := CreateKeyring(storeID, providers...)
		if err != nil {
			return fail(err)
		}
		if err := writeFileAtomic(keysPath, keyData); err != nil {
			keyring.Destroy()
			return fail(err)
		}
		repository := &Repository{
			dataDir: root,
			lock:    lock,
			keyring: keyring,
			refs:    make(map[string]catalogRef),
		}
		if err := repository.commitCatalog(catalogDocument{
			Version:  repositoryVersion,
			StoreID:  append([]byte(nil), storeID[:]...),
			Revision: 1,
			Profiles: []catalogProfile{},
		}); err != nil {
			keyring.Destroy()
			return fail(err)
		}
		repository.revision = 1
		return repository, nil
	}

	keyData, err := readBoundedFile(keysPath, maxKeyringFile)
	if err != nil {
		return fail(err)
	}
	keyring, err := UnlockKeyring(keyData, providers...)
	clear(keyData)
	if err != nil {
		return fail(err)
	}
	repository := &Repository{
		dataDir: root,
		lock:    lock,
		keyring: keyring,
		refs:    make(map[string]catalogRef),
	}
	if catalogExists {
		if err := repository.loadCatalog(); err != nil {
			keyring.Destroy()
			return fail(err)
		}
		return repository, nil
	}
	storeID := keyring.StoreID()
	if err := repository.commitCatalog(catalogDocument{
		Version:  repositoryVersion,
		StoreID:  append([]byte(nil), storeID[:]...),
		Revision: 1,
		Profiles: []catalogProfile{},
	}); err != nil {
		keyring.Destroy()
		return fail(err)
	}
	repository.revision = 1
	return repository, nil
}

// OpenRepository unlocks the existing keyring and verifies every committed
// profile before returning.
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
		return fail(ErrNotInitialized)
	}
	if err != nil {
		return fail(err)
	}
	keyring, err := UnlockKeyring(keyData, providers...)
	clear(keyData)
	if err != nil {
		return fail(err)
	}
	repository := &Repository{
		dataDir: root,
		lock:    lock,
		keyring: keyring,
		refs:    make(map[string]catalogRef),
	}
	if err := repository.loadCatalog(); err != nil {
		keyring.Destroy()
		return fail(err)
	}
	return repository, nil
}

// List returns committed names in lexical order.
func (r *Repository) List() ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Read returns a caller-owned plaintext copy for the committed name.
func (r *Repository) Read(name string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return nil, err
	}
	if err := validateCatalogName(name); err != nil {
		return nil, err
	}
	ref, ok := r.refs[name]
	if !ok {
		return nil, fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	return r.readObject(ref)
}

// Save creates a profile or commits a new generation under the existing stable
// object ID. It never substitutes an empty profile for a damaged one.
func (r *Repository) Save(name string, data []byte) error {
	return r.put([]Item{{Name: name, Data: data}}, true)
}

// Import commits profiles that must not overwrite an existing name.
func (r *Repository) Import(items []Item) error {
	return r.put(items, false)
}

func (r *Repository) put(items []Item, overwrite bool) error {
	if len(items) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return err
	}

	next := cloneRefs(r.refs)
	oldGenerations := make([]catalogRef, 0, len(items))
	seen := make(map[string]struct{}, len(items))
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
		if !exists {
			objectID := internalObjectID
			if !strings.EqualFold(item.Name, "internal") {
				var err error
				objectID, err = NewID()
				if err != nil {
					return err
				}
				if objectID == catalogObjectID || objectID == internalObjectID {
					return ErrFormat
				}
			}
			ref = catalogRef{name: item.Name, objectID: objectID}
		} else {
			oldGenerations = append(oldGenerations, ref)
		}
		generationID, err := NewID()
		if err != nil {
			return err
		}
		ref.generationID = generationID
		if err := r.writeObject(ref, item.Data); err != nil {
			return err
		}
		next[item.Name] = ref
	}

	nextCatalog := r.catalogDocument(next, r.revision+1)
	if err := r.commitCatalog(nextCatalog); err != nil {
		if errors.Is(err, ErrDurability) {
			r.refs = next
			r.revision++
		}
		return err
	}
	r.refs = next
	r.revision++
	for _, ref := range oldGenerations {
		_ = removeIfExists(r.objectPath(ref))
	}
	return nil
}

// Rename changes only the catalog name. The stable object and body generation
// stay byte-for-byte unchanged.
func (r *Repository) Rename(oldName, newName string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
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
	ref, ok := r.refs[oldName]
	if !ok {
		return fmt.Errorf("%q: %w", oldName, ErrNotFound)
	}
	if existing, collision := findRef(r.refs, newName); collision {
		return fmt.Errorf("%s: %w", existing.name, ErrExists)
	}
	next := cloneRefs(r.refs)
	delete(next, oldName)
	ref.name = newName
	next[newName] = ref
	if err := r.commitCatalog(r.catalogDocument(next, r.revision+1)); err != nil {
		if errors.Is(err, ErrDurability) {
			r.refs = next
			r.revision++
		}
		return err
	}
	r.refs = next
	r.revision++
	return nil
}

// Delete removes the catalog reference before cleaning its ciphertext.
func (r *Repository) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return err
	}
	ref, ok := r.refs[name]
	if !ok {
		return nil
	}
	next := cloneRefs(r.refs)
	delete(next, name)
	if err := r.commitCatalog(r.catalogDocument(next, r.revision+1)); err != nil {
		if errors.Is(err, ErrDurability) {
			r.refs = next
			r.revision++
		}
		return err
	}
	r.refs = next
	r.revision++
	_ = removeIfExists(r.objectPath(ref))
	return nil
}

// Status returns non-secret state for diagnostics.
func (r *Repository) Status() Status {
	if r == nil {
		return Status{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{
		StoreID:  r.keyring.StoreID(),
		Revision: r.revision,
		Profiles: len(r.refs),
		Closed:   r.closed,
	}
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
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.refs = nil
	keyring := r.keyring
	r.keyring = nil
	r.mu.Unlock()
	keyring.Destroy()
	return r.lock.Close()
}

func (r *Repository) ready() error {
	if r == nil || r.closed || r.keyring == nil {
		return ErrClosed
	}
	return nil
}

func (r *Repository) loadCatalog() error {
	raw, err := readBoundedFile(repositoryCatalogPath(r.dataDir), envelopeHeaderSize+MaxCatalogPlaintext)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotInitialized
	}
	if err != nil {
		return err
	}
	envelope, err := parseCatalogEnvelope(raw, r.keyring.StoreID(), catalogObjectID)
	if err != nil {
		return err
	}
	plaintext, err := r.keyring.Open(envelope)
	if err != nil {
		return err
	}
	defer clear(plaintext)
	catalog, refs, err := decodeCatalog(plaintext, r.keyring.StoreID())
	if err != nil {
		return err
	}
	for _, ref := range refs {
		data, err := r.readUnlocked(ref)
		if err != nil {
			return fmt.Errorf("verify profile %q: %w", ref.name, err)
		}
		clear(data)
	}
	r.catalogGeneration = envelope.identity.GenerationID
	r.revision = catalog.Revision
	r.refs = refs
	return nil
}

func (r *Repository) commitCatalog(catalog catalogDocument) error {
	if catalog.Version != repositoryVersion || len(catalog.StoreID) != len(ID{}) ||
		catalog.Revision == 0 || len(catalog.Profiles) > maxCatalogEntries {
		return ErrFormat
	}
	plaintext, err := json.Marshal(catalog)
	if err != nil {
		return fmt.Errorf("encode catalog: %w", err)
	}
	defer clear(plaintext)
	if len(plaintext) > MaxCatalogPlaintext {
		return ErrTooLarge
	}
	generationID, err := NewID()
	if err != nil {
		return err
	}
	identity := Identity{
		StoreID:      r.keyring.StoreID(),
		ObjectID:     catalogObjectID,
		GenerationID: generationID,
		Kind:         KindCatalog,
	}
	envelope, err := r.keyring.Seal(plaintext, identity)
	if err != nil {
		return err
	}
	path := repositoryCatalogPath(r.dataDir)
	durabilityErr := error(nil)
	if err := writeFileAtomic(path, envelope.Bytes()); err != nil {
		if !errors.Is(err, ErrDurability) {
			return err
		}
		durabilityErr = err
	}

	parsed, err := parseCatalogEnvelope(envelope.Bytes(), identity.StoreID, catalogObjectID)
	if err != nil {
		return err
	}
	check, err := r.keyring.Open(parsed)
	if err != nil {
		return err
	}
	defer clear(check)
	if !bytes.Equal(check, plaintext) {
		return ErrAuthentication
	}
	r.catalogGeneration = generationID
	return durabilityErr
}

func (r *Repository) writeObject(ref catalogRef, data []byte) error {
	identity := Identity{
		StoreID:      r.keyring.StoreID(),
		ObjectID:     ref.objectID,
		GenerationID: ref.generationID,
		Kind:         KindProfile,
	}
	envelope, err := r.keyring.Seal(data, identity)
	if err != nil {
		return err
	}
	path := r.objectPath(ref)
	if err := writeFileAtomic(path, envelope.Bytes()); err != nil {
		return err
	}
	parsed, err := Parse(envelope.Bytes(), identity)
	if err != nil {
		return err
	}
	check, err := r.keyring.Open(parsed)
	if err != nil {
		return err
	}
	defer clear(check)
	if !bytes.Equal(check, data) {
		return ErrAuthentication
	}
	return nil
}

func (r *Repository) readObject(ref catalogRef) ([]byte, error) {
	return r.readUnlocked(ref)
}

func (r *Repository) readUnlocked(ref catalogRef) ([]byte, error) {
	path := r.objectPath(ref)
	raw, err := readBoundedFile(path, envelopeHeaderSize+MaxProfilePlaintext+chacha20poly1305.Overhead)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: missing object %s", ErrNotFound, hex.EncodeToString(ref.objectID[:]))
	}
	if err != nil {
		return nil, err
	}
	identity := Identity{
		StoreID:      r.keyring.StoreID(),
		ObjectID:     ref.objectID,
		GenerationID: ref.generationID,
		Kind:         KindProfile,
	}
	envelope, err := Parse(raw, identity)
	if err != nil {
		return nil, err
	}
	return r.keyring.Open(envelope)
}

func (r *Repository) objectPath(ref catalogRef) string {
	return filepath.Join(
		r.dataDir,
		"ssh_configs",
		"objects",
		hex.EncodeToString(ref.objectID[:]),
		hex.EncodeToString(ref.generationID[:])+".enc",
	)
}

func (r *Repository) catalogDocument(refs map[string]catalogRef, revision uint64) catalogDocument {
	profiles := make([]catalogProfile, 0, len(refs))
	for _, ref := range refs {
		profiles = append(profiles, catalogProfile{
			Name:         ref.name,
			ObjectID:     append([]byte(nil), ref.objectID[:]...),
			GenerationID: append([]byte(nil), ref.generationID[:]...),
		})
	}
	sort.Slice(profiles, func(i, j int) bool {
		return strings.ToLower(profiles[i].Name) < strings.ToLower(profiles[j].Name)
	})
	storeID := r.keyring.StoreID()
	return catalogDocument{
		Version:  repositoryVersion,
		StoreID:  append([]byte(nil), storeID[:]...),
		Revision: revision,
		Profiles: profiles,
	}
}

func decodeCatalog(data []byte, storeID ID) (catalogDocument, map[string]catalogRef, error) {
	var catalog catalogDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return catalogDocument{}, nil, ErrFormat
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return catalogDocument{}, nil, ErrFormat
	}
	if catalog.Version != repositoryVersion || catalog.Revision == 0 ||
		len(catalog.StoreID) != len(ID{}) || !bytes.Equal(catalog.StoreID, storeID[:]) ||
		len(catalog.Profiles) > maxCatalogEntries {
		return catalogDocument{}, nil, ErrFormat
	}
	refs := make(map[string]catalogRef, len(catalog.Profiles))
	folded := make(map[string]string, len(catalog.Profiles))
	for _, profile := range catalog.Profiles {
		if err := validateCatalogName(profile.Name); err != nil ||
			len(profile.ObjectID) != len(ID{}) || len(profile.GenerationID) != len(ID{}) {
			return catalogDocument{}, nil, ErrFormat
		}
		var objectID, generationID ID
		copy(objectID[:], profile.ObjectID)
		copy(generationID[:], profile.GenerationID)
		if objectID.IsZero() || generationID.IsZero() || objectID == catalogObjectID ||
			(strings.EqualFold(profile.Name, "internal") && objectID != internalObjectID) ||
			(!strings.EqualFold(profile.Name, "internal") && objectID == internalObjectID) {
			return catalogDocument{}, nil, ErrFormat
		}
		nameKey := strings.ToLower(profile.Name)
		if existing, ok := folded[nameKey]; ok {
			return catalogDocument{}, nil, fmt.Errorf("%w: duplicate names %q and %q", ErrFormat, existing, profile.Name)
		}
		folded[nameKey] = profile.Name
		refs[profile.Name] = catalogRef{
			name:         profile.Name,
			objectID:     objectID,
			generationID: generationID,
		}
	}
	return catalog, refs, nil
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

func repositoryCatalogPath(dataDir string) string {
	return filepath.Join(dataDir, "ssh_configs", "store.enc")
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
