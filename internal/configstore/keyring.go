package configstore

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

// The keyring is where the password meets the envelopes.
//
// ONE KEY, DERIVED AND NEVER STORED. The Main Key is Argon2id(password, salt); the
// file on disk holds the salt, the cost parameters and a VERIFIER — an AEAD tag over
// the store id and key id under the derived key — and nothing that can be turned back
// into the key. A wrong password fails the verifier; a modified cost parameter fails
// it too, because the parameters are part of the authenticated data, which is also
// what stops a tampered file from asking the KDF for unbounded work.
//
// This is the subset of the audited design that needs no key provider: the password
// is typed by a human (see the config-storage commands), so there is no system
// keychain, no DPAPI, no Secret Service and no wrapped-Main-Key snapshot on disk.
const (
	keyringVersion uint16 = 1

	minPasswordChars = 8
	maxPasswordBytes = 1024

	// Argon2id costs, matching the audited design. Memory is in KiB.
	defaultArgon2Time    = 3
	defaultArgon2Memory  = 64 * 1024
	defaultArgon2Threads = 1

	keyringSaltSize = 16
)

var (
	// ErrPasswordRequired reports a keyring operation with no password supplied.
	ErrPasswordRequired = errors.New("configstore: enter the encryption password")
	// ErrPasswordInvalid reports a password outside the accepted bounds.
	ErrPasswordInvalid = errors.New("configstore: password must contain at least 8 characters and at most 1024 UTF-8 bytes")
	// ErrPasswordConfirmation reports mismatched initial password inputs.
	ErrPasswordConfirmation = errors.New("configstore: password confirmation does not match")
)

// passwordDomain separates this verifier's key from every other use of the derived
// key, so a tag from one purpose can never be replayed as another.
var passwordDomain = []byte("termcp/config/password/v1")

// Argon2Parameters are persisted with the keyring. Memory is in KiB and is
// validated before the KDF runs, so a modified file cannot request unbounded work.
type Argon2Parameters struct {
	Time    uint32 `json:"time"`
	Memory  uint32 `json:"memory_kib"`
	Threads uint8  `json:"threads"`
}

func defaultArgon2Parameters() Argon2Parameters {
	return Argon2Parameters{
		Time:    defaultArgon2Time,
		Memory:  defaultArgon2Memory,
		Threads: defaultArgon2Threads,
	}
}

func (p Argon2Parameters) validate() error {
	if p.Time < 1 || p.Time > 10 ||
		p.Memory < 8*1024 || p.Memory > 256*1024 ||
		p.Threads < 1 || p.Threads > 8 {
		return fmt.Errorf("%w: Argon2 parameters outside supported limits", ErrFormat)
	}
	return nil
}

// storedKeyring is the on-disk form. It holds KDF inputs and a verifier; it never
// holds the Main Key, in any encoding.
type storedKeyring struct {
	Version  uint16           `json:"version"`
	StoreID  string           `json:"store_id"`
	KeyID    []byte           `json:"key_id"`
	Salt     []byte           `json:"salt"`
	Argon2   Argon2Parameters `json:"argon2id"`
	Nonce    []byte           `json:"nonce"`
	Verifier []byte           `json:"verifier"`
}

// Keyring is an unlocked keyring: the store it belongs to and the Main Key derived
// from the password this process was given.
type Keyring struct {
	storeID ID
	keyID   ID
	main    MainKey
	stored  storedKeyring
}

// passwordAAD authenticates everything a keyring file must not be able to change:
// the store identity and the KDF inputs. Without the parameters in the AAD, an
// attacker could lower the cost in the file and have the victim derive a weaker key
// from the same password, or raise it and make unlocking a denial of service.
func passwordAAD(store ID, st *storedKeyring) []byte {
	out := append([]byte(nil), passwordDomain...)
	out = append(out, store[:]...)
	out = append(out, st.KeyID...)
	out = append(out, st.Salt...)
	out = binary.LittleEndian.AppendUint32(out, st.Argon2.Time)
	out = binary.LittleEndian.AppendUint32(out, st.Argon2.Memory)
	out = append(out, st.Argon2.Threads)
	return out
}

