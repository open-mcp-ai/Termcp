package configstore

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync/atomic"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	keyringVersion   uint16 = 2
	maxKeyringFile          = 64 << 10
	maxProviderSlots        = 8
	/* maxMainKeys keeps the keyring file bounded. A keychain holds several keys on
	   purpose; there is no rotation any more, so this is only a sanity limit. */
	maxMainKeys = 32
	// maxMnemonicBytes bounds an unsealed slot before BIP39 validation runs.
	maxMnemonicBytes = 512
)

var keyringDomain = []byte("termcp/config/keyring/v1")

// ErrKeyNotLoaded reports an object whose Main Key is not plugged in. The key
// still exists in the keychain; plugging it is what makes the object readable.
var ErrKeyNotLoaded = errors.New("configstore: the key that opens this object is not loaded")

/* The keyring file is a keychain, and every key in it has exactly ONE root: the
 * mnemonic a person wrote down.
 *
 *   {"version":2,"store_id":…,"masked":false,
 *    "keys":[{"id":…,"plugged":true,"slots":[{provider,parameters,nonce,wrapped}…]}…]}
 *
 * There is no `catalog_key_id` any more: there is no index for a key to own.
 * Version 2 files that still carry one are accepted and the field is ignored;
 * writing drops it.
 *
 * A slot seals THOSE WORDS under one Provider KEK — this machine's credential
 * store, a mounted KEK file, an environment variable. They are several ways to
 * open the same secret, not several secrets: the Main Key is derived from the
 * words, so exporting the mnemonic and using this machine are the same thing.
 *
 * That is why there is no "mnemonic slot": the phrase is the root, not a wrapper
 * around it. A key whose slots no provider here can open is FAILED — unreadable
 * until someone re-imports the words — which is a state the file records rather
 * than hides.
 */
type storedKeyring struct {
	Version uint16 `json:"version"`
	StoreID []byte `json:"store_id"`
	// CatalogKeyID is accepted from version 2 files and otherwise unused. There
	// is no index, so no key owns one.
	CatalogKeyID []byte      `json:"catalog_key_id,omitempty"`
	Keys         []storedKey `json:"keys"`
	/* EncryptNew says whether a NEW profile is sealed. It lives here, beside the
	   keys, because it has to be answerable when no key is plugged: the catalog is
	   encrypted, so a preference stored there would be unknown exactly in the state
	   the panel offers on purpose. It is not a secret — the keychain file already
	   says encryption is on. */
	EncryptNew *bool `json:"encrypt_new,omitempty"`
	/* Masked is the global address/username masking switch (§10.2). It is a
	   display-layer setting, so it is stored in clear and affects serialization
	   only. */
	Masked bool `json:"masked,omitempty"`
}

type storedKey struct {
	ID []byte `json:"id"`
	/* Label is what a human calls this key. It sits in clear, next to the plugged
	   bit, for the same reason: it has to be readable when nothing is inserted —
	   a keychain you cannot identify is a keychain you cannot decide about. That
	   also means a label is NOT a secret, so the UI asks for a name and says so. */
	Label string `json:"label,omitempty"`
	/* Plugged is a public boolean: it says whether the next start inserts this key
	   by itself. It encrypts nothing, so flipping it needs no secret. */
	Plugged bool `json:"plugged"`
	/* Passphrase is a public boolean too: it says that this key's hosts need a
	   passphrase to be activated. The passphrase itself is NEVER stored — one per
	   key, never on disk (§2.1) — so this only records that the second layer is
	   cryptographic for this key rather than the catalog boolean a key without one
	   uses. */
	Passphrase bool            `json:"passphrase,omitempty"`
	Slots      []storedKeySlot `json:"slots"`
}

type storedKeySlot struct {
	ID         []byte             `json:"id"`
	Provider   string             `json:"provider"`
	Parameters providerParameters `json:"parameters"`
	Nonce      []byte             `json:"nonce"`
	Wrapped    []byte             `json:"wrapped"`
}

// keyringState captures one immutable generation of plugged-in key material.
// Plugging or unplugging publishes a replacement; reading reads the pointer.
type keyringState struct {
	storeID  ID
	mainKeys map[ID]MainKey
	/* activation maps a key to the activation keys it currently holds, by object
	   id. They are per HOST, because each host has its own salt: one passphrase
	   under one key derives as many activation keys as that key seals hosts. */
	activation map[ID]map[ID][32]byte
}

