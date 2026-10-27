// Package configstore implements the versioned cryptographic core for Termcp's
// encrypted connection profiles. Plaintext profiles remain a supported storage
// mode; this package moves only the objects a caller explicitly selects.
package configstore

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
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
// keyID identifies main in the currently unlocked keyring.
func Seal(plaintext []byte, identity Identity, keyID ID, main MainKey) (*Envelope, error) {
	return sealWithRandom(plaintext, identity, keyID, main, rand.Reader)
}

func sealWithRandom(plaintext []byte, identity Identity, keyID ID, main MainKey, random io.Reader) (*Envelope, error) {
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

	var dataKey [chacha20poly1305.KeySize]byte
	var dataNonce [chacha20poly1305.NonceSizeX]byte
	var wrapNonce [chacha20poly1305.NonceSizeX]byte
	if _, err := io.ReadFull(random, dataKey[:]); err != nil {
		return nil, fmt.Errorf("generate DEK: %w", err)
	}
	defer clear(dataKey[:])
	if _, err := io.ReadFull(random, dataNonce[:]); err != nil {
		return nil, fmt.Errorf("generate data nonce: %w", err)
	}
	if _, err := io.ReadFull(random, wrapNonce[:]); err != nil {
		return nil, fmt.Errorf("generate wrap nonce: %w", err)
	}

	dataAEAD, err := chacha20poly1305.NewX(dataKey[:])
	if err != nil {
		return nil, fmt.Errorf("data cipher: %w", err)
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

	ciphertext := dataAEAD.Seal(nil, dataNonce[:], plaintext, appendDomain(dataDomain, header))
	digest := sha256.Sum256(ciphertext)
	wrapAAD := make([]byte, 0, len(wrapDomain)+len(header)+len(keyID)+len(digest))
	wrapAAD = append(wrapAAD, wrapDomain...)
	wrapAAD = append(wrapAAD, header...)
	wrapAAD = append(wrapAAD, keyID[:]...)
	wrapAAD = append(wrapAAD, digest[:]...)
	wrapped := mainAEAD.Seal(nil, wrapNonce[:], dataKey[:], wrapAAD)

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

// Open unwraps the DEK and authenticates and decrypts the entire body.
func (e *Envelope) Open(main MainKey) ([]byte, error) {
	if e == nil || len(e.raw) != envelopeHeaderSize+int(binary.LittleEndian.Uint32(e.raw[85:89])) {
		return nil, ErrFormat
	}
	dataKey, err := e.unwrapDataKey(main)
	if err != nil {
		return nil, err
	}
	defer clear(dataKey[:])
	aead, err := chacha20poly1305.NewX(dataKey[:])
	if err != nil {
		return nil, fmt.Errorf("data cipher: %w", err)
	}
	header := e.raw[:dataHeaderSize]
	dataNonce := e.raw[61:85]
	ciphertext := e.raw[envelopeHeaderSize:]
	plaintext, err := aead.Open(nil, dataNonce, ciphertext, appendDomain(dataDomain, header))
	if err != nil {
		return nil, ErrAuthentication
	}
	return plaintext, nil
}

// Rewrap changes only the Main Key wrapper. The stable data header and body
// ciphertext remain byte-for-byte identical.
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
	var dataKey [chacha20poly1305.KeySize]byte
	if e == nil || len(e.raw) < envelopeHeaderSize+chacha20poly1305.Overhead {
		return dataKey, ErrFormat
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
		return dataKey, fmt.Errorf("wrap cipher: %w", err)
	}
	plain, err := aead.Open(nil, wrapNonce, wrapped, wrapAAD)
	if err != nil || len(plain) != len(dataKey) {
		clear(plain)
		return dataKey, ErrAuthentication
	}
	copy(dataKey[:], plain)
	clear(plain)
	return dataKey, nil
}

func appendDomain(domain, body []byte) []byte {
	out := make([]byte, 0, len(domain)+len(body))
	out = append(out, domain...)
	return append(out, body...)
}
