package configstore

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxLocalState = 16 << 20

// localState is the single commit point for key protection, detached EDKs and
// bootstrap authentication. Immutable profile bodies stay in host directories;
// their identity and generation remain authenticated independently of names.
type localState struct {
	Version           int               `json:"version"`
	Revision          uint64            `json:"revision"`
	ProfileIdentities map[string]string `json:"profile_identities,omitempty"`
	ProfilePaths      map[string]string `json:"profile_paths,omitempty"`
	Keyring           json.RawMessage   `json:"keyring,omitempty"`
	Login             json.RawMessage   `json:"login,omitempty"`
	Initializing      []byte            `json:"initializing,omitempty"`
	Heads             map[string]string `json:"heads"`
	Envelopes         map[string][]byte `json:"envelopes"`
	Legacy            []string          `json:"legacy_cleanup,omitempty"`
}

var localStateLocks sync.Map
var localStateCache sync.Map

type cachedLocalState struct {
	info  os.FileInfo
	state localState
}

func cloneLocalState(s localState) localState {
	s.ProfileIdentities = maps.Clone(s.ProfileIdentities)
	s.ProfilePaths = maps.Clone(s.ProfilePaths)
	s.Heads = maps.Clone(s.Heads)
	s.Envelopes = maps.Clone(s.Envelopes)
	s.Legacy = append([]string{}, s.Legacy...)
	return s
}

func canonicalStateRoot(root string) string {
	absolute, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return root
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		return resolved
	}
	return absolute
}

func stateLock(root string) *sync.Mutex {
	root = canonicalStateRoot(root)
	v, _ := localStateLocks.LoadOrStore(filepath.Clean(root), new(sync.Mutex))
	return v.(*sync.Mutex)
}
func LocalStatePath(root string) string {
	return filepath.Join(canonicalStateRoot(root), "local-state", "state.json")
}
func localObjects(root string) string {
	return filepath.Join(canonicalStateRoot(root), "local-state", "objects")
}
func stateDirectory(root string) string { return filepath.Dir(LocalStatePath(root)) }
func emptyLocalState() localState {
	return localState{Version: 1, Heads: map[string]string{}, Envelopes: map[string][]byte{}, ProfileIdentities: map[string]string{}, ProfilePaths: map[string]string{}}
}

func loadLocalState(root string) (localState, error) {
	path := LocalStatePath(root)
	if err := requireRealDirectory(filepath.Dir(path)); err != nil {
		return localState{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return localState{}, err
	}
	if !info.Mode().IsRegular() {
		return localState{}, ErrFormat
	}
	if value, ok := localStateCache.Load(path); ok {
		cached := value.(cachedLocalState)
		if os.SameFile(info, cached.info) && info.Size() == cached.info.Size() && info.ModTime().Equal(cached.info.ModTime()) {
			return cached.state, nil
		}
	}
	raw, err := readPhysicalFile(path, maxLocalState)
	if err != nil {
		return localState{}, err
	}
	var state localState
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&state); err != nil {
		return state, ErrFormat
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return state, ErrFormat
	}
	if state.Version != 1 || state.Revision == 0 || state.Heads == nil || state.Envelopes == nil || len(state.Envelopes) > maxCatalogEntries*2+16 || len(state.Login) > 8192 {
		return state, ErrFormat
	}
	if len(state.Keyring) > 0 {
		if _, err = parseStoredKeyring(state.Keyring); err != nil {
			return state, err
		}
	}
	if state.ProfileIdentities == nil {
		state.ProfileIdentities = map[string]string{}
	}
	if len(state.Login) > 0 && (len(state.Keyring) == 0 || state.Heads["authentication"] == "") {
		return state, ErrFormat
	}
	for object, id := range state.ProfileIdentities {
		raw, e := hex.DecodeString(object)
		if e != nil || len(raw) != 16 || hex.EncodeToString(raw) != object || !validProfileID(id) {
			return state, ErrFormat
		}
	}
	if len(state.Keyring) == 0 && len(state.Envelopes) > 0 {
		return state, ErrFormat
	}
	if len(state.Keyring) > 0 && state.Heads["catalog"] == "" && len(state.Initializing) != 16 {
		return state, ErrFormat
	}
	for key, header := range state.Envelopes {
		if len(header) != envelopeHeaderSize || localEnvelopeKey(header) != key {
			return state, ErrFormat
		}
	}
	if state.ProfilePaths == nil {
		state.ProfilePaths = map[string]string{}
	}
	seenPaths := map[string]bool{}
	for key, rel := range state.ProfilePaths {
		header, ok := state.Envelopes[key]
		if !ok || header[12] != byte(KindProfile) || !validHostBodyEntry(rel) ||
			strings.TrimSuffix(filepath.Base(rel), ".enc") != strings.Split(key, "/")[1] || seenPaths[rel] {
			return state, ErrFormat
		}
		seenPaths[rel] = true
	}
	for name, key := range state.Heads {
		if name != "catalog" && name != "authentication" {
			return state, ErrFormat
		}
		if _, ok := state.Envelopes[key]; !ok {
			return state, ErrFormat
		}
	}
	for _, p := range state.Legacy {
		if !validLegacyPath(p) {
			return state, ErrFormat
		}
	}
	localStateCache.Store(path, cachedLocalState{info: info, state: state})
	return state, nil
}

