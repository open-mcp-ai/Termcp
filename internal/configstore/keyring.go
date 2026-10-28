package configstore

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	keyringVersion   uint16 = 1
	maxKeyringFile          = 64 << 10
	maxProviderSlots        = 8
	maxMainKeys             = 2
)

var keyringDomain = []byte("termcp/config/keyring/v1")

type storedKeyring struct {
	Version  uint16                    `json:"version"`
	StoreID  []byte                    `json:"store_id"`
	Slots    []storedKeySlot           `json:"slots"`
	Password *storedPasswordDerivation `json:"password,omitempty"`
}

type storedKeySlot struct {
	ID         []byte             `json:"id"`
	Provider   string             `json:"provider"`
	Parameters providerParameters `json:"parameters"`
	Nonce      []byte             `json:"nonce"`
	Wrapped    []byte             `json:"wrapped"`
}

type storedKeyringPayload struct {
	Version     uint16                  `json:"version"`
	StoreID     []byte                  `json:"store_id"`
	ActiveKeyID []byte                  `json:"active_key_id"`
	Keys        []storedMainKeyMaterial `json:"keys"`
}

type storedMainKeyMaterial struct {
	ID       []byte `json:"id"`
	Material []byte `json:"material"`
}

// Keyring owns unwrapped Main Keys. It does not expose their bytes to callers;
// profile persistence can only ask it to seal or open an Envelope.
type Keyring struct {
	mu          sync.RWMutex
	storeID     ID
	activeKeyID ID
	mainKeys    map[ID]MainKey
	closed      bool
	password    *storedPasswordDerivation
}

// CreateKeyring generates one Main Key and exports it through one or more
// Provider KEKs. The returned bytes are the config.keys representation.
func CreateKeyring(storeID ID, providers ...Provider) ([]byte, *Keyring, error) {
	main, err := GenerateMainKey()
	if err != nil {
		return nil, nil, err
	}
	defer clear(main[:])
	return createKeyringWithMainKey(storeID, main, providers...)
}

func createKeyringWithMainKey(storeID ID, main MainKey, providers ...Provider) ([]byte, *Keyring, error) {
	defer clear(main[:])
	if storeID.IsZero() || isZeroBytes(main[:]) {
		return nil, nil, ErrFormat
	}
	keyID, err := NewID()
	if err != nil {
		clear(main[:])
		return nil, nil, err
	}
	keyring := &Keyring{
		storeID:     storeID,
		activeKeyID: keyID,
		mainKeys:    map[ID]MainKey{keyID: main},
	}
	clear(main[:])
	data, err := keyring.Marshal(providers...)
	if err != nil {
		keyring.Destroy()
		return nil, nil, err
	}
	return data, keyring, nil
}

// UnlockKeyring validates a keyring file and tries only the Provider names
// supplied by the caller. A matched Provider with failing authentication returns
// ErrAuthentication; a missing Provider returns ErrProviderUnavailable.
func UnlockKeyring(data []byte, providers ...Provider) (*Keyring, error) {
	stored, err := parseStoredKeyring(data)
	if err != nil {
		return nil, err
	}
	var storeID ID
	copy(storeID[:], stored.StoreID)
	matchedProvider := false
	derivedProviderKEK := false
	for _, slot := range stored.Slots {
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
			payloadBytes, err := aead.Open(nil, slot.Nonce, slot.Wrapped, keyringSlotAAD(stored, slot))
			if err != nil {
				continue
			}
			keyring, err := parseKeyringPayload(payloadBytes, storeID)
			clear(payloadBytes)
			if err == nil {
				if stored.Password != nil {
					if err := verifyDerivedKeyring(keyring, stored.Password); err != nil {
						keyring.Destroy()
						continue
					}
					keyring.password = stored.Password
				}
				return keyring, nil
			}
		}
	}
	if !matchedProvider {
		return nil, ErrProviderUnavailable
	}
	if !derivedProviderKEK {
		return nil, ErrProviderUnavailable
	}
	return nil, ErrAuthentication
}

