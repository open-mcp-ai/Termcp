package configstore

import (
	"encoding/hex"
	"errors"
	"fmt"
)

/* ChangePassphrase rotates the SECOND layer of one key and leaves the first alone:
 * the 24 words, the key's ID and its address are untouched, and each host's DEK
 * stays wrapped by the same Main Key.
 *
 * It is not a field edit, because a passphrase is not a field. The body key of a
 * host is HKDF(DEK ‖ activation key), and the activation key is derived from the
 * passphrase and that host's own salt, so every host sealed by this key has to be
 * re-sealed: a new salt, a new `.adk` (or an `active` marker when the passphrase is
 * being removed), and a new generation of ciphertext. Rotating the key instead
 * (RotateKey) is the other way to change a passphrase: it changes the words too and
 * hands the hosts over.
 *
 * The whole selection is proved against the OLD passphrase before anything is
 * written, so a wrong passphrase changes nothing at all. After that the hosts are
 * re-sealed one directory at a time, which is the same per-host transaction every
 * other save uses: a failure part-way leaves each host readable by whichever
 * passphrase its own `.adk` names, and the caller sees the error.
 */
func (r *Repository) ChangePassphrase(id ID, oldPassphrase, newPassphrase string) error {
	keys, err := r.Keys()
	if err != nil {
		return err
	}
	var target KeyInfo
	found := false
	for _, key := range keys {
		if hex.EncodeToString(id[:]) == key.ID {
			target, found = key, true
		}
	}
	if !found {
		return ErrKeyNotFound
	}
	if !target.Plugged {
		/* The hosts have to be opened to be sealed again, so the key must be here. */
		return ErrKeyNotLoaded
	}
	/* The passphrase is an Argon2id input, not a stored string: it has no format to
	   enforce, and normalising it (trimming, say) would silently change the secret
	   someone typed. The API bounds its length; this does not need a second rule. */

	names := r.hostsForKey(id, 0)
	/* Step 1: prove the old passphrase against EVERY host and put the plaintext in
	   hand. Doing this before any write is what makes a wrong passphrase a no-op,
	   and it is also the only check that can fail for a reason that is not a disk
	   error. */
	if len(names) > 0 {
		passes := map[ID]string{}
		if target.Passphrase || oldPassphrase != "" {
			passes[id] = oldPassphrase
		}
		if _, err := r.Activate(names, passes); err != nil {
			return err
		}
	}
	items := make([]Item, 0, len(names))
	for _, name := range names {
		data, err := r.Read(name)
		if err != nil {
			return fmt.Errorf("%q: %w", name, err)
		}
		items = append(items, Item{Name: name, Data: data})
	}

	/* Step 2: the keychain's boolean decides which shape the next save writes —
	   `put` asks `keyHasPassphrase` when the caller passes no phrase — so removing a
	   passphrase has to say so on the key BEFORE the hosts are re-sealed, and adding
	   one has to say it after. Each ordering is the one that leaves the boolean
	   describing the hosts on disk if the save fails. */
	removing := newPassphrase == ""
	if removing {
		if err := r.setKeyPassphrase(id, false); err != nil {
			return err
		}
	}
	if len(items) > 0 {
		if err := r.put(items, true, SaveOptions{KeyID: id, Passphrase: newPassphrase}); err != nil {
			if removing {
				/* The hosts are still gated on the old passphrase; put the fact back. */
				if restoreErr := r.setKeyPassphrase(id, true); restoreErr != nil {
					return errors.Join(err, restoreErr)
				}
			}
			return err
		}
	}
	if !removing {
		return r.setKeyPassphrase(id, true)
	}
	return nil
}

// setKeyPassphrase records whether this key's hosts need a passphrase. It is a
// public boolean, so flipping it needs no secret and touches no ciphertext.
func (r *Repository) setKeyPassphrase(id ID, has bool) error {
	defer r.acquireWrite()()
	if err := r.readyKeyring(); err != nil {
		return err
	}
	stored, err := r.readKeyringFile()
	if err != nil {
		return err
	}
	index, _, err := storedKeyIndex(stored, id)
	if err != nil {
		return err
	}
	if stored.Keys[index].Passphrase == has {
		return nil
	}
	stored.Keys[index].Passphrase = has
	return r.writeKeyringFile(stored)
}
