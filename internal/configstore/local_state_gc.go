package configstore

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
)

// pruneLocalObjects runs only after the catalog, active profiles and auth object
// have authenticated. Old or never-published generations are not a fallback.
func (r *Repository) pruneLocalObjects() {
	lock := stateLock(r.dataDir)
	lock.Lock()
	defer lock.Unlock()
	state, err := loadLocalState(r.dataDir)
	if err != nil {
		return
	}
	state = cloneLocalState(state)
	keep := map[string]bool{}
	for _, key := range state.Heads {
		keep[key] = true
	}
	for _, ref := range r.refs {
		keep[hex.EncodeToString(ref.objectID[:])+"/"+hex.EncodeToString(ref.generationID[:])] = true
	}
	removed := false
	removedBodies := []string{}
	for key := range state.Envelopes {
		if !keep[key] {
			delete(state.Envelopes, key)
			if rel := state.ProfilePaths[key]; rel != "" {
				removedBodies = append(removedBodies, filepath.Join(r.dataDir, filepath.FromSlash(rel)))
				delete(state.ProfilePaths, key)
			}
			removed = true
		}
	}
	if removed {
		if err := publishLocalState(r.dataDir, state, writeFileAtomic); err != nil {
			return
		}
	}
	for _, path := range removedBodies {
		_ = removeIfExists(path)
	}
	r.detachLegacyHostBodies(state)
	r.removeCentralProfileCopies(state)
	_ = filepath.WalkDir(localObjects(r.dataDir), func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(localObjects(r.dataDir), path)
		if err != nil {
			return nil
		}
		key := strings.TrimSuffix(filepath.ToSlash(rel), ".enc")
		expected, err := localBodyPath(r.dataDir, key)
		if err == nil && expected == path && !keep[key] {
			_ = removeIfExists(path)
		}
		return nil
	})
}
