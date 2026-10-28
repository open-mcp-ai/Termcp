package configstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveProfileDirectories follows whole-directory renames without changing
// the encrypted object's identity or generation. Candidate bodies must match
// the authenticated catalog identity and decrypt before any catalog is updated.
func (r *Repository) resolveProfileDirectories(refs map[string]catalogRef) (map[string]catalogRef, bool, error) {
	root := filepath.Join(r.dataDir, "ssh_configs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, false, err
	}
	dirs := make([]string, 0, len(entries))
	canonical := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || validateCatalogName(name) != nil {
			continue
		}
		if previous, exists := canonical[strings.ToLower(name)]; exists {
			return nil, false, fmt.Errorf("%w: profile directories %q and %q collide", ErrExists, previous, name)
		}
		canonical[strings.ToLower(name)] = name
		dirs = append(dirs, name)
	}
	next := make(map[string]catalogRef, len(refs))
	moved := false
	for _, original := range refs {
		ref := original
		_, currentErr := os.Lstat(r.objectPath(ref))
		if currentErr == nil {
			if name := canonical[strings.ToLower(ref.name)]; name != "" {
				ref.name = name
			}
		} else if !errors.Is(currentErr, os.ErrNotExist) {
			return nil, false, currentErr
		} else if legacy, err := pathExists(r.legacyObjectPath(ref)); err != nil {
			return nil, false, err
		} else if !legacy {
			found := ""
			for _, dir := range dirs {
				candidate := original
				candidate.name = dir
				if _, err := os.Lstat(r.objectPath(candidate)); err != nil {
					continue
				}
				raw, err := r.readObjectRaw(candidate)
				if err != nil {
					return nil, false, err
				}
				data, err := r.openObjectRaw(candidate, raw)
				clear(data)
				if err != nil {
					return nil, false, err
				}
				if found != "" {
					return nil, false, fmt.Errorf("%w: encrypted object has multiple profile directories", ErrExists)
				}
				found = dir
			}
			if found == "" {
				return nil, false, fmt.Errorf("%w: committed profile %q is missing", ErrNotFound, original.name)
			}
			ref.name = found
		}
		if _, exists := next[ref.name]; exists {
			return nil, false, ErrExists
		}
		moved = moved || ref.name != original.name
		next[ref.name] = ref
	}
	return next, moved, nil
}

func (r *Repository) refreshProfileDirectories() error {
	next, moved, err := r.resolveProfileDirectories(r.refs)
	if err != nil || !moved {
		return err
	}
	if err := r.commitCatalog(r.catalogDocument(next, r.revision+1)); err != nil {
		if errors.Is(err, ErrDurability) {
			r.refs, r.revision = next, r.revision+1
		}
		return err
	}
	r.refs, r.revision = next, r.revision+1
	return nil
}
