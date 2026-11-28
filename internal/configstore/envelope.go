// Package configstore implements the versioned cryptographic core for Termcp's
// encrypted connection profiles. Plaintext profiles remain a supported storage
// mode; this package moves only the objects a caller explicitly selects.
package configstore

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// FormatVersion is the on-disk configuration envelope version.
	FormatVersion uint16 = 1

	algorithmXChaCha20Poly1305 uint16 = 1
	envelopeHeaderSize                = 177
	dataHeaderSize                    = 89
	wrappedKeySize                    = chacha20poly1305.KeySize + chacha20poly1305.Overhead

	MaxProfilePlaintext   = 256 << 10
	MaxCatalogPlaintext   = 4 << 20
	MaxMigrationPlaintext = 8 << 20
)

var (
	// ErrFormat reports a malformed envelope or keyring.
	ErrFormat = errors.New("configstore: invalid format")
	// ErrVersion reports an unsupported format version.
	ErrVersion = errors.New("configstore: unsupported version")
	// ErrTooLarge reports a bounded input that exceeds its object kind's limit.
	ErrTooLarge = errors.New("configstore: input exceeds size limit")
	// ErrIdentityMismatch reports a valid-looking file presented for another
	// store, object, generation, or object kind.
	ErrIdentityMismatch = errors.New("configstore: object identity mismatch")
	// ErrAuthentication deliberately combines a wrong key with tampered
	// authenticated data. Callers must not try to distinguish the two.
	ErrAuthentication = errors.New("configstore: authentication failed")
	// ErrDurability reports that a file was published but its directory entry
	// was not durably synced. In-memory state follows the published file.
	ErrDurability = errors.New("configstore: publication durability is unconfirmed")
)

var (
	dataDomain = []byte("termcp/config/data/v1")
	wrapDomain = []byte("termcp/config/wrap/v1")
)

/* The four derivation domains of format v1. They are part of the format, not a
 * choice a caller makes: changing one changes what a sealed byte string means,
 * so it costs a FormatVersion bump.
 *
 *   mainKeyDomain    mnemonic  -> Main Key          (keyring.go)
 *   dataKeyDomain    DEK + activation key -> body key
 *   activateDomain   .adk associated data
 *   activatedDomain  the fixed plaintext .adk authenticates
 */
const (
	mainKeyDomain   = "termcp/config/main/v1"
	dataKeyDomain   = "termcp/config/data-key/v1"
	activateDomain  = "termcp/config/activate/v1"
	activatedDomain = "termcp/config/activated/v1"
)

// noActivationKey stands in for a host whose key carries no passphrase. Using a
// fixed value rather than a branch keeps one code path: the body key is always
// derived from DEK and an activation key, and for such a host the second half is
// the authorization boolean in the catalog rather than a secret.
var noActivationKey [32]byte

// ObjectKind selects the authenticated purpose and size limit of an envelope.
type ObjectKind uint8

const (
	KindProfile   ObjectKind = 1
	KindCatalog   ObjectKind = 2
	KindMigration ObjectKind = 3
)

func (k ObjectKind) valid() bool {
	return k >= KindProfile && k <= KindMigration
}

func (k ObjectKind) maxPlaintext() (int, bool) {
	switch k {
	case KindProfile:
		return MaxProfilePlaintext, true
	case KindCatalog:
		return MaxCatalogPlaintext, true
	case KindMigration:
		return MaxMigrationPlaintext, true
	default:
		return 0, false
	}
}

// ID is a public 128-bit store, object, generation, or key identifier.
type ID [16]byte

// NewID returns a random non-zero identifier.
func NewID() (ID, error) {
	for {
		var id ID
		if _, err := io.ReadFull(rand.Reader, id[:]); err != nil {
			return ID{}, err
		}
		if !id.IsZero() {
			return id, nil
		}
	}
}

// IsZero reports whether id is the reserved all-zero value.
func (id ID) IsZero() bool {
	return id == ID{}
}

// Identity is the public identity authenticated by an envelope.
type Identity struct {
	StoreID      ID
	ObjectID     ID
	GenerationID ID
	Kind         ObjectKind
}