// Keyring owns the key material that is in memory. It does not expose its bytes
// to callers; profile persistence can only ask it to seal or open an Envelope.
//
// A key absent from the map is not lost — it is still in the keyring file, and
// objects it sealed answer ErrKeyNotLoaded until it is plugged in again.
type Keyring struct {
	state atomic.Pointer[keyringState]
}

// NewKeyMaterial returns a fresh 24-word phrase and the Main Key it derives.
func NewKeyMaterial(storeID ID) (string, MainKey, error) {
	if storeID.IsZero() {
		return "", MainKey{}, ErrFormat
	}
	phrase, err := NewMnemonic()
	if err != nil {
		return "", MainKey{}, err
	}
	main, err := deriveMainKey(storeID, phrase)
	if err != nil {
		return "", MainKey{}, err
	}
	return phrase, main, nil
}

/* deriveMainKey is the only place a Main Key comes from. The mnemonic is the
 * whole input: the Store ID is the salt, so the same words in another store are a
 * different key and one store's file cannot be replayed into another.
 */
func deriveMainKey(storeID ID, mnemonic string) (MainKey, error) {
	var main MainKey
	normalized, err := NormalizeMnemonic(mnemonic)
	if err != nil {
		return main, err
	}
	reader := hkdf.New(sha256.New, []byte(normalized), storeID[:], []byte(mainKeyDomain))
	if _, err := io.ReadFull(reader, main[:]); err != nil {
		return main, fmt.Errorf("derive Main Key: %w", err)
	}
	return main, nil
}

// CreateKeyring creates a keychain holding one key whose root is mnemonic, sealed
// by every provider that can address this host. The returned bytes are the
// config.keys representation.
func CreateKeyring(storeID ID, mnemonic, passphrase, label string, providers ...Provider) ([]byte, *Keyring, error) {
	if storeID.IsZero() {
		return nil, nil, ErrFormat
	}
	/* The key a store is born with carries a name like every other key. An
	   initialization has no human to ask, so it uses DefaultKeyLabel — a name the
	   owner can change on the first look, which is a better answer than a row that
	   says "unnamed" forever (see NormalizeKeyLabel). */
	if label == "" {
		label = DefaultKeyLabel
	}
	label, err := NormalizeKeyLabel(label)
	if err != nil {
		return nil, nil, err
	}
	main, err := deriveMainKey(storeID, mnemonic)
	if err != nil {
		return nil, nil, err
	}
	defer clear(main[:])
	normalized, err := NormalizeMnemonic(mnemonic)
	if err != nil {
		return nil, nil, err
	}
	keyID, err := NewID()
	if err != nil {
		return nil, nil, err
	}
	stored := newStoredKeyring(storeID, keyID)
	key, wrapped, err := sealKeySlots(stored, keyID, normalized, providers...)
	if err != nil {
		return nil, nil, err
	}
	if wrapped == 0 {
		return nil, nil, ErrProviderUnavailable
	}
	key.Plugged = true
	key.Passphrase = passphrase != ""
	key.Label = label
	stored.Keys = append(stored.Keys, key)
	data, err := encodeStoredKeyring(stored)
	if err != nil {
		return nil, nil, err
	}
	keyring := &Keyring{}
	keyring.withState(storeID, map[ID]MainKey{keyID: main}, nil)
	return data, keyring, nil
}

func newStoredKeyring(storeID, firstKeyID ID) storedKeyring {
	return storedKeyring{
		Version: keyringVersion,
		StoreID: append([]byte(nil), storeID[:]...),
	}
}

// keyringFileOptions is the part of config.keys that is not key material: the two
// preferences a person set, readable with nothing plugged in.
type keyringFileOptions struct {
	EncryptNew bool
	Masked     bool
}

// LoadKeyring parses a keychain file and plugs in every key one of the providers
// can unwrap AND that was plugged when the process last stopped. The returned
// Keyring is usable even when nothing could be plugged — it still knows the store
// and catalog key ids — and the error then reports why.
func LoadKeyring(data []byte, providers ...Provider) (*Keyring, error) {
	keyring, _, err := LoadKeyringWithOptions(data, providers...)
	return keyring, err
}

