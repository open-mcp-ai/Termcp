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

/* EncryptionHost is one host's non-secret storage facts (§7.2):
 *
 *   name      the profile name
 *   mode      plaintext / encrypted / temporary / builtin / unknown
 *   key_id    which key seals it, when it is ciphertext
 *   activated whether its second layer is on right now
 *   passphrase whether activating it needs its key's passphrase
 *   orphaned  whether its key was deleted, which is permanent
 */
type EncryptionHost struct {
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	KeyID      string `json:"key_id,omitempty"`
	Activated  bool   `json:"activated"`
	Passphrase bool   `json:"passphrase"`
	Orphaned   bool   `json:"orphaned"`
}

// EncryptionStatus is the page's non-secret configuration-encryption state.
type EncryptionStatus struct {
	Initialized               bool             `json:"initialized"`
	EncryptNew                bool             `json:"encrypt_new"`
	Masked                    bool             `json:"masked"`
	LegacyEncryptionArtifacts bool             `json:"legacy_encryption_artifacts"`
	Hosts                     []EncryptionHost `json:"hosts"`
	// Keys counts the keychain and KeysPlugged the part of it in memory.
	Keys        int    `json:"keys"`
	KeysPlugged int    `json:"keys_plugged"`
	ErrorCode   string `json:"error_code,omitempty"`
	// SystemAvailable reports whether this OS offers a native credential store.
	SystemAvailable bool `json:"system_available"`
}

// EncryptionResult is one selected host's outcome.
type EncryptionResult struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
	Error string `json:"error,omitempty"`
}

// KeychainStatus is the keychain panel's whole view.
type KeychainStatus struct {
	Keys []configstore.KeyInfo `json:"keys"`
}

// KeychainStatus lists the keys, readable without any key being plugged in: the
// panel has to render the list before anyone can insert one.
func (s *Store) KeychainStatus() (KeychainStatus, error) {
	status := KeychainStatus{Keys: []configstore.KeyInfo{}}
	repo := s.repositorySnapshot()
	if repo == nil {
		return status, nil
	}
	keys, err := repo.Keys()
	if err != nil {
		if errors.Is(err, configstore.ErrNotInitialized) {
			return status, nil
		}
		return KeychainStatus{}, err
	}
	/* Each key's address goes out with the list. It is derived, not stored, and only
	   a plugged key has one — which is the same rule as everywhere else here: a key
	   that is not in memory cannot answer at its address, so the row shows why. */
	for i := range keys {
		if !keys[i].Plugged {
			continue
		}
		var id configstore.ID
		if err := id.UnmarshalText([]byte(keys[i].ID)); err != nil {
			continue
		}
		if public, err := s.SharePublicKey(id); err == nil {
			keys[i].PublicKey = public
		}
	}
	status.Keys = keys
	return status, nil
}

// AddUnlockTo gives one key another way to be plugged in: a machine provider such
// as a mounted KEK file. The slots all seal the same words, so this adds a path,
// never a second root.
func (s *Store) AddUnlockTo(id configstore.ID, provider configstore.Provider) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	if err := repo.AddUnlockTo(id, provider); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// RemoveUnlockFrom drops every slot of one kind from one key, refusing to remove
// the last one: an unwrappable key strands every host sealed by it.
func (s *Store) RemoveUnlockFrom(id configstore.ID, kind string) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	if err := repo.RemoveUnlockFrom(id, kind); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// SetNewProfileEncryption enables or disables encryption for newly created
// persistent profiles. Enabling creates a keychain on first use.
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

// Masked reports the global address/username masking switch (§10.2).
func (s *Store) Masked() bool {
	st := s.stateSnapshot()
	if st.repo != nil {
		return st.repo.Masked()
	}
	return st.masked
}

/* SetMasked records the global masking switch. It changes serialization only.
 *
 * The durable home for it is config.keys (§3.1), so a store with no keychain can
 * only keep it for this process. That is the honest answer: the switch hides
 * addresses on screen, and a plaintext store has no file to write it to that would
 * not be a second source of truth about encryption.
 */