func (identity Identity) validate() error {
	if err := identity.validateExceptGeneration(); err != nil {
		return err
	}
	if identity.GenerationID.IsZero() {
		return ErrFormat
	}
	return nil
}

func (identity Identity) validateExceptGeneration() error {
	if !identity.Kind.valid() || identity.StoreID.IsZero() || identity.ObjectID.IsZero() {
		return ErrFormat
	}
	return nil
}

// MainKey wraps independently generated data keys. The envelope API never uses
// it as a data-encryption key.
type MainKey [chacha20poly1305.KeySize]byte

// GenerateMainKey returns a random 256-bit Main Key.
func GenerateMainKey() (MainKey, error) {
	var key MainKey
	if _, err := io.ReadFull(rand.Reader, key[:]); err != nil {
		return MainKey{}, fmt.Errorf("generate Main Key: %w", err)
	}
	return key, nil
}

// Envelope is a parsed configuration envelope. Its bytes own a private copy of
// the on-disk representation, so later caller mutation cannot alter Open input.
type Envelope struct {
	raw      []byte
	identity Identity
	keyID    ID
}

// Seal encrypts plaintext with a fresh random DEK and wraps that DEK with main.
// keyID identifies main in the currently loaded keychain. activationKey is the
// host's second layer: a value derived from its passphrase, or noActivationKey
// when the key that seals the host carries none. The body key is derived from
// both halves, so neither the DEK alone nor the activation key alone opens it.
func Seal(plaintext []byte, identity Identity, keyID ID, main MainKey, activationKey [32]byte) (*Envelope, error) {
	return sealWithRandom(plaintext, identity, keyID, main, activationKey, rand.Reader)
}

func sealWithRandom(plaintext []byte, identity Identity, keyID ID, main MainKey, activationKey [32]byte, random io.Reader) (*Envelope, error) {
	if err := identity.validate(); err != nil {
		return nil, err
	}
	if keyID.IsZero() {
		return nil, ErrFormat
	}
	limit, _ := identity.Kind.maxPlaintext()
	if len(plaintext) > limit {
		return nil, ErrTooLarge
	}
	if random == nil {
		return nil, fmt.Errorf("%w: nil random source", ErrFormat)
	}

	var dek [chacha20poly1305.KeySize]byte
	var dataNonce [chacha20poly1305.NonceSizeX]byte
	var wrapNonce [chacha20poly1305.NonceSizeX]byte
	if _, err := io.ReadFull(random, dek[:]); err != nil {
		return nil, fmt.Errorf("generate DEK: %w", err)
	}
	defer clear(dek[:])
	if _, err := io.ReadFull(random, dataNonce[:]); err != nil {
		return nil, fmt.Errorf("generate data nonce: %w", err)
	}
	if _, err := io.ReadFull(random, wrapNonce[:]); err != nil {
		return nil, fmt.Errorf("generate wrap nonce: %w", err)
	}

	mainAEAD, err := chacha20poly1305.NewX(main[:])
	if err != nil {
		return nil, fmt.Errorf("wrap cipher: %w", err)
	}

	header := make([]byte, dataHeaderSize)
	copy(header[:8], "TCPCFG01")
	binary.LittleEndian.PutUint16(header[8:10], FormatVersion)
	binary.LittleEndian.PutUint16(header[10:12], algorithmXChaCha20Poly1305)
	header[12] = byte(identity.Kind)
	copy(header[13:29], identity.StoreID[:])
	copy(header[29:45], identity.ObjectID[:])
	copy(header[45:61], identity.GenerationID[:])
	copy(header[61:85], dataNonce[:])
	binary.LittleEndian.PutUint32(header[85:89], uint32(len(plaintext)+chacha20poly1305.Overhead))

	dataKey, err := deriveDataKey(header, dek, activationKey)
	if err != nil {
		return nil, err
	}
	defer clear(dataKey[:])
	dataAEAD, err := chacha20poly1305.NewX(dataKey[:])
	if err != nil {
		return nil, fmt.Errorf("data cipher: %w", err)
	}

	ciphertext := dataAEAD.Seal(nil, dataNonce[:], plaintext, appendDomain(dataDomain, header))
	digest := sha256.Sum256(ciphertext)
	wrapAAD := make([]byte, 0, len(wrapDomain)+len(header)+len(keyID)+len(digest))
	wrapAAD = append(wrapAAD, wrapDomain...)
	wrapAAD = append(wrapAAD, header...)
	wrapAAD = append(wrapAAD, keyID[:]...)
	wrapAAD = append(wrapAAD, digest[:]...)
	wrapped := mainAEAD.Seal(nil, wrapNonce[:], dek[:], wrapAAD)

	raw := make([]byte, 0, len(header)+len(keyID)+len(wrapNonce)+len(wrapped)+len(ciphertext))
	raw = append(raw, header...)
	raw = append(raw, keyID[:]...)
	raw = append(raw, wrapNonce[:]...)
	raw = append(raw, wrapped...)
	raw = append(raw, ciphertext...)
	return &Envelope{
		raw:      raw,
		identity: identity,
		keyID:    keyID,
	}, nil
}