// LoadKeyringWithOptions also reports the stored preferences. They live with the
// keys precisely so they can be read here, in the same call that plugs the keys
// in — no key required, no second file to keep in step with this one.
func LoadKeyringWithOptions(data []byte, providers ...Provider) (*Keyring, keyringFileOptions, error) {
	stored, err := parseStoredKeyring(data)
	if err != nil {
		return nil, keyringFileOptions{}, err
	}
	var storeID ID
	copy(storeID[:], stored.StoreID)
	options := keyringFileOptions{EncryptNew: stored.encryptNewPreference(), Masked: stored.Masked}
	keys := make(map[ID]MainKey, len(stored.Keys))
	var lastErr error
	for _, key := range stored.Keys {
		if !key.Plugged {
			/* Unplugged is a decision, not a failure: the material stays out of
			   memory until a person inserts the key again. */
			continue
		}
		var id ID
		copy(id[:], key.ID)
		_, main, err := openKeySlots(stored, key, providers...)
		if err != nil {
			lastErr = err
			continue
		}
		keys[id] = main
	}
	keyring := &Keyring{}
	keyring.withState(storeID, keys, nil)
	if len(keys) == 0 && lastErr != nil {
		return keyring, options, lastErr
	}
	return keyring, options, nil
}

/*
encryptNewPreference answers "seal new profiles by default?". A keychain that

	does not say means yes: a store with keys in it is a store where someone chose
	encryption, and defaulting the other way would quietly start writing hosts in
	plaintext.
*/
func (stored storedKeyring) encryptNewPreference() bool {
	if stored.EncryptNew == nil {
		return true
	}
	return *stored.EncryptNew
}

// unsealKey returns one key's mnemonic and Main Key. It is what both plugging and
// exporting use, so "the words that open this key" has one implementation.
func unsealKey(stored storedKeyring, id ID, providers ...Provider) (string, MainKey, error) {
	for _, key := range stored.Keys {
		var keyID ID
		copy(keyID[:], key.ID)
		if keyID != id {
			continue
		}
		return openKeySlots(stored, key, providers...)
	}
	return "", MainKey{}, ErrKeyNotFound
}

// openKeySlots tries every provider against the key's own slots. A matched
// Provider with failing authentication is ErrAuthentication; a key whose slots
// nobody in the chain can address is ErrProviderUnavailable.
func openKeySlots(stored storedKeyring, key storedKey, providers ...Provider) (string, MainKey, error) {
	var keyID ID
	copy(keyID[:], key.ID)
	var storeID ID
	copy(storeID[:], stored.StoreID)
	matchedProvider := false
	derivedProviderKEK := false
	for _, slot := range key.Slots {
		for _, provider := range providers {
			if provider == nil || provider.providerName() != slot.Provider {
				continue
			}
			matchedProvider = true
			if err := provider.validateParameters(slot.Parameters); err != nil {
				continue
			}
			kek, err := provider.unwrapKey(slot.Parameters)
			if err != nil {
				continue
			}
			derivedProviderKEK = true
			aead, err := chacha20poly1305.NewX(kek[:])
			clear(kek[:])
			if err != nil {
				continue
			}
			plain, err := aead.Open(nil, slot.Nonce, slot.Wrapped, keyringSlotAAD(stored, keyID, slot))
			if err != nil || len(plain) == 0 || len(plain) > maxMnemonicBytes {
				clear(plain)
				continue
			}
			phrase := string(plain)
			clear(plain)
			/* A slot that authenticates but does not hold a valid phrase is a
			   format error, not a wrong KEK: the AAD already proved this slot
			   belongs here. Refusing keeps a corrupt file from deriving a key
			   nobody could ever reproduce from paper. */
			main, err := deriveMainKey(storeID, phrase)
			if err != nil {
				continue
			}
			return phrase, main, nil
		}
	}
	if !matchedProvider || !derivedProviderKEK {
		return "", MainKey{}, ErrProviderUnavailable
	}
	return "", MainKey{}, ErrAuthentication
}

// sealKeySlots wraps one key's mnemonic under every provider this host can use.
func sealKeySlots(stored storedKeyring, keyID ID, mnemonic string, providers ...Provider) (storedKey, int, error) {
	key := storedKey{ID: append([]byte(nil), keyID[:]...)}
	wrapped := 0
	for _, provider := range providers {
		if provider == nil {
			return storedKey{}, 0, ErrFormat
		}
		if len(key.Slots) >= maxProviderSlots {
			return storedKey{}, 0, ErrTooLarge
		}
		slot, err := sealSlot(stored, keyID, mnemonic, provider)
		if err != nil {
			if errors.Is(err, ErrProviderUnavailable) {
				continue
			}
			return storedKey{}, 0, err
		}
		key.Slots = append(key.Slots, slot)
		wrapped++
	}
	return key, wrapped, nil
}

