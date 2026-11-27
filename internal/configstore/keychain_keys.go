package configstore

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

/* Keychain management: several keys in one store, each with its own mnemonic.
 *
 * A key here is a Main Key derived from 24 words, and each one carries its OWN
 * wrapper slots over those words. Key A's slots open key A and nothing else, so a
 * host sealed by key B stays shut even while A is plugged in. One store can
 * therefore hold hosts at different trust levels, and losing one key's words does
 * not cost the others.
 *
 * There is no "default key" and no index owner. Which key seals a host is a
 * property of that host, published in its envelope header, so nothing about the
 * keychain decides where a new host goes.
 */

// KeyInfo describes one key to a caller that may only show it.
type KeyInfo struct {
	ID string `json:"id"`
	// Label is the name a human gave this key. Empty is a legal state — an older
	// keychain, or a key created before anyone bothered — and callers fall back to
	// the short id rather than inventing a name.
	Label string `json:"label,omitempty"`
	// Plugged reports whether this key's material is in memory right now.
	Plugged bool `json:"plugged"`
	// Auto reports a wrapper THIS key has that this machine can present without a
	// human, so a key
	// whose plugged bit is set comes back by itself on the next start.
	Auto bool `json:"auto"`
	// Passphrase reports that this key's hosts need a passphrase to be activated.
	// The passphrase is never stored; this only says the second layer is
	// cryptographic rather than the catalog boolean.
	Passphrase bool `json:"passphrase"`
	// Hosts counts the profiles sealed by this key, so removing it can say what
	// it would strand instead of failing afterwards. The association is kept here,
	// on the key's side of it, without the key knowing WHICH hosts: the panel shows
	// the number, and the hosts themselves remain the only place that names a key.
	Hosts int `json:"hosts"`
	// PublicKey is this key's sharing address, filled in by the layer that owns the
	// sharing format. Empty when the key is not plugged: an address is derived from
	// a Main Key, so a key that is not here cannot answer at one.
	PublicKey string `json:"public_key,omitempty"`
}

const maxKeychainKeys = maxMainKeys

var (
	// ErrNotDamaged reports a discard request for a keychain that reads fine.
	ErrNotDamaged = errors.New("configstore: the keychain is readable")
	// ErrSealedProfiles reports a discard that would orphan ciphertext.
	ErrSealedProfiles = errors.New("configstore: profiles are still sealed by this keychain")
	// ErrInvalidLabel reports a name that cannot be shown beside a key.
	ErrInvalidLabel = errors.New("configstore: invalid key name")
	// ErrKeyNotFound reports an operation on a Main Key this store does not have.
	ErrKeyNotFound = errors.New("configstore: no such key")
	// ErrKeyInUse reports a removal of a key that still seals profiles.
	ErrKeyInUse = errors.New("configstore: hosts are still encrypted with this key")
	// ErrBackupUnconfirmed reports a removal without the operator confirming the
	// mnemonic is written down. Deleting a key destroys its only copy here.
	ErrBackupUnconfirmed = errors.New("configstore: confirm the mnemonic is backed up first")
	// ErrLastWrapper reports removal of a key's only remaining way in.
	ErrLastWrapper = errors.New("configstore: this is the key's last way in")
	// ErrWrapperNotFound reports removal of a wrapper kind the key does not have.
	ErrWrapperNotFound = errors.New("configstore: this key has no such wrapper")
)

