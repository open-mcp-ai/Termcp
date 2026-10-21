package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// This file wires the OS keychain into the vault's unlock chain (design doc
// §四): the device identity lives in the system credential store when one is
// available, so routine unlocks need no passphrase. The scrypt-encrypted
// identity.age stays as the portable fallback — keychain first, file second.
//
// The patterns here follow the ones PR #118 validated on three platforms:
// secrets travel over stdin, never argv/env/temp files; stderr is discarded
// (a credential tool may echo sensitive values); every command has a hard
// timeout so a hung desktop agent cannot wedge the daemon.

// keychainService names every keychain item this vault creates. Versioned so
// a future format change cannot be misread by an older binary.
const keychainService = "org.termcp.vault.v1"

// keychainRefFile holds the random reference of this vault's keychain item.
// The reference is not a secret — it exists so the keychain entry name leaks
// neither the local login name nor the data-dir path, and so several vaults
// on one account never collide.
const keychainRefFile = "keychain-ref"

// ErrKeychainUnavailable reports that no OS credential store answered:
// headless Linux without Secret Service, a missing CLI, or a store failure.
// It is not fatal — the passphrase path always remains.
var ErrKeychainUnavailable = errors.New("keychain unavailable")

// secretStore is the platform credential backend. The secret is the vault's
// age identity string. Methods take the vault because some backends keep
// their protected blob inside it (Windows DPAPI) while others address items
// in an OS-wide store by reference (macOS, Linux).
type secretStore interface {
	// Available reports cheaply whether the backend exists at all (binary
	// present, service reachable). It must not prompt the user.
	Available() bool
	Store(v *Vault, ref, secret string) error
	Load(v *Vault, ref string) (string, error)
}

// platformSecretStore is a variable so tests can substitute a fake without
// cgo or a desktop session.
var platformSecretStore = newPlatformSecretStore()

// noKeychainEnv forces the keychain off (tests, containers, user preference).
const noKeychainEnv = "TERMCP_VAULT_NO_KEYCHAIN"

func keychainDisabled() bool { return os.Getenv(noKeychainEnv) != "" }

// credentialCommand runs a credential CLI with the secret on stdin. stderr is
// dropped on purpose; the 15 s cap exists because a desktop agent showing an
// approval dialog must not hang a daemon forever.
func credentialCommand(path string, args []string, input io.Reader) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin, cmd.Stderr = input, io.Discard
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

// keychainRef reads the stored reference, or "" when this vault never set up
// keychain unlock.
func (v *Vault) keychainRef() string {
	data, err := os.ReadFile(filepath.Join(v.dir, keychainRefFile))
	if err != nil {
		return ""
	}
	return string(bytes.TrimSpace(data))
}

// storeInKeychain saves the identity under a fresh random reference and
// records the reference. Best-effort by contract: callers treat failure as
// "passphrase path only", not as an Init failure.
func (v *Vault) storeInKeychain(identityString string) bool {
	store := platformSecretStore
	if keychainDisabled() || store == nil || !store.Available() {
		return false
	}
	ref := make([]byte, 16)
	if _, err := rand.Read(ref); err != nil {
		return false
	}
	refHex := hex.EncodeToString(ref)
	if err := store.Store(v, refHex, identityString); err != nil {
		return false
	}
	if err := atomicWrite0600(filepath.Join(v.dir, keychainRefFile), []byte(refHex+"\n")); err != nil {
		return false
	}
	return true
}

// CanAutoUnlock reports whether UnlockAuto has a chance: keychain use is not
// disabled, a backend answers, and this vault holds a reference.
func (v *Vault) CanAutoUnlock() bool {
	if keychainDisabled() {
		return false
	}
	store := platformSecretStore
	return store != nil && store.Available() && v.keychainRef() != ""
}

// UnlockAuto opens the vault from the OS credential store without a
// passphrase. Callers fall back to Unlock(passphrase) on error.
func (v *Vault) UnlockAuto() error {
	if !v.Exists() {
		return ErrNotInitialized
	}
	if keychainDisabled() {
		return ErrKeychainUnavailable
	}
	store := platformSecretStore
	if store == nil || !store.Available() {
		return ErrKeychainUnavailable
	}
	ref := v.keychainRef()
	if ref == "" {
		return ErrKeychainUnavailable
	}
	secret, err := store.Load(v, ref)
	if err != nil {
		return ErrKeychainUnavailable
	}
	id, err := parseIdentityString(secret)
	if err != nil {
		return err
	}
	v.setUnlocked(id)
	return nil
}
