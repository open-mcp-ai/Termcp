package sshconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
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

	s.mu.Lock()
	defer s.mu.Unlock()
	dirs, err := os.ReadDir(s.root())
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	existing := make(map[string]bool, len(dirs)+len(s.temporary))
	existing["internal"] = true
	for name := range s.temporary {
		existing[strings.ToLower(name)] = true
	}
	for _, dir := range dirs {
		if dir.IsDir() && !strings.HasPrefix(dir.Name(), ".") {
			existing[strings.ToLower(dir.Name())] = true
		}
	}
	result := &BatchImportResult{Imported: len(items)}
	for i := range items {
		original := items[i].name
		if existing[strings.ToLower(original)] {
			items[i].name = availableBatchName(original, existing, reserved)
			result.Renamed = append(result.Renamed, BatchRename{From: original, To: items[i].name})
		}
		existing[strings.ToLower(items[i].name)] = true
	}
	if temporary {
		if s.temporary == nil {
			s.temporary = make(map[string][]byte)
		}
		for _, item := range items {
			s.temporary[item.name] = append([]byte(nil), item.data...)
		}
	} else {
		created := make([]string, 0, len(items))
		for _, item := range items {
			p := filepath.Join(s.root(), item.name, "config.toml")
			if err = os.MkdirAll(filepath.Dir(p), 0700); err == nil {
				created = append(created, p)
				err = writeFileAtomic(p, item.data)
			}
			if err != nil {
				for _, path := range created {
					_ = os.Remove(path)
					_ = os.Remove(filepath.Dir(path))
				}
				return nil, err
			}
		}
	}
	s.notifyChange()
	return result, nil
}

func availableBatchName(name string, used, reserved map[string]bool) string {
	for n := 2; ; n++ {
		suffix := fmt.Sprintf("-%d", n)
		base := name
		if len(base)+len(suffix) > 64 {
			base = base[:64-len(suffix)]
		}
		candidate := base + suffix
		folded := strings.ToLower(candidate)
		if !used[folded] && !reserved[folded] {
			return candidate
		}
	}
}
