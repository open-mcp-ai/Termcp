package configstore

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
)

/* The second layer: a per-host activation key derived from the key's passphrase,
 * and the `.adk` sidecar that records which activation parameters a generation
 * was activated with.
 *
 * The passphrase never reaches disk, so the activation key cannot either: `.adk`
 * holds only the salt and cost parameters needed to derive it again, plus a
 * marker that proves this exact generation was activated with it. The marker is
 * not a wrapper around the DEK — it cannot be opened into key material — so
 * holding `.adk` gives an attacker nothing to run offline except the passphrase
 * guess that Argon2id's cost is meant to make expensive.
 */

const (
	// activationFileSize is fixed: the format has no variable-length field, so
	// parsing needs no length table and a wrong-sized file is refused outright.
	activationFileSize = 158
	activationVersion  = uint16(1)

	activationMagic = "TCPACT01"

	/* activationHeaderSize is everything the AAD covers: magic through the ADK
	   nonce, i.e. offsets 0–109 as the format table numbers them. The 48-byte ADK
	   ciphertext after it is what that AAD authenticates. */
	activationHeaderSize = 110
	activationSaltOffset = 70
	activationNonceOffset = 86
	activationADKOffset   = 110
	activationNonceSize   = chacha20poly1305.NonceSizeX
	activationADKSize     = chacha20poly1305.KeySize + chacha20poly1305.Overhead
	/* The ADK field is a fixed 48 bytes; the marker it holds is the domain tag plus
	   a 16-byte tag, so the field is padded to the format's width. The padding is
	   required to be zero, which makes flipping it a format error rather than a
	   silent no-op. */
	activationADKUsed = len(activatedDomain) + chacha20poly1305.Overhead
)

var (
	// ErrNotActivated reports a host whose second layer is not in memory. The key
	// that seals it may well be plugged; this host still needs activating.
	ErrNotActivated = errors.New("configstore: the host is not activated")
	// ErrPassphraseRequired reports an activation request that did not cover every
	// key the selection needs.
	ErrPassphraseRequired = errors.New("configstore: a passphrase is required for one or more keys")
	// ErrPassphraseWrong reports a passphrase that did not open a key's activation
	// record. It names the key, never the host: one passphrase belongs to a whole
	// key, so the host would add nothing.
	ErrPassphraseWrong = errors.New("configstore: the passphrase is incorrect")
	// ErrKeyPassphraseMissing reports a passphrase offered for a key that has none.
	ErrKeyPassphraseMissing = errors.New("configstore: this key has no passphrase")
	// ErrOrphaned reports ciphertext whose sealing key is gone.
	ErrOrphaned = errors.New("configstore: the key that sealed this object was deleted")
)

// ActivationParameters are the Argon2id costs one host's activation key was
// derived with. They are stored in the `.adk` so a restart can derive the same
// key from the same passphrase without remembering anything.
type ActivationParameters struct {
	Time    uint32
	Memory  uint32 // KiB
	Threads uint8
}

func defaultActivationParameters() ActivationParameters {
	cost := defaultArgon2Parameters()
	return ActivationParameters{Time: cost.Time, Memory: cost.Memory, Threads: cost.Threads}
}

/* validate runs BEFORE any derivation. A file that names a cost outside these
 * bounds is refused rather than obeyed: an attacker who can edit `.adk` could
 * otherwise ask this process to spend gigabytes and hours on one read.
 */
func (p ActivationParameters) validate() error {
	if p.Time < 1 || p.Time > 10 ||
		p.Memory < 8*1024 || p.Memory > 256*1024 ||
		p.Threads < 1 || p.Threads > 8 {
		return ErrFormat
	}
	return nil
}

// DeriveActivationKey turns a passphrase into one host's 32-byte activation key.
// The salt is that host's own, so the same passphrase under one key yields a
// different activation key for every host it seals.
func DeriveActivationKey(passphrase string, salt [16]byte, parameters ActivationParameters) ([32]byte, error) {
	var out [32]byte
	if err := parameters.validate(); err != nil {
		return out, err
	}
	if passphrase == "" {
		return out, ErrPassphraseRequired
	}
	derived := argon2.IDKey(
		[]byte(passphrase),
		salt[:],
		parameters.Time,
		parameters.Memory,
		parameters.Threads,
		uint32(len(out)),
	)
	copy(out[:], derived)
	clear(derived)
	return out, nil
}

/* activationRecord is one `.adk` file: the parameters that reproduce an
 * activation key, and the marker that proves the generation beside it was
 * activated with that key.
 */
type activationRecord struct {
	identity   Identity
	parameters ActivationParameters
	salt       [16]byte
	nonce      [activationNonceSize]byte
	adk        [activationADKSize]byte
}

// newActivation derives nothing: the caller already holds the activation key, and
// this records which parameters reproduce it.
func newActivation(identity Identity, parameters ActivationParameters, salt [16]byte, activationKey [32]byte) (activationRecord, error) {
	record := activationRecord{identity: identity, parameters: parameters, salt: salt}
	if err := identity.validate(); err != nil {
		return record, err
	}
	if err := parameters.validate(); err != nil {
		return record, err
	}
	if _, err := io.ReadFull(rand.Reader, record.nonce[:]); err != nil {
		return record, fmt.Errorf("generate activation nonce: %w", err)
	}
	return record, nil
}