func sealSlot(stored storedKeyring, keyID ID, mnemonic string, provider Provider) (storedKeySlot, error) {
	parameters, err := provider.newParameters()
	if err != nil {
		return storedKeySlot{}, err
	}
	if err := provider.validateParameters(parameters); err != nil {
		return storedKeySlot{}, err
	}
	slotID, err := NewID()
	if err != nil {
		return storedKeySlot{}, err
	}
	kek, err := provider.unwrapKey(parameters)
	if err != nil {
		return storedKeySlot{}, err
	}
	aead, err := chacha20poly1305.NewX(kek[:])
	clear(kek[:])
	if err != nil {
		return storedKeySlot{}, fmt.Errorf("keyring cipher: %w", err)
	}
	var nonce [chacha20poly1305.NonceSizeX]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		return storedKeySlot{}, fmt.Errorf("generate keyring nonce: %w", err)
	}
	slot := storedKeySlot{
		ID:         append([]byte(nil), slotID[:]...),
		Provider:   provider.providerName(),
		Parameters: parameters,
		Nonce:      append([]byte(nil), nonce[:]...),
	}
	slot.Wrapped = aead.Seal(nil, nonce[:], []byte(mnemonic), keyringSlotAAD(stored, keyID, slot))
	return slot, nil
}

// SealWith encrypts one object under the named Main Key. The key must be plugged:
// an object sealed by a key nobody holds would be born unreadable.
func (k *Keyring) SealWith(plaintext []byte, identity Identity, keyID ID, activationKey [32]byte) (*Envelope, error) {
	if k == nil {
		return nil, ErrFormat
	}
	st := k.state.Load()
	if st == nil {
		return nil, ErrClosed
	}
	if identity.StoreID != st.storeID {
		return nil, ErrIdentityMismatch
	}
	main, ok := st.mainKeys[keyID]
	if !ok {
		return nil, ErrKeyNotLoaded
	}
	defer clear(main[:])
	return Seal(plaintext, identity, keyID, main, activationKey)
}

// Open unwraps an already parsed profile, catalog, or migration envelope. The
// activation key is the host's second layer; the caller looks it up (and answers
// ErrNotActivated when it is missing) because only it knows the host's state.
func (k *Keyring) Open(envelope *Envelope, activationKey [32]byte) ([]byte, error) {
	if k == nil || envelope == nil {
		return nil, ErrFormat
	}
	st := k.state.Load()
	if st == nil {
		return nil, ErrClosed
	}
	if envelope.identity.StoreID != st.storeID {
		return nil, ErrIdentityMismatch
	}
	main, ok := st.mainKeys[envelope.keyID]
	if !ok {
		return nil, ErrKeyNotLoaded
	}
	defer clear(main[:])
	return envelope.Open(main, activationKey)
}

// RewrapTo replaces an envelope's DEK wrapper with another key of this keychain.
// Both keys must be plugged: the old one to unwrap, the new one to seal. The body
// ciphertext is untouched, because the body key does not depend on the Main Key.
func (k *Keyring) RewrapTo(envelope *Envelope, newKeyID ID) (*Envelope, error) {
	if k == nil || envelope == nil || newKeyID.IsZero() {
		return nil, ErrFormat
	}
	st := k.state.Load()
	if st == nil {
		return nil, ErrClosed
	}
	if envelope.identity.StoreID != st.storeID {
		return nil, ErrIdentityMismatch
	}
	old, ok := st.mainKeys[envelope.keyID]
	if !ok {
		return nil, ErrKeyNotLoaded
	}
	next, ok := st.mainKeys[newKeyID]
	if !ok {
		return nil, ErrKeyNotLoaded
	}
	defer clear(old[:])
	defer clear(next[:])
	return envelope.Rewrap(old, newKeyID, next)
}

/*
DeriveSubkey returns a purpose-separated subkey of a plugged-in Main Key. The

	Main Key itself never leaves this type; callers that need key material for a
	feature (sharing, for instance) get a value that is useless in any other
	domain because the info string is bound in.
*/
func (k *Keyring) DeriveSubkey(info string) ([32]byte, error) {
	var out [32]byte
	st := k.state.Load()
	if st == nil {
		return out, ErrClosed
	}
	id, ok := k.anySealableKey()
	if !ok {
		return out, ErrKeyNotLoaded
	}
	return k.deriveSubkeyWith(st, id, info)
}

