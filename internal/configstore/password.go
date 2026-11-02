package configstore

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

const passwordKeyringVersion = uint16(2)

var (
	ErrPasswordRequired      = errors.New("configstore: enter the encryption password")
	ErrPasswordNotConfigured = errors.New("configstore: an encryption password has not been configured")
	ErrPasswordInvalid       = errors.New("configstore: password must contain at least 8 characters and at most 1024 UTF-8 bytes")
)

type storedPasswordDerivation struct {
	KeyID    []byte                 `json:"key_id"`
	Salt     []byte                 `json:"salt"`
	Argon2   Argon2Parameters       `json:"argon2id"`
	Nonce    []byte                 `json:"nonce"`
	Verifier []byte                 `json:"verifier"`
	Previous *storedPreviousMainKey `json:"previous,omitempty"`
}

type storedPreviousMainKey struct {
	KeyID   []byte `json:"key_id"`
	Nonce   []byte `json:"nonce"`
	Wrapped []byte `json:"wrapped"`
}

func validatePasswordDerivation(p *storedPasswordDerivation) error {
	if p == nil || len(p.KeyID) != 16 || isZeroBytes(p.KeyID) || len(p.Salt) != 16 ||
		len(p.Nonce) != chacha20poly1305.NonceSizeX || len(p.Verifier) != 32+chacha20poly1305.Overhead || p.Argon2.validate() != nil {
		return ErrFormat
	}
	if old := p.Previous; old != nil {
		if len(old.KeyID) != 16 || isZeroBytes(old.KeyID) || bytes.Equal(old.KeyID, p.KeyID) ||
			len(old.Nonce) != chacha20poly1305.NonceSizeX || len(old.Wrapped) != chacha20poly1305.KeySize+chacha20poly1305.Overhead {
			return ErrFormat
		}
	}
	return nil
}

func passwordAAD(store ID, p *storedPasswordDerivation) []byte {
	out := append([]byte("termcp/config/password/v1"), store[:]...)
	out = append(out, p.KeyID...)
	out = append(out, p.Salt...)
	out = binary.LittleEndian.AppendUint32(out, p.Argon2.Time)
	out = binary.LittleEndian.AppendUint32(out, p.Argon2.Memory)
	out = append(out, p.Argon2.Threads)
	return out
}

func deriveMain(password []byte, p *storedPasswordDerivation) (MainKey, error) {
	var main MainKey
	if len(password) == 0 || len(password) > 1024 || validatePasswordDerivation(p) != nil {
		return main, ErrAuthentication
	}
	key := argon2.IDKey(password, p.Salt, p.Argon2.Time, p.Argon2.Memory, p.Argon2.Threads, uint32(len(main)))
	copy(main[:], key)
	clear(key)
	return main, nil
}

type passwordDeriver func([]byte, *storedPasswordDerivation) (MainKey, error)

func newPasswordKeyring(store ID, password []byte) (*Keyring, error) {
	return newPasswordKeyringUsing(store, password, deriveMain)
}

func newPasswordKeyringUsing(store ID, password []byte, derive passwordDeriver) (*Keyring, error) {
	keyID, err := NewID()
	if err != nil {
		return nil, err
	}
	p := &storedPasswordDerivation{
		KeyID: append([]byte(nil), keyID[:]...), Salt: make([]byte, 16),
		Argon2: defaultArgon2Parameters(), Nonce: make([]byte, chacha20poly1305.NonceSizeX),
		Verifier: make([]byte, 32+chacha20poly1305.Overhead),
	}
	if _, err := io.ReadFull(rand.Reader, p.Salt); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rand.Reader, p.Nonce); err != nil {
		return nil, err
	}
	main, err := derive(password, p)
	if err != nil {
		return nil, err
	}
	defer clear(main[:])
	aead, err := chacha20poly1305.NewX(main[:])
	if err != nil {
		return nil, err
	}
	proof := append(append([]byte(nil), store[:]...), keyID[:]...)
	p.Verifier = aead.Seal(nil, p.Nonce, proof, passwordAAD(store, p))
	return &Keyring{storeID: store, activeKeyID: keyID, mainKeys: map[ID]MainKey{keyID: main}, password: p}, nil
}

