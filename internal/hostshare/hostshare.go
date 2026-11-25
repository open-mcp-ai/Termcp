// Package hostshare moves one connection profile between two people as text.
//
// The sender never sees the recipient's secret: a host is sealed to the
// recipient's public key with an ephemeral X25519 exchange, and the result is a
// single printable blob that survives a chat window. The recipient's key is
// derived from their store's Main Key, so there is no extra keypair to lose — a
// store with a loaded key can receive, and one with nothing loaded cannot.
//
// What this does NOT provide is authenticity. Anyone can seal a profile to a
// public key, so a blob proves only that whoever wrote it had the public key,
// not that they are who the chat message claims. The UI must therefore treat an
// imported profile as untrusted input: show what it is, and let the recipient
// decide before it lands in their list.
package hostshare

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
)

const (
	// Prefix identifies a share blob, so a paste either is one or is refused
	// before any parsing happens.
	Prefix = "termcp-host-v1:"
	// maxBlob bounds a pasted blob: a profile is capped at 256 KiB, and base64
	// plus JSON framing keeps it well under this.
	maxBlob = 512 << 10

	infoDomain = "termcp/hostshare/v1"
	aadDomain  = "termcp/hostshare/aad/v1"
)

var (
	ErrFormat     = errors.New("hostshare: not a share blob")
	ErrDecrypt    = errors.New("hostshare: this blob was not sealed for this key")
	ErrTooLarge   = errors.New("hostshare: blob exceeds the size limit")
	ErrRecipient  = errors.New("hostshare: recipient public key is invalid")
	ErrUnknownKey = errors.New("hostshare: identity is unavailable")
)

// card is the wire shape. Field names are short because the whole thing travels
// through chat clients that mangle whitespace but not base64.
type card struct {
	Name       string `json:"n"`
	Ephemeral  []byte `json:"e"`
	Nonce      []byte `json:"i"`
	Ciphertext []byte `json:"c"`
	Sender     string `json:"s,omitempty"`
}

// PublicKey renders a 32-byte secret as the base64 the other side pastes in.
func PublicKey(secret [32]byte) string {
	var priv [32]byte
	copy(priv[:], secret[:])
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(pub)
}

// ParsePublicKey accepts the text form produced by PublicKey.
func ParsePublicKey(encoded string) ([32]byte, error) {
	var pub [32]byte
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(raw) != curve25519.PointSize {
		return pub, ErrRecipient
	}
	copy(pub[:], raw)
	return pub, nil
}

// Seal encrypts toml for the holder of recipientPub. name travels inside the
// authenticated envelope, so a blob cannot be relabelled to overwrite a
// different profile on the way in.
func Seal(recipientPub [32]byte, name string, toml []byte, senderPub string) (string, error) {
	var ephPriv [32]byte
	if _, err := io.ReadFull(rand.Reader, ephPriv[:]); err != nil {
		return "", err
	}
	defer clear(ephPriv[:])
	ephPub, err := curve25519.X25519(ephPriv[:], curve25519.Basepoint)
	if err != nil {
		return "", ErrRecipient
	}
	shared, err := curve25519.X25519(ephPriv[:], recipientPub[:])
	if err != nil {
		return "", ErrRecipient
	}
	defer clear(shared)
	aead, err := newAEAD(shared, ephPub, recipientPub[:])
	if err != nil {
		return "", err
	}
	nonce := make([]byte, chacha20poly1305.NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := card{
		Name:      name,
		Ephemeral: ephPub,
		Nonce:     nonce,
		Sender:    senderPub,
	}
	out.Ciphertext = aead.Seal(nil, nonce, toml, aad(name))
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// Open decrypts a blob sealed for the holder of secret and returns the profile
// name it was sealed under.
func Open(secret [32]byte, blob string) (string, []byte, error) {
	stored, err := parse(blob)
	if err != nil {
		return "", nil, err
	}
	var priv [32]byte
	copy(priv[:], secret[:])
	shared, err := curve25519.X25519(priv[:], stored.Ephemeral)
	if err != nil {
		return "", nil, ErrDecrypt
	}
	defer clear(shared)
	aead, err := newAEAD(shared, stored.Ephemeral, recipientOf(secret))
	if err != nil {
		return "", nil, err
	}
	plaintext, err := aead.Open(nil, stored.Nonce, stored.Ciphertext, aad(stored.Name))
	if err != nil {
		return "", nil, ErrDecrypt
	}
	return stored.Name, plaintext, nil
}

// Sender reports who the blob claims to have come from. It is informational
// only: nothing binds it to the ciphertext, so it is a label, not a proof.
func Sender(blob string) string {
	stored, err := parse(blob)
	if err != nil {
		return ""
	}
	return stored.Sender
}

// LooksLike reports whether the text even claims to be a share blob.
func LooksLike(blob string) bool {
	return strings.HasPrefix(strings.TrimSpace(blob), Prefix)
}

func parse(blob string) (card, error) {
	var out card
	text := strings.TrimSpace(blob)
	// Chat clients wrap long lines; restoring them costs nothing and saves the
	// user from having to paste it in one piece.
	text = strings.Join(strings.Fields(text), "")
	if !strings.HasPrefix(text, Prefix) {
		return out, ErrFormat
	}
	encoded := strings.TrimPrefix(text, Prefix)
	if len(encoded) > maxBlob {
		return out, ErrTooLarge
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return out, ErrFormat
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, ErrFormat
	}
	if len(out.Ephemeral) != curve25519.PointSize ||
		len(out.Nonce) != chacha20poly1305.NonceSize ||
		len(out.Ciphertext) < chacha20poly1305.Overhead ||
		strings.TrimSpace(out.Name) == "" {
		return out, ErrFormat
	}
	return out, nil
}

func recipientOf(secret [32]byte) []byte {
	var priv [32]byte
	copy(priv[:], secret[:])
	pub, err := curve25519.X25519(priv[:], curve25519.Basepoint)
	if err != nil {
		return nil
	}
	return pub
}

func newAEAD(shared, ephPub, recipientPub []byte) (interface {
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}, error) {
	reader := hkdf.New(sha256.New, shared, nil, append(append([]byte(infoDomain), ephPub...), recipientPub...))
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := io.ReadFull(reader, key); err != nil {
		return nil, fmt.Errorf("hostshare: derive key: %w", err)
	}
	defer clear(key)
	return chacha20poly1305.New(key)
}

func aad(name string) []byte {
	return append([]byte(aadDomain), strings.TrimSpace(name)...)
}