// bind seals the activation marker against this exact body ciphertext, so a
// record cannot be moved onto another generation, object, or host.
func (r *activationRecord) bind(activationKey [32]byte, bodyCiphertext []byte) error {
	aead, err := chacha20poly1305.NewX(activationKey[:])
	if err != nil {
		return fmt.Errorf("activation cipher: %w", err)
	}
	sealed := aead.Seal(nil, r.nonce[:], []byte(activatedDomain), r.additionalData(bodyCiphertext))
	if len(sealed) != activationADKUsed {
		return ErrFormat
	}
	r.adk = [activationADKSize]byte{}
	copy(r.adk[:], sealed)
	return nil
}

// verify authenticates the marker. A record written for another generation, or
// with another passphrase, fails here and says only that it failed.
func (r activationRecord) verify(activationKey [32]byte, bodyCiphertext []byte) error {
	aead, err := chacha20poly1305.NewX(activationKey[:])
	if err != nil {
		return fmt.Errorf("activation cipher: %w", err)
	}
	plain, err := aead.Open(nil, r.nonce[:], r.adk[:activationADKUsed], r.additionalData(bodyCiphertext))
	if err != nil {
		return ErrPassphraseWrong
	}
	defer clear(plain)
	if !bytes.Equal(plain, []byte(activatedDomain)) {
		return ErrPassphraseWrong
	}
	return nil
}

func (r activationRecord) additionalData(bodyCiphertext []byte) []byte {
	header := r.headerBytes()
	digest := sha256.Sum256(bodyCiphertext)
	out := make([]byte, 0, len(activateDomain)+len(header)+len(digest))
	out = append(out, activateDomain...)
	out = append(out, header...)
	return append(out, digest[:]...)
}

// headerBytes is the first 110 bytes: everything the AAD covers — magic through the
// ADK nonce, salt included, because the AAD must name the parameters it was sealed
// with or a swapped salt would authenticate.
func (r activationRecord) headerBytes() []byte {
	out := make([]byte, 0, activationHeaderSize)
	out = append(out, activationMagic...)
	out = binary.LittleEndian.AppendUint16(out, activationVersion)
	out = binary.LittleEndian.AppendUint16(out, algorithmXChaCha20Poly1305)
	out = append(out, byte(r.identity.Kind))
	out = append(out, r.identity.StoreID[:]...)
	out = append(out, r.identity.ObjectID[:]...)
	out = append(out, r.identity.GenerationID[:]...)
	out = binary.LittleEndian.AppendUint32(out, r.parameters.Time)
	out = binary.LittleEndian.AppendUint32(out, r.parameters.Memory)
	out = append(out, r.parameters.Threads)
	out = append(out, r.salt[:]...)
	out = append(out, r.nonce[:]...)
	return out
}

// Bytes renders the fixed 158-byte file.
func (r activationRecord) Bytes() []byte {
	out := make([]byte, 0, activationFileSize)
	out = append(out, r.headerBytes()...)
	out = append(out, r.adk[:]...)
	return out
}

/* parseActivation checks framing, the frozen enum values, the identity the caller
 * expects, and the KDF cost — in that order, so a malformed or hostile file costs
 * a few comparisons and never an Argon2id run.
 */
func parseActivation(raw []byte, expected Identity) (activationRecord, error) {
	var record activationRecord
	if len(raw) != activationFileSize {
		return record, ErrFormat
	}
	if string(raw[:8]) != activationMagic {
		return record, ErrFormat
	}
	if binary.LittleEndian.Uint16(raw[8:10]) != activationVersion {
		return record, ErrVersion
	}
	if binary.LittleEndian.Uint16(raw[10:12]) != algorithmXChaCha20Poly1305 {
		return record, ErrFormat
	}
	kind := ObjectKind(raw[12])
	if !kind.valid() {
		return record, ErrFormat
	}
	var storeID, objectID, generationID ID
	copy(storeID[:], raw[13:29])
	copy(objectID[:], raw[29:45])
	copy(generationID[:], raw[45:61])
	if storeID.IsZero() || objectID.IsZero() || generationID.IsZero() {
		return record, ErrFormat
	}
	actual := Identity{StoreID: storeID, ObjectID: objectID, GenerationID: generationID, Kind: kind}
	if actual != expected {
		return record, ErrIdentityMismatch
	}
	record.parameters = ActivationParameters{
		Time:    binary.LittleEndian.Uint32(raw[61:65]),
		Memory:  binary.LittleEndian.Uint32(raw[65:69]),
		Threads: raw[69],
	}
	if err := record.parameters.validate(); err != nil {
		return record, err
	}
	record.identity = actual
	copy(record.salt[:], raw[activationSaltOffset:activationSaltOffset+16])
	copy(record.nonce[:], raw[activationNonceOffset:activationNonceOffset+activationNonceSize])
	copy(record.adk[:], raw[activationADKOffset:activationADKOffset+activationADKSize])
	if !isZeroBytes(record.adk[activationADKUsed:]) {
		return activationRecord{}, ErrFormat
	}
	return record, nil
}

// WriteActivation publishes one `.adk` through the same atomic path as every
// other config file: the generation's ciphertext is written first, this record
// second, and the catalog reference last.
func WriteActivation(path string, record activationRecord) error {
	return writeFileAtomic(path, record.Bytes())
}

// ReadActivation loads one `.adk`, bounded by its fixed size.
func ReadActivation(path string, expected Identity) (activationRecord, error) {
	raw, err := readBoundedFile(path, activationFileSize)
	if err != nil {
		return activationRecord{}, err
	}
	defer clear(raw)
	return parseActivation(raw, expected)
}