/*
envelopeIdentity reads the public identity out of an envelope header, after

	checking the file is one of ours for this store. Nothing is trusted: Open
	authenticates the header again before any plaintext is returned, so a wrong
	answer here costs a failed read, never a wrong one.
*/
func envelopeIdentity(raw []byte, storeID ID, kind ObjectKind) (ID, ID, bool) {
	var objectID, generationID ID
	if len(raw) < envelopeHeaderSize || !bytes.Equal(raw[:8], []byte("TCPCFG01")) {
		return objectID, generationID, false
	}
	if binary.LittleEndian.Uint16(raw[8:10]) != FormatVersion {
		return objectID, generationID, false
	}
	if ObjectKind(raw[12]) != kind {
		return objectID, generationID, false
	}
	var gotStore ID
	copy(gotStore[:], raw[13:29])
	if gotStore != storeID {
		return objectID, generationID, false
	}
	copy(objectID[:], raw[29:45])
	copy(generationID[:], raw[45:61])
	if objectID.IsZero() || generationID.IsZero() {
		return objectID, generationID, false
	}
	return objectID, generationID, true
}

/*
envelopeKeyID reads only the public key id out of an envelope header. It exists

	because "which key seals this" is a question the panel must answer without any
	key loaded — an unreadable index is exactly when it matters most — and the id is
	a plain field, not part of the secret. Framing is checked so a stray file cannot
	masquerade as a key holder.
*/
func envelopeKeyID(raw []byte) (ID, bool) {
	var keyID ID
	if len(raw) < envelopeHeaderSize || !bytes.Equal(raw[:8], []byte("TCPCFG01")) {
		return keyID, false
	}
	if binary.LittleEndian.Uint16(raw[8:10]) != FormatVersion {
		return keyID, false
	}
	copy(keyID[:], raw[89:105])
	return keyID, !keyID.IsZero()
}

// Parse validates framing and the expected public identity without running the
// expensive or secret-dependent authentication path.
func Parse(raw []byte, expected Identity) (*Envelope, error) {
	if err := expected.validate(); err != nil {
		return nil, err
	}
	return parseEnvelope(raw, expected, false)
}

// parseCatalogEnvelope authenticates the catalog's fixed store/object identity.
// Its generation is published in the authenticated catalog header and becomes
// the expected generation for the next parse; there is no earlier index from
// which that generation could be known.
func parseCatalogEnvelope(raw []byte, storeID, objectID ID) (*Envelope, error) {
	expected := Identity{
		StoreID:  storeID,
		ObjectID: objectID,
		Kind:     KindCatalog,
	}
	if err := expected.validateExceptGeneration(); err != nil {
		return nil, err
	}
	return parseEnvelope(raw, expected, true)
}