/* DeriveSubkeyFor is DeriveSubkey pinned to ONE key instead of to whichever key
 * seals the index. A feature whose identity belongs to a host's key — receiving a
 * shared host, for instance — has to derive from that key, or every key in the
 * keychain would answer to the same address and the sender could not say which of
 * them should end up holding what they sent.
 */
func (k *Keyring) DeriveSubkeyFor(keyID ID, info string) ([32]byte, error) {
	var out [32]byte
	st := k.state.Load()
	if st == nil {
		return out, ErrClosed
	}
	if _, ok := st.mainKeys[keyID]; !ok {
		return out, ErrKeyNotLoaded
	}
	return k.deriveSubkeyWith(st, keyID, info)
}

func (k *Keyring) deriveSubkeyWith(st *keyringState, keyID ID, info string) ([32]byte, error) {
	var out [32]byte
	main := st.mainKeys[keyID]
	defer clear(main[:])
	reader := hkdf.New(sha256.New, main[:], st.storeID[:], []byte(info))
	if _, err := io.ReadFull(reader, out[:]); err != nil {
		return out, err
	}
	return out, nil
}

// StoreID returns the public identity bound to this keyring.
func (k *Keyring) StoreID() ID {
	if k == nil {
		return ID{}
	}
	st := k.state.Load()
	if st == nil {
		return ID{}
	}
	return st.storeID
}

// anySealableKey returns one plugged key, deterministically the lowest id.
// Store-wide derivations (the legacy sharing address) use it; per-host work
// always names its own key instead.
func (k *Keyring) anySealableKey() (ID, bool) {
	st := k.state.Load()
	if st == nil {
		return ID{}, false
	}
	lowest := ID{}
	for id := range st.mainKeys {
		if lowest.IsZero() || lessID(id, lowest) {
			lowest = id
		}
	}
	return lowest, !lowest.IsZero()
}

// key returns one plugged-in Main Key. The caller owns the copy.
func (k *Keyring) key(id ID) (MainKey, bool) {
	if k == nil {
		return MainKey{}, false
	}
	st := k.state.Load()
	if st == nil {
		return MainKey{}, false
	}
	key, ok := st.mainKeys[id]
	return key, ok
}

func (k *Keyring) keysCount() int {
	if k == nil {
		return 0
	}
	st := k.state.Load()
	if st == nil {
		return 0
	}
	return len(st.mainKeys)
}

func (k *Keyring) isClosed() bool {
	if k == nil {
		return true
	}
	return k.state.Load() == nil
}

func (k *Keyring) withState(storeID ID, keys map[ID]MainKey, activation map[ID]map[ID][32]byte) {
	k.state.Store(&keyringState{
		storeID:    storeID,
		mainKeys:   keys,
		activation: activation,
	})
}

// withKey publishes this keyring's state with one more key plugged in.
func (k *Keyring) withKey(id ID, main MainKey) {
	st := k.state.Load()
	keys := make(map[ID]MainKey, len(st.mainKeys)+1)
	for keyID, material := range st.mainKeys {
		keys[keyID] = material
	}
	keys[id] = main
	k.withState(st.storeID, keys, st.activation)
}

// withoutKey publishes this keyring's state with one key unplugged. Its activation
// keys go with it: they are the second layer of hosts nobody can read now.
func (k *Keyring) withoutKey(id ID) {
	st := k.state.Load()
	keys := make(map[ID]MainKey, len(st.mainKeys))
	for keyID, material := range st.mainKeys {
		if keyID == id {
			clear(material[:])
			continue
		}
		keys[keyID] = material
	}
	activation := make(map[ID]map[ID][32]byte, len(st.activation))
	for keyID, perObject := range st.activation {
		if keyID == id {
			clearActivationMap(perObject)
			continue
		}
		activation[keyID] = perObject
	}
	k.withState(st.storeID, keys, activation)
}

// snapshotKeys returns the ids of the keys currently plugged in. It is a copy, so
// a caller may range over it while other keys plug or unplug.
func (k *Keyring) snapshotKeys() map[ID]struct{} {
	st := k.state.Load()
	out := make(map[ID]struct{}, len(st.mainKeys))
	for id := range st.mainKeys {
		out[id] = struct{}{}
	}
	return out
}

