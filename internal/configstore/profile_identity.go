package configstore

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const InternalProfileIdentity = "00000000-0000-4000-8000-000000000001"

func validProfileID(id string) bool {
	u, err := uuid.Parse(id)
	return err == nil && u != uuid.Nil && u.String() == id
}

func legacyProfileID(root, name string) string {
	raw, err := readPhysicalFile(filepath.Join(root, "ssh_configs", name, "identity"), 64)
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(raw))
	if !validProfileID(id) {
		return ""
	}
	return id
}

func (r *Repository) ProfileIdentity(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ready(); err != nil {
		return "", err
	}
	if err := r.refreshProfileDirectories(); err != nil {
		return "", err
	}
	ref, ok := r.refs[name]
	if !ok || !validProfileID(ref.profileIdentity) {
		return "", ErrNotFound
	}
	return ref.profileIdentity, nil
}

func (r *Repository) SaveWithProfileIdentity(name string, data []byte, id string) error {
	return r.put([]Item{{Name: name, Data: data, ProfileIdentity: id}}, true)
}

func (r *Repository) resolveCentralProfiles(refs map[string]catalogRef) (map[string]catalogRef, bool, error) {
	state, err := loadLocalState(r.dataDir)
	if err != nil {
		return nil, false, err
	}
	sidecars := map[string]string{}
	entries, err := os.ReadDir(filepath.Join(r.dataDir, "ssh_configs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	for _, entry := range entries {
		if !entry.IsDir() || validateCatalogName(entry.Name()) != nil {
			continue
		}
		id := legacyProfileID(r.dataDir, entry.Name())
		if id == "" {
			continue
		}
		if previous, ok := sidecars[id]; ok && previous != entry.Name() {
			return nil, false, ErrIdentityMismatch
		}
		sidecars[id] = entry.Name()
	}
	result := make(map[string]catalogRef, len(refs))
	changed := false
	ids := map[string]bool{}
	for _, ref := range refs {
		if _, err := r.readObjectRaw(ref); err != nil {
			return nil, false, err
		}
		if ref.profileIdentity == "" {
			id := state.ProfileIdentities[hex.EncodeToString(ref.objectID[:])]
			if id == "" {
				id = legacyProfileID(r.dataDir, ref.name)
			}
			if strings.EqualFold(ref.name, "internal") {
				id = InternalProfileIdentity
			}
			if id == "" {
				generated, err := uuid.NewRandom()
				if err != nil {
					return nil, false, err
				}
				id = generated.String()
			}
			ref.profileIdentity = id
			changed = true
		}
		if !validProfileID(ref.profileIdentity) || ids[ref.profileIdentity] {
			return nil, false, ErrIdentityMismatch
		}
		ids[ref.profileIdentity] = true
		if actual := sidecars[ref.profileIdentity]; actual != "" && actual != ref.name {
			ref.name = actual
			changed = true
		}
		if _, ok := result[ref.name]; ok {
			return nil, false, ErrExists
		}
		result[ref.name] = ref
	}
	return result, changed, nil
}

func (r *Repository) publishCatalogEnvelope(raw []byte, profiles []catalogProfile) error {
	lock := stateLock(r.dataDir)
	lock.Lock()
	defer lock.Unlock()
	state, err := loadLocalState(r.dataDir)
	if err != nil {
		return err
	}
	state = cloneLocalState(state)
	old := state.Heads["catalog"]
	if err = applyLocalEntry(r.dataDir, &state, "catalog", raw); err != nil {
		return err
	}
	state.ProfileIdentities = map[string]string{}
	for _, p := range profiles {
		if !validProfileID(p.ProfileIdentity) {
			return ErrIdentityMismatch
		}
		state.ProfileIdentities[hex.EncodeToString(p.ObjectID)] = p.ProfileIdentity
		key := hex.EncodeToString(p.ObjectID) + "/" + hex.EncodeToString(p.GenerationID)
		state.ProfilePaths[key] = "ssh_configs/" + p.Name + "/" + hex.EncodeToString(p.GenerationID) + ".enc"
	}
	err = publishLocalState(r.dataDir, state, writeFileAtomic)
	if err == nil && old != "" && old != state.Heads["catalog"] {
		if path, e := localBodyPath(r.dataDir, old); e == nil {
			_ = removeIfExists(path)
		}
	}
	return err
}