func verifyDerivedKeyring(k *Keyring, p *storedPasswordDerivation) error {
	if validatePasswordDerivation(p) != nil {
		return ErrFormat
	}
	var active ID
	copy(active[:], p.KeyID)
	main, ok := k.mainKeys[active]
	if !ok || active != k.activeKeyID {
		return ErrAuthentication
	}
	aead, err := chacha20poly1305.NewX(main[:])
	clear(main[:])
	if err != nil {
		return err
	}
	proof, err := aead.Open(nil, p.Nonce, p.Verifier, passwordAAD(k.storeID, p))
	defer clear(proof)
	want := append(append([]byte(nil), k.storeID[:]...), active[:]...)
	if err != nil || !bytes.Equal(proof, want) {
		return ErrAuthentication
	}
	return nil
}

func unlockPasswordKeyring(stored storedKeyring, password []byte) (*Keyring, error) {
	return unlockPasswordKeyringUsing(stored, password, deriveMain)
}

func unlockPasswordKeyringUsing(stored storedKeyring, password []byte, derive passwordDeriver) (*Keyring, error) {
	if stored.Password == nil {
		return nil, ErrPasswordNotConfigured
	}
	main, err := derive(password, stored.Password)
	if err != nil {
		return nil, err
	}
	defer clear(main[:])
	var store, active ID
	copy(store[:], stored.StoreID)
	copy(active[:], stored.Password.KeyID)
	k := &Keyring{storeID: store, activeKeyID: active, mainKeys: map[ID]MainKey{active: main}, password: stored.Password}
	if err := verifyDerivedKeyring(k, stored.Password); err != nil {
		k.Destroy()
		return nil, err
	}
	if old := stored.Password.Previous; old != nil {
		aead, err := chacha20poly1305.NewX(main[:])
		if err != nil {
			k.Destroy()
			return nil, err
		}
		aad := append(passwordAAD(store, stored.Password), []byte("/previous/")...)
		aad = append(aad, old.KeyID...)
		data, err := aead.Open(nil, old.Nonce, old.Wrapped, aad)
		if err != nil {
			k.Destroy()
			return nil, ErrAuthentication
		}
		var oldID ID
		var oldMain MainKey
		copy(oldID[:], old.KeyID)
		copy(oldMain[:], data)
		clear(data)
		k.mainKeys[oldID] = oldMain
		clear(oldMain[:])
	}
	return k, nil
}

func completedPasswordDerivation(p *storedPasswordDerivation) *storedPasswordDerivation {
	if p == nil {
		return nil
	}
	copy := *p
	copy.Previous = nil
	return &copy
}

func validateNewPassword(password, confirmation string) error {
	if password != confirmation {
		return ErrPasswordConfirmation
	}
	if len(password) > 1024 || utf8.RuneCountInString(password) < 8 || strings.TrimSpace(password) == "" {
		return ErrPasswordInvalid
	}
	return nil
}

// ValidatePassword shares the master-password input constraints with login UI.
func ValidatePassword(password, confirmation string) error {
	return validateNewPassword(password, confirmation)
}

