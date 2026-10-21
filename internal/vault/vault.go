// Package vault is termcp's encrypted credential store: age-encrypted
// per-profile files plus a recipients table, designed for the zero-trust
// threat model of docs/design/credential-vault.md.
//
// Two invariants shape every decision here:
//
//	I1: a credential is ciphertext in every persistence and sync step.
//	I2: an unlock private key never leaves its device.
//
// Concretely: each ssh profile is one age file under vault/ssh_configs/, the
// recipients table lists the X25519 public keys every file is sealed to, and
// this device's identity lives in vault/identity.age — itself encrypted with
// a passphrase via age's scrypt format, so even the identity at rest is
// ciphertext. Syncing the vault directory over any untrusted channel leaks
// nothing; adding a device is re-sealing to a new public key, never moving a
// private one.
//
// The package is deliberately self-contained: it knows nothing about SSH,
// TOML profiles beyond "bytes in, bytes out", keychains, or the CLI. Those
// are separate components layered on top.
package vault

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// ErrLocked reports an operation that needs the device identity while the
// vault is locked. Unlock with the passphrase first.
var ErrLocked = errors.New("vault is locked")

// ErrNotInitialized reports an operation on a data directory that has no
// vault yet (no identity.age). Run Init first.
var ErrNotInitialized = errors.New("vault is not initialized")

// ErrDecrypt reports a ciphertext that cannot be opened: the file is
// corrupt, was tampered with, or this device's identity is not among its
// recipients. The cause is deliberately one error — distinguishing them
// would tell an attacker which files to keep trying.
var ErrDecrypt = errors.New("cannot decrypt: corrupt file or not a recipient")

// nameRe mirrors internal/sshconfig's profile naming rule. It is duplicated
// rather than imported on purpose: vault is a standalone component, and a
// 20-byte regexp is not a coupling worth taking.
var nameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// ValidateName reports whether name may label a vault profile.
func ValidateName(name string) error {
	if !nameRe.MatchString(strings.TrimSpace(name)) {
		return fmt.Errorf("invalid profile name %q (letters, digits, _, -; max 64)", name)
	}
	return nil
}

// Vault is an encrypted credential store rooted at dataDir/vault/.
//
// The unlocked identity is held in memory only, guarded by mu. Lock drops
// the reference; Go cannot promise memory zeroization, so Lock is best-effort
// — the guarantee that matters is I1 (at-rest ciphertext), which does not
// depend on process memory hygiene.
type Vault struct {
	dir string

	mu sync.Mutex
	id identity // nil while locked
}

// New returns a Vault rooted at dataDir. It performs no I/O: call Init for a
// fresh vault, or Unlock to open an existing one.
func New(dataDir string) *Vault {
	return &Vault{dir: filepath.Join(dataDir, "vault")}
}

// Dir returns the vault's root directory (dataDir/vault).
func (v *Vault) Dir() string { return v.dir }

// Exists reports whether this data directory holds a vault.
func (v *Vault) Exists() bool {
	_, err := os.Stat(v.identityPath())
	return err == nil
}

// IsUnlocked reports whether the device identity is currently in memory.
func (v *Vault) IsUnlocked() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.id != nil
}

// Lock drops the in-memory identity. Best-effort only; see Vault.
func (v *Vault) Lock() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.id = nil
}

// unlockedIdentity returns the identity or ErrLocked.
func (v *Vault) unlockedIdentity() (identity, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.id == nil {
		return nil, ErrLocked
	}
	return v.id, nil
}

// setUnlocked installs the identity as the unlocked state.
func (v *Vault) setUnlocked(id identity) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.id = id
}

func (v *Vault) identityPath() string   { return filepath.Join(v.dir, "identity.age") }
func (v *Vault) recipientsPath() string { return filepath.Join(v.dir, "recipients.toml") }
func (v *Vault) profilesDir() string    { return filepath.Join(v.dir, "ssh_configs") }

func (v *Vault) profilePath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(v.profilesDir(), name+".age"), nil
}

// atomicWrite0600 writes data through a temp file and rename, so a crash
// cannot leave a half-written vault file behind. Mode is 0600: ciphertext is
// safe to leak in principle, but there is no reason to widen the audience.
func atomicWrite0600(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-vault-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
