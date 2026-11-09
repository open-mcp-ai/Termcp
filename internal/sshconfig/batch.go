package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/open-mcp-ai/termcp/internal/fsutil"
)

const maxBatchSize = 16 << 20

type BatchRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type BatchImportResult struct {
	Imported int           `json:"imported"`
	Renamed  []BatchRename `json:"renamed,omitempty"`
}

// ExportBatch returns a TOML document containing remote profiles, including
// temporary ones. An empty only exports every stored profile; otherwise exactly
// the named ones are exported — a name that does not resolve is skipped rather
// than failing the whole file, so a selection captured before a deletion still
// downloads. The built-in internal profile is intentionally excluded in both
// modes (the KindRemote filter below is what drops it).
func (s *Store) ExportBatch(only []string) ([]byte, error) {
	names := only
	if len(names) == 0 {
		list, err := s.List()
		if err != nil {
			return nil, err
		}
		names = list
	}
	profiles := make([]map[string]any, 0, len(names))
	for _, name := range names {
		if IsInternalName(name) {
			continue
		}
		raw, err := s.ReadRaw(name)
		if err != nil {
			if len(only) > 0 && os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}
		var profile map[string]any
		if err := toml.Unmarshal(raw, &profile); err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}
		ent, err := ParseAndValidate(raw)
		if err != nil {
			return nil, fmt.Errorf("profile %q: %w", name, err)
		}
		if ent.Kind != KindRemote {
			continue
		}
		profile["name"] = name
		profiles = append(profiles, profile)
	}
	return toml.Marshal(struct {
		Connections []map[string]any `toml:"connections"`
	}{Connections: profiles})
}

// ImportBatch validates the entire TOML document, then assigns unique names
// before creating profiles. Existing profiles are never overwritten.
func (s *Store) ImportBatch(data []byte, temporary bool) (*BatchImportResult, error) {
	if len(data) > maxBatchSize {
		return nil, fmt.Errorf("import file too large (max %d bytes)", maxBatchSize)
	}
	var doc struct {
		Connections []map[string]any `toml:"connections"`
	}
	var top map[string]any
	err := toml.Unmarshal(data, &top)
	if err != nil {
		return nil, fmt.Errorf("import toml: %w", err)
	}
	if len(top) != 1 {
		return nil, fmt.Errorf("import toml contains unknown top-level fields")
	}
	if _, ok := top["connections"]; !ok {
		return nil, fmt.Errorf("import toml must contain connections")
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("import toml: %w", err)
	}
	if len(doc.Connections) == 0 {
		return nil, fmt.Errorf("import requires at least one connection")
	}
	type item struct {
		name string
		data []byte
	}
	items := make([]item, 0, len(doc.Connections))
	reserved := make(map[string]bool, len(doc.Connections))
	for i, profile := range doc.Connections {
		name, ok := profile["name"].(string)
		if !ok || name != strings.TrimSpace(name) || ValidateName(name) != nil {
			return nil, fmt.Errorf("connection %d has an invalid name", i+1)
		}
		reserved[strings.ToLower(name)] = true
		delete(profile, "name")
		raw, err := toml.Marshal(profile)
		if err != nil {
			return nil, fmt.Errorf("connection %q: %w", name, err)
		}
		ent, err := ParseAndValidate(raw)
		if err != nil {
			return nil, fmt.Errorf("connection %q: %w", name, err)
		}
		if ent.Kind != KindRemote {
			return nil, fmt.Errorf("connection %q must be remote", name)
		}
		items = append(items, item{name: name, data: raw})
	}

	dirs, err := os.ReadDir(s.root())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	existing := make(map[string]bool, len(dirs)+len(items))
	existing["internal"] = true
	s.temporary.Range(func(k, _ any) bool {
		existing[strings.ToLower(k.(string))] = true
		return true
	})
	for _, dir := range dirs {
		if dir.IsDir() && !strings.HasPrefix(dir.Name(), ".") {
			existing[strings.ToLower(dir.Name())] = true
		}
	}
	if !temporary {
		if err := os.MkdirAll(s.root(), 0700); err != nil {
			return nil, err
		}
	}
	result := &BatchImportResult{}

	// ONE NAME AT A TIME, claimed under that name's own token, and the claim is
	// double-checked rather than assumed: an import that read the directory before a
	// concurrent save landed must not write over it. The store-wide lock this replaces
	// also serialised every unrelated profile behind the whole import.
	claim := func(name string, data []byte) (bool, error) {
		unlock := s.lockName(name)
		defer unlock()
		if s.checkNameCollision(name, "") != nil {
			return false, nil
		}
		if temporary {
			s.temporary.Store(name, append([]byte(nil), data...))
			return true, nil
		}
		p := filepath.Join(s.root(), name, "config.toml")
		if err := os.Mkdir(filepath.Dir(p), 0700); err != nil {
			if os.IsExist(err) {
				return false, nil
			}
			return false, err
		}
		if err := fsutil.WriteFileAtomic(p, data); err != nil {
			_ = os.Remove(filepath.Dir(p))
			return false, err
		}
		return true, nil
	}
	created := make([]string, 0, len(items))
	rollback := func() {
		for _, name := range created {
			if temporary {
				s.temporary.Delete(name)
				continue
			}
			p := filepath.Join(s.root(), name, "config.toml")
			// Only remove what this import is still responsible for: a concurrent save
			// may have replaced it, and that is not ours to delete.
			if _, err := os.Stat(p); err == nil {
				_ = os.Remove(p)
				_ = os.Remove(filepath.Dir(p))
			}
		}
	}
	for _, item := range items {
		original := item.name
		name := item.name
		for {
			ok, cerr := claim(name, item.data)
			if cerr != nil {
				rollback()
				return nil, cerr
			}
			if ok {
				if name != original {
					result.Renamed = append(result.Renamed, BatchRename{From: original, To: name})
				}
				result.Imported++
				created = append(created, name)
				break
			}
			name = availableBatchName(original, existing, reserved)
			existing[strings.ToLower(name)] = true
		}
	}
	s.notifyChange()
	return result, nil
}

// availableBatchName finds a free name for a batch whose own is taken. The
// 64-byte limit is shared with the document store, and the rule with it: see
// fsutil.AvailableName.
func availableBatchName(name string, used, reserved map[string]bool) string {
	return fsutil.AvailableName(name, used, reserved, 64)
}