// Marshal wraps the same in-memory Main Keys under the supplied Providers.
func (k *Keyring) Marshal(providers ...Provider) ([]byte, error) {
	if k == nil {
		return nil, ErrFormat
	}
	if len(providers) > maxProviderSlots || len(providers) == 0 && k.password == nil {
		return nil, ErrFormat
	}
	plaintext, err := k.marshalPayload()
	if err != nil {
		return nil, err
	}
	defer clear(plaintext)
	storeID := k.StoreID()
	stored := storedKeyring{
		Version:  keyringVersion,
		StoreID:  append([]byte(nil), storeID[:]...),
		Slots:    make([]storedKeySlot, 0, len(providers)),
		Password: k.password,
	}
	if k.password != nil {
		stored.Version = passwordKeyringVersion
	}
	for _, provider := range providers {
		if provider == nil {
			return nil, ErrFormat
		}
		parameters, err := provider.newParameters()
		if err != nil {
			if k.password != nil && provider.providerName() != "env" && errors.Is(err, ErrProviderUnavailable) {
				continue
			}
			return nil, err
		}
		if err := provider.validateParameters(parameters); err != nil {
			return nil, err
		}
		slotID, err := NewID()
		if err != nil {
			return nil, err
		}
		kek, err := provider.unwrapKey(parameters)
		if err != nil {
			return nil, err
		}
		aead, err := chacha20poly1305.NewX(kek[:])
		clear(kek[:])
		if err != nil {
			return nil, fmt.Errorf("keyring cipher: %w", err)
		}
		var nonce [chacha20poly1305.NonceSizeX]byte
		if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
			return nil, fmt.Errorf("generate keyring nonce: %w", err)
		}
		slot := storedKeySlot{
			ID:         append([]byte(nil), slotID[:]...),
			Provider:   provider.providerName(),
			Parameters: parameters,
			Nonce:      append([]byte(nil), nonce[:]...),
		}
		slot.Wrapped = aead.Seal(nil, nonce[:], plaintext, keyringSlotAAD(stored, slot))
		stored.Slots = append(stored.Slots, slot)
	}
	out, err := json.Marshal(stored)
	if err != nil {
		return nil, fmt.Errorf("encode keyring file: %w", err)
	}
	if len(out) > maxKeyringFile {
		return nil, ErrTooLarge
	}
	return out, nil
}

func (k *Keyring) marshalPayload() ([]byte, error) {
	storeID, keys, activeKeyID, err := k.snapshot()
	if err != nil {
		return nil, err
	}
	defer clearMainKeyMap(keys)
	payload := storedKeyringPayload{
		Version:     keyringVersion,
		StoreID:     append([]byte(nil), storeID[:]...),
		ActiveKeyID: append([]byte(nil), activeKeyID[:]...),
		Keys:        make([]storedMainKeyMaterial, 0, len(keys)),
	}
	for id, material := range keys {
		payload.Keys = append(payload.Keys, storedMainKeyMaterial{
			ID:       append([]byte(nil), id[:]...),
			Material: append([]byte(nil), material[:]...),
		})
	}
	plaintext, err := json.Marshal(payload)
	clearStoredPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("encode keyring: %w", err)
	}
	if len(plaintext) > maxKeyringFile {
		clear(plaintext)
		return nil, ErrTooLarge
	}
	return plaintext, nil
}