// Keys lists the keychain in a stable order, by id.
func (r *Repository) Keys() ([]KeyInfo, error) {
	if r.snap().closed {
		return nil, ErrClosed
	}
	keyring := r.snap().keyring
	if keyring == nil {
		return nil, ErrNotInitialized
	}
	stored, err := r.readKeyringFile()
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotInitialized
	}
	if err != nil {
		return nil, err
	}
	usage := r.hostsPerKey()
	out := make([]KeyInfo, 0, len(stored.Keys))
	for _, key := range stored.Keys {
		var id ID
		copy(id[:], key.ID)
		_, plugged := keyring.key(id)
		out = append(out, KeyInfo{
			ID:         hex.EncodeToString(id[:]),
			Label:      key.Label,
			Plugged:    plugged,
			Auto:       keyHasMachineSlot(key),
			Passphrase: keyHasPassphrase(key),
			Hosts:      usage[id],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// NormalizeKeyLabel trims a label and refuses one that cannot be shown beside a
// key without ambiguity. An empty label is allowed: it means "not named yet", and
// the UI then prints the short id instead of inventing a name.
func NormalizeKeyLabel(label string) (string, error) {
	trimmed := strings.TrimSpace(label)
	/* EVERY KEY HAS A NAME. An empty one used to be legal, and the panel rendered it
	   as the key's short id or as "unnamed" — a row you cannot tell apart from the
	   next one, in a list whose whole job is telling keys apart. The rule lives here,
	   at the store, so every entrance agrees: the panel form, the REST route, an
	   agent's tool call. */
	if trimmed == "" {
		return "", fmt.Errorf("%w: a key needs a name", ErrInvalidLabel)
	}
	if len(trimmed) > maxKeyLabelLength {
		return "", fmt.Errorf("%w: a key name is at most %d characters", ErrInvalidLabel, maxKeyLabelLength)
	}
	for _, r := range trimmed {
		/* Control characters would break the one-line row the label is printed in;
		   nothing else is restricted, because a name is the user's word for their
		   own machine and any script is legitimate. */
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: a key name cannot contain control characters", ErrInvalidLabel)
		}
	}
	return trimmed, nil
}

// maxKeyLabelLength bounds a label so a row cannot be pushed off the panel.
const maxKeyLabelLength = 64

/* DefaultKeyLabel names the key a store is born with.
 *
 * Initialization has no human to ask — it happens when encryption is switched on
 * before any key exists, and in tests — and a key still needs a name (see
 * NormalizeKeyLabel). This is that name: ordinary, renameable, and never a
 * placeholder the panel has to special-case. */
const DefaultKeyLabel = "default"

// CreateKey derives a new key from mnemonic and adds it to the keychain, sealed
// by the supplied providers. It does not take the index over: adding a key says
// nothing about which object should be re-sealed.
func (r *Repository) CreateKey(mnemonic, passphrase string, providers ...Provider) (KeyInfo, error) {
	return r.CreateKeyWithLabel(mnemonic, passphrase, "", providers...)
}

// CreateKeyWithLabel is CreateKey with a name for the key. The name is required:
// an unnamed key is a row nobody can identify, and the panel's list is exactly the
// place where that matters (see NormalizeKeyLabel).
func (r *Repository) CreateKeyWithLabel(mnemonic, passphrase, label string, providers ...Provider) (KeyInfo, error) {
	defer r.acquireWrite()()
	/* Two different faults share one field, and only one of them may be replaced.
	   A keychain file that does not PARSE is a leftover: nobody can read it, and
	   refusing to add a key would leave a feature that can neither add nor remove.
	   A keychain that parses but whose keys cannot be opened here is not damaged at
	   all — it is waiting for the words — and deleting it would destroy the only
	   thing that can ever read the profiles it seals. */
	if loadErr := r.snap().loadErr; loadErr != nil && r.snap().keyring == nil {
		if sealed := r.SealedProfiles(); sealed > 0 {
			return KeyInfo{}, fmt.Errorf("%w: %d sealed profile(s) depend on it", ErrSealedProfiles, sealed)
		}
		/* discardDamagedKeychain assumes the writer token, which this function
		   already holds: calling the public wrapper here would take it twice and
		   deadlock the request. */
		if err := r.discardDamagedKeychain(); err != nil {
			return KeyInfo{}, err
		}
	}
	/* Deliberately not ready(): ready() requires the catalog key to be plugged, and
	   creating a key is precisely what makes that possible. A store whose keychain
	   is empty has no catalog key, so requiring one here would mean a new keychain
	   could never be created. */
	if r.snap().closed {
		return KeyInfo{}, ErrClosed
	}
	normalized, err := NormalizeMnemonic(mnemonic)
	if err != nil {
		return KeyInfo{}, err
	}
	label, err = NormalizeKeyLabel(label)
	if err != nil {
		return KeyInfo{}, err
	}
	stored, fresh, err := r.keyringFileOrNew()
	if err != nil {
		return KeyInfo{}, err
	}
	if fresh {
		/* Discarding a damaged keychain leaves no file behind, so this call has to
		   be able to start one: everything below assumes a keyring to add to. A
		   keychain with keys in it encrypts new profiles by default — the opposite
		   would let a store with a working keychain save hosts in plaintext without
		   anyone choosing that. */
		enabled := true
		stored.EncryptNew = &enabled
		keyring := &Keyring{}
		keyring.withState(idOrZero(stored.StoreID), map[ID]MainKey{}, nil)
		r.mutate(func(next *repoSnapshot) {
			next.keyring = keyring
			next.encryptNew = true
		})
	}
	if len(stored.Keys) >= maxKeychainKeys {
		return KeyInfo{}, fmt.Errorf("%w: the keychain is full", ErrTooLarge)
	}
	main, err := deriveMainKey(idOrZero(stored.StoreID), normalized)
	if err != nil {
		return KeyInfo{}, err
	}
	defer clear(main[:])
	id, err := NewID()
	if err != nil {
		return KeyInfo{}, err
	}
	key, wrapped, err := sealKeySlots(stored, id, normalized, providers...)
	if err != nil {
		return KeyInfo{}, err
	}
	key.Passphrase = passphrase != ""
	key.Label = label
	if wrapped == 0 {
		/* No provider here can seal the words, so the key would exist in memory
		   only and vanish on restart — a "key" nobody can ever open again. */
		return KeyInfo{}, ErrProviderUnavailable
	}
	key.Plugged = true
	stored.Keys = append(stored.Keys, key)
	if err := r.writeKeyringFile(stored); err != nil {
		return KeyInfo{}, err
	}
	/* Publish after the file: a key in memory but not on disk would vanish on the
	   next start while this session kept sealing profiles with it. */
	r.snap().keyring.withKey(id, main)
	return KeyInfo{
		ID:         hex.EncodeToString(id[:]),
		Label:      key.Label,
		Plugged:    true,
		Auto:       keyHasMachineSlot(key),
		Passphrase: keyHasPassphrase(key),
	}, nil
}

// PlugKey brings one key into memory using the supplied providers and records
// `plugged` so the next start does the same by itself. Only that key becomes
// readable; the others stay exactly as they were.
func (r *Repository) PlugKey(id ID, providers ...Provider) error {
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
	_, main, err := unsealKey(stored, id, providers...)
	if err != nil {
		return err
	}
	defer clear(main[:])
	stored.Keys[index].Plugged = true
	if err := r.writeKeyringFile(stored); err != nil {
		return err
	}
	r.snap().keyring.withKey(id, main)
	// One key being reachable is enough for the keychain to be usable again, so the
	// "nothing could be opened" fault of the last start no longer applies.
	r.mutate(func(next *repoSnapshot) { next.loadErr = nil })
	/* Inserting a key does not touch any other host: each host's second layer is
	   its own `.adk` / `active` file, so only this key's hosts change state. */
	return r.loadRefs()
}

/* PlugKeyWithPassphrase inserts one key AND turns on the second layer of every host
 * it seals, in the same step.
 *
 * The two used to be separate decisions, and the first one alone leaves a state that
 * looks finished and is not: the panel says the key is loaded, every host it seals
 * says "not activated", and nothing can be read. A passphrase-carrying key needs its
 * passphrase for exactly one of those steps, so it is asked once and spent on both.
 *
 * A wrong passphrase changes nothing: the insert is rolled back, because "loaded but
 * unable to open anything" is the state this method exists to remove — a person who
 * typed it wrong should be left where they started, not half way.
 *
 * A key with no passphrase needs nothing here: inserting it restores its hosts from
 * their `active` markers, which is what the boolean layer is.
 */
func (r *Repository) PlugKeyWithPassphrase(id ID, passphrase string, providers ...Provider) error {
	if err := r.PlugKey(id, providers...); err != nil {
		return err
	}
	stored, err := r.readKeyringFile()
	if err != nil {
		return err
	}
	key, known := storedKeyByID(stored, id)
	if !known || !keyHasPassphrase(key) {
		return nil
	}
	names := r.hostsForKey(id, 0)
	if len(names) == 0 {
		/* Nothing to activate, and nothing to prove the passphrase against either:
		   a key that seals no host has no second layer yet. */
		return nil
	}
	if _, err := r.Activate(names, map[ID]string{id: passphrase}); err != nil {
		if rollback := r.UnplugKey(id); rollback != nil {
			return errors.Join(err, rollback)
		}
		return err
	}
	return nil
}

// UnplugKey drops one key's material from memory and records `plugged=false`.
// Profiles sealed by it stop being readable until it is plugged in again; nothing
// is deleted, and its activation keys go with it — the second layer of a host
// nobody can read is not worth keeping.
func (r *Repository) UnplugKey(id ID) error {
	defer r.acquireWrite()()
	if err := r.readyKeyring(); err != nil {
		return err
	}
	keyring := r.snap().keyring
	if _, ok := keyring.key(id); !ok {
		if _, _, err := r.keyringFileKey(id); err != nil {
			return err
		}
		return ErrKeyNotLoaded
	}
	stored, err := r.readKeyringFile()
	if err != nil {
		return err
	}
	if index, _, err := storedKeyIndex(stored, id); err == nil {
		stored.Keys[index].Plugged = false
		if err := r.writeKeyringFile(stored); err != nil {
			return err
		}
	}
	/* Unplugging one key cannot affect another key's hosts: there is no shared
	   index for it to take with it. */
	keyring.withoutKey(id)
	return r.loadRefs()
}

// keyringFileKey reports whether the keychain file knows this key at all, which
// is what separates "not in this store" from "not plugged in".
func (r *Repository) keyringFileKey(id ID) (storedKeyring, storedKey, error) {
	stored, err := r.readKeyringFile()
	if err != nil {
		return storedKeyring{}, storedKey{}, err
	}
	_, key, err := storedKeyIndex(stored, id)
	return stored, key, err
}

// AddUnlockTo gives one key another way to be plugged. A provider of a kind the
// key already has replaces that slot: the slots all seal the same words, so a
// second slot of one kind is the same path twice.
func (r *Repository) AddUnlockTo(id ID, provider Provider) error {
	if provider == nil {
		return ErrProviderUnavailable
	}
	defer r.acquireWrite()()
	/* ready() also demands the catalog index be readable. Key management does not
	   need the index — only the keychain file — and requiring it would make the
	   keys unmanageable exactly when the index is what is broken. */
	if err := r.readyKeyring(); err != nil {
		return err
	}
	stored, err := r.readKeyringFile()
	if err != nil {
		return err
	}
	index, key, err := storedKeyIndex(stored, id)
	if err != nil {
		return err
	}
	/* The mnemonic comes from the key's own slots, which is the only place it is:
	   wrapping a key requires holding it, and asking for the words again is the
	   price of starting a fresh wrapper on a key that is not in memory. */
	phrase, main, err := unsealKey(stored, id, providersFromKeyring(r)...)
	clear(main[:])
	if err != nil {
		return ErrKeyNotLoaded
	}
	replaced := key.Slots[:0]
	for _, slot := range key.Slots {
		if slot.Provider != provider.providerName() {
			replaced = append(replaced, slot)
		}
	}
	slot, err := sealSlot(stored, id, phrase, provider)
	if err != nil {
		return err
	}
	key.Slots = append(replaced, slot)
	stored.Keys[index] = key
	return r.writeKeyringFile(stored)
}

// RemoveUnlockFrom drops every slot of one kind from one key.
func (r *Repository) RemoveUnlockFrom(id ID, kind string) error {
	defer r.acquireWrite()()
	/* No plugged key is required: dropping a slot rewrites the wrapper list and
	   never touches key material, so a key that is not plugged can still be
	   tidied. Requiring it would make "remove the provider I lost" impossible in
	   exactly the state that needs it. */
	if err := r.readyKeyring(); err != nil {
		return err
	}
	stored, err := r.readKeyringFile()
	if err != nil {
		return err
	}
	index, key, err := storedKeyIndex(stored, id)
	if err != nil {
		return err
	}
	remaining := make([]storedKeySlot, 0, len(key.Slots))
	for _, slot := range key.Slots {
		if slot.Provider != kind {
			remaining = append(remaining, slot)
		}
	}
	if len(remaining) == len(key.Slots) {
		return ErrWrapperNotFound
	}
	if len(remaining) == 0 {
		// The last wrapper is the last way to open this key. Removing it strands
		// every host sealed by the key without deleting anything.
		return ErrLastWrapper
	}
	key.Slots = remaining
	stored.Keys[index] = key
	return r.writeKeyringFile(stored)
}

// RemoveKey drops a key that seals no profiles.
//
// Deleting is destruction, not disabling: the slots go with it, so the words on
// paper become the only copy. That is why the caller has to say the backup was
// confirmed. Removing the last key with nothing sealed by it turns encryption
// off: there is no ciphertext to strand, and "delete the only key" can only
// honestly mean that.
/* SetKeyLabel renames one key. A label is public — it sits in the keychain file in
 * clear so it can be read while nothing is inserted — and renaming it therefore
 * touches no ciphertext: no envelope is re-sealed, and no host notices.
 */
func (r *Repository) SetKeyLabel(id ID, label string) error {
	label, err := NormalizeKeyLabel(label)
	if err != nil {
		return err
	}
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
	stored.Keys[index].Label = label
	return r.writeKeyringFile(stored)
}

func (r *Repository) RemoveKey(id ID, backupConfirmed bool) error {
	defer r.acquireWrite()()
	/* ready() also demands the catalog index be readable. Key management does not
	   need the index — only the keychain file — and requiring it would make the
	   keys unmanageable exactly when the index is what is broken. */
	if err := r.readyKeyring(); err != nil {
		return err
	}
	if !backupConfirmed {
		return ErrBackupUnconfirmed
	}
	stored, err := r.readKeyringFile()
	if err != nil {
		return err
	}
	index, _, err := storedKeyIndex(stored, id)
	if err != nil {
		return err
	}
	usage := r.hostsPerKey()
	if usage[id] > 0 {
		/* The names, not just the count: "3 hosts still use it" leaves the person
		   to go looking for which three, and the panel that would show them is the
		   one they are standing in. Bounded, because an error is not a listing. */
		names := r.hostsForKey(id, maxKeyInUseNames)
		if len(names) > 0 {
			return fmt.Errorf("%w: %s", ErrKeyInUse, strings.Join(names, ", "))
		}
		return fmt.Errorf("%w: %d host(s) still use it", ErrKeyInUse, usage[id])
	}
	if len(stored.Keys) == 1 {
		return r.destroyKeyring()
	}
	remaining := make([]storedKey, 0, len(stored.Keys)-1)
	for i, key := range stored.Keys {
		if i != index {
			remaining = append(remaining, key)
		}
	}
	keyring := r.snap().keyring
	keyring.withoutKey(id)
	stored.Keys = remaining
	if err := r.writeKeyringFile(stored); err != nil {
		return err
	}
	return nil
}

// ExportMnemonic returns one key's 24 words. The slots are the only copy, and this
// machine can already open them, so no passphrase is asked for: "export" means
// what the user expects, and it is the same ability as using the key here.
func (r *Repository) ExportMnemonic(id ID) (string, error) {
	if r.snap().closed {
		return "", ErrClosed
	}
	if r.snap().keyring == nil {
		return "", ErrNotInitialized
	}
	stored, err := r.readKeyringFile()
	if err != nil {
		return "", err
	}
	phrase, main, err := unsealKey(stored, id, DefaultProviders()...)
	clear(main[:])
	if err != nil {
		return "", err
	}
	return phrase, nil
}

/* RotateKey gives a key new words. It is not a re-encryption: the DEK and the body
 * ciphertext stay byte-identical, and only the wrapper that holds the DEK changes
 * hands. That is what makes rotation cheap — no host is decrypted — but it is also
 * why it cannot be done in place. Re-wrapping the objects first would leave them
 * unreadable under the keychain file still on disk, and rewriting the keychain
 * first would orphan every object at once. There is no instant at which half of a
 * two-file swap is correct.
 *
 * So rotation is built out of three operations that are each safe on their own:
 *
 *   1. add a second key, with the new words and the same label
 *   2. hand every object over one at a time (rewrap only: body untouched)
 *   3. delete the old key, which is refused until it seals nothing
 *
 * A crash leaves both keys present and every object readable by exactly one of
 * them, which is a state the panel can simply show.
 */
func (r *Repository) RotateKey(id ID, passphrase string, providers ...Provider) (KeyInfo, string, error) {
	keys, err := r.Keys()
	if err != nil {
		return KeyInfo{}, "", err
	}
	var old KeyInfo
	found := false
	for _, key := range keys {
		if hex.EncodeToString(id[:]) == key.ID {
			old, found = key, true
		}
	}
	if !found {
		return KeyInfo{}, "", ErrKeyNotFound
	}
	if !old.Plugged {
		/* Both ends are needed: the old key to unwrap each DEK, the new one to seal
		   it again. Half a rotation would leave objects split across two keys. */
		return KeyInfo{}, "", ErrKeyNotLoaded
	}
	phrase, err := NewMnemonic()
	if err != nil {
		return KeyInfo{}, "", err
	}
	/* The passphrase travels with the rotation. It used to be dropped silently:
	   the new key came out without a second layer, and nothing said so — a security
	   property changed as a side effect of an unrelated action. The caller now
	   supplies one (the old one, or a new one), and an empty string means the person
	   asked for a key with no passphrase. */
	created, err := r.CreateKeyWithLabel(phrase, passphrase, old.Label, providers...)
	if err != nil {
		return KeyInfo{}, "", err
	}
	newID, err := hex.DecodeString(created.ID)
	if err != nil {
		return KeyInfo{}, "", ErrFormat
	}
	var next ID
	copy(next[:], newID)
	if err := r.handObjectsTo(id, next); err != nil {
		return KeyInfo{}, "", err
	}
	if err := r.RemoveKey(id, true); err != nil {
		/* The objects have already moved. Reporting the failure is right: the old
		   key still exists and still opens nothing, and the panel shows both, so a
		   retry is a delete rather than a second rotation. */
		return created, phrase, err
	}
	return created, phrase, nil
}

// handObjectsTo rewraps every object sealed by from so that it is sealed by to.
// The bodies are not touched, so no activation key is involved: this moves the
// wrapper that holds the DEK, nothing else.
func (r *Repository) handObjectsTo(from, to ID) error {
	defer r.acquireWrite()()
	if err := r.usable(); err != nil {
		return err
	}
	if err := r.loadRefs(); err != nil {
		return err
	}
	refs := r.snap().refs
	for _, ref := range refs {
		raw, err := r.readObjectRaw(ref)
		if err != nil {
			return err
		}
		envelope, err := Parse(raw, Identity{
			StoreID:      r.snap().keyring.StoreID(),
			ObjectID:     ref.objectID,
			GenerationID: ref.generationID,
			Kind:         KindProfile,
		})
		if err != nil {
			return err
		}
		if envelope.WrapKeyID() != from {
			continue
		}
		rewrapped, err := r.snap().keyring.RewrapTo(envelope, to)
		if err != nil {
			return err
		}
		if err := writeFileAtomic(r.objectPath(ref), rewrapped.Bytes()); err != nil {
			return err
		}
		/* The activation marker is sealed with the same Main Key, so it moves too;
		   leaving it would silently deactivate the host. */
		if ref.activated {
			updated := ref
			updated.keyID = to
			if _, err := r.readActivation(ref); err != nil {
				if err := r.writeActiveMarker(updated); err != nil {
					return err
				}
			}
		}
	}
	return r.loadRefs()
}

// destroyKeyring removes encryption entirely. It is only reached when no profile
// is sealed by the remaining key, so no host data is lost.
func (r *Repository) destroyKeyring() error {
	if err := os.Remove(r.keyringPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if cur := r.snap().keyring; cur != nil {
		cur.Destroy()
	}
	r.mutate(func(next *repoSnapshot) {
		next.keyring = nil
		next.refs = nil
		next.encryptNew = false
	})
	return nil
}

// KeyringDetached reports that encryption was switched off by a keychain
// operation, so callers can tell "no encryption" from "this repository broke".
func (r *Repository) KeyringDetached() bool {
	return !r.snap().closed && r.snap().keyring == nil
}

// readKeyringFile reads and parses config.keys.
func (r *Repository) readKeyringFile() (storedKeyring, error) {
	data, err := readBoundedFile(r.keyringPath(), maxKeyringFile)
	if err != nil {
		return storedKeyring{}, err
	}
	return parseStoredKeyring(data)
}

func (r *Repository) writeKeyringFile(stored storedKeyring) error {
	data, err := encodeStoredKeyring(stored)
	if err != nil {
		return err
	}
	return writeFileAtomic(r.keyringPath(), data)
}

func (r *Repository) keyringPath() string {
	return filepath.Join(r.dataDir, "keys", "config.keys")
}

// hostsPerKey counts the profiles each key seals. It reads the PUBLIC key id out
// of each envelope header, so the answer does not depend on any key being plugged:
// "which key would this delete strand" has to be answerable in exactly the state
// where nobody is plugged in, or the guard is a promise the panel cannot keep.
// maxKeyInUseNames bounds the names carried in a refusal: enough to act on, short
// enough that the message is still a sentence.
const maxKeyInUseNames = 8

// hostsForKey lists the profiles sealed by one key, sorted so the same refusal
// reads the same way twice. It walks the same places hostsPerKey does, because a
// count and a list that disagree would be worse than neither.
func (r *Repository) hostsForKey(id ID, limit int) []string {
	seen := map[string]bool{}
	var names []string
	add := func(dir, path string) {
		if seen[dir] {
			return
		}
		raw, err := readBoundedFile(path, int64(envelopeHeaderSize+MaxProfilePlaintext+chacha20poly1305.Overhead))
		if err != nil {
			return
		}
		if keyID, ok := envelopeKeyID(raw); !ok || keyID != id {
			return
		}
		seen[dir] = true
		names = append(names, filepath.Base(dir))
	}
	for _, ref := range r.snap().refs {
		add(filepath.Dir(r.objectPath(ref)), r.objectPath(ref))
	}
	root := filepath.Join(r.dataDir, "ssh_configs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "objects" {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			if !file.IsDir() && strings.HasSuffix(file.Name(), ".enc") {
				add(filepath.Join(root, entry.Name()), filepath.Join(root, entry.Name(), file.Name()))
			}
		}
	}
	sort.Strings(names)
	if limit > 0 && len(names) > limit {
		names = append(names[:limit], fmt.Sprintf("…+%d", len(names)-limit))
	}
	return names
}

func (r *Repository) hostsPerKey() map[ID]int {
	out := map[ID]int{}
	seen := map[string]bool{}
	add := func(path string) {
		raw, err := readBoundedFile(path, int64(envelopeHeaderSize+MaxProfilePlaintext+chacha20poly1305.Overhead))
		if err != nil {
			return
		}
		keyID, ok := envelopeKeyID(raw)
		if !ok {
			return
		}
		dir := filepath.Dir(path)
		if seen[dir] {
			return
		}
		seen[dir] = true
		out[keyID]++
	}
	for _, ref := range r.snap().refs {
		add(r.objectPath(ref))
	}
	/* A readable catalog is a cache, not the source: the same headers are on disk
	   for every profile the catalog does not list (unplugged index, damaged index,
	   hand-copied directory). */
	root := filepath.Join(r.dataDir, "ssh_configs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "objects" {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			if !file.IsDir() && strings.HasSuffix(file.Name(), ".enc") {
				add(filepath.Join(root, entry.Name(), file.Name()))
			}
		}
	}
	return out
}