// activationKey returns one host's second layer, or noActivationKey when this key
// holds none for it. Callers decide whether "none" is a refusal (a host that needs
// a passphrase) or the normal case (a key without one).
func (k *Keyring) activationKey(keyID, objectID ID) ([32]byte, bool) {
	st := k.state.Load()
	if st == nil {
		return noActivationKey, false
	}
	perObject, ok := st.activation[keyID]
	if !ok {
		return noActivationKey, false
	}
	key, ok := perObject[objectID]
	return key, ok
}

// withActivationKey publishes the state with one host's activation key in memory.
func (k *Keyring) withActivationKey(keyID, objectID ID, activationKey [32]byte) {
	st := k.state.Load()
	activation := make(map[ID]map[ID][32]byte, len(st.activation)+1)
	for id, perObject := range st.activation {
		copied := make(map[ID][32]byte, len(perObject))
		for objectID, material := range perObject {
			copied[objectID] = material
		}
		activation[id] = copied
	}
	perObject, ok := activation[keyID]
	if !ok {
		perObject = make(map[ID][32]byte, 1)
		activation[keyID] = perObject
	}
	perObject[objectID] = activationKey
	keys := make(map[ID]MainKey, len(st.mainKeys))
	for id, material := range st.mainKeys {
		keys[id] = material
	}
	k.withState(st.storeID, keys, activation)
}

// withoutActivationKeys publishes the state with the named hosts' second layer
// dropped. Deactivating touches no ciphertext, so this is the whole of it.
func (k *Keyring) withoutActivationKeys(objects map[ID]struct{}) {
	st := k.state.Load()
	activation := make(map[ID]map[ID][32]byte, len(st.activation))
	for id, perObject := range st.activation {
		copied := make(map[ID][32]byte, len(perObject))
		for objectID, material := range perObject {
			if _, drop := objects[objectID]; drop {
				clear(material[:])
				continue
			}
			copied[objectID] = material
		}
		activation[id] = copied
	}
	keys := make(map[ID]MainKey, len(st.mainKeys))
	for id, material := range st.mainKeys {
		keys[id] = material
	}
	k.withState(st.storeID, keys, activation)
}

// Destroy revokes this keyring's in-process key material. Existing Envelope values
// become unreadable through it, and a later Seal or Open reports ErrClosed.
//
// ponytail: revocation swaps the state pointer; it does not overwrite the key
// bytes. Zeroing a buffer a concurrent reader may still be copying from is a
// data race, and Go makes no promise about erasure anyway (copies, the stack,
// a core dump). The lock this replaced bought exclusion between Destroy and
// Seal, which no caller ever needed: Destroy runs on construction failure and
// at shutdown, and revocation covers both.
func (k *Keyring) Destroy() {
	if k == nil {
		return
	}
	k.state.Store(nil)
}

func clearActivationMap(perObject map[ID][32]byte) {
	for objectID, material := range perObject {
		clear(material[:])
		delete(perObject, objectID)
	}
}

func encodeStoredKeyring(stored storedKeyring) ([]byte, error) {
	out, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("encode keyring file: %w", err)
	}
	if len(out) > maxKeyringFile {
		return nil, ErrTooLarge
	}
	return out, nil
}

