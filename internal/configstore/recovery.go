package configstore

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

var (
	ErrLocked      = errors.New("configstore: keyring is locked; enter the encryption password")
	ErrNotLocked   = errors.New("configstore: keyring is already unlocked")
	ErrRecoveryKey = errors.New("configstore: Main Key could not authenticate all encrypted configurations")
)

const recoveryKeyPrefix = "termcp-main-key-v1:"
const maxRecoveryKeyLength = 4096

func keyUnlockErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrPasswordRequired):
		return "password_required"
	case errors.Is(err, ErrProviderUnavailable):
		return "key_provider_unavailable"
	case errors.Is(err, ErrAuthentication):
		return "keyring_authentication_failed"
	default:
		return ""
	}
}

// DecodeMainKey accepts a canonical Base64-encoded 256-bit Main Key. It never
// derives a key from a password or any other low-entropy text.
func DecodeMainKey(encoded string) (MainKey, error) {
	var main MainKey
	encoded = strings.TrimSpace(encoded)
	if len(encoded) != base64.StdEncoding.EncodedLen(len(main)) {
		return main, ErrRecoveryKey
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	defer clear(decoded)
	if err != nil || len(decoded) != len(main) || isZeroBytes(decoded) {
		return main, ErrRecoveryKey
	}
	copy(main[:], decoded)
	return main, nil
}

func decodeRecoveryKey(encoded string, storeID, catalogKeyID ID) (*Keyring, error) {
	if len(encoded) > maxRecoveryKeyLength {
		return nil, ErrRecoveryKey
	}
	encoded = strings.TrimSpace(encoded)
	if strings.HasPrefix(encoded, recoveryKeyPrefix) {
		plaintext, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(encoded, recoveryKeyPrefix))
		defer clear(plaintext)
		if err != nil {
			return nil, ErrRecoveryKey
		}
		keyring, err := parseKeyringPayload(plaintext, storeID)
		if err != nil {
			return nil, ErrRecoveryKey
		}
		return keyring, nil
	}
	main, err := DecodeMainKey(encoded)
	if err != nil {
		return nil, err
	}
	defer clear(main[:])
	return &Keyring{storeID: storeID, activeKeyID: catalogKeyID, mainKeys: map[ID]MainKey{catalogKeyID: main}}, nil
}

// RecoverMainKey verifies the backup against the catalog and every committed
// object before replacing only config.keys with fresh provider protection.
// validate, when supplied, must be a pure parser and must not call Repository.
func (r *Repository) RecoverMainKey(encoded string, validate func(string, []byte) error, providers ...Provider) error {
	return r.recoverMainKey(encoded, validate, providers, writeFileAtomic)
}

func (r *Repository) recoverMainKey(
	encoded string,
	validate func(string, []byte) error,
	providers []Provider,
	publish func(string, []byte) error,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.unlockErr == nil {
		return ErrNotLocked
	}
	raw, err := readBoundedFile(repositoryCatalogPath(r.dataDir), envelopeHeaderSize+MaxCatalogPlaintext+chacha20poly1305.Overhead)
	if err != nil {
		return err
	}
	header, err := parseCatalogEnvelope(raw, r.lockedStoreID, catalogObjectID)
	if err != nil {
		return ErrRecoveryKey
	}
	candidate, err := decodeRecoveryKey(encoded, r.lockedStoreID, header.WrapKeyID())
	if err != nil {
		return err
	}
	adopted := false
	defer func() {
		if !adopted {
			candidate.Destroy()
		}
	}()
	probe := &Repository{dataDir: r.dataDir, keyring: candidate}
	catalog, refs, generationID, err := probe.readCatalogSnapshot()
	if err != nil {
		return ErrRecoveryKey
	}
	resolvedRefs, _, err := probe.resolveProfileDirectories(refs)
	if err != nil {
		return ErrRecoveryKey
	}
	for _, ref := range resolvedRefs {
		data, err := probe.readUnlocked(ref)
		if err != nil {
			clear(data)
			return ErrRecoveryKey
		}
		if validate != nil {
			err = validate(ref.name, data)
		}
		clear(data)
		if err != nil {
			return fmt.Errorf("%w: a stored profile is invalid", ErrRecoveryKey)
		}
	}
	keysPath := filepath.Join(r.dataDir, "keys", "config.keys")
	previous, err := readBoundedFile(keysPath, maxKeyringFile)
	if err != nil {
		return err
	}
	stored, err := parseStoredKeyring(previous)
	clear(previous)
	if err != nil || !bytes.Equal(stored.StoreID, r.lockedStoreID[:]) {
		return ErrIdentityMismatch
	}
	wrapped, err := candidate.Marshal(providers...)
	if err != nil {
		return err
	}
	defer clear(wrapped)
	err = publish(keysPath, wrapped)
	if err != nil && !errors.Is(err, ErrDurability) {
		return err
	}
	r.keyring = candidate
	r.unlockErr = nil
	r.refs = refs
	r.encryptNew = catalog.EncryptNew
	r.revision = catalog.Revision
	r.catalogGeneration = generationID
	adopted = true
	return err
}

func (r *Repository) readCatalogSnapshot() (catalogDocument, map[string]catalogRef, ID, error) {
	raw, err := readBoundedFile(repositoryCatalogPath(r.dataDir), envelopeHeaderSize+MaxCatalogPlaintext+chacha20poly1305.Overhead)
	if errors.Is(err, os.ErrNotExist) {
		return catalogDocument{}, nil, ID{}, ErrNotInitialized
	}
	if err != nil {
		return catalogDocument{}, nil, ID{}, err
	}
	envelope, err := parseCatalogEnvelope(raw, r.keyring.StoreID(), catalogObjectID)
	if err != nil {
		return catalogDocument{}, nil, ID{}, err
	}
	plaintext, err := r.keyring.Open(envelope)
	if err != nil {
		return catalogDocument{}, nil, ID{}, err
	}
	defer clear(plaintext)
	catalog, refs, err := decodeCatalog(plaintext, r.keyring.StoreID())
	return catalog, refs, envelope.identity.GenerationID, err
}
