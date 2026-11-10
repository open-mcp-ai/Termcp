package configstore

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

const passwordKeyringVersion = uint16(2)

var (
	ErrPasswordRequired      = errors.New("configstore: enter the encryption password")
	ErrPasswordNotConfigured = errors.New("configstore: an encryption password has not been configured")
	ErrPasswordInvalid       = errors.New("configstore: password must contain at least 8 characters and at most 1024 UTF-8 bytes")
)

type storedPasswordDerivation struct {
	KeyID    []byte                 `json:"key_id"`
	Salt     []byte                 `json:"salt"`
	Argon2   Argon2Parameters       `json:"argon2id"`
	Nonce    []byte                 `json:"nonce"`
	Verifier []byte                 `json:"verifier"`
	Previous *storedPreviousMainKey `json:"previous,omitempty"`
}

type storedPreviousMainKey struct {
	KeyID   []byte `json:"key_id"`
	Nonce   []byte `json:"nonce"`
	Wrapped []byte `json:"wrapped"`
}

func validatePasswordDerivation(p *storedPasswordDerivation) error {
	if p == nil || len(p.KeyID) != 16 || isZeroBytes(p.KeyID) || len(p.Salt) != 16 ||
		len(p.Nonce) != chacha20poly1305.NonceSizeX || len(p.Verifier) != 32+chacha20poly1305.Overhead || p.Argon2.validate() != nil {
		return ErrFormat
	}
	if old := p.Previous; old != nil {
		if len(old.KeyID) != 16 || isZeroBytes(old.KeyID) || bytes.Equal(old.KeyID, p.KeyID) ||
			len(old.Nonce) != chacha20poly1305.NonceSizeX || len(old.Wrapped) != chacha20poly1305.KeySize+chacha20poly1305.Overhead {
			return ErrFormat
		}
	}
	return nil
}

func passwordAAD(store ID, p *storedPasswordDerivation) []byte {
	out := append([]byte("termcp/config/password/v1"), store[:]...)
	out = append(out, p.KeyID...)
	out = append(out, p.Salt...)
	out = binary.LittleEndian.AppendUint32(out, p.Argon2.Time)
	out = binary.LittleEndian.AppendUint32(out, p.Argon2.Memory)
	out = append(out, p.Argon2.Threads)
	return out
}

func deriveMain(password []byte, p *storedPasswordDerivation) (MainKey, error) {
	var main MainKey
	if len(password) == 0 || len(password) > 1024 || validatePasswordDerivation(p) != nil {
		return main, ErrAuthentication
	}
	key := argon2.IDKey(password, p.Salt, p.Argon2.Time, p.Argon2.Memory, p.Argon2.Threads, uint32(len(main)))
	copy(main[:], key)
	clear(key)
	return main, nil
}

func newPasswordKeyring(store ID, password []byte) (*Keyring, error) {
	keyID, err := NewID()
	if err != nil {
		return nil, err
	}
	p := &storedPasswordDerivation{
		KeyID: append([]byte(nil), keyID[:]...), Salt: make([]byte, 16),
		Argon2: defaultArgon2Parameters(), Nonce: make([]byte, chacha20poly1305.NonceSizeX),
		Verifier: make([]byte, 32+chacha20poly1305.Overhead),
	}
	if _, err := io.ReadFull(rand.Reader, p.Salt); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rand.Reader, p.Nonce); err != nil {
		return nil, err
	}
	main, err := deriveMain(password, p)
	if err != nil {
		return nil, err
	}
	defer clear(main[:])
	aead, err := chacha20poly1305.NewX(main[:])
	if err != nil {
		return nil, err
	}
	proof := append(append([]byte(nil), store[:]...), keyID[:]...)
	p.Verifier = aead.Seal(nil, p.Nonce, proof, passwordAAD(store, p))
	k := &Keyring{password: p}
	k.state.Store(&keyringState{
		storeID:     store,
		activeKeyID: keyID,
		mainKeys:    map[ID]MainKey{keyID: main},
	})
	return k, nil
}

func verifyDerivedKeyring(k *Keyring, p *storedPasswordDerivation) error {
	if validatePasswordDerivation(p) != nil {
		return ErrFormat
	}
	var active ID
	copy(active[:], p.KeyID)
	st := k.state.Load()
	if st == nil {
		return ErrProviderUnavailable
	}
	main, ok := st.mainKeys[active]
	if !ok || active != st.activeKeyID {
		return ErrAuthentication
	}
	aead, err := chacha20poly1305.NewX(main[:])
	clear(main[:])
	if err != nil {
		return err
	}
	proof, err := aead.Open(nil, p.Nonce, p.Verifier, passwordAAD(st.storeID, p))
	defer clear(proof)
	want := append(append([]byte(nil), st.storeID[:]...), active[:]...)
	if err != nil || !bytes.Equal(proof, want) {
		return ErrAuthentication
	}
	return nil
}

func unlockPasswordKeyring(stored storedKeyring, password []byte) (*Keyring, error) {
	if stored.Password == nil {
		return nil, ErrPasswordNotConfigured
	}
	main, err := deriveMain(password, stored.Password)
	if err != nil {
		return nil, err
	}
	defer clear(main[:])
	var store, active ID
	copy(store[:], stored.StoreID)
	copy(active[:], stored.Password.KeyID)
	k := &Keyring{password: stored.Password}
	mainKeys := map[ID]MainKey{active: main}
	if old := stored.Password.Previous; old != nil {
		aead, err := chacha20poly1305.NewX(main[:])
		if err != nil {
			return nil, err
		}
		aad := append(passwordAAD(store, stored.Password), []byte("/previous/")...)
		aad = append(aad, old.KeyID...)
		data, err := aead.Open(nil, old.Nonce, old.Wrapped, aad)
		if err != nil {
			return nil, ErrAuthentication
		}
		var oldID ID
		var oldMain MainKey
		copy(oldID[:], old.KeyID)
		copy(oldMain[:], data)
		clear(data)
		mainKeys[oldID] = oldMain
		clear(oldMain[:])
	}
	k.state.Store(&keyringState{
		storeID:     store,
		activeKeyID: active,
		mainKeys:    mainKeys,
	})
	if err := verifyDerivedKeyring(k, stored.Password); err != nil {
		k.Destroy()
		return nil, err
	}
	return k, nil
}

func completedPasswordDerivation(p *storedPasswordDerivation) *storedPasswordDerivation {
	if p == nil {
		return nil
	}
	copy := *p
	copy.Previous = nil
	return &copy
}

func validateNewPassword(password, confirmation string) error {
	if password != confirmation {
		return ErrPasswordConfirmation
	}
	if len(password) > 1024 || utf8.RuneCountInString(password) < 8 || strings.TrimSpace(password) == "" {
		return ErrPasswordInvalid
	}
	return nil
}
