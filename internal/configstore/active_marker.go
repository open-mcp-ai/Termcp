package configstore

/* The second layer for a key WITHOUT a passphrase.
 *
 * A passphrase key expresses "this host is active" cryptographically: the body
 * key needs an activation key that only the passphrase can produce, and `.adk`
 * records how to reproduce it. A key without a passphrase has no such secret, so
 * the second layer is an authenticated marker instead of a derivation.
 *
 * That marker lives in the host's own directory, beside the generation it
 * activates, because it is a fact about ONE host. It used to live in a global
 * encrypted index (`store.enc`), which meant one unplugged key could make every
 * other key's hosts unwritable, and saving one host rewrote every host's entry.
 * Nothing about that global object was secret either: names are directory names,
 * and object, generation and key ids are public envelope headers.
 *
 * The file is sealed with the Main Key that seals the host's body, so the same
 * key that opens the body opens the marker, and deleting the key makes both
 * unreadable. It is bound to Store ID / Object ID / Generation ID, so a marker
 * copied onto another host or another generation authenticates as nothing.
 *
 * Size is fixed, so parsing needs no length table and a wrong-sized file is
 * refused before any allocation.
 */

import (
	"encoding/binary"
	"errors"
	"os"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	activeMarkerFileName = "active"
	activeMarkerSize     = 1 + 16 + 16 + 16 + chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead
	activeMarkerVersion  = byte(1)
)

var activeMarkerAAD = []byte("termcp/config/active/v1")

// activeMarkerPath returns the marker beside a host's generation.
func (r *Repository) activeMarkerPath(ref catalogRef) string {
	return hostDir(r.dataDir, ref.name) + string(os.PathSeparator) + activeMarkerFileName
}

// writeActiveMarker seals the marker for this exact generation with its own key.
func (r *Repository) writeActiveMarker(ref catalogRef) error {
	keyring := r.snap().keyring
	if keyring == nil {
		return ErrNotInitialized
	}
	main, ok := keyring.key(ref.keyID)
	if !ok {
		return ErrKeyNotLoaded
	}
	defer clear(main[:])

	aead, err := chacha20poly1305.NewX(main[:])
	if err != nil {
		return err
	}
	nonce, err := randomBytes(chacha20poly1305.NonceSizeX)
	if err != nil {
		return err
	}
	storeID := keyring.StoreID()
	header := make([]byte, 0, 1+16+16+16+len(nonce))
	header = append(header, activeMarkerVersion)
	header = append(header, storeID[:]...)
	header = append(header, ref.objectID[:]...)
	header = append(header, ref.generationID[:]...)
	header = append(header, nonce...)
	out := aead.Seal(header, nonce, nil, activeMarkerAAD)
	return writeFileAtomic(r.activeMarkerPath(ref), out)
}

// readActiveMarker reports whether this exact generation carries a valid marker.
//
// A marker that cannot be read or authenticated is treated as "not active" rather
// than as damage: it is a gate, and a gate that fails closed leaves the host
// inactive, which is the safe answer. The body itself is still authenticated
// separately, so a torn marker cannot make an unreadable host look readable.
func (r *Repository) readActiveMarker(ref catalogRef) bool {
	keyring := r.snap().keyring
	if keyring == nil {
		return false
	}
	raw, err := readBoundedFile(r.activeMarkerPath(ref), activeMarkerSize)
	if err != nil || len(raw) != activeMarkerSize || raw[0] != activeMarkerVersion {
		return false
	}
	main, ok := keyring.key(ref.keyID)
	if !ok {
		return false
	}
	defer clear(main[:])
	aead, err := chacha20poly1305.NewX(main[:])
	if err != nil {
		return false
	}
	storeID := keyring.StoreID()
	offset := 1
	if !bytesEqual(raw[offset:offset+16], storeID[:]) {
		return false
	}
	offset += 16
	if !bytesEqual(raw[offset:offset+16], ref.objectID[:]) {
		return false
	}
	offset += 16
	if !bytesEqual(raw[offset:offset+16], ref.generationID[:]) {
		return false
	}
	offset += 16
	nonce := raw[offset : offset+chacha20poly1305.NonceSizeX]
	sealed := raw[activeMarkerSize-chacha20poly1305.Overhead:]
	if _, err := aead.Open(nil, nonce, sealed, activeMarkerAAD); err != nil {
		return false
	}
	return true
}

// clearActiveMarker removes the marker for a host's current generation.
func (r *Repository) clearActiveMarker(ref catalogRef) error {
	return removeIfExists(r.activeMarkerPath(ref))
}

// generationActivated decides the persisted second layer for one generation.
//
// A passphrase-bearing key has a real `.adk`: its presence means this generation
// can be re-activated with the passphrase, and the host starts inactive after a
// restart because the derived key is not on disk. A key without one has only the
// marker, so the marker IS the state and it survives restarts.
func (r *Repository) generationActivated(ref catalogRef, stored storedKeyring) bool {
	key, known := storedKeyByID(stored, ref.keyID)
	if known && keyHasPassphrase(key) {
		if _, err := r.readActivation(ref); err != nil {
			return false
		}
		// The activation key lives only in memory; after a restart it is gone.
		_, inMemory := r.snap().keyring.activationKey(ref.keyID, ref.objectID)
		return inMemory
	}
	return r.readActiveMarker(ref)
}

// encodeGenerationRef is the small on-disk identity of one committed generation.
// It is not secret; it is what the header already publishes.
func encodeGenerationRef(ref catalogRef) []byte {
	out := make([]byte, 0, 2+16+16+16)
	out = binary.BigEndian.AppendUint16(out, 1)
	out = append(out, ref.objectID[:]...)
	out = append(out, ref.generationID[:]...)
	out = append(out, ref.keyID[:]...)
	return out
}

var errMarkerMismatch = errors.New("configstore: activation marker does not match this generation")

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := range a {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