func parseEnvelope(raw []byte, expected Identity, catalogGeneration bool) (*Envelope, error) {
	limit, _ := expected.Kind.maxPlaintext()
	if len(raw) > envelopeHeaderSize+limit+chacha20poly1305.Overhead {
		return nil, ErrTooLarge
	}
	if len(raw) < envelopeHeaderSize+chacha20poly1305.Overhead {
		return nil, ErrFormat
	}
	if !bytes.Equal(raw[:8], []byte("TCPCFG01")) {
		return nil, ErrFormat
	}
	if binary.LittleEndian.Uint16(raw[8:10]) != FormatVersion {
		return nil, ErrVersion
	}
	if binary.LittleEndian.Uint16(raw[10:12]) != algorithmXChaCha20Poly1305 {
		return nil, ErrFormat
	}
	kind := ObjectKind(raw[12])
	if !kind.valid() {
		return nil, ErrFormat
	}
	var storeID, objectID, generationID, keyID ID
	copy(storeID[:], raw[13:29])
	copy(objectID[:], raw[29:45])
	copy(generationID[:], raw[45:61])
	copy(keyID[:], raw[89:105])
	if storeID.IsZero() || objectID.IsZero() || generationID.IsZero() || keyID.IsZero() {
		return nil, ErrFormat
	}
	ciphertextSize := int(binary.LittleEndian.Uint32(raw[85:89]))
	if ciphertextSize < chacha20poly1305.Overhead || ciphertextSize > limit+chacha20poly1305.Overhead {
		return nil, ErrFormat
	}
	if len(raw) != envelopeHeaderSize+ciphertextSize {
		return nil, ErrFormat
	}
	actual := Identity{
		StoreID:      storeID,
		ObjectID:     objectID,
		GenerationID: generationID,
		Kind:         kind,
	}
	if catalogGeneration {
		if actual.StoreID != expected.StoreID || actual.ObjectID != expected.ObjectID ||
			actual.Kind != expected.Kind {
			return nil, ErrIdentityMismatch
		}
		expected.GenerationID = actual.GenerationID
	} else if actual != expected {
		return nil, ErrIdentityMismatch
	}
	return &Envelope{
		raw:      bytes.Clone(raw),
		identity: actual,
		keyID:    keyID,
	}, nil
}

// Bytes returns a copy of the complete on-disk envelope.
func (e *Envelope) Bytes() []byte {
	if e == nil {
		return nil
	}
	return bytes.Clone(e.raw)
}

// Identity returns the authenticated public identity requested at Parse time.
func (e *Envelope) Identity() Identity {
	if e == nil {
		return Identity{}
	}
	return e.identity
}

// WrapKeyID returns the Main Key identifier needed to unwrap this envelope.
// This header field is public; authentication still happens in Open or Rewrap.
func (e *Envelope) WrapKeyID() ID {
	if e == nil {
		return ID{}
	}
	return e.keyID
}

// Open unwraps the DEK, combines it with activationKey, and authenticates and
// decrypts the entire body. A host whose second layer is missing answers
// ErrAuthentication here, which is exactly the AND the two layers promise: the
// DEK alone is not a body key.
func (e *Envelope) Open(main MainKey, activationKey [32]byte) ([]byte, error) {
	if e == nil || len(e.raw) != envelopeHeaderSize+int(binary.LittleEndian.Uint32(e.raw[85:89])) {
		return nil, ErrFormat
	}
	dek, err := e.unwrapDataKey(main)
	if err != nil {
		return nil, err
	}
	defer clear(dek[:])
	header := e.raw[:dataHeaderSize]
	dataKey, err := deriveDataKey(header, dek, activationKey)
	if err != nil {
		return nil, err
	}
	defer clear(dataKey[:])
	aead, err := chacha20poly1305.NewX(dataKey[:])
	if err != nil {
		return nil, fmt.Errorf("data cipher: %w", err)
	}
	dataNonce := e.raw[61:85]
	ciphertext := e.raw[envelopeHeaderSize:]
	plaintext, err := aead.Open(nil, dataNonce, ciphertext, appendDomain(dataDomain, header))
	if err != nil {
		return nil, ErrAuthentication
	}
	return plaintext, nil
}

/*
deriveDataKey is the join of the two layers: the DEK that the inserted key

	unwraps, and the activation key that the passphrase derives. The stable header D
	is the HKDF salt, so every generation gets a different body key even from the
	same pair — which is what makes two generations' body ciphertexts unrelated.
*/
func deriveDataKey(header []byte, dek [chacha20poly1305.KeySize]byte, activationKey [32]byte) ([32]byte, error) {
	var out [32]byte
	ikm := make([]byte, 0, len(dek)+len(activationKey))
	ikm = append(ikm, dek[:]...)
	ikm = append(ikm, activationKey[:]...)
	defer clear(ikm)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, header, []byte(dataKeyDomain)), out[:]); err != nil {
		return out, fmt.Errorf("derive data key: %w", err)
	}
	return out, nil
}