func InitializeRepositoryWithPassword(dataDir, password, confirmation string, providers ...Provider) (*Repository, error) {
	if err := validateNewPassword(password, confirmation); err != nil {
		return nil, err
	}
	root, err := prepareDataDir(dataDir)
	if err != nil {
		return nil, err
	}
	lock, err := acquireDirectoryLock(root)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Repository, error) { _ = lock.Close(); return nil, err }
	if err := migrateLocalState(root); err != nil {
		return fail(err)
	}
	keysPath := filepath.Join(root, "keys", "config.keys")
	initialPath := filepath.Join(root, "keys", "config.initializing")
	if exists, err := pathExists(repositoryCatalogPath(root)); err != nil {
		return fail(err)
	} else if exists {
		return fail(ErrAlreadyInitialized)
	}
	if exists, err := pathExists(keysPath); err != nil {
		return fail(err)
	} else if exists {
		// Only a recorded, incomplete initialization can create a missing
		// catalog. A lost catalog in an established vault must fail closed.
		marker, err := readBoundedFile(initialPath, 16)
		if err != nil {
			return fail(ErrAlreadyInitialized)
		}
		raw, err := readBoundedFile(keysPath, maxKeyringFile)
		if err != nil {
			return fail(err)
		}
		stored, err := parseStoredKeyring(raw)
		clear(raw)
		if err != nil {
			return fail(err)
		}
		if !bytes.Equal(marker, stored.StoreID) {
			return fail(ErrIdentityMismatch)
		}
		if exists, err := HasAuthentication(root); err != nil {
			return fail(err)
		} else if exists {
			return fail(ErrAlreadyInitialized)
		}
		if exists, err := hasProfileCiphertext(root); err != nil {
			return fail(err)
		} else if exists {
			return fail(ErrAlreadyInitialized)
		}
		secret := []byte(password)
		k, err := unlockPasswordKeyring(stored, secret)
		clear(secret)
		if err != nil {
			return fail(err)
		}
		r := &Repository{dataDir: root, lock: lock, keyring: k, refs: make(map[string]catalogRef), password: stored.Password, automaticUnlock: len(stored.Slots) > 0}
		if err := r.commitCatalog(catalogDocument{Version: repositoryVersion, StoreID: stored.StoreID, Revision: 1, Profiles: []catalogProfile{}}); err != nil {
			k.Destroy()
			return fail(err)
		}
		r.revision = 1
		_ = removeStoreEntry(initialPath)
		cleanupLocalMigration(root)
		return r, nil
	}
	store, err := NewID()
	if err != nil {
		return fail(err)
	}
	secret := []byte(password)
	defer clear(secret)
	k, err := newPasswordKeyring(store, secret)
	if err != nil {
		return fail(err)
	}
	data, err := k.Marshal(providers...)
	if err != nil {
		k.Destroy()
		return fail(err)
	}
	stored, err := parseStoredKeyring(data)
	if err != nil {
		k.Destroy()
		return fail(err)
	}
	if err := writeStoreFile(initialPath, store[:]); err != nil {
		k.Destroy()
		return fail(err)
	}
	if err := writeStoreFile(filepath.Join(root, "keys", "config.keys"), data); err != nil {
		k.Destroy()
		return fail(err)
	}
	r := &Repository{dataDir: root, lock: lock, keyring: k, refs: make(map[string]catalogRef), automaticUnlock: len(stored.Slots) > 0}
	if err := r.commitCatalog(catalogDocument{Version: repositoryVersion, StoreID: append([]byte(nil), store[:]...), Revision: 1, Profiles: []catalogProfile{}}); err != nil {
		k.Destroy()
		return fail(err)
	}
	r.revision = 1
	_ = removeStoreEntry(initialPath)
	return r, nil
}