func publishLocalState(root string, state localState, publish func(string, []byte) error) error {
	state.Revision++
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > maxLocalState {
		return ErrTooLarge
	}
	if err := publish(LocalStatePath(root), raw); err != nil {
		return err
	}
	if err := syncDir(root); err != nil {
		return fmt.Errorf("%w: local state directory", ErrDurability)
	}
	return nil
}

func localEnvelopeKey(header []byte) string {
	if len(header) < envelopeHeaderSize {
		return ""
	}
	return hex.EncodeToString(header[29:45]) + "/" + hex.EncodeToString(header[45:61])
}

func localBodyPath(root, key string) (string, error) {
	parts := strings.Split(key, "/")
	if len(parts) != 2 {
		return "", ErrFormat
	}
	for _, p := range parts {
		b, err := hex.DecodeString(p)
		if err != nil || len(b) != 16 || hex.EncodeToString(b) != p {
			return "", ErrFormat
		}
	}
	return filepath.Join(localObjects(root), parts[0], parts[1]+".enc"), nil
}

// logicalStorePath recognizes only the repository's fixed metadata addresses
// and canonical object paths, including host-local profile generations.
// Other files use ordinary physical IO.
func logicalStorePath(path string) (root, entry string, ok bool) {
	clean := filepath.Clean(path)
	switch filepath.Base(clean) {
	case "auth.enc":
		return filepath.Dir(clean), "authentication", true
	case "config.keys", "config.initializing":
		if filepath.Base(filepath.Dir(clean)) == "keys" {
			return filepath.Dir(filepath.Dir(clean)), filepath.Base(clean), true
		}
	case "store.enc":
		if filepath.Base(filepath.Dir(clean)) == "ssh_configs" {
			return filepath.Dir(filepath.Dir(clean)), "catalog", true
		}
	}
	if parent := filepath.Dir(clean); filepath.Base(filepath.Dir(parent)) == "ssh_configs" {
		entry := "ssh_configs/" + filepath.Base(parent) + "/" + filepath.Base(clean)
		if validHostBodyEntry(entry) {
			return filepath.Dir(filepath.Dir(parent)), entry, true
		}
	}
	if strings.HasSuffix(clean, ".enc") {
		objectDir := filepath.Dir(clean)
		objectsDir := filepath.Dir(objectDir)
		if filepath.Base(objectsDir) == "objects" && filepath.Base(filepath.Dir(objectsDir)) == "local-state" {
			key := filepath.Base(objectDir) + "/" + strings.TrimSuffix(filepath.Base(clean), ".enc")
			root = filepath.Dir(filepath.Dir(objectsDir))
			if _, err := localBodyPath(root, key); err == nil {
				return root, key, true
			}
		}
	}
	return "", "", false
}