// deriveMainKey runs the KDF. The caller owns the result and must Destroy it.
func deriveMainKey(password []byte, salt []byte, p Argon2Parameters) (MainKey, error) {
	var main MainKey
	if len(password) == 0 || len(password) > maxPasswordBytes {
		return main, ErrPasswordInvalid
	}
	if err := p.validate(); err != nil {
		return main, err
	}
	raw := argon2.IDKey(password, salt, p.Time, p.Memory, p.Threads, uint32(len(main)))
	copy(main[:], raw)
	clear(raw)
	return main, nil
}

// checkPassword enforces the policy the error message states.
func checkPassword(password string) error {
	if password == "" {
		return ErrPasswordRequired
	}
	if utf8.RuneCountInString(password) < minPasswordChars || len(password) > maxPasswordBytes {
		return ErrPasswordInvalid
	}
	return nil
}

// newStoredKeyring builds the file contents for a fresh password: a new salt, a new
// nonce, a new key id, and the verifier that proves the derivation.
func newStoredKeyring(store ID, password string) (*storedKeyring, ID, MainKey, error) {
	var main MainKey
	var zero ID
	if err := checkPassword(password); err != nil {
		return nil, zero, main, err
	}
	keyID, err := NewID()
	if err != nil {
		return nil, zero, main, err
	}
	st := &storedKeyring{
		Version:  keyringVersion,
		StoreID:  hex.EncodeToString(store[:]),
		KeyID:    append([]byte(nil), keyID[:]...),
		Salt:     make([]byte, keyringSaltSize),
		Argon2:   defaultArgon2Parameters(),
		Nonce:    make([]byte, chacha20poly1305.NonceSizeX),
		Verifier: make([]byte, 32+chacha20poly1305.Overhead),
	}
	if _, err := io.ReadFull(rand.Reader, st.Salt); err != nil {
		return nil, zero, main, err
	}
	if _, err := io.ReadFull(rand.Reader, st.Nonce); err != nil {
		return nil, zero, main, err
	}
	main, err = deriveMainKey([]byte(password), st.Salt, st.Argon2)
	if err != nil {
		return nil, zero, main, err
	}
	aead, err := chacha20poly1305.NewX(main[:])
	if err != nil {
		clear(main[:])
		return nil, zero, main, err
	}
	// The proof is what the verifier authenticates: this store, this key id.
	proof := append(append([]byte(nil), store[:]...), st.KeyID...)
	st.Verifier = aead.Seal(nil, st.Nonce, proof, passwordAAD(store, st))
	return st, keyID, main, nil
}

// CreatePasswordKeyring derives a Main Key from password and returns the keyring
// together with the bytes to persist.
func CreatePasswordKeyring(storeID ID, password, confirmation string) (*Keyring, []byte, error) {
	if password != confirmation {
		return nil, nil, ErrPasswordConfirmation
	}
	st, keyID, main, err := newStoredKeyring(storeID, password)
	if err != nil {
		return nil, nil, err
	}
	k := &Keyring{storeID: storeID, keyID: keyID, main: main, stored: *st}
	raw, err := k.Marshal()
	if err != nil {
		k.Destroy()
		return nil, nil, err
	}
	return k, raw, nil
}