// UnlockPassword derives Main Key directly from the password. It publishes no
// key material and remains usable without any operating-system or ENV KEK.
func (r *Repository) UnlockPassword(password string) error {
	r.passwordMu.Lock()
	defer r.passwordMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.unlockErr == nil {
		return ErrNotLocked
	}
	data, err := readBoundedFile(filepath.Join(r.dataDir, "keys", "config.keys"), maxKeyringFile)
	if err != nil {
		return err
	}
	defer clear(data)
	stored, err := parseStoredKeyring(data)
	if err != nil {
		return err
	}
	secret := []byte(password)
	defer clear(secret)
	derive := r.passwordDeriver()
	var candidate *Keyring
	err = r.passwordWorkLocked(data, func() error { var e error; candidate, e = unlockPasswordKeyringUsing(stored, secret, derive); return e })
	if err != nil {
		if candidate != nil {
			candidate.Destroy()
		}
		return err
	}
	if r.unlockErr == nil {
		candidate.Destroy()
		return ErrNotLocked
	}
	probe := &Repository{dataDir: r.dataDir, keyring: candidate}
	if err = probe.loadCatalog(); err != nil {
		candidate.Destroy()
		return err
	}
	if err = probe.loadAuthGeneration(); err != nil {
		candidate.Destroy()
		return err
	}
	r.keyring, r.unlockErr = candidate, nil
	r.refs, r.encryptNew, r.revision, r.catalogGeneration = probe.refs, probe.encryptNew, probe.revision, probe.catalogGeneration
	r.authGeneration = probe.authGeneration
	r.password = stored.Password
	r.automaticUnlock = false
	cleanupLocalMigration(r.dataDir)
	r.pruneLocalObjects()
	return nil
}

// availablePasswordProtection retains only auto-unlock slots that can be
// authenticated now. Password derivation is the independent recovery path.
func availablePasswordProtection(stored storedKeyring, expected *Keyring, providers []Provider) *keyringProtection {
	p := &keyringProtection{stored: stored}
	p.stored.Slots = nil
	for _, slot := range stored.Slots {
		for _, provider := range providers {
			if provider == nil || provider.providerName() != slot.Provider {
				continue
			}
			kek, err := provider.unwrapKey(slot.Parameters)
			if err != nil {
				clear(kek[:])
				continue
			}
			aead, err := chacha20poly1305.NewX(kek[:])
			if err != nil {
				clear(kek[:])
				continue
			}
			data, err := aead.Open(nil, slot.Nonce, slot.Wrapped, keyringSlotAAD(stored, slot))
			if err != nil {
				clear(kek[:])
				continue
			}
			unlocked, err := parseKeyringPayload(data, expected.StoreID())
			clear(data)
			valid := err == nil && sameKeyring(unlocked, expected)
			if unlocked != nil {
				unlocked.Destroy()
			}
			if valid {
				p.stored.Slots = append(p.stored.Slots, slot)
				p.keks = append(p.keks, kek)
				clear(kek[:])
				break
			}
			clear(kek[:])
		}
	}
	return p
}

func (r *Repository) ChangePassword(current, password, confirmation string, providers ...Provider) error {
	return r.changePassword(current, password, confirmation, providers, writeStoreFile)
}