func (s *Store) SetMasked(masked bool) error {
	st := s.stateSnapshot()
	if st.repo == nil {
		s.state.Store(&storeState{encryptNew: st.encryptNew, masked: masked})
		s.notifyChange()
		return nil
	}
	if err := st.repo.SetMasked(masked); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// EncryptionStatus reports which hosts currently use plaintext or encrypted
// storage, and for each encrypted one its two-layer state.
func (s *Store) EncryptionStatus() (EncryptionStatus, error) {
	status := EncryptionStatus{Hosts: []EncryptionHost{}, SystemAvailable: configstore.SystemAvailable()}
	st := s.stateSnapshot()
	status.Initialized = st.repo != nil
	status.EncryptNew = st.encryptNew
	/* A store with no keychain still has a mask: hiding addresses is a property of
	   what this page prints, not of whether encryption was enabled. Reading it only
	   from the repository made the switch look dead on a fresh install — the file
	   was written where one existed, and silently ignored where none did. */
	status.Masked = st.masked
	if st.repo != nil {
		repoStatus := st.repo.Status()
		status.KeysPlugged = repoStatus.LoadedKeys
		status.Masked = repoStatus.Masked
		status.ErrorCode = repoStatus.ErrorCode
		if keys, err := st.repo.Keys(); err == nil {
			status.Keys = len(keys)
		}
	}
	names, err := s.List()
	if err != nil {
		return EncryptionStatus{}, err
	}
	for _, name := range names {
		host := EncryptionHost{Name: name}
		mode, err := s.StorageMode(name)
		if err != nil {
			/* One unreadable profile must not take the panel down with it: the
			   operator opens this screen precisely to deal with such a profile, and
			   a failed status call leaves them with "the operation failed" and no
			   way to act. Report the name with an explicit mode instead. */
			host.Mode = "unknown"
			status.Hosts = append(status.Hosts, host)
			continue
		}
		host.Mode = mode
		if mode == StorageEncrypted && st.repo != nil {
			if state, err := st.repo.HostState(name); err == nil {
				host.KeyID = keyIDText(state.KeyID)
				host.Activated = state.Activated
				host.Passphrase = state.NeedsPassphrase
				host.Orphaned = state.Orphaned
			} else if errors.Is(err, configstore.ErrNotFound) {
				host.Orphaned = true
			}
		}
		status.Hosts = append(status.Hosts, host)
	}
	return status, nil
}

/* ---- the four keychain actions -------------------------------------------
 * Every one of them is one request, because a key is independent: inserting one
 * unwraps exactly that key, exporting one writes down exactly its words, and
 * removing one destroys exactly its own slots.
 */

// CreateKey adds a key to the keychain, creating the keychain first when
// encryption has never been enabled. secret is the mnemonic to adopt; empty means
// "generate one and return it once". passphrase, when non-empty, marks the key so
// its hosts need that passphrase to activate — the phrase itself is never stored.
func (s *Store) CreateKey(secret, passphrase string, providers ...configstore.Provider) (configstore.KeyInfo, string, error) {
	/* A key made without a human in the loop still needs a name; see
	   configstore.DefaultKeyLabel. The named entry point is what the panel and the
	   REST route use, and it takes the caller's word. */
	return s.CreateKeyWithLabel(secret, passphrase, configstore.DefaultKeyLabel, providers...)
}

// CreateKeyWithLabel is CreateKey with a human name for the new key. The name is
// required: an unnamed key is a row nobody can tell from the next one.
func (s *Store) CreateKeyWithLabel(secret, passphrase, label string, providers ...configstore.Provider) (configstore.KeyInfo, string, error) {
	if _, err := configstore.NormalizeKeyLabel(label); err != nil {
		return configstore.KeyInfo{}, "", err
	}
	generated := ""
	mnemonic := secret
	if strings.TrimSpace(mnemonic) == "" {
		phrase, err := configstore.NewMnemonic()
		if err != nil {
			return configstore.KeyInfo{}, "", err
		}
		mnemonic, generated = phrase, phrase
	} else if normalized, err := configstore.NormalizeMnemonic(mnemonic); err != nil {
		return configstore.KeyInfo{}, "", err
	} else {
		mnemonic = normalized
	}
	repo := s.repositorySnapshot()
	if repo == nil {
		/* The first key is also the first keychain: there is nothing to add it to,
		   so the repository is created around it. */
		storeID, err := configstore.NewID()
		if err != nil {
			return configstore.KeyInfo{}, "", err
		}
		_ = storeID
		created, err := configstore.InitializeRepositoryWithMnemonic(s.dataDir, mnemonic, passphrase, providers...)
		if err != nil {
			return configstore.KeyInfo{}, "", err
		}
		if err := created.SetEncryptNew(true); err != nil {
			_ = created.Close()
			return configstore.KeyInfo{}, "", err
		}
		s.setEncryption(created, true)
		s.notifyChange()
		info, err := firstKeyInfo(created)
		if err != nil {
			return configstore.KeyInfo{}, generated, err
		}
		/* InitializeRepositoryWithMnemonic names the key it creates after
		   configstore.DefaultKeyLabel, and the caller's name is applied here: the
		   keychain is created around a key, so "created, then named" is the shape
		   whether the person asked for it or not. */
		if label != configstore.DefaultKeyLabel {
			if err := created.SetKeyLabel(mustKeyID(info.ID), label); err != nil {
				return configstore.KeyInfo{}, generated, err
			}
			info, err = firstKeyInfo(created)
		}
		return info, generated, err
	}
	info, err := repo.CreateKeyWithLabel(mnemonic, passphrase, label, providers...)
	if err != nil {
		return configstore.KeyInfo{}, "", err
	}
	s.notifyChange()
	return info, generated, nil
}

// RotateKey replaces one key's recovery phrase. The new words are returned once,
// like a fresh key's, because that is what they are: the old ones no longer open
// anything.
func (s *Store) RotateKey(id configstore.ID, passphrase string, providers ...configstore.Provider) (configstore.KeyInfo, string, error) {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.KeyInfo{}, "", configstore.ErrNotInitialized
	}
	info, phrase, err := repo.RotateKey(id, passphrase, providers...)
	s.notifyChange()
	return info, phrase, err
}