// UnlockPasswordKeyring verifies the file and derives the Main Key.
//
// THE FILE IS PARSED DEFENSIVELY. Every length is checked and the cost parameters
// are bounded BEFORE the KDF runs, because the file is the one thing an attacker who
// can write to the data directory controls.
func UnlockPasswordKeyring(raw []byte, password string) (*Keyring, error) {
	if err := checkPassword(password); err != nil {
		return nil, err
	}
	var st storedKeyring
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	if st.Version != keyringVersion {
		return nil, fmt.Errorf("%w: keyring version %d", ErrVersion, st.Version)
	}
	store, err := parseIDHex(st.StoreID)
	if err != nil {
		return nil, err
	}
	if len(st.KeyID) != 16 || isZeroBytes(st.KeyID) || len(st.Salt) != keyringSaltSize ||
		len(st.Nonce) != chacha20poly1305.NonceSizeX ||
		len(st.Verifier) != 32+chacha20poly1305.Overhead {
		return nil, ErrFormat
	}
	if err := st.Argon2.validate(); err != nil {
		return nil, err
	}
	main, err := deriveMainKey([]byte(password), st.Salt, st.Argon2)
	if err != nil {
		return nil, err
	}
	aead, err := chacha20poly1305.NewX(main[:])
	if err != nil {
		clear(main[:])
		return nil, err
	}
	if _, err := aead.Open(nil, st.Nonce, st.Verifier, passwordAAD(store, &st)); err != nil {
		// A wrong password and a tampered file are the same answer on purpose.
		clear(main[:])
		return nil, ErrAuthentication
	}
	var keyID ID
	copy(keyID[:], st.KeyID)
	k := &Keyring{storeID: store, keyID: keyID, main: main, stored: st}
	return k, nil
}

// Marshal returns the bytes to persist. It contains no key material: the Main Key is
// a function of the password and the salt, and is never written anywhere.
func (k *Keyring) Marshal() ([]byte, error) {
	if k == nil {
		return nil, ErrFormat
	}
	raw, err := json.Marshal(k.stored)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// StoreID is the store this keyring belongs to.
func (k *Keyring) StoreID() ID { return k.storeID }

// KeyID identifies the Main Key generation, so an envelope sealed under a retired
// key can be told apart from one sealed under the current key.
func (k *Keyring) KeyID() ID { return k.keyID }

// Seal encrypts one object under this keyring's Main Key.
func (k *Keyring) Seal(plaintext []byte, identity Identity) (*Envelope, error) {
	if k == nil {
		return nil, ErrFormat
	}
	return Seal(plaintext, identity, k.keyID, k.main)
}

// Open decrypts one object. The envelope's key id must be this keyring's, which is
// what makes a key rotation detectable instead of silently unreadable.
func (k *Keyring) Open(env *Envelope) ([]byte, error) {
	if k == nil || env == nil {
		return nil, ErrFormat
	}
	if env.WrapKeyID() != k.keyID {
		return nil, ErrAuthentication
	}
	return env.Open(k.main)
}

// Rekey derives a NEW Main Key from a new password. The caller rewraps every
// envelope with the returned keyring and persists its Marshal output; the old
// keyring stays usable for reading until the last envelope is rewrapped.
func (k *Keyring) Rekey(password, confirmation string) (*Keyring, error) {
	if k == nil {
		return nil, ErrFormat
	}
	if password != confirmation {
		return nil, ErrPasswordConfirmation
	}
	st, keyID, main, err := newStoredKeyring(k.storeID, password)
	if err != nil {
		return nil, err
	}
	next := &Keyring{storeID: k.storeID, keyID: keyID, main: main, stored: *st}
	return next, nil
}

// Destroy zeroes the Main Key. Callers do it when a keyring goes out of scope for
// good, so the key does not linger in memory for the rest of the process.
func (k *Keyring) Destroy() {
	if k == nil {
		return
	}
	clear(k.main[:])
}

// isZeroBytes reports whether every byte is zero, which no valid id or salt is.
func isZeroBytes(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// parseIDHex decodes a 16-byte id from its hex spelling.
func parseIDHex(s string) (ID, error) {
	var id ID
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != len(id) {
		return id, ErrFormat
	}
	copy(id[:], raw)
	if id.IsZero() {
		return id, ErrFormat
	}
	return id, nil
}

// String keeps a keyring out of logs: printing one must not print the key.
func (k *Keyring) String() string {
	if k == nil {
		return "configstore.Keyring(nil)"
	}
	return "configstore.Keyring(store=" + hex.EncodeToString(k.storeID[:]) + " key=" + hex.EncodeToString(k.keyID[:]) + ")"
}

var _ fmt.Stringer = (*Keyring)(nil)

// trimNewline is used by tests that read a persisted keyring.
func trimNewline(b []byte) []byte { return []byte(strings.TrimRight(string(b), "\n")) }
