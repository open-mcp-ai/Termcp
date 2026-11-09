package extensions

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/open-mcp-ai/termcp/internal/fsutil"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// This file is the panel's half of authoring: one TOML document holding many
// templates, for the export a user downloads and the import that puts a
// colleague's set of extensions back. It is the same layer the single-template
// routes use — every entry is validated by Extension.Validate before anything is
// written — so a document cannot introduce a template a hand-written file would
// not be allowed to hold.
//
// The document is deliberately the same shape an export produces, `[[extensions]]`
// tables each carrying its `name`:
//
//	[[extensions]]
//	name = "docs"
//	target = "internal"
//	paths = ["~/skills/docs/"]
//
// so an export imports back unchanged.

// MaxDocumentSize caps an import. Documents are small — a template is a few
// hundred bytes — and a cap keeps a runaway upload from filling the data
// directory. It matches the profile import's limit so the two behave alike.
const MaxDocumentSize = 16 << 20

// BatchRename reports a template stored under a different name than the document
// asked for, which is the one thing an import can do that the caller has to be
// told about.
type BatchRename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// BatchImportResult is what an import did, in the terms the panel reports it.
type BatchImportResult struct {
	Imported int           `json:"imported"`
	Renamed  []BatchRename `json:"renamed,omitempty"`
}

// ExportDocument renders templates as one TOML document. An empty only exports
// every stored template; otherwise exactly the named ones — a name that no longer
// resolves is skipped rather than failing the whole file, so a selection captured
// before a deletion still downloads.
func (s *Store) ExportDocument(only []string) ([]byte, error) {
	names := only
	if len(names) == 0 {
		list, err := s.List()
		if err != nil {
			return nil, err
		}
		names = list
	}
	entries := make([]map[string]any, 0, len(names))
	for _, name := range names {
		raw, err := s.Template(name)
		if err != nil {
			if len(only) > 0 && errors.Is(err, ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("extension %q: %w", name, err)
		}
		// Round-tripped through a map rather than re-marshalled from the struct:
		// what is exported is the document that exists — holes, comments and keys
		// this build does not know included — so it can be installed elsewhere.
		//
		// THE VALUES ARE NOT EXPORTED. They are this installation's: its hosts, its
		// ports, and whatever a header or a path carries. A package that shipped them
		// would hand the next person somebody else's machines, and the one place a
		// credential is likely to sit is exactly there.
		var entry map[string]any
		if err := toml.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("extension %q: %w", name, err)
		}
		entry["name"] = name
		entries = append(entries, entry)
	}
	return toml.Marshal(struct {
		Extensions []map[string]any `toml:"extensions"`
	}{Extensions: entries})
}