// Seal encrypts one object with a fresh DEK under the active Main Key.
func (k *Keyring) Seal(plaintext []byte, identity Identity) (*Envelope, error) {
	if k == nil {
		return nil, ErrFormat
	}
	if identity.StoreID != k.storeID {
		return nil, ErrIdentityMismatch
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.closed {
		return nil, ErrProviderUnavailable
	}
	main, ok := k.mainKeys[k.activeKeyID]
	if !ok {
		return nil, ErrFormat
	}
	return Seal(plaintext, identity, k.activeKeyID, main)
}

// Open unwraps an already parsed profile, catalog, or migration envelope.
func (k *Keyring) Open(envelope *Envelope) ([]byte, error) {
	if k == nil || envelope == nil || envelope.identity.StoreID != k.storeID {
		return nil, ErrIdentityMismatch
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.closed {
		return nil, ErrProviderUnavailable
	}
	main, ok := k.mainKeys[envelope.keyID]
	if !ok {
		return nil, ErrProviderUnavailable
	}
	return envelope.Open(main)
}

// StoreID returns the public identity bound to this keyring.
func (k *Keyring) StoreID() ID {
	if k == nil {
		return ID{}
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.storeID
}

// Destroy clears the in-process Main Keys. Existing Envelope values become
// unreadable through this Keyring, though Go cannot promise total memory erasure.
func (k *Keyring) Destroy() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for id, key := range k.mainKeys {
		clear(key[:])
		delete(k.mainKeys, id)
	}
	k.closed = true
}

func (k *Keyring) snapshot() (ID, map[ID]MainKey, ID, error) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.closed || k.storeID.IsZero() || k.activeKeyID.IsZero() ||
		len(k.mainKeys) == 0 || len(k.mainKeys) > maxMainKeys {
		return ID{}, nil, ID{}, ErrFormat
	}
	if _, ok := k.mainKeys[k.activeKeyID]; !ok {
		return ID{}, nil, ID{}, ErrFormat
	}
	keys := make(map[ID]MainKey, len(k.mainKeys))
	for id, key := range k.mainKeys {
		if id.IsZero() {
			return ID{}, nil, ID{}, ErrFormat
		}
		keys[id] = key
	}
	return k.storeID, keys, k.activeKeyID, nil
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
	if stored.Version != keyringVersion && stored.Version != passwordKeyringVersion {
		return storedKeyring{}, ErrVersion
	}
	if len(stored.StoreID) != 16 || isZeroBytes(stored.StoreID) ||
		len(stored.Slots) > maxProviderSlots || len(stored.Slots) == 0 && stored.Password == nil {
		return storedKeyring{}, ErrFormat
	}
	if stored.Version == passwordKeyringVersion {
		if stored.Password == nil || validatePasswordDerivation(stored.Password) != nil {
			return storedKeyring{}, ErrFormat
		}
	} else if stored.Password != nil {
		return storedKeyring{}, ErrFormat
	}
	seen := make(map[string]bool)
	for _, slot := range stored.Slots {
		if len(slot.ID) != 16 || isZeroBytes(slot.ID) || seen[string(slot.ID)] ||
			len(slot.Nonce) != chacha20poly1305.NonceSizeX ||
			len(slot.Wrapped) < chacha20poly1305.Overhead ||
			len(slot.Wrapped) > maxKeyringFile+chacha20poly1305.Overhead {
			return storedKeyring{}, ErrFormat
		}
		seen[string(slot.ID)] = true
		provider, ok := registeredProvider(slot.Provider)
		if !ok {
			return storedKeyring{}, ErrFormat
		}
		if err := provider.validateParameters(slot.Parameters); err != nil {
			return storedKeyring{}, ErrFormat
		}
	}
	return stored, nil
}

func parseKeyringPayload(data []byte, expectedStoreID ID) (*Keyring, error) {
	var payload storedKeyringPayload
	if len(data) > maxKeyringFile {
		return nil, ErrTooLarge
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return nil, ErrFormat
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, ErrFormat
	}
	if payload.Version != keyringVersion || len(payload.StoreID) != 16 ||
		len(payload.ActiveKeyID) != 16 || len(payload.Keys) == 0 ||
		len(payload.Keys) > maxMainKeys {
		return nil, ErrFormat
	}
	var storeID, activeKeyID ID
	copy(storeID[:], payload.StoreID)
	copy(activeKeyID[:], payload.ActiveKeyID)
	if storeID != expectedStoreID || activeKeyID.IsZero() {
		return nil, ErrFormat
	}
	keys := make(map[ID]MainKey, len(payload.Keys))
	for _, record := range payload.Keys {
		if len(record.ID) != 16 || len(record.Material) != chacha20poly1305.KeySize {
			clearStoredPayload(payload)
			return nil, ErrFormat
		}
		var id ID
		copy(id[:], record.ID)
		if id.IsZero() {
			clearStoredPayload(payload)
			return nil, ErrFormat
		}
		if _, exists := keys[id]; exists {
			clearStoredPayload(payload)
			return nil, ErrFormat
		}
		var main MainKey
		copy(main[:], record.Material)
		keys[id] = main
	}
	clearStoredPayload(payload)
	if _, ok := keys[activeKeyID]; !ok {
		for _, key := range keys {
			clear(key[:])
		}
		return nil, ErrFormat
	}
	return &Keyring{
		storeID:     storeID,
		activeKeyID: activeKeyID,
		mainKeys:    keys,
	}, nil
}

func keyringSlotAAD(stored storedKeyring, slot storedKeySlot) []byte {
	out := make([]byte, 0, len(keyringDomain)+64+len(slot.Provider)+providerParameterSize(slot.Parameters))
	out = append(out, keyringDomain...)
	out = binary.LittleEndian.AppendUint16(out, stored.Version)
	out = append(out, stored.StoreID...)
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
	if stored.Version == passwordKeyringVersion {
		metadata, _ := json.Marshal(stored.Password)
		out = appendLengthPrefixed(out, metadata)
	}
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

func registeredProvider(name string) (Provider, bool) {
	switch name {
	case "env":
		return NewEnvProvider(""), true
	case "password":
		// This fixed-cost instance validates slot shape only. No password is
		// retained and no KDF is run during structural parsing.
		return &PasswordProvider{cost: defaultArgon2Parameters()}, true
	case "keychain", "secret-service":
		return credentialProvider{name: name}, true
	case "dpapi":
		return dpapiProviderForValidation(), true
	default:
		return nil, false
	}
}

func clearStoredPayload(payload storedKeyringPayload) {
	for i := range payload.Keys {
		clear(payload.Keys[i].Material)
		clear(payload.Keys[i].ID)
	}
	clear(payload.StoreID)
	clear(payload.ActiveKeyID)
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
