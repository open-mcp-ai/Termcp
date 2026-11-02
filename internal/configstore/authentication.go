package configstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

var authenticationObjectID = repeatedID(0xa7)

func authenticationPath(dataDir string) string { return filepath.Join(dataDir, "auth.enc") }

// HasAuthentication identifies committed authentication independently of a
// plaintext marker. A missing marker must not reopen first-run setup.
func HasAuthentication(dataDir string) (bool, error) { return pathExists(authenticationPath(dataDir)) }

// A stale initialization marker cannot authorize replacing a profile catalog.
func hasProfileCiphertext(dataDir string) (bool, error) {
	if state, err := loadLocalState(dataDir); err == nil {
		for _, h := range state.Envelopes {
			if ObjectKind(h[12]) == KindProfile {
				return true, nil
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "ssh_configs"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(dataDir, "ssh_configs", entry.Name()))
		if err != nil {
			return false, err
		}
		for _, file := range files {
			if strings.EqualFold(filepath.Ext(file.Name()), ".enc") {
				return true, nil
			}
		}
	}
	return false, nil
}

func parseAuthentication(raw []byte, store ID) (*Envelope, error) {
	if len(raw) < envelopeHeaderSize {
		return nil, ErrFormat
	}
	var generation ID
	copy(generation[:], raw[45:61])
	return Parse(raw, Identity{StoreID: store, ObjectID: authenticationObjectID, GenerationID: generation, Kind: KindAuthentication})
}

func (r *Repository) loadAuthGeneration() error {
	raw, err := readBoundedFile(authenticationPath(r.dataDir), (1<<20)+envelopeHeaderSize+16)
	if errors.Is(err, os.ErrNotExist) {
		r.authGeneration = ID{}
		return nil
	}
	if err != nil {
		return err
	}
	defer clear(raw)
	envelope, err := parseAuthentication(raw, r.keyring.StoreID())
	if err != nil {
		return err
	}
	data, err := r.keyring.Open(envelope)
	clear(data)
	if err != nil {
		return err
	}
	r.authGeneration = envelope.Identity().GenerationID
	return nil
}

// VerifyPassword checks the password-derived keyring even when a provider has
// already unlocked it. Automatic unlock is not an Admin login.
func (r *Repository) VerifyPassword(password string) error {
	r.passwordMu.Lock()
	defer r.passwordMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return err
	}
	raw, err := readBoundedFile(filepath.Join(r.dataDir, "keys", "config.keys"), maxKeyringFile)
	if err != nil {
		return err
	}
	defer clear(raw)
	stored, err := parseStoredKeyring(raw)
	if err != nil {
		return err
	}
	if stored.Password == nil {
		return ErrPasswordNotConfigured
	}
	secret := []byte(password)
	defer clear(secret)
	derive := r.passwordDeriver()
	var checked *Keyring
	err = r.passwordWorkLocked(raw, func() error { var e error; checked, e = unlockPasswordKeyringUsing(stored, secret, derive); return e })
	if checked != nil {
		defer checked.Destroy()
	}
	if err != nil {
		return err
	}
	if err = r.ready(); err != nil {
		return err
	}
	if !sameKeyring(checked, r.keyring) {
		return ErrAuthentication
	}
	return nil
}

func (r *Repository) ReadAuthentication() ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return nil, err
	}
	raw, err := readBoundedFile(authenticationPath(r.dataDir), (1<<20)+envelopeHeaderSize+16)
	if err != nil {
		return nil, err
	}
	defer clear(raw)
	envelope, err := parseAuthentication(raw, r.keyring.StoreID())
	if err != nil {
		return nil, err
	}
	return r.keyring.Open(envelope)
}

func (r *Repository) WriteAuthentication(data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.writable(); err != nil {
		return err
	}
	generation, err := NewID()
	if err != nil {
		return err
	}
	envelope, err := r.keyring.Seal(data, Identity{StoreID: r.keyring.StoreID(), ObjectID: authenticationObjectID, GenerationID: generation, Kind: KindAuthentication})
	if err != nil {
		return err
	}
	err = writeStoreFile(authenticationPath(r.dataDir), envelope.Bytes())
	if err == nil || errors.Is(err, ErrDurability) {
		r.authGeneration = generation
	}
	return err
}