// Rewrap changes only the Main Key wrapper. The stable data header and body
// ciphertext remain byte-for-byte identical — the body key does not depend on
// the Main Key, so re-wrapping a DEK cannot disturb it.
func (e *Envelope) Rewrap(oldMain MainKey, newKeyID ID, newMain MainKey) (*Envelope, error) {
	if e == nil || newKeyID.IsZero() {
		return nil, ErrFormat
	}
	dataKey, err := e.unwrapDataKey(oldMain)
	if err != nil {
		return nil, err
	}
	defer clear(dataKey[:])
	header := e.raw[:dataHeaderSize]
	ciphertext := e.raw[envelopeHeaderSize:]
	digest := sha256.Sum256(ciphertext)
	wrapAAD := make([]byte, 0, len(wrapDomain)+len(header)+len(newKeyID)+len(digest))
	wrapAAD = append(wrapAAD, wrapDomain...)
	wrapAAD = append(wrapAAD, header...)
	wrapAAD = append(wrapAAD, newKeyID[:]...)
	wrapAAD = append(wrapAAD, digest[:]...)
	aead, err := chacha20poly1305.NewX(newMain[:])
	if err != nil {
		return nil, fmt.Errorf("wrap cipher: %w", err)
	}
	var wrapNonce [chacha20poly1305.NonceSizeX]byte
	if _, err := io.ReadFull(rand.Reader, wrapNonce[:]); err != nil {
		return nil, fmt.Errorf("generate wrap nonce: %w", err)
	}
	wrapped := aead.Seal(nil, wrapNonce[:], dataKey[:], wrapAAD)

	raw := make([]byte, 0, len(e.raw))
	raw = append(raw, header...)
	raw = append(raw, newKeyID[:]...)
	raw = append(raw, wrapNonce[:]...)
	raw = append(raw, wrapped...)
	raw = append(raw, ciphertext...)
	return &Envelope{
		raw:      raw,
		identity: e.identity,
		keyID:    newKeyID,
	}, nil
}

func (e *Envelope) unwrapDataKey(main MainKey) ([chacha20poly1305.KeySize]byte, error) {
	var dek [chacha20poly1305.KeySize]byte
	if e == nil || len(e.raw) < envelopeHeaderSize+chacha20poly1305.Overhead {
		return dek, ErrFormat
	}
	header := e.raw[:dataHeaderSize]
	keyID := e.raw[89:105]
	wrapNonce := e.raw[105:129]
	wrapped := e.raw[129:177]
	ciphertext := e.raw[envelopeHeaderSize:]
	digest := sha256.Sum256(ciphertext)
	wrapAAD := make([]byte, 0, len(wrapDomain)+len(header)+len(keyID)+len(digest))
	wrapAAD = append(wrapAAD, wrapDomain...)
	wrapAAD = append(wrapAAD, header...)
	wrapAAD = append(wrapAAD, keyID...)
	wrapAAD = append(wrapAAD, digest[:]...)
	aead, err := chacha20poly1305.NewX(main[:])
	if err != nil {
		return dek, fmt.Errorf("wrap cipher: %w", err)
	}
	plain, err := aead.Open(nil, wrapNonce, wrapped, wrapAAD)
	if err != nil || len(plain) != len(dek) {
		clear(plain)
		return dek, ErrAuthentication
	}
	copy(dek[:], plain)
	clear(plain)
	return dek, nil
}

func appendDomain(domain, body []byte) []byte {
	out := make([]byte, 0, len(domain)+len(body))
	out = append(out, domain...)
	return append(out, body...)
}

// MarshalText renders an ID as hex, so it can cross an HTTP boundary as a string
// without a second representation appearing in the UI.
func (id ID) MarshalText() ([]byte, error) {
	return []byte(hex.EncodeToString(id[:])), nil
}

// UnmarshalText parses the hex form MarshalText produces.
func (id *ID) UnmarshalText(text []byte) error {
	decoded, err := hex.DecodeString(string(text))
	if err != nil || len(decoded) != len(id) {
		return ErrFormat
	}
	copy(id[:], decoded)
	return nil
}
