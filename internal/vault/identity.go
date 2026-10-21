package vault

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"filippo.io/age"
)

// identity is the capability the whole vault gates on: this device's age
// identity. It is an interface rather than *age.X25519Identity so the
// keychain-backed variants (a later component) can slot in without touching
// crypt.go.
type identity interface {
	age.Identity
}

// selfRecipientName labels this device's public key in recipients.toml. It is
// a fixed string rather than the hostname: a recipient table is meant to be
// synced across devices, and embedding a hostname would leak it into every
// copy.
const selfRecipientName = "self"

// scryptLogN is the work factor for the passphrase-encrypted identity file.
// 18 is age's interactive default (~1 s of work — exactly right against an
// offline brute-forcer holding identity.age, exactly wrong in a test loop).
// Tests lower it through the package variable; the decrypting side reads the
// factor from the stanza, so a cheap test file still opens.
var scryptLogN = 18

// Init creates a fresh vault in the data directory: a new X25519 identity,
// persisted as identity.age encrypted with the passphrase via age's scrypt
// format, plus a recipients.toml holding the new public key as "self".
//
// Init refuses to overwrite an existing vault: clobbering the identity makes
// every sealed profile permanently unreadable, which is a failure no caller
// intends. Delete the vault directory deliberately first if that is truly
// wanted.
//
// The passphrase is the fallback unlock path. A later component adds OS
// keychain storage for the identity; the passphrase file stays as the
// portable escape hatch, which is why it exists from day one.
func (v *Vault) Init(passphrase string) (recipient string, err error) {
	if v.Exists() {
		return "", fmt.Errorf("vault already initialized at %s", v.dir)
	}
	if passphrase == "" {
		return "", fmt.Errorf("passphrase must not be empty")
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", fmt.Errorf("generate identity: %w", err)
	}
	if err := v.storeIdentity(id, passphrase); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	if err := v.saveRecipients(&recipientFile{
		Recipients: []Recipient{{
			Name:      selfRecipientName,
			Recipient: id.Recipient().String(),
			AddedAt:   now,
			Note:      "this device",
		}},
	}); err != nil {
		// Roll the identity back too: a vault with an identity but no
		// recipients table is a trap — Init refuses to retry (identity
		// exists) while Encrypt refuses to run (empty table).
		_ = os.Remove(v.identityPath())
		return "", err
	}
	// Keychain is a convenience, never a requirement: when it answers, the
	// identity is mirrored there for passphrase-free unlocks; the scrypt file
	// already on disk remains the portable fallback.
	v.storeInKeychain(id.String())
	v.setUnlocked(id)
	return id.Recipient().String(), nil
}

// Unlock reads identity.age and opens it with the passphrase. On success the
// vault is unlocked and ready for Encrypt/Decrypt.
func (v *Vault) Unlock(passphrase string) error {
	data, err := os.ReadFile(v.identityPath())
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNotInitialized
		}
		return err
	}
	scryptID, err := age.NewScryptIdentity(passphrase)
	if err != nil {
		return fmt.Errorf("scrypt identity: %w", err)
	}
	r, err := age.Decrypt(bytes.NewReader(data), scryptID)
	if err != nil {
		// A wrong passphrase and a corrupt file land here alike; scrypt's
		// MAC check is what reports either. One message, no oracle.
		return fmt.Errorf("unlock: wrong passphrase or corrupt identity file")
	}
	plain, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("unlock: read identity: %w", err)
	}
	id, err := parseIdentityString(string(bytes.TrimSpace(plain)))
	if err != nil {
		return err
	}
	v.setUnlocked(id)
	return nil
}

// parseIdentityString parses a stored age identity, accepting the trailing
// newline the CLI tools and file format both tend to append.
func parseIdentityString(s string) (*age.X25519Identity, error) {
	id, err := age.ParseX25519Identity(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("parse identity: %w", err)
	}
	return id, nil
}

// storeIdentity persists id encrypted with the passphrase. The identity file
// is therefore ciphertext at rest like everything else in the vault.
func (v *Vault) storeIdentity(id *age.X25519Identity, passphrase string) error {
	scryptRec, err := age.NewScryptRecipient(passphrase)
	if err != nil {
		return fmt.Errorf("scrypt recipient: %w", err)
	}
	scryptRec.SetWorkFactor(scryptLogN)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, scryptRec)
	if err != nil {
		return fmt.Errorf("encrypt identity: %w", err)
	}
	if _, err := io.WriteString(w, id.String()+"\n"); err != nil {
		return fmt.Errorf("encrypt identity: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("encrypt identity: %w", err)
	}
	return atomicWrite0600(v.identityPath(), buf.Bytes())
}
