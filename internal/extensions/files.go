package extensions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// ExtensionFile is one file an extension is stored as, as the editor's file tabs
// show it. A plain template is one file; a modular one is config.toml, an optional
// notes sidecar, and one file per host.
//
// The list is DERIVED FROM DISK, not from the parsed struct: the tabs exist so an
// author can see and edit what is actually stored, and a list built from fields
// would hide exactly the file they came to fix.
type ExtensionFile struct {
	// Path is relative to the extension's directory, in slash form, and is what a
	// read or write names: "config.toml", "notes.md", "targets/cluster-bj.toml".
	Path  string `json:"path"`
	Label string `json:"label"`
}

// Files lists the files an extension is stored as, config.toml first.
func (s *Store) Files(name string) ([]ExtensionFile, error) {
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	if !fileExists(p) {
		return nil, fmt.Errorf("extension %q %w", name, ErrNotFound)
	}
	dir := filepath.Dir(p)
	out := []ExtensionFile{{Path: "config.toml", Label: "config.toml"}}
	if notes := modularNotesPath(dir); fileExists(notes) {
		base := filepath.Base(notes)
		out = append(out, ExtensionFile{Path: base, Label: base})
	}
	entries, err := os.ReadDir(filepath.Join(dir, "targets"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".toml") || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		rel := "targets/" + de.Name()
		out = append(out, ExtensionFile{Path: rel, Label: rel})
	}
	return out, nil
}

// filePath resolves one of Files' paths inside the extension's directory, plus the
// paths an author may add from the editor: a target file and the notes sidecar.
//
// THE NAME IS CHECKED, NOT THE SUFFIX. A path arrives from a URL, so
// "../ssh_configs/x" has to be a refusal rather than a tidy-up, and a new file may
// only be something this layer already knows how to read back: targets/<host>.toml,
// with <host> a name ssh_config itself would accept, or notes.md / notes.txt.
func (s *Store) filePath(name, rel string, create bool) (string, error) {
	p, err := s.configPath(name)
	if err != nil {
		return "", err
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSpace(rel))))
	if clean == "config.toml" {
		return filepath.Join(filepath.Dir(p), "config.toml"), nil
	}
	if !create {
		files, err := s.Files(name)
		if err != nil {
			return "", err
		}
		for _, f := range files {
			if f.Path == clean {
				return filepath.Join(filepath.Dir(p), filepath.FromSlash(f.Path)), nil
			}
		}
		return "", fmt.Errorf("extension %q has no file %q", name, rel)
	}
	switch {
	case strings.HasPrefix(clean, "targets/") && strings.HasSuffix(clean, ".toml"):
		host := strings.TrimSuffix(strings.TrimPrefix(clean, "targets/"), ".toml")
		if err := sshconfig.ValidateName(host); err != nil {
			return "", err
		}
	case clean == "notes.md" || clean == "notes.txt":
	default:
		return "", fmt.Errorf("extension %q cannot store a file at %q", name, rel)
	}
	return filepath.Join(filepath.Dir(p), filepath.FromSlash(clean)), nil
}

// ReadFile returns one stored file's bytes.
func (s *Store) ReadFile(name, rel string) ([]byte, error) {
	p, err := s.filePath(name, rel, false)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// WriteFile stores one file and announces the extension. The bytes go in as they
// arrived: this is the file view, so notes.md is the Markdown the author typed.
//
// THE LOADER IS THE VALIDATOR. A directory layout is only meaningful as a whole —
// config.toml and targets/ and the sidecar are read together — so a write is
// checked by reading the extension back after it lands, and rolled back if that
// fails. Anything less would let one tab save a template nothing can load, and the
// error the author then got would name a file they did not touch.
func (s *Store) WriteFile(name, rel string, data []byte) error {
	p, err := s.filePath(name, rel, true)
	if err != nil {
		return err
	}
	if _, err := s.configPath(name); err != nil {
		return err
	}
	before, err := os.ReadFile(p)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if err := writeFileAtomic(p, data); err != nil {
		return err
	}
	if _, err := s.Load(name); err != nil {
		if existed {
			_ = writeFileAtomic(p, before)
		} else {
			_ = retryFS(func() error { return os.Remove(p) })
		}
		return err
	}
	s.notifyChange(name)
	return nil
}