func parseStoredKeyring(data []byte) (storedKeyring, error) {
	var stored storedKeyring
	if len(data) > maxKeyringFile {
		return stored, ErrTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return storedKeyring{}, ErrFormat
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return storedKeyring{}, ErrFormat
	}
	if stored.Version != keyringVersion {
		return storedKeyring{}, ErrVersion
	}
	if len(stored.StoreID) != len(ID{}) || isZeroBytes(stored.StoreID) ||
		len(stored.Keys) == 0 || len(stored.Keys) > maxMainKeys {
		return storedKeyring{}, ErrFormat
	}
	if len(stored.CatalogKeyID) != 0 && len(stored.CatalogKeyID) != len(ID{}) {
		return storedKeyring{}, ErrFormat
	}
	keyIDs := make(map[ID]struct{}, len(stored.Keys))
	slotIDs := make(map[ID]struct{})
	for _, key := range stored.Keys {
		if len(key.ID) != len(ID{}) || isZeroBytes(key.ID) ||
			len(key.Slots) == 0 || len(key.Slots) > maxProviderSlots {
			return storedKeyring{}, ErrFormat
		}
		var id ID
		copy(id[:], key.ID)
		if _, exists := keyIDs[id]; exists {
			return storedKeyring{}, ErrFormat
		}
		keyIDs[id] = struct{}{}
		for _, slot := range key.Slots {
			/* A slot seals the mnemonic, whose length is the phrase's, so the only
			   bound is "at least a tag, at most a phrase". A 48-byte ciphertext was
			   the shape of the model that sealed a 32-byte Main Key instead. */
			if len(slot.ID) != len(ID{}) || isZeroBytes(slot.ID) ||
				len(slot.Nonce) != chacha20poly1305.NonceSizeX ||
				len(slot.Wrapped) <= chacha20poly1305.Overhead ||
				len(slot.Wrapped) > maxMnemonicBytes+chacha20poly1305.Overhead {
				return storedKeyring{}, ErrFormat
			}
			var slotID ID
			copy(slotID[:], slot.ID)
			if _, exists := slotIDs[slotID]; exists {
				return storedKeyring{}, ErrFormat
			}
			slotIDs[slotID] = struct{}{}
			provider, ok := registeredProvider(slot.Provider)
			if !ok || provider.validateParameters(slot.Parameters) != nil {
				return storedKeyring{}, ErrFormat
			}
		}
	}
	if len(stored.CatalogKeyID) == len(ID{}) {
		var legacy ID
		copy(legacy[:], stored.CatalogKeyID)
		if _, ok := keyIDs[legacy]; !ok {
			return storedKeyring{}, ErrFormat
		}
	}
	return stored, nil
}

func storedCatalogKeyID(stored storedKeyring) ID {
	var id ID
	copy(id[:], stored.CatalogKeyID)
	return id
}

// keyringSlotAAD binds a slot to the key it wraps. Without the key id in the
// associated data, one key's slot could be moved under another key's header and
// the file would still authenticate.
func keyringSlotAAD(stored storedKeyring, keyID ID, slot storedKeySlot) []byte {
	out := make([]byte, 0, len(keyringDomain)+80+len(slot.Provider)+providerParameterSize(slot.Parameters))
	out = append(out, keyringDomain...)
	out = binary.LittleEndian.AppendUint16(out, stored.Version)
	out = append(out, stored.StoreID...)
	out = append(out, keyID[:]...)
	out = append(out, slot.ID...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(slot.Provider)))
	out = append(out, slot.Provider...)
	out = appendLengthPrefixed(out, slot.Parameters.Salt)
	if slot.Parameters.Argon2 != nil {
		out = binary.LittleEndian.AppendUint32(out, slot.Parameters.Argon2.Time)
		out = binary.LittleEndian.AppendUint32(out, slot.Parameters.Argon2.Memory)
		out = append(out, slot.Parameters.Argon2.Threads)
	}
	out = appendLengthPrefixed(out, []byte(slot.Parameters.Reference))
	out = appendLengthPrefixed(out, slot.Parameters.Protected)
	return out
}

func providerParameterSize(parameters providerParameters) int {
	size := 8 + len(parameters.Salt) + len(parameters.Reference) + len(parameters.Protected)
	if parameters.Argon2 != nil {
		size += 9
	}
	return size
}

func appendLengthPrefixed(out, value []byte) []byte {
	out = binary.LittleEndian.AppendUint32(out, uint32(len(value)))
	return append(out, value...)
}

/* registeredProvider answers with a shape-only provider for structural parsing.
 * Every one of them validates parameters and derives nothing: reading a keychain
 * file must never run a KDF or touch a credential store.
 *
 * "password" and "mnemonic" are deliberately absent. They were slot providers of
 * the model this format replaced — a password that derived a Main Key, and a
 * phrase used as a wrapper — and a file that still names one is from a version
 * that never shipped. Refusing it here turns "half-migrated keychain" into a
 * format error instead of a key that silently cannot be opened.
 */
func registeredProvider(name string) (Provider, bool) {
	switch name {
	case "env":
		return NewEnvProvider(""), true
	case "file":
		return fileProvider{}, true
	case "keychain", "secret-service":
		return credentialProvider{name: name}, true
	case "dpapi":
		return dpapiProviderForValidation(), true
	default:
		return nil, false
	}
}

func clearMainKeyMap(keys map[ID]MainKey) {
	for id, key := range keys {
		clear(key[:])
		delete(keys, id)
	}
}

func isZeroBytes(value []byte) bool {
	var zero byte
	for _, b := range value {
		zero |= b
	}
	return zero == 0
}