func readLocalEntry(root string, state localState, entry string, max int64) ([]byte, error) {
	switch entry {
	case "config.keys":
		if len(state.Keyring) == 0 {
			return nil, os.ErrNotExist
		}
		if int64(len(state.Keyring)) > max {
			return nil, ErrTooLarge
		}
		return bytes.Clone(state.Keyring), nil
	case "config.initializing":
		if len(state.Initializing) == 0 {
			return nil, os.ErrNotExist
		}
		return bytes.Clone(state.Initializing), nil
	case "catalog", "authentication":
		entry = state.Heads[entry]
	}
	var body []byte
	var err error
	if validHostBodyEntry(entry) {
		body, err = readPhysicalFile(filepath.Join(root, filepath.FromSlash(entry)), max)
		if err != nil {
			return nil, err
		}
		entry = bodyEnvelopeKey(body)
	}
	header, ok := state.Envelopes[entry]
	if !ok {
		return nil, os.ErrNotExist
	}
	if body == nil {
		path, err := bodyPathInState(root, state, entry)
		if err != nil {
			return nil, err
		}
		body, err = readPhysicalFile(path, max)
		if err != nil {
			return nil, err
		}
	}
	body, err = detachedBody(header, body)
	if err != nil {
		return nil, err
	}

	raw := append(bytes.Clone(header), body[dataHeaderSize:]...)
	if int64(len(raw)) > max {
		clear(raw)
		return nil, ErrTooLarge
	}
	return raw, nil
}

func readStoreFile(path string, max int64) ([]byte, error) {
	root, entry, ok := logicalStorePath(path)
	if !ok {
		return readPhysicalFile(path, max)
	}
	state, err := loadLocalState(root)
	if errors.Is(err, os.ErrNotExist) {
		return readPhysicalFile(path, max)
	}
	if err != nil {
		return nil, err
	}
	return readLocalEntry(root, state, entry, max)
}