/* ChangeKeyPassphrase rotates one key's second layer: same 24 words, same id, same
 * address, and every host it seals is re-sealed under the new passphrase. The hosts
 * are unreadable between the old passphrase and the new one, so both are supplied in
 * the same call and a wrong old one changes nothing. */
func (s *Store) ChangeKeyPassphrase(id configstore.ID, oldPassphrase, newPassphrase string) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	if err := repo.ChangePassphrase(id, oldPassphrase, newPassphrase); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// SetKeyLabel renames one key. A name is public and re-seals nothing, so this is
// the one action on a key that destroys no ciphertext.
func (s *Store) SetKeyLabel(id configstore.ID, label string) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	if err := repo.SetKeyLabel(id, label); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

func mustKeyID(hexID string) configstore.ID {
	var id configstore.ID
	_ = id.UnmarshalText([]byte(hexID))
	return id
}

// firstKeyInfo returns the keychain's first key. With no index owner there is no
// special entry to look for: a fresh keychain holds exactly the one just created.
func firstKeyInfo(repo *configstore.Repository) (configstore.KeyInfo, error) {
	keys, err := repo.Keys()
	if err != nil {
		return configstore.KeyInfo{}, err
	}
	if len(keys) == 0 {
		return configstore.KeyInfo{}, configstore.ErrKeyNotFound
	}
	return keys[0], nil
}

// PlugKey inserts one key: it unwraps the mnemonic and derives the Main Key into
// memory, then records `plugged` so the next start does the same by itself.
/* PlugKeyWithPassphrase is the one-step "use this key": the words go into memory and
 * every host sealed by the key is activated with the passphrase, which is asked once
 * for both. See Repository.PlugKeyWithPassphrase for why the two are one action. */
func (s *Store) PlugKeyWithPassphrase(id configstore.ID, passphrase string, providers ...configstore.Provider) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	if err := repo.PlugKeyWithPassphrase(id, passphrase, providers...); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

func (s *Store) PlugKey(id configstore.ID, providers ...configstore.Provider) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	if err := repo.PlugKey(id, providers...); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// UnplugKey drops one key's material and every activation key it held. Nothing is
// deleted: plugging it back in restores everything.
func (s *Store) UnplugKey(id configstore.ID) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	if err := repo.UnplugKey(id); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// ExportMnemonic returns one key's 24 words. This machine already holds the slot,
// so it asks for nothing: exporting and using the key here are the same ability.
func (s *Store) ExportMnemonic(id configstore.ID) (string, error) {
	repo := s.repositorySnapshot()
	if repo == nil {
		return "", configstore.ErrNotInitialized
	}
	return repo.ExportMnemonic(id)
}

// DiscardDamagedKeychain is the human exit for a keychain file this build cannot
// read. It refuses while profiles are still sealed by it, but a damaged leftover
// with no ciphertext behind it must not dead-end the panel — otherwise "broken"
// is a state with no button.
func (s *Store) DiscardDamagedKeychain() error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	if err := repo.DiscardDamagedKeychain(); err != nil {
		return err
	}
	if repo.KeyringDetached() {
		if err := repo.Close(); err != nil {
			return err
		}
		s.setEncryption(nil, false)
	}
	s.notifyChange()
	return nil
}