// storedKeyByID finds one key in a parsed keychain file.
func storedKeyByID(stored storedKeyring, id ID) (storedKey, bool) {
	_, key, err := storedKeyIndex(stored, id)
	return key, err == nil
}

// storedKeyForKey is storedKeyByID against the keychain file this repository is
// working from. It is the only place the passphrase flag can be read: it is a file
// property, not something the in-memory keyring carries.
func storedKeyForKey(r *Repository, id ID) storedKey {
	stored, err := r.readKeyringFile()
	if err != nil {
		return storedKey{}
	}
	key, _ := storedKeyByID(stored, id)
	return key
}

func storedKeyIndex(stored storedKeyring, id ID) (int, storedKey, error) {
	for i, key := range stored.Keys {
		var keyID ID
		copy(keyID[:], key.ID)
		if keyID == id {
			return i, key, nil
		}
	}
	return 0, storedKey{}, ErrKeyNotFound
}

// keyHasSlot reports one wrapper kind on one key.
func keyHasSlot(key storedKey, kind string) bool {
	for _, slot := range key.Slots {
		if slot.Provider == kind {
			return true
		}
	}
	return false
}

// keyHasPassphrase reports whether this key's hosts need a passphrase to activate.
func keyHasPassphrase(key storedKey) bool { return key.Passphrase }