func storeFileExists(path string) (bool, error) {
	root, entry, ok := logicalStorePath(path)
	if ok {
		state, err := loadLocalState(root)
		if err == nil {
			switch entry {
			case "config.keys":
				return len(state.Keyring) > 0, nil
			case "config.initializing":
				return len(state.Initializing) > 0, nil
			case "catalog", "authentication":
				return state.Heads[entry] != "", nil
			default:
				if validHostBodyEntry(entry) {
					raw, err := readPhysicalFile(path, int64(envelopeHeaderSize+MaxProfilePlaintext+16))
					if errors.Is(err, os.ErrNotExist) {
						return false, nil
					}
					if err != nil {
						return false, err
					}
					_, exists := state.Envelopes[bodyEnvelopeKey(raw)]
					return exists, nil
				}
				_, exists := state.Envelopes[entry]
				return exists, nil
			}
		}
		if !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
	}
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func applyLocalEntry(root string, state *localState, entry string, data []byte) error {
	switch entry {
	case "config.keys":
		stored, err := parseStoredKeyring(data)
		if err != nil {
			return err
		}
		if len(state.Keyring) == 0 && state.Heads["catalog"] == "" {
			state.Initializing = bytes.Clone(stored.StoreID)
		}
		state.Keyring = bytes.Clone(data)
		return nil
	case "config.initializing":
		if len(data) != 16 {
			return ErrFormat
		}
		state.Initializing = bytes.Clone(data)
		return nil
	}
	if len(data) < envelopeHeaderSize {
		return ErrFormat
	}
	key := localEnvelopeKey(data)
	path, err := bodyPathInState(root, *state, key)
	if err != nil {
		return err
	}
	if validHostBodyEntry(entry) {
		path = filepath.Join(root, filepath.FromSlash(entry))
	}
	var expected Identity
	copy(expected.StoreID[:], data[13:29])
	copy(expected.ObjectID[:], data[29:45])
	copy(expected.GenerationID[:], data[45:61])
	expected.Kind = ObjectKind(data[12])
	if _, err := Parse(data, expected); err != nil {
		return err
	}
	if entry == "catalog" && expected.Kind != KindCatalog || entry == "authentication" && expected.Kind != KindAuthentication {
		return ErrIdentityMismatch
	}
	if entry != "catalog" && entry != "authentication" && entry != key && !validHostBodyEntry(entry) {
		return ErrIdentityMismatch
	}
	if validHostBodyEntry(entry) {
		if expected.Kind != KindProfile || strings.TrimSuffix(filepath.Base(entry), ".enc") != strings.Split(key, "/")[1] {
			return ErrIdentityMismatch
		}
		state.ProfilePaths[key] = entry
	}
	body := append(bytes.Clone(data[:dataHeaderSize]), data[envelopeHeaderSize:]...)
	existing, err := readPhysicalFile(path, int64(envelopeHeaderSize+(8<<20)))
	if err == nil {
		normalized, e := detachedBody(data[:envelopeHeaderSize], existing)
		if e != nil || !bytes.Equal(normalized, body) {
			return ErrIdentityMismatch
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := writeFileAtomic(path, body); err != nil {
			return err
		}
		parent := localObjects(root)
		if validHostBodyEntry(entry) {
			parent = filepath.Join(root, "ssh_configs")
		}
		if err := syncDir(parent); err != nil {
			return fmt.Errorf("%w: object directory", ErrDurability)
		}
	} else {
		return err
	}
	state.Envelopes[key] = bytes.Clone(data[:envelopeHeaderSize])
	if entry == "catalog" || entry == "authentication" {
		old := state.Heads[entry]
		state.Heads[entry] = key
		if entry == "catalog" {
			state.Initializing = nil
		}
		if old != key {
			delete(state.Envelopes, old)
		}
	}
	return nil
}

func writeStoreFile(path string, data []byte) error {
	root, entry, ok := logicalStorePath(path)
	if !ok {
		return writeFileAtomic(path, data)
	}
	lock := stateLock(root)
	lock.Lock()
	defer lock.Unlock()
	state, err := loadLocalState(root)
	if errors.Is(err, os.ErrNotExist) {
		state = emptyLocalState()
	} else if err != nil {
		return err
	}
	state = cloneLocalState(state)
	oldHeads := maps.Clone(state.Heads)
	if err = applyLocalEntry(root, &state, entry, data); err != nil {
		return err
	}
	err = publishLocalState(root, state, writeFileAtomic)
	if err == nil {
		for head, key := range oldHeads {
			if key != state.Heads[head] {
				if path, e := localBodyPath(root, key); e == nil {
					_ = removeIfExists(path)
				}
			}
		}
	}
	return err
}

// publishEnvelopeBatch changes wrappers together. Bodies are checked for exact
// equality and never rewritten during MK rotation.
func (r *Repository) publishEnvelopeBatch(updates map[string][]byte, publish func(string, []byte) error) error {
	lock := stateLock(r.dataDir)
	lock.Lock()
	defer lock.Unlock()
	state, err := loadLocalState(r.dataDir)
	if err != nil {
		return err
	}
	state = cloneLocalState(state)
	for path, raw := range updates {
		_, entry, ok := logicalStorePath(path)
		if !ok {
			return ErrFormat
		}
		if err := applyLocalEntry(r.dataDir, &state, entry, raw); err != nil {
			return err
		}
	}
	// Fault injection belongs at the actual single-file publication boundary.
	return publishLocalState(r.dataDir, state, publish)
}

func removeStoreEntry(path string) error {
	root, entry, ok := logicalStorePath(path)
	if !ok {
		return removeIfExists(path)
	}
	lock := stateLock(root)
	lock.Lock()
	defer lock.Unlock()
	state, err := loadLocalState(root)
	if errors.Is(err, os.ErrNotExist) {
		return removeIfExists(path)
	}
	if err != nil {
		return err
	}
	state = cloneLocalState(state)
	if entry == "config.initializing" {
		state.Initializing = nil
	} else {
		if validHostBodyEntry(entry) {
			raw, e := readPhysicalFile(path, int64(envelopeHeaderSize+MaxProfilePlaintext+16))
			if e != nil {
				return e
			}
			entry = bodyEnvelopeKey(raw)
		}
		delete(state.Envelopes, entry)
		delete(state.ProfilePaths, entry)
	}
	return publishLocalState(root, state, writeFileAtomic)
}

func validLegacyPath(path string) bool {
	p := filepath.ToSlash(path)
	return !filepath.IsAbs(path) && p != ".." && !strings.HasPrefix(p, "../") && filepath.ToSlash(filepath.Clean(path)) == p &&
		(p == "keys/config.keys" || p == "keys/config.initializing" || p == "auth.enc" || strings.HasPrefix(p, "ssh_configs/") && strings.HasSuffix(p, ".enc"))
}

// migrateLocalState publishes a complete central index before old files can be
// removed. It also works while password-only vaults are locked; cryptographic
// verification and source cleanup happen after the vault has been unlocked.
func migrateLocalState(root string) error {
	lock := stateLock(root)
	lock.Lock()
	defer lock.Unlock()
	if _, err := loadLocalState(root); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	keyPath := filepath.Join(root, "keys", "config.keys")
	keys, err := readPhysicalFile(keyPath, maxKeyringFile)
	if errors.Is(err, os.ErrNotExist) {
		if present, e := hasProfileCiphertext(root); e != nil {
			return e
		} else if present {
			return ErrAlreadyInitialized
		}
		entries, e := os.ReadDir(localObjects(root))
		if e == nil && len(entries) > 0 {
			return ErrFormat
		}
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		return nil
	}
	if err != nil {
		return err
	}
	stored, err := parseStoredKeyring(keys)
	if err != nil {
		return err
	}
	state := emptyLocalState()
	state.Keyring = keys
	state.Legacy = append(state.Legacy, "keys/config.keys")
	if data, e := readPhysicalFile(filepath.Join(root, "keys", "config.initializing"), 16); e == nil {
		state.Initializing = data
		state.Legacy = append(state.Legacy, "keys/config.initializing")
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	if _, err := os.Lstat(repositoryCatalogPath(root)); errors.Is(err, os.ErrNotExist) && len(state.Initializing) != 16 {
		return ErrAlreadyInitialized
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	add := func(path, head string) error {
		raw, err := readPhysicalFile(path, int64(envelopeHeaderSize+(8<<20)))
		if err != nil {
			return err
		}
		if len(raw) < envelopeHeaderSize || !bytes.Equal(raw[13:29], stored.StoreID) {
			return ErrIdentityMismatch
		}
		if ObjectKind(raw[12]) == KindProfile {
			if id := legacyProfileID(root, filepath.Base(filepath.Dir(path))); id != "" {
				state.ProfileIdentities[hex.EncodeToString(raw[29:45])] = id
			}
		}
		entry := head
		if entry == "" {
			entry = localEnvelopeKey(raw)
			rel, e := filepath.Rel(root, path)
			if e == nil && ObjectKind(raw[12]) == KindProfile && validHostBodyEntry(filepath.ToSlash(rel)) {
				entry = filepath.ToSlash(rel)
			}
		}
		if err := applyLocalEntry(root, &state, entry, raw); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || !validLegacyPath(rel) {
			return ErrFormat
		}
		if !validHostBodyEntry(entry) {
			state.Legacy = append(state.Legacy, filepath.ToSlash(rel))
		}
		return nil
	}
	for _, item := range []struct{ path, head string }{{repositoryCatalogPath(root), "catalog"}, {authenticationPath(root), "authentication"}} {
		if err := add(item.path, item.head); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	profiles := filepath.Join(root, "ssh_configs")
	err = filepath.WalkDir(profiles, func(path string, entry os.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) && path == profiles {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return ErrFormat
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".enc") || path == repositoryCatalogPath(root) {
			return nil
		}
		return add(path, "")
	})
	if err != nil {
		return err
	}
	return publishLocalState(root, state, writeFileAtomic)
}

func cleanupLocalMigration(root string) {
	lock := stateLock(root)
	lock.Lock()
	defer lock.Unlock()
	state, err := loadLocalState(root)
	if err != nil || len(state.Legacy) == 0 {
		return
	}
	state = cloneLocalState(state)
	remaining := []string{}
	retainedHosts := map[string]bool{}
	for _, path := range state.ProfilePaths {
		retainedHosts[path] = true
	}
	for _, path := range state.Legacy {
		// Earlier central-layout migrations may have listed the same host path
		// as a cleanup source. It is now authoritative and must be retained.
		if retainedHosts[path] {
			continue
		}
		if err := removeIfExists(filepath.Join(root, filepath.FromSlash(path))); err != nil {
			remaining = append(remaining, path)
		}
	}
	state.Legacy = remaining
	_ = publishLocalState(root, state, writeFileAtomic)
}

// Login bootstrap is readable by the server while the data vault is locked.
// It contains only a password verifier and public account identity, never keys.
func (r *Repository) ReadLoginCredential() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrClosed
	}
	state, err := loadLocalState(r.dataDir)
	if err != nil {
		return nil, err
	}
	return bytes.Clone(state.Login), nil
}

func (r *Repository) WriteLoginCredential(expected, next []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	lock := stateLock(r.dataDir)
	lock.Lock()
	defer lock.Unlock()
	state, err := loadLocalState(r.dataDir)
	if err != nil {
		return err
	}
	state = cloneLocalState(state)
	if !bytes.Equal(state.Login, expected) {
		return ErrKeyringChanged
	}
	if len(next) > 8192 || !json.Valid(next) {
		return ErrFormat
	}
	state.Login = bytes.Clone(next)
	return publishLocalState(r.dataDir, state, writeFileAtomic)
}

func (r *Repository) WriteAuthenticationAndLogin(data, login []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.writable(); err != nil {
		return err
	}
	if len(login) > 8192 || !json.Valid(login) {
		return ErrFormat
	}
	generation, err := NewID()
	if err != nil {
		return err
	}
	envelope, err := r.keyring.Seal(data, Identity{StoreID: r.keyring.StoreID(), ObjectID: authenticationObjectID, GenerationID: generation, Kind: KindAuthentication})
	if err != nil {
		return err
	}
	lock := stateLock(r.dataDir)
	lock.Lock()
	defer lock.Unlock()
	state, err := loadLocalState(r.dataDir)
	if err != nil {
		return err
	}
	if len(state.Login) > 0 {
		return ErrAlreadyInitialized
	}
	state = cloneLocalState(state)
	if err = applyLocalEntry(r.dataDir, &state, "authentication", envelope.Bytes()); err != nil {
		return err
	}
	state.Login = bytes.Clone(login)
	err = publishLocalState(r.dataDir, state, writeFileAtomic)
	if err == nil || errors.Is(err, ErrDurability) {
		r.authGeneration = generation
	}
	return err
}

func localSyncDirectory(path string) string {
	root, _, ok := logicalStorePath(path)
	if ok {
		if _, err := os.Stat(LocalStatePath(root)); err == nil {
			return stateDirectory(root)
		}
	}
	return filepath.Dir(path)
}

func (r *Repository) centralObjectPath(ref catalogRef) string {
	path, _ := localBodyPath(r.dataDir, hex.EncodeToString(ref.objectID[:])+"/"+hex.EncodeToString(ref.generationID[:]))
	return path
}

func (r *Repository) centralStateExists() bool {
	_, err := os.Stat(LocalStatePath(r.dataDir))
	return err == nil
}
