package configstore

/* One-way migration out of the index-based layout.
 *
 * The old format kept every host's name, object id, generation id and activation
 * boolean in `ssh_configs/store.enc`, an envelope sealed by whichever key owned
 * the index. Everything except the activation boolean was already published by
 * the host directories, so migration is: read the index if we can, turn its
 * activation booleans into per-host `active` markers, and delete the file.
 *
 * If no plugged key can open the index, its booleans are unavailable and hosts
 * simply start inactive — a safe default that a person can reverse with one
 * click. That is deliberately NOT a startup failure: refusing to open would
 * strand a store behind a file that only ever held a cache.
 */

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/crypto/chacha20poly1305"
)

const (
	legacyRepositoryVersion = uint16(1)
	legacyMaxCatalogEntries = 10_000
)

var legacyCatalogObjectID = repeatedID(0xca)

type legacyCatalogDocument struct {
	Version  uint16           `json:"version"`
	StoreID  []byte           `json:"store_id"`
	Revision uint64           `json:"revision"`
	Profiles []legacyCatalogProfile `json:"profiles"`
}

type legacyCatalogProfile struct {
	Name         string `json:"name"`
	ObjectID     []byte `json:"object_id"`
	GenerationID []byte `json:"generation_id"`
	Activated    bool   `json:"activated"`
}

// legacyCatalogPath is where the old index lived.
func legacyCatalogPath(dataDir string) string {
	return filepath.Join(dataDir, "ssh_configs", "store.enc")
}

// HasManagedCatalog reports whether the pre-directory encrypted index exists.
// Callers use it to recognise a store this build has not migrated yet.
func HasManagedCatalog(dataDir string) (bool, error) {
	root, err := filepath.Abs(filepath.Clean(dataDir))
	if err != nil {
		return false, err
	}
	root, err = filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return pathExists(legacyCatalogPath(root))
}

/* migrateLegacyIndex converts the old index and removes it.
 *
 * It runs on every open, so it is cheap when there is nothing to do: one stat.
 * Everything it needs comes from the index being migrated: the old `objects/`
 * layout has no name on disk, so an index that cannot be decrypted leaves both
 * the old file and the objects where they are instead of guessing.
 */
func (r *Repository) migrateLegacyIndex() error {
	path := legacyCatalogPath(r.dataDir)
	exists, err := pathExists(path)
	if err != nil || !exists {
		return err
	}
	activated, readErr := r.readLegacyIndex(path)
	if readErr != nil {
		/* An index we cannot parse or decrypt is not allowed to veto the store, and
		   it is not allowed to be deleted either: the host directories are the
		   authoritative record now, and this file is the only thing that could ever
		   name a host sitting in the old objects/ layout. */
		return nil
	}
	if err := r.migrateLegacyObjects(activated); err != nil {
		return err
	}
	for name, ref := range activated {
		if err := r.writeActiveMarker(ref); err != nil {
			return err
		}
		_ = name
	}
	return removeIfExists(path)
}

// migrateLegacyObjects moves objects/<object>/<generation>.enc into the host
// directory the index names for it.
func (r *Repository) migrateLegacyObjects(activated map[string]catalogRef) error {
	for _, ref := range activated {
		newPath := r.objectPath(ref)
		if exists, err := pathExists(newPath); err != nil {
			return err
		} else if exists {
			continue
		}
		legacyPath := r.legacyObjectPath(ref)
		raw, err := readBoundedFile(legacyPath,
			int64(envelopeHeaderSize+MaxProfilePlaintext+chacha20poly1305.Overhead))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := writeFileAtomic(newPath, raw); err != nil {
			return err
		}
		_ = removeIfExists(legacyPath)
		_ = os.Remove(filepath.Dir(legacyPath))
	}
	_ = os.Remove(filepath.Join(hostsRoot(r.dataDir), "objects"))
	return nil
}

// readLegacyIndex returns the hosts the old index marked activated.
func (r *Repository) readLegacyIndex(path string) (map[string]catalogRef, error) {
	raw, err := readBoundedFile(path, envelopeHeaderSize+MaxCatalogPlaintext+chacha20poly1305.Overhead)
	if err != nil {
		return nil, err
	}
	keyring := r.snap().keyring
	if keyring == nil {
		return nil, ErrNotInitialized
	}
	envelope, err := parseCatalogEnvelope(raw, keyring.StoreID(), legacyCatalogObjectID)
	if err != nil {
		return nil, err
	}
	plaintext, err := keyring.Open(envelope, noActivationKey)
	if err != nil {
		return nil, err
	}
	defer clear(plaintext)
	var catalog legacyCatalogDocument
	decoder := json.NewDecoder(bytes.NewReader(plaintext))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&catalog); err != nil {
		return nil, ErrFormat
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, ErrFormat
	}
	if catalog.Version != legacyRepositoryVersion || len(catalog.Profiles) > legacyMaxCatalogEntries {
		return nil, ErrFormat
	}
	out := make(map[string]catalogRef, len(catalog.Profiles))
	for _, profile := range catalog.Profiles {
		if !profile.Activated || validateCatalogName(profile.Name) != nil ||
			len(profile.ObjectID) != len(ID{}) || len(profile.GenerationID) != len(ID{}) {
			continue
		}
		var ref catalogRef
		ref.name = profile.Name
		copy(ref.objectID[:], profile.ObjectID)
		copy(ref.generationID[:], profile.GenerationID)
		out[profile.Name] = ref
	}
	/* The marker is sealed with the key that seals the body, so resolve each
	   host's key id from its own public header rather than trusting the index. */
	for name, ref := range out {
		raw, err := readBoundedFile(r.objectPath(ref),
			int64(envelopeHeaderSize+MaxProfilePlaintext+chacha20poly1305.Overhead))
		if err != nil {
			raw, err = readBoundedFile(r.legacyObjectPath(ref),
				int64(envelopeHeaderSize+MaxProfilePlaintext+chacha20poly1305.Overhead))
		}
		if err != nil {
			/* No ciphertext anywhere: the index names a host the filesystem does not
			   have. Dropping it is the honest answer, and the marker below would have
			   nothing to validate against anyway. */
			delete(out, name)
			continue
		}
		keyID, ok := envelopeKeyID(raw)
		if !ok {
			delete(out, name)
			continue
		}
		ref.keyID = keyID
		out[name] = ref
	}
	return out, nil
}