// keyHasMachineSlot reports a wrapper this machine can present with no human in
// the loop, which is what "comes back by itself on the next start" means.
func keyHasMachineSlot(key storedKey) bool {
	for _, slot := range key.Slots {
		if !isHumanWrapper(slot.Provider) {
			return true
		}
	}
	return false
}

// isHumanWrapper reports the providers somebody typed something into. In this
// format the only such provider is gone (the phrase is the root, not a wrapper),
// but the classification still has to exist: a key with nothing machine-openable
// cannot honour `plugged=true` on the next start.
func isHumanWrapper(name string) bool {
	return name == "password" || name == "mnemonic"
}

// IsHumanProvider reports a wrapper a person presents, as opposed to one this
// machine presents by itself.
func IsHumanProvider(name string) bool { return isHumanWrapper(name) }

// ProviderName reports the provider's slot name ("dpapi", "env", …), which is the
// only vocabulary the keychain file records.
func ProviderName(provider Provider) string {
	if provider == nil {
		return ""
	}
	return provider.providerName()
}

func lessID(a, b ID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// readyKeyring requires an open repository that has a keychain file, plugged or
// not: reading and editing the file is how a key gets plugged in the first place.
func (r *Repository) readyKeyring() error {
	st := r.snap()
	if st.closed {
		return ErrClosed
	}
	if st.keyring == nil {
		return ErrNotInitialized
	}
	return nil
}

// SealedProfiles counts the profiles that would be lost if the keychain file were
// discarded. With a damaged keychain the catalog cannot be read, so the count comes
// from the ciphertext directories on disk: a directory holding a *.enc file is a
// profile that was sealed and can only be opened by a key this file no longer
// provides.
func (r *Repository) SealedProfiles() int {
	root := filepath.Join(r.dataDir, "ssh_configs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "objects" {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, entry.Name()))
		if err != nil {
			continue
		}
		for _, file := range files {
			if !file.IsDir() && strings.HasSuffix(file.Name(), ".enc") {
				count++
				break
			}
		}
	}
	return count
}

// DiscardDamagedKeychain removes a keychain file this build cannot read, so the
// store can start again with an empty keychain. It is the explicit human exit for
// a damaged file: without it the panel can only say "broken" and wait for someone
// to guess that adding a key would replace it.
//
// It refuses while any profile is still sealed, and it refuses a keychain that
// reads fine — this is an escape hatch for data that is already unreadable, not a
// "delete my keys" button. Deleting readable keys has its own route with its own
// guards (§9.3).
func (r *Repository) DiscardDamagedKeychain() error {
	defer r.acquireWrite()()
	return r.discardDamagedKeychain()
}

// discardDamagedKeychain does the work above under a writer token the caller
// already holds.
func (r *Repository) discardDamagedKeychain() error {
	st := r.snap()
	if st.closed {
		return ErrClosed
	}
	if st.loadErr == nil {
		return ErrNotDamaged
	}
	if sealed := r.SealedProfiles(); sealed > 0 {
		return fmt.Errorf("%w: %d sealed profile(s) would become unreadable", ErrSealedProfiles, sealed)
	}
	if err := os.Remove(filepath.Join(r.dataDir, "keys", "config.keys")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	/* An old index was sealed by a key this file no longer provides, so it is moved
	   aside rather than left to veto the store. The host directories are the
	   authoritative record; the bytes are kept, not destroyed. */
	catalog := legacyCatalogPath(r.dataDir)
	if _, err := os.Stat(catalog); err == nil {
		aside := catalog + ".unreadable"
		_ = os.Remove(aside)
		if err := os.Rename(catalog, aside); err != nil {
			return err
		}
	}
	r.mutate(func(next *repoSnapshot) {
		next.loadErr = nil
		next.encryptNew = false
		next.refs = map[string]catalogRef{}
	})
	return nil
}

// keyringFileOrNew reads the keychain file, or returns an empty keychain for this
// data directory when there is none. fresh reports which happened, so the caller
// knows it must publish an in-memory keyring as well.
func (r *Repository) keyringFileOrNew() (storedKeyring, bool, error) {
	stored, err := r.readKeyringFile()
	if err == nil {
		return stored, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return storedKeyring{}, false, err
	}
	storeID, err := NewID()
	if err != nil {
		return storedKeyring{}, false, err
	}
	return storedKeyring{
		Version: keyringVersion,
		StoreID: append([]byte(nil), storeID[:]...),
		Keys:    []storedKey{},
	}, true, nil
}

func idOrZero(raw []byte) ID {
	var id ID
	copy(id[:], raw)
	return id
}

// providersFromKeyring is the chain key management uses when it has to re-open a
// key's words to seal a new wrapper: exactly the providers this host can present.
func providersFromKeyring(r *Repository) []Provider {
	return DefaultProviders()
}