func (r *Repository) changePassword(current, password, confirmation string, providers []Provider, publish func(string, []byte) error) error {
	r.passwordMu.Lock()
	defer r.passwordMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return err
	}
	path := filepath.Join(r.dataDir, "keys", "config.keys")
	data, err := readBoundedFile(path, maxKeyringFile)
	if err != nil {
		return err
	}
	defer clear(data)
	stored, err := parseStoredKeyring(data)
	if err != nil {
		return err
	}
	if (stored.Password == nil) != (r.keyring.password == nil) {
		return ErrAuthentication
	}
	if stored.Password != nil {
		secret := []byte(current)
		derive := r.passwordDeriver()
		var checked *Keyring
		err := r.passwordWorkLocked(data, func() error { var e error; checked, e = unlockPasswordKeyringUsing(stored, secret, derive); return e })
		clear(secret)
		if err != nil {
			if checked != nil {
				checked.Destroy()
			}
			return err
		}
		if err = r.ready(); err != nil {
			checked.Destroy()
			return err
		}
		same := sameKeyring(checked, r.keyring)
		checked.Destroy()
		if !same {
			return ErrAuthentication
		}
	} else if current != "" {
		return ErrPasswordNotConfigured
	}
	_, pending := r.keyring.rotationStatus()
	if pending && stored.Password == nil {
		return ErrRotationPending
	}
	if pending && (password != "" || confirmation != "") {
		return ErrRotationPending
	}
	if !pending {
		if err := validateNewPassword(password, confirmation); err != nil {
			return err
		}
	}
	snapshot, err := r.passwordSnapshotLocked()
	if err != nil {
		return err
	}
	defer snapshot.Destroy()
	var protection *keyringProtection
	err = r.passwordWorkLocked(data, func() error { protection = availablePasswordProtection(stored, snapshot, providers); return nil })
	if protection != nil {
		defer protection.destroy()
	}
	if err != nil {
		return err
	}
	if err = r.ready(); err != nil {
		return err
	}
	if err := syncDir(localSyncDirectory(path)); err != nil {
		return fmt.Errorf("%w: sync keyring directory", ErrDurability)
	}
	if !pending {
		secret := []byte(password)
		store := r.keyring.StoreID()
		derive := r.passwordDeriver()
		var next *Keyring
		err := r.passwordWorkLocked(data, func() error { var e error; next, e = newPasswordKeyringUsing(store, secret, derive); return e })
		clear(secret)
		if err != nil {
			if next != nil {
				next.Destroy()
			}
			return err
		}
		if err = r.ready(); err != nil {
			next.Destroy()
			return err
		}
		// Writes allowed during derivation must be included in the rotation.
		for _, target := range r.rotationTargets() {
			if _, err := r.verifyRotationTarget(target); err != nil {
				next.Destroy()
				return err
			}
		}

		oldID, _ := r.keyring.rotationStatus()
		oldMain := r.keyring.mainKeys[oldID]
		next.mainKeys[oldID] = oldMain
		old := &storedPreviousMainKey{KeyID: append([]byte(nil), oldID[:]...), Nonce: make([]byte, chacha20poly1305.NonceSizeX)}
		if _, err := io.ReadFull(rand.Reader, old.Nonce); err != nil {
			next.Destroy()
			clear(oldMain[:])
			return err
		}
		active := next.mainKeys[next.activeKeyID]
		aead, err := chacha20poly1305.NewX(active[:])
		clear(active[:])
		if err != nil {
			next.Destroy()
			clear(oldMain[:])
			return err
		}
		aad := append(passwordAAD(next.storeID, next.password), []byte("/previous/")...)
		aad = append(aad, old.KeyID...)
		old.Wrapped = aead.Seal(nil, old.Nonce, oldMain[:], aad)
		clear(oldMain[:])
		next.password.Previous = old
		if err := r.publishRotationKeyring(path, next, protection, publish); err != nil {
			return err
		}
	}
	if err := r.rewrapActiveObjects(publish); err != nil {
		return err
	}
	active, err := r.keyring.activeOnly()
	if err != nil {
		return err
	}
	return r.publishRotationKeyring(path, active, protection, publish)
}

func (r *Repository) RebindPasswordProvider(providers ...Provider) error {
	r.passwordMu.Lock()
	defer r.passwordMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return err
	}
	if r.keyring.password == nil {
		return ErrPasswordNotConfigured
	}
	expected, err := readBoundedFile(filepath.Join(r.dataDir, "keys", "config.keys"), maxKeyringFile)
	if err != nil {
		return err
	}
	defer clear(expected)
	snapshot, err := r.passwordSnapshotLocked()
	if err != nil {
		return err
	}
	defer snapshot.Destroy()
	var data []byte
	err = r.passwordWorkLocked(expected, func() error { var e error; data, e = snapshot.Marshal(providers...); return e })
	defer clear(data)
	if err != nil {
		return err
	}
	if err = r.ready(); err != nil {
		return err
	}
	stored, err := parseStoredKeyring(data)
	if err != nil {
		return err
	}
	if len(stored.Slots) == 0 {
		return ErrProviderUnavailable
	}
	err = writeStoreFile(filepath.Join(r.dataDir, "keys", "config.keys"), data)
	if err == nil || errors.Is(err, ErrDurability) {
		r.automaticUnlock = len(stored.Slots) > 0
	}
	return err
}
