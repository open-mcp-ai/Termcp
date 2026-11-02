package configstore

import (
	"bytes"
	"encoding/hex"
	"path/filepath"
	"strings"
)

// Profiles retain generation ciphertexts inside their own host directory. Only
// wrapping metadata and the authentication/catalog bodies use local-state.
func validHostBodyEntry(entry string) bool {
	parts := strings.Split(entry, "/")
	if len(parts) != 3 || parts[0] != "ssh_configs" || validateCatalogName(parts[1]) != nil || filepath.Ext(parts[2]) != ".enc" {
		return false
	}
	generation := strings.TrimSuffix(parts[2], ".enc")
	raw, err := hex.DecodeString(generation)
	return err == nil && len(raw) == 16 && hex.EncodeToString(raw) == generation
}
func bodyEnvelopeKey(raw []byte) string {
	if len(raw) < dataHeaderSize {
		return ""
	}
	return hex.EncodeToString(raw[29:45]) + "/" + hex.EncodeToString(raw[45:61])
}
func bodyPathInState(root string, state localState, key string) (string, error) {
	if rel := state.ProfilePaths[key]; rel != "" {
		if !validHostBodyEntry(rel) {
			return "", ErrFormat
		}
		return filepath.Join(root, filepath.FromSlash(rel)), nil
	}
	return localBodyPath(root, key)
}

// migrateCentralProfileBodies copies only authenticated profiles. Its state
// publication makes host paths authoritative before central copies are pruned.
func (r *Repository) migrateCentralProfileBodies(refs map[string]catalogRef) error {
	lock := stateLock(r.dataDir)
	lock.Lock()
	defer lock.Unlock()
	state, err := loadLocalState(r.dataDir)
	if err != nil {
		return err
	}
	state = cloneLocalState(state)
	changed := false
	for _, ref := range refs {
		key := hex.EncodeToString(ref.objectID[:]) + "/" + hex.EncodeToString(ref.generationID[:])
		rel, err := filepath.Rel(r.dataDir, r.objectPath(ref))
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if existing := state.ProfilePaths[key]; existing != "" {
			if existing != rel {
				return ErrIdentityMismatch
			}
			continue
		}
		raw, err := readLocalEntry(r.dataDir, state, key, maxLocalState)
		if err != nil {
			return err
		}
		plain, err := r.openObjectRaw(ref, raw)
		clear(plain)
		if err != nil {
			return err
		}
		if err = applyLocalEntry(r.dataDir, &state, rel, raw); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return publishLocalState(r.dataDir, state, writeFileAtomic)
	}
	return nil
}

func (r *Repository) removeCentralProfileCopies(state localState) {
	for key, rel := range state.ProfilePaths {
		if !validHostBodyEntry(rel) {
			continue
		}
		path, err := localBodyPath(r.dataDir, key)
		if err != nil {
			continue
		}
		_ = removeIfExists(path)
	}
}

// detachedBody accepts legacy full envelopes during verified in-place migration.
// Normal bodies and future password rotations keep the exact ciphertext bytes.
func detachedBody(header, body []byte) ([]byte, error) {
	if len(header) != envelopeHeaderSize || len(body) < dataHeaderSize || !bytes.Equal(body[:dataHeaderSize], header[:dataHeaderSize]) {
		return nil, ErrIdentityMismatch
	}
	size := int(uint32(header[85]) | uint32(header[86])<<8 | uint32(header[87])<<16 | uint32(header[88])<<24)
	switch len(body) {
	case dataHeaderSize + size:
		return body, nil
	case envelopeHeaderSize + size:
		return append(bytes.Clone(body[:dataHeaderSize]), body[envelopeHeaderSize:]...), nil
	default:
		return nil, ErrFormat
	}
}

func (r *Repository) detachLegacyHostBodies(state localState) {
	for _, ref := range r.refs {
		path := r.objectPath(ref)
		body, err := readPhysicalFile(path, maxLocalState)
		if err != nil {
			continue
		}
		key := bodyEnvelopeKey(body)
		header := state.Envelopes[key]
		detached, err := detachedBody(header, body)
		if err == nil && !bytes.Equal(detached, body) {
			_ = writeFileAtomic(path, detached)
		}
	}
}
