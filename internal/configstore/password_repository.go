package configstore

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"
)

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
	for _, path := range []string{filepath.Join(root, "keys", "config.keys"), repositoryCatalogPath(root)} {
		if exists, err := pathExists(path); err != nil {
			return fail(err)
		} else if exists {
			return fail(ErrAlreadyInitialized)
		}
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
	if err := writeFileAtomic(filepath.Join(root, "keys", "config.keys"), data); err != nil {
		k.Destroy()
		return fail(err)
	}
	r := newRepository(root)
	r.lock = lock
	r.mutate(func(next *repoSnapshot) {
		next.keyring = k
		next.lockedStoreID = store
		next.automaticUnlock = len(stored.Slots) > 0
	})
	if err := r.commitCatalog(catalogDocument{Version: repositoryVersion, StoreID: append([]byte(nil), store[:]...), Revision: 1, Profiles: []catalogProfile{}}); err != nil {
		k.Destroy()
		return fail(err)
	}
	r.setRevision(1)
	return r, nil
}

// UnlockPassword derives Main Key directly from the password. It publishes no
// key material and remains usable without any operating-system or ENV KEK.
func (r *Repository) UnlockPassword(password string) error {
	defer r.acquireWrite()()
	if r.snap().closed {
		return ErrClosed
	}
	if r.snap().unlockErr == nil {
		return ErrNotLocked
	}
	data, err := readBoundedFile(filepath.Join(r.dataDir, "keys", "config.keys"), maxKeyringFile)
	if err != nil {
		return err
	}
	stored, err := parseStoredKeyring(data)
	if err != nil {
		return err
	}
	secret := []byte(password)
	defer clear(secret)
	k, err := unlockPasswordKeyring(stored, secret)
	if err != nil {
		return err
	}
	probe := newRepository(r.dataDir)
	probe.storeKeyring(k, k.StoreID())
	if err := probe.loadCatalog(); err != nil {
		k.Destroy()
		return err
	}
	pst := probe.snap()
	r.mutate(func(next *repoSnapshot) {
		next.keyring = k
		next.unlockErr = nil
		next.refs = pst.refs
		next.encryptNew = pst.encryptNew
		next.revision = pst.revision
		next.catalogGeneration = pst.catalogGeneration
		next.password = stored.Password
		next.automaticUnlock = false
	})
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
	return r.changePassword(current, password, confirmation, providers, writeFileAtomic)
}

func (r *Repository) changePassword(current, password, confirmation string, providers []Provider, publish func(string, []byte) error) error {
	defer r.acquireWrite()()
	if err := r.ready(); err != nil {
		return err
	}
	path := filepath.Join(r.dataDir, "keys", "config.keys")
	data, err := readBoundedFile(path, maxKeyringFile)
	if err != nil {
		return err
	}
	stored, err := parseStoredKeyring(data)
	if err != nil {
		return err
	}
	if (stored.Password == nil) != (r.snap().keyring.password == nil) {
		return ErrAuthentication
	}
	if stored.Password != nil {
		secret := []byte(current)
		checked, err := unlockPasswordKeyring(stored, secret)
		clear(secret)
		if err != nil {
			return err
		}
		same := sameKeyring(checked, r.snap().keyring)
		checked.Destroy()
		if !same {
			return ErrAuthentication
		}
	} else if current != "" {
		return ErrPasswordNotConfigured
	}
	_, pending := r.snap().keyring.rotationStatus()
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
	protection := availablePasswordProtection(stored, r.snap().keyring, providers)
	defer protection.destroy()
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: sync keyring directory", ErrDurability)
	}
	if !pending {
		for _, target := range r.rotationTargets() {
			if _, err := r.verifyRotationTarget(target); err != nil {
				return err
			}
		}
		secret := []byte(password)
		next, err := newPasswordKeyring(r.snap().keyring.StoreID(), secret)
		clear(secret)
		if err != nil {
			return err
		}
		oldID, _ := r.snap().keyring.rotationStatus()
		oldMain, _ := r.snap().keyring.key(oldID)
		nextSt := next.state.Load()
		nextMainKeys := map[ID]MainKey{
			oldID:              oldMain,
			nextSt.activeKeyID: nextSt.mainKeys[nextSt.activeKeyID],
		}
		next.withState(nextSt.storeID, nextSt.activeKeyID, nextMainKeys)
		old := &storedPreviousMainKey{KeyID: append([]byte(nil), oldID[:]...), Nonce: make([]byte, chacha20poly1305.NonceSizeX)}
		if _, err := io.ReadFull(rand.Reader, old.Nonce); err != nil {
			next.Destroy()
			clear(oldMain[:])
			return err
		}
		active := nextSt.mainKeys[nextSt.activeKeyID]
		aead, err := chacha20poly1305.NewX(active[:])
		clear(active[:])
		if err != nil {
			next.Destroy()
			clear(oldMain[:])
			return err
		}
		aad := append(passwordAAD(nextSt.storeID, next.password), []byte("/previous/")...)
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
	active, err := r.snap().keyring.activeOnly()
	if err != nil {
		return err
	}
	return r.publishRotationKeyring(path, active, protection, publish)
}

func (r *Repository) RebindPasswordProvider(providers ...Provider) error {
	defer r.acquireWrite()()
	if err := r.ready(); err != nil {
		return err
	}
	if r.snap().keyring.password == nil {
		return ErrPasswordNotConfigured
	}
	data, err := r.snap().keyring.Marshal(providers...)
	if err != nil {
		return err
	}
	stored, err := parseStoredKeyring(data)
	if err != nil {
		return err
	}
	if len(stored.Slots) == 0 {
		return ErrProviderUnavailable
	}
	err = writeFileAtomic(filepath.Join(r.dataDir, "keys", "config.keys"), data)
	if err == nil || errors.Is(err, ErrDurability) {
		r.mutate(func(next *repoSnapshot) { next.automaticUnlock = len(stored.Slots) > 0 })
	}
	return err
}
