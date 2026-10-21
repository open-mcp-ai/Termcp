package vault

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	"filippo.io/age"
)

// Encrypt seals plaintext as the profile's age file, addressed to every
// device in the recipients table. Encryption cryptographically needs only
// public keys, but Encrypt still requires an unlocked vault: sealing a
// profile this device could not read back would let a caller strand data on
// a locked machine, which is a failure shape, not a feature.
//
// The write is atomic (temp file + rename, 0600): a crash must never leave a
// half-written ciphertext that Decrypt would then report as corruption.
func (v *Vault) Encrypt(profileName string, plaintext []byte) error {
	if _, err := v.unlockedIdentity(); err != nil {
		return err
	}
	path, err := v.profilePath(profileName)
	if err != nil {
		return err
	}
	recipients, err := v.recipientKeys()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, recipients...)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}
	return atomicWrite0600(path, buf.Bytes())
}

// Decrypt opens the profile's age file with this device's identity.
//
// A missing file, a corrupt file, and "sealed to other devices but not this
// one" all surface as one error wrapped over ErrDecrypt — an attacker reading
// errors learns nothing about which files are worth keeping.
func (v *Vault) Decrypt(profileName string) ([]byte, error) {
	id, err := v.unlockedIdentity()
	if err != nil {
		return nil, err
	}
	path, err := v.profilePath(profileName)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("profile %q: %w", profileName, fs.ErrNotExist)
		}
		return nil, err
	}
	r, err := age.Decrypt(bytes.NewReader(data), id)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", profileName, ErrDecrypt)
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		// age authenticates incrementally; a tampered tail fails here.
		return nil, fmt.Errorf("profile %q: %w", profileName, ErrDecrypt)
	}
	return plain, nil
}

// ResealAll re-encrypts every sealed profile against the CURRENT recipients
// table. It is the multi-device verb: after `recipients add <new-device>`,
// existing ciphertext is still sealed to the old device set — ResealAll
// re-wraps each profile so the new device can open it (and a removed device
// stops opening newly written files). Needs an unlocked vault; returns the
// number of profiles resealed.
func (v *Vault) ResealAll() (int, error) {
	if _, err := v.unlockedIdentity(); err != nil {
		return 0, err
	}
	names, err := v.List()
	if err != nil {
		return 0, err
	}
	for _, name := range names {
		plain, err := v.Decrypt(name)
		if err != nil {
			return 0, fmt.Errorf("reseal %q: %w", name, err)
		}
		if err := v.Encrypt(name, plain); err != nil {
			return 0, fmt.Errorf("reseal %q: %w", name, err)
		}
	}
	return len(names), nil
}

// Delete removes the profile's age file. Missing is not an error: deleting
// is a cleanup gesture, and the caller's goal — no ciphertext left — is
// already met.
func (v *Vault) Delete(profileName string) error {
	path, err := v.profilePath(profileName)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Has reports whether a sealed profile file exists for the name. It answers
// without unlocking: the filename is the profile name, so presence is public.
func (v *Vault) Has(profileName string) bool {
	path, err := v.profilePath(profileName)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// List returns the sealed profile names, without unlocking. The filename IS
// the profile name by design (design doc §三): the initial format keeps a
// single track instead of a plaintext metadata index.
func (v *Vault) List() ([]string, error) {
	entries, err := os.ReadDir(v.profilesDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if name, ok := strings.CutSuffix(e.Name(), ".age"); ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}
