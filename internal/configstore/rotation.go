package configstore

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"golang.org/x/crypto/chacha20poly1305"
)

// ErrRotationPending prevents mutations until an interrupted Main Key rotation
// has finished. Reads remain available through the persisted two-key keyring.
var ErrRotationPending = errors.New("configstore: resume Main Key rotation before modifying encrypted configuration")

type rotationTarget struct {
	path     string
	identity Identity
}

// RotateMainKey generates a new random Main Key and rewraps every committed
// profile and the catalog. An interrupted operation resumes the same rotation.
// Existing provider slots, DEKs, identities, and body ciphertext are preserved.
func (r *Repository) RotateMainKey(providers ...Provider) error {
	return r.rotateMainKey(providers, writeFileAtomic)
}

func (r *Repository) rotateMainKey(providers []Provider, publish func(string, []byte) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return err
	}
	if r.keyring.password != nil {
		return ErrPasswordRequired
	}
	keysPath := filepath.Join(r.dataDir, "keys", "config.keys")
	keyData, err := readBoundedFile(keysPath, maxKeyringFile)
	if err != nil {
		return err
	}
	protection, err := unlockKeyringProtection(keyData, r.keyring, providers)
	clear(keyData)
	if err != nil {
		return err
	}
	defer protection.destroy()
	// A previous publication may have returned ErrDurability. Confirm the
	// keyring's directory entry before writing anything under its active key.
	if err := syncDir(filepath.Dir(keysPath)); err != nil {
		return fmt.Errorf("%w: sync keyring directory: %v", ErrDurability, err)
	}
	targets := r.rotationTargets()
	for _, target := range targets {
		if _, err := r.verifyRotationTarget(target); err != nil {
			return err
		}
	}
	if _, pending := r.keyring.rotationStatus(); !pending {
		dual, err := r.keyring.beginRotation()
		if err != nil {
			return err
		}
		if err := r.publishRotationKeyring(keysPath, dual, protection, publish); err != nil {
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
	return r.publishRotationKeyring(keysPath, active, protection, publish)
}

func (r *Repository) rewrapActiveObjects(publish func(string, []byte) error) error {
	activeID, _ := r.keyring.rotationStatus()
	for _, target := range r.rotationTargets() {
		envelope, err := r.readRotationTarget(target)
		if err != nil {
			return err
		}
		if envelope.WrapKeyID() != activeID {
			rewrapped, err := r.keyring.Rewrap(envelope)
			if err != nil {
				return err
			}
			if err := publish(target.path, rewrapped.Bytes()); err != nil {
				return err
			}
		}
		verified, err := r.verifyRotationTarget(target)
		if err != nil {
			return err
		}
		if verified.WrapKeyID() != activeID {
			return ErrAuthentication
		}
		// Also sync skipped files from an interrupted run before retiring the
		// old key; their prior rename may not have been confirmed durable.
		if err := syncDir(filepath.Dir(target.path)); err != nil {
			return fmt.Errorf("%w: sync rewrapped configuration: %v", ErrDurability, err)
		}
	}
	return nil
}

func (r *Repository) publishRotationKeyring(
	path string,
	next *Keyring,
	protection *keyringProtection,
	publish func(string, []byte) error,
) error {
	data, err := protection.marshal(next)
	if err != nil {
		next.Destroy()
		return err
	}
	defer clear(data)
	err = publish(path, data)
	if err != nil && !errors.Is(err, ErrDurability) {
		next.Destroy()
		return err
	}
	previous := r.keyring
	r.keyring = next
	r.password = next.password
	r.automaticUnlock = len(protection.stored.Slots) > 0
	previous.Destroy()
	if err != nil {
		return err
	}
	written, err := readBoundedFile(path, maxKeyringFile)
	if err != nil {
		return err
	}
	defer clear(written)
	if !bytes.Equal(written, data) {
		return ErrAuthentication
	}
	return nil
}

func (r *Repository) rotationTargets() []rotationTarget {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	targets := make([]rotationTarget, 0, len(names)+1)
	storeID := r.keyring.StoreID()
	for _, name := range names {
		ref := r.refs[name]
		targets = append(targets, rotationTarget{
			path: r.objectPath(ref),
			identity: Identity{
				StoreID: storeID, ObjectID: ref.objectID,
				GenerationID: ref.generationID, Kind: KindProfile,
			},
		})
	}
	return append(targets, rotationTarget{
		path: repositoryCatalogPath(r.dataDir),
		identity: Identity{
			StoreID: storeID, ObjectID: catalogObjectID,
			GenerationID: r.catalogGeneration, Kind: KindCatalog,
		},
	})
}

func (r *Repository) readRotationTarget(target rotationTarget) (*Envelope, error) {
	limit, _ := target.identity.Kind.maxPlaintext()
	raw, err := readBoundedFile(target.path, int64(envelopeHeaderSize+limit+chacha20poly1305.Overhead))
	if err != nil {
		return nil, err
	}
	return Parse(raw, target.identity)
}

func (r *Repository) verifyRotationTarget(target rotationTarget) (*Envelope, error) {
	envelope, err := r.readRotationTarget(target)
	if err != nil {
		return nil, err
	}
	plaintext, err := r.keyring.Open(envelope)
	clear(plaintext)
	return envelope, err
}

func (r *Repository) writable() error {
	if err := r.ready(); err != nil {
		return err
	}
	if _, pending := r.keyring.rotationStatus(); pending {
		return ErrRotationPending
	}
	return nil
}
