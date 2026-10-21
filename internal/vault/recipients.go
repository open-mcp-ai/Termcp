package vault

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"filippo.io/age"
	"github.com/BurntSushi/toml"
)

// Recipient is one device public key the vault seals profiles to. The table
// is public data — it syncs alongside the ciphertext over untrusted channels
// — but it is written with the same atomic care as everything else here,
// because a half-written recipients file means the next Encrypt seals to a
// partial device set and the missing devices can never open those files.
type Recipient struct {
	Name      string    `toml:"name"`
	Recipient string    `toml:"recipient"` // age1... X25519 public key
	AddedAt   time.Time `toml:"added_at"`
	Note      string    `toml:"note,omitempty"`
}

// recipientFile is the on-disk shape of recipients.toml.
type recipientFile struct {
	Recipients []Recipient `toml:"recipient"`
}

// ErrRecipientNotFound reports removing or naming a recipient that is not in
// the table.
var ErrRecipientNotFound = errors.New("recipient not found")

// Recipients returns the current recipients table. Reading it needs no
// unlock — it holds public keys only.
func (v *Vault) Recipients() ([]Recipient, error) {
	f, err := v.loadRecipients()
	if err != nil {
		return nil, err
	}
	return append([]Recipient(nil), f.Recipients...), nil
}

// AddRecipient adds a device public key to the table. Files encrypted after
// this call are sealed to it; files encrypted before are not — re-sealing is
// an explicit, per-profile operation the caller drives.
func (v *Vault) AddRecipient(name, recipient, note string) error {
	if _, err := age.ParseX25519Recipient(recipient); err != nil {
		return fmt.Errorf("recipient: %w", err)
	}
	f, err := v.loadRecipients()
	if err != nil {
		return err
	}
	for _, r := range f.Recipients {
		if r.Name == name {
			return fmt.Errorf("recipient name %q already exists", name)
		}
		if r.Recipient == recipient {
			return fmt.Errorf("recipient key already present as %q", r.Name)
		}
	}
	f.Recipients = append(f.Recipients, Recipient{
		Name:      name,
		Recipient: recipient,
		AddedAt:   time.Now().UTC(),
		Note:      note,
	})
	return v.saveRecipients(f)
}

// RemoveRecipient drops a device from the table. Existing ciphertext stays
// sealed to it until re-encrypted — removing a recipient is a forward-looking
// revocation, not a retroactive one; age gives no way to unseal from afar.
func (v *Vault) RemoveRecipient(name string) error {
	f, err := v.loadRecipients()
	if err != nil {
		return err
	}
	out := f.Recipients[:0]
	found := false
	for _, r := range f.Recipients {
		if r.Name == name {
			found = true
			continue
		}
		out = append(out, r)
	}
	if !found {
		return fmt.Errorf("%w: %q", ErrRecipientNotFound, name)
	}
	f.Recipients = out
	return v.saveRecipients(f)
}

// loadRecipients reads recipients.toml. A missing file is an empty table, so
// Recipients works on a directory that has not been Init'd; Encrypt refuses
// on an empty table instead of sealing a profile to nobody.
func (v *Vault) loadRecipients() (*recipientFile, error) {
	data, err := os.ReadFile(v.recipientsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &recipientFile{}, nil
		}
		return nil, err
	}
	var f recipientFile
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("recipients.toml: %w", err)
	}
	return &f, nil
}

func (v *Vault) saveRecipients(f *recipientFile) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(f); err != nil {
		return err
	}
	return atomicWrite0600(v.recipientsPath(), buf.Bytes())
}

// recipientKeys parses every table entry into an age recipient. A single
// malformed entry fails the whole Encrypt: silently skipping it would seal a
// profile to fewer devices than the table promises.
func (v *Vault) recipientKeys() ([]age.Recipient, error) {
	f, err := v.loadRecipients()
	if err != nil {
		return nil, err
	}
	if len(f.Recipients) == 0 {
		return nil, ErrNotInitialized
	}
	out := make([]age.Recipient, 0, len(f.Recipients))
	for _, r := range f.Recipients {
		rc, err := age.ParseX25519Recipient(r.Recipient)
		if err != nil {
			return nil, fmt.Errorf("recipient %q: %w", r.Name, err)
		}
		out = append(out, rc)
	}
	return out, nil
}
