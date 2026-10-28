package configstore

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
)

func (k *Keyring) rotationStatus() (ID, bool) {
	if k == nil {
		return ID{}, false
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.activeKeyID, !k.closed && len(k.mainKeys) > 1
}

func (k *Keyring) beginRotation() (*Keyring, error) {
	storeID, keys, _, err := k.snapshot()
	if err != nil {
		return nil, err
	}
	if len(keys) != 1 {
		clearMainKeyMap(keys)
		return nil, ErrRotationPending
	}
	main, err := GenerateMainKey()
	if err != nil {
		clearMainKeyMap(keys)
		return nil, err
	}
	defer clear(main[:])
	keyID, err := NewID()
	if err != nil {
		clearMainKeyMap(keys)
		return nil, err
	}
	if _, exists := keys[keyID]; exists {
		clearMainKeyMap(keys)
		return nil, ErrFormat
	}
	keys[keyID] = main
	return &Keyring{storeID: storeID, activeKeyID: keyID, mainKeys: keys}, nil
}

func (k *Keyring) activeOnly() (*Keyring, error) {
	storeID, keys, activeKeyID, err := k.snapshot()
	if err != nil {
		return nil, err
	}
	defer clearMainKeyMap(keys)
	return &Keyring{
		storeID:     storeID,
		activeKeyID: activeKeyID,
		mainKeys:    map[ID]MainKey{activeKeyID: keys[activeKeyID]},
		password:    completedPasswordDerivation(k.password),
	}, nil
}

// Rewrap replaces an envelope's DEK wrapper with the active Main Key. Its
// identity, data nonce, and body ciphertext do not change.
func (k *Keyring) Rewrap(envelope *Envelope) (*Envelope, error) {
	if k == nil || envelope == nil || envelope.identity.StoreID != k.storeID {
		return nil, ErrIdentityMismatch
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.closed {
		return nil, ErrProviderUnavailable
	}
	old, ok := k.mainKeys[envelope.keyID]
	if !ok {
		return nil, ErrProviderUnavailable
	}
	main, ok := k.mainKeys[k.activeKeyID]
	if !ok {
		return nil, ErrFormat
	}
	return envelope.Rewrap(old, k.activeKeyID, main)
}

// keyringProtection holds existing slot KEKs only for one rotation operation.
// Reusing every slot preserves recovery methods and system credential entries.
type keyringProtection struct {
	stored storedKeyring
	keks   []keyEncryptionKey
}

func unlockKeyringProtection(data []byte, expected *Keyring, providers []Provider) (*keyringProtection, error) {
	stored, err := parseStoredKeyring(data)
	if err != nil {
		return nil, err
	}
	storeID := expected.StoreID()
	if !bytes.Equal(stored.StoreID, storeID[:]) {
		return nil, ErrIdentityMismatch
	}
	protection := &keyringProtection{stored: stored, keks: make([]keyEncryptionKey, len(stored.Slots))}
	for i, slot := range stored.Slots {
		derived, authenticated := false, false
		for _, provider := range providers {
			if provider == nil || provider.providerName() != slot.Provider {
				continue
			}
			if err := provider.validateParameters(slot.Parameters); err != nil {
				continue
			}
			kek, err := provider.unwrapKey(slot.Parameters)
			if err != nil {
				clear(kek[:])
				continue
			}
			derived = true
			aead, err := chacha20poly1305.NewX(kek[:])
			if err != nil {
				clear(kek[:])
				continue
			}
			plaintext, err := aead.Open(nil, slot.Nonce, slot.Wrapped, keyringSlotAAD(stored, slot))
			if err != nil {
				clear(kek[:])
				continue
			}
			unlocked, err := parseKeyringPayload(plaintext, expected.StoreID())
			clear(plaintext)
			if err == nil {
				authenticated = sameKeyring(unlocked, expected)
				unlocked.Destroy()
			}
			if authenticated {
				protection.keks[i] = kek
				clear(kek[:])
				break
			}
			clear(kek[:])
		}
		if !authenticated {
			protection.destroy()
			if derived {
				return nil, ErrAuthentication
			}
			return nil, ErrProviderUnavailable
		}
	}
	return protection, nil
}

func sameKeyring(a, b *Keyring) bool {
	storeA, keysA, activeA, err := a.snapshot()
	if err != nil {
		return false
	}
	defer clearMainKeyMap(keysA)
	storeB, keysB, activeB, err := b.snapshot()
	if err != nil {
		return false
	}
	defer clearMainKeyMap(keysB)
	if storeA != storeB || activeA != activeB || len(keysA) != len(keysB) {
		return false
	}
	for id, material := range keysA {
		if other, ok := keysB[id]; !ok || other != material {
			return false
		}
	}
	return true
}

func (p *keyringProtection) marshal(keyring *Keyring) ([]byte, error) {
	plaintext, err := keyring.marshalPayload()
	if err != nil {
		return nil, err
	}
	defer clear(plaintext)
	stored := p.stored
	stored.Password = keyring.password
	if keyring.password != nil {
		stored.Version = passwordKeyringVersion
	}
	stored.Slots = append([]storedKeySlot(nil), stored.Slots...)
	for i := range stored.Slots {
		slot := &stored.Slots[i]
		aead, err := chacha20poly1305.NewX(p.keks[i][:])
		if err != nil {
			return nil, err
		}
		slot.Nonce = make([]byte, chacha20poly1305.NonceSizeX)
		if _, err := io.ReadFull(rand.Reader, slot.Nonce); err != nil {
			return nil, err
		}
		slot.Wrapped = aead.Seal(nil, slot.Nonce, plaintext, keyringSlotAAD(stored, *slot))
	}
	data, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	if len(data) > maxKeyringFile {
		return nil, ErrTooLarge
	}
	return data, nil
}

func (p *keyringProtection) destroy() {
	for i := range p.keks {
		clear(p.keks[i][:])
	}
}