// RemoveKey destroys one key. backupConfirmed is the human's statement that the
// words are written down; the key may seal no host and may not own the index.
func (s *Store) RemoveKey(id configstore.ID, backupConfirmed bool) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	if err := repo.RemoveKey(id, backupConfirmed); err != nil {
		return err
	}
	if repo.KeyringDetached() {
		// The keychain is gone; drop the repository so nothing calls into a store
		// whose keys were revoked.
		if err := repo.Close(); err != nil {
			return err
		}
		s.setEncryption(nil, false)
	}
	s.notifyChange()
	return nil
}

// Activate turns on the second layer for a selection of hosts, in one batch.
func (s *Store) Activate(names []string, passphrases map[configstore.ID]string) (map[string]error, error) {
	repo := s.repositorySnapshot()
	if repo == nil {
		return nil, configstore.ErrNotInitialized
	}
	names = cleanProfileSelection(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: select at least one host", ErrInvalid)
	}
	results, err := repo.Activate(names, passphrases)
	if err != nil {
		return nil, err
	}
	s.notifyChange()
	return results, nil
}

// Deactivate drops the second layer for a selection of hosts. No file changes: the
// body key is computed per read, so forgetting it is the whole operation.
func (s *Store) Deactivate(names []string) error {
	repo := s.repositorySnapshot()
	if repo == nil {
		return configstore.ErrNotInitialized
	}
	names = cleanProfileSelection(names)
	if len(names) == 0 {
		return fmt.Errorf("%w: select at least one host", ErrInvalid)
	}
	if err := repo.Deactivate(names); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// NewMnemonic exposes phrase generation to the panel: the phrase is key material,
// and a client-generated one would be a second implementation of the same rule.
func (s *Store) NewMnemonic() (string, error) { return configstore.NewMnemonic() }

func keyIDText(id configstore.ID) string {
	if id.IsZero() {
		return ""
	}
	return hex.EncodeToString(id[:])
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
	/* Ciphertext left behind by an earlier key: the catalog that described it went
	   with the key, so no entry answers for this name, but the bytes are still on
	   disk and still encrypted. Reporting "not found" made one such profile fail the
	   whole status call, which is how the encryption panel became unopenable. */
	if s.hasCiphertextOnDisk(name) {
		return StorageEncrypted, nil
	}
	return "", fmt.Errorf("ssh config %q %w", name, ErrNotFound)
}

// EncryptProfiles moves selected persistent plaintext profiles into the encrypted
// repository, sealed with keyID (empty means the index owner). passphrase is the
// key's passphrase, required when that key carries one: it is what writes each
// host's `.adk` (§8.2 step 2).
func (s *Store) EncryptProfiles(names []string, keyID configstore.ID, passphrase string) ([]EncryptionResult, error) {
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
	keys, err := repo.Keys()
	if err != nil {
		return nil, err
	}
	selected := configstore.KeyInfo{}
	if keyID.IsZero() {
		/* No implicit choice: the caller must say which key seals the selection,
		   because a host's key is a property of that host (§8.2). The one exception
		   is the single-key store, where there is nothing to choose between. */
		if len(keys) != 1 {
			return nil, configstore.ErrKeyRequired
		}
		selected = keys[0]
		copyID, err := parseKeyID(selected.ID)
		if err != nil {
			return nil, err
		}
		keyID = copyID
	} else {
		known := false
		for _, key := range keys {
			if key.ID == keyIDText(keyID) {
				known = true
				selected = key
			}
		}
		if !known {
			return nil, configstore.ErrKeyNotFound
		}
	}
	if !selected.Plugged {
		return nil, configstore.ErrKeyNotLoaded
	}
	if selected.Passphrase && passphrase == "" {
		return nil, fmt.Errorf("%w: key %s", configstore.ErrPassphraseRequired, keyIDText(keyID)[:8])
	}

	// Serialise all selected names so concurrent saves cannot race with this batch.
	unlock := s.lockNames(names...)
	defer unlock()

	results := make([]EncryptionResult, 0, len(names))
	items := make([]configstore.Item, 0, len(names))
	/* Re-keyed hosts are written one at a time, each replacing its own object: a
	   batch import creates objects, and these already have one. */
	var rekeys []string
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
		_, encrypted, err := s.encryptedFold(name)
		if err != nil {
			return nil, err
		}
		if encrypted {
			/* Already sealed. Two cases, and telling them apart matters: the same key
			   means nothing to do, a different key means "move it here". */
			state, err := repo.HostState(name)
			if err == nil && state.KeyID == keyID {
				results = append(results, EncryptionResult{Name: name, OK: true, Code: "already_encrypted"})
				continue
			}
			rekeys = append(rekeys, name)
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
	if len(items) == 0 && len(rekeys) == 0 {
		return results, nil
	}
	options := configstore.SaveOptions{KeyID: keyID, Passphrase: passphrase}
	if len(items) > 0 {
		if err := s.importCiphertexts(repo, items, options); err != nil {
			for _, item := range items {
				clear(item.Data)
			}
			return nil, err
		}
		for i, name := range sources {
			if err := removePlaintextProfile(s.plaintextPath(name)); err != nil {
				results = append(results, EncryptionResult{Name: name, Code: "plaintext_cleanup_failed", Error: err.Error()})
				continue
			}
			results = append(results, EncryptionResult{Name: items[i].Name, OK: true})
		}
	}
	for _, name := range rekeys {
		results = append(results, s.rekeyOne(repo, name, options))
	}
	for _, item := range items {
		clear(item.Data)
	}
	s.notifyChange()
	return results, nil
}

// RestoreProfiles writes selected encrypted profiles back to plaintext before
// removing their catalog references. It is the only path that turns ciphertext into
// plaintext, so it needs an active host: an INACTIVE or ORPHANED one cannot be read
// and therefore cannot be restored (§8.3).
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
		if errors.Is(err, configstore.ErrNotActivated) {
			results = append(results, EncryptionResult{Name: name, Code: "host_not_activated", Error: "activate the host before restoring it"})
			continue
		}
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

// importCiphertexts publishes a converted selection as one catalog commit.
func (s *Store) importCiphertexts(repo *configstore.Repository, items []configstore.Item, options configstore.SaveOptions) error {
	if s.importCiphertext != nil {
		return s.importCiphertext(repo, items, options)
	}
	return repo.ImportWithOptions(items, options)
}

/* rekeyOne moves a single sealed host to another key. The plaintext is read into
 * memory first, which is the whole point: it proves the ciphertext still opens
 * before the only surviving copy is replaced. Reading needs the old key inserted
 * AND the host activated — the two-layer rule is what makes this a deliberate
 * action rather than something that silently succeeds on a locked host.
 */
func (s *Store) rekeyOne(repo *configstore.Repository, name string, options configstore.SaveOptions) EncryptionResult {
	data, err := s.ReadRaw(name)
	if err != nil {
		code := "read_failed"
		if errors.Is(err, configstore.ErrNotActivated) || errors.Is(err, configstore.ErrPassphraseRequired) {
			code = "not_activated"
		} else if errors.Is(err, configstore.ErrKeyNotLoaded) {
			code = "key_not_loaded"
		}
		return EncryptionResult{Name: name, Code: code, Error: err.Error()}
	}
	defer clear(data)
	if _, err := ParseAndValidate(data); err != nil {
		return EncryptionResult{Name: name, Code: "invalid_profile", Error: err.Error()}
	}
	/* Write through the repository, not SaveWithMode: EncryptProfiles already holds
	   this name's token for the whole batch, and SaveWithMode would take it again
	   and deadlock on its own caller. Everything SaveWithMode would check has been
	   checked here already — the name is validated, the body is parsed, and the
	   object exists by construction. */
	if err := s.saveCiphertext(repo, name, data, SaveMode{KeyID: options.KeyID, Passphrase: options.Passphrase}); err != nil {
		return EncryptionResult{Name: name, Code: "rekey_failed", Error: err.Error()}
	}
	return EncryptionResult{Name: name, OK: true}
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

// SealedProfiles reports how many profiles a damaged keychain would strand.
func (s *Store) SealedProfiles() int {
	repo := s.repositorySnapshot()
	if repo == nil {
		return 0
	}
	return repo.SealedProfiles()
}

// parseKeyID reads the hex id the panel speaks back into a key id.
func parseKeyID(text string) (configstore.ID, error) {
	var id configstore.ID
	if err := id.UnmarshalText([]byte(text)); err != nil {
		return id, configstore.ErrKeyNotFound
	}
	return id, nil
}