// ImportDocument validates every entry, then stores the templates under names
// nothing else is using. Nothing is overwritten: a name taken by a stored
// extension or by an earlier entry of the same file is stored under the first free
// suffixed name instead, and the result says which entries moved.
//
// All of it happens under one lock with one change notification at the end, so the
// reconciler sees a document arrive rather than one extension at a time, and a
// failure partway through rolls the written templates back out.
func (s *Store) ImportDocument(data []byte) (*BatchImportResult, error) {
	if len(data) > MaxDocumentSize {
		return nil, fmt.Errorf("import file too large (max %d bytes)", MaxDocumentSize)
	}
	// Two passes, like the profile import: the first rejects a file that carries
	// anything but `extensions`, so a document with a typo in its table header is
	// refused rather than silently importing nothing.
	var top map[string]any
	if err := toml.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("import toml: %w", err)
	}
	if len(top) != 1 {
		return nil, fmt.Errorf("import toml contains unknown top-level fields")
	}
	if _, ok := top["extensions"]; !ok {
		return nil, fmt.Errorf("import toml must contain extensions")
	}
	var doc struct {
		Extensions []map[string]any `toml:"extensions"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("import toml: %w", err)
	}
	if len(doc.Extensions) == 0 {
		return nil, fmt.Errorf("import requires at least one extension")
	}

	type item struct {
		name string
		data []byte
	}
	items := make([]item, 0, len(doc.Extensions))
	// Names the document itself claims, so a stored template is never renamed onto
	// a name this same file is about to create.
	reserved := make(map[string]bool, len(doc.Extensions))
	for i, entry := range doc.Extensions {
		name, ok := entry["name"].(string)
		if !ok || name != strings.TrimSpace(name) || sshconfig.ValidateName(name) != nil {
			return nil, fmt.Errorf("extension %d has an invalid name", i+1)
		}
		reserved[strings.ToLower(name)] = true
		delete(entry, "name")
		raw, err := toml.Marshal(entry)
		if err != nil {
			return nil, fmt.Errorf("extension %q: %w", name, err)
		}
		// A PACKAGE ARRIVES WITH HOLES AND NO VALUES, so what can be checked here is
		// what a template has to satisfy on its own: it parses, and every hole it asks
		// for is declared. Whether it can run yet is the installation's question.
		//
		// A DOCUMENT WITH NO HOLES IS CHECKED ALL THE WAY, because nothing is missing
		// from it: it either validates now or it never will, and importing it would put
		// a card in the panel that can only ever say it is broken.
		_, written, err := TemplateOf(raw)
		if err != nil {
			return nil, fmt.Errorf("extension %q: %w", name, err)
		}
		if len(written) == 0 {
			if err := validatePlainTemplate(raw, name); err != nil {
				return nil, fmt.Errorf("extension %q: %w", name, err)
			}
		}
		items = append(items, item{name: name, data: raw})
	}

	list, err := s.List()
	if err != nil {
		return nil, err
	}
	existing := make(map[string]bool, len(list)+len(items))
	for _, n := range list {
		existing[strings.ToLower(n)] = true
	}
	result := &BatchImportResult{}

	// CLAIM EACH NAME BY CREATING ITS DIRECTORY, and let the filesystem arbitrate.
	//
	// os.Mkdir fails when the name is taken, and that answer is atomic across
	// processes and goroutines alike — so a template that appeared since List(), or a
	// second import running at the same time, loses the race here and gets a suffixed
	// name instead of being overwritten. This is what the store-wide lock used to buy,
	// without serialising the saves of unrelated extensions behind the import.
	type claimed struct {
		name string
		path string
		data []byte
	}
	if err := os.MkdirAll(s.root(), 0700); err != nil {
		return nil, err
	}
	created := make([]claimed, 0, len(items))
	rollback := func() {
		for _, c := range created {
			// ONLY WHAT IS STILL OURS. A concurrent save may have rewritten this file
			// since; deleting it then would throw away work this import never wrote.
			if cur, err := os.ReadFile(c.path); err == nil && bytes.Equal(cur, c.data) {
				_ = os.Remove(c.path)
				_ = os.Remove(filepath.Dir(c.path))
			}
		}
	}
	for _, it := range items {
		original := it.name
		name := it.name
		for {
			p, cerr := s.configPath(name)
			if cerr != nil {
				rollback()
				return nil, cerr
			}
			err := os.Mkdir(filepath.Dir(p), 0700)
			if err == nil {
				if err := fsutil.WriteFileAtomic(p, it.data); err != nil {
					_ = os.Remove(filepath.Dir(p))
					rollback()
					return nil, err
				}
				created = append(created, claimed{name: name, path: p, data: it.data})
				break
			}
			if !os.IsExist(err) {
				rollback()
				return nil, err
			}
			// Taken: pick the next free name and try again. `existing` is kept current
			// so two entries in one document never pick the same suffix.
			name = availableDocumentName(original, existing, reserved)
			existing[strings.ToLower(name)] = true
		}
		if name != original {
			result.Renamed = append(result.Renamed, BatchRename{From: original, To: name})
		}
		existing[strings.ToLower(name)] = true
	}
	result.Imported = len(created)
	// Each created name is announced, because the runtime's unit of work is a template.
	for _, c := range created {
		s.notifyChange(c.name)
	}
	return result, nil
}

// validatePlainTemplate checks a document that has no holes, the way Load would.
// It exists so an import can refuse what it would otherwise store as a card that
// only ever reports itself broken.
func validatePlainTemplate(raw []byte, name string) error {
	e, err := Parse(raw)
	if err != nil {
		return err
	}
	e.Name = name
	e.Normalize()
	return e.Validate()
}

// availableDocumentName finds a free name for a template whose own is taken: the
// first suffix nothing uses, counting both what is stored and what this document
// is about to store. The 64-byte limit is the document name's own.
func availableDocumentName(name string, used, reserved map[string]bool) string {
	return fsutil.AvailableName(name, used, reserved, 64)
}
