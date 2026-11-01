package extensions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// ErrNotFound reports that no extension carries the name a caller asked for.
// Callers branch on it with errors.Is rather than matching a message.
var ErrNotFound = errors.New("extension not found")

// ErrExists reports a name collision, so a batch caller can present it as a
// per-name outcome without re-reading the store.
var ErrExists = errors.New("extension already exists")

// Store persists extensions as dataDir/extensions/<name>/config.toml, the same
// shape ssh_configs uses: one directory per name, written atomically, so a
// crash cannot leave a half-written template behind.
type Store struct {
	dataDir  string
	mu       sync.Mutex
	onChange atomic.Value // func(name string), fired after Save/Delete/Rename
}

// SetOnChange registers a callback fired after a template changes, naming it.
//
// THE NAME IS THE POINT. Without it the only thing a subscriber can do is list the
// whole store and compare it against what it holds — a walk, in other words, with
// a diff in place of a clock. With it, the write says which template moved, and the
// runtime does exactly that template.
func (s *Store) SetOnChange(fn func(name string)) {
	s.onChange.Store(fn)
}

func (s *Store) notifyChange(name string) {
	if fn, _ := s.onChange.Load().(func(string)); fn != nil {
		fn(name)
	}
}

// NewStore returns a Store rooted at dataDir, made absolute when that succeeds.
func NewStore(dataDir string) *Store {
	d := filepath.Clean(strings.TrimSpace(dataDir))
	if d == "" {
		d = "."
	}
	if abs, err := filepath.Abs(d); err == nil {
		d = abs
	}
	return &Store{dataDir: d}
}

func (s *Store) root() string {
	return filepath.Join(s.dataDir, "extensions")
}

func (s *Store) configPath(name string) (string, error) {
	if err := sshconfig.ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.root(), name, "config.toml"), nil
}

// Exists reports whether a template is stored under name.
//
// ONE STAT, and that is the point: the alternative callers reached for was
// `List()` — a ReadDir plus a Stat per entry — which turns "is this one configured"
// into a whole-directory scan. Asking that question once per extension made the
// listing O(n²) in filesystem calls (issue #152: 80 templates is 6480 stats), which
// is milliseconds on a warm Linux box and seconds on a Windows machine where every
// stat can be inspected first.
//
// An unusable name (one the reader would reject) is not a stored template.
func (s *Store) Exists(name string) bool {
	p, err := s.configPath(name)
	if err != nil {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// Load reads and validates one extension by name.
//
// MODULAR EXTENSION LAYOUT:
// When config.toml does not define notes, notes.md or notes.txt in the same directory
// is loaded as the extension's notes.
// When targets/ sub-directory exists, *.toml files within it are read and appended
// to Targets (unless config.toml declares a conflicting legacy Target).
func (s *Store) Load(name string) (*Extension, error) {
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("extension %q %w", name, ErrNotFound)
		}
		return nil, err
	}
	e, err := Parse(data)
	if err != nil {
		return nil, err
	}
	e.Name = name

	// Modular notes: notes.md (or notes.txt) is the text when it exists, so a long
	// operating manual is a file an editor opens, not an escaped TOML string.
	extDir := filepath.Dir(p)
	if b, err := os.ReadFile(modularNotesPath(extDir)); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		e.Notes = strings.TrimSpace(string(b))
	}

	// THE DIRECTORY WINS, and that is the same rule notes.md follows: a file the
	// author wrote is what they meant. When `targets/` holds anything it IS the host
	// list — `target =` and `[[targets]]` in config.toml are then ignored, not merged
	// — because merging left no way to reach the layout from the editor: adding the
	// first file made the directory invalid, and the one write that fixes it (dropping
	// `target =`) was refused while the directory was invalid. Replacement has one
	// precedence to remember; merging has two orders and a conflict to resolve.
	targetsDir := filepath.Join(extDir, "targets")
	entries, err := os.ReadDir(targetsDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading extension targets directory: %w", err)
	}
	var fromFiles []TargetDecl
	// ReadDir sorts by filename, so the list is stable across loads.
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".toml") || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		tb, err := os.ReadFile(filepath.Join(targetsDir, de.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading target file %s: %w", de.Name(), err)
		}
		var decl TargetDecl
		if err := toml.Unmarshal(tb, &decl); err != nil {
			return nil, fmt.Errorf("parsing target file %s: %w", de.Name(), err)
		}
		if decl.Host == "" {
			// The filename is the host, so `targets/cluster-bj.toml` needs no key.
			decl.Host = strings.TrimSuffix(de.Name(), ".toml")
		}
		fromFiles = append(fromFiles, decl)
	}
	if len(fromFiles) > 0 {
		e.Targets = fromFiles
		e.Target = ""
		e.Forwards = nil
	}

	e.Normalize()
	if err := e.Validate(); err != nil {
		// A template that no longer validates is reported rather than silently
		// downgraded: the reconciler would otherwise keep failing to bring it
		// up with no indication of why.
		return nil, err
	}
	return e, nil
}

// NameState reports whether a name is already stored and whether what is stored
// still loads. It is the one answer to "may this create proceed", and both
// authoring surfaces (the MCP extension tool and the WebUI's routes) ask it, so
// the two cannot drift into one of them overwriting what the other protects.
//
//	taken=false            the name is free — or unusable, which the save reports
//	taken=true, loadable=true   a live template holds it: a create is a conflict
//	taken=true, loadable=false  a stale file holds it: a create REPAIRS it
//
// The third case is the one worth naming. A config.toml that no longer parses or
// validates makes Load fail, and the update path (which loads, then patches) cannot
// reach it either. If create refused it too, a broken template would be fixable
// only by someone with shell access to the data directory. Overwriting it is
// therefore not an accident to prevent — it is the documented way out.
func (s *Store) NameState(name string) (taken, loadable bool) {
	if s.configPathErr(name) != nil {
		return false, false
	}
	if _, err := s.ReadRaw(name); err != nil {
		return false, false
	}
	_, err := s.Load(name)
	return true, err == nil
}

// configPathErr reports whether a name can be addressed at all. An unusable name
// is not "taken": the reader would reject it before touching the filesystem, and
// answering "already exists" for it sends the caller looking for a template to
// delete instead of fixing their name. Those names fall through to Validate.
func (s *Store) configPathErr(name string) error {
	_, err := s.configPath(name)
	return err
}

// ReadRaw returns config.toml bytes for an editor/export round trip. Modular notes
// and targets remain their own files; callers that need runtime data use Load.
func (s *Store) ReadRaw(name string) ([]byte, error) {
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("extension %q %w", name, ErrNotFound)
		}
		return nil, err
	}
	if _, err := Parse(data); err != nil {
		return nil, err
	}
	return data, nil
}

// List returns the sorted names of stored extensions.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.root())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if st, err := os.Stat(filepath.Join(s.root(), e.Name(), "config.toml")); err == nil && !st.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Save writes a template under its own name, creating the directory first.
// The name is the directory, so renaming means a move.
func (s *Store) Save(e *Extension) error {
	e.Normalize()
	if err := e.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	err := s.saveLocked(e)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	// ANNOUNCED AFTER THE LOCK IS RELEASED. The subscriber reads the template back
	// through this same store, so calling it under s.mu was a deadlock waiting for a
	// subscriber that does more than signal a channel — which is exactly what the
	// reconciler became when it stopped walking and started syncing by name.
	s.notifyChange(e.Name)
	return nil
}

// saveLocked writes one validated template. The caller holds s.mu and owns the
// change notification, which is what lets an import write a whole document under
// one lock and announce it once rather than once per template.
func (s *Store) saveLocked(e *Extension) error {
	p, err := s.configPath(e.Name)
	if err != nil {
		return err
	}
	if err := e.Validate(); err != nil {
		return err
	}
	// MODULAR FILES STAY THE SOURCE OF TRUTH FOR WHAT THEY HOLD. A save that copied
	// targets/*.toml back into config.toml would leave the same host list in two
	// places, and the next load would either double the hosts or refuse the file —
	// so the document written here carries only what config.toml owns.
	doc := *e
	dir := filepath.Dir(p)
	if hasModularTargets(dir) {
		doc.Targets = nil
	}
	// A notes sidecar keeps the text, so config.toml does not carry a second copy of
	// it — and the edit the author made is written where it will be read back, not
	// dropped. A sidecar that does not exist is no reason to create one: notes stay
	// in the template exactly as they were before sidecars existed.
	var notesPath string
	if p := modularNotesPath(dir); fileExists(p) {
		notesPath = p
		doc.Notes = ""
	}
	data, err := toml.Marshal(&doc)
	if err != nil {
		return fmt.Errorf("marshal extension: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if notesPath != "" {
		if err := writeFileAtomic(notesPath, []byte(e.Notes+"\n")); err != nil {
			return err
		}
	}
	return writeFileAtomic(p, data)
}

// modularNotesPath is the sidecar file that holds an extension's notes. Its
// existence — not its contents — is what makes it the source of truth, so a
// save never duplicates the text into config.toml.
func modularNotesPath(extDir string) string {
	for _, name := range []string{"notes.md", "notes.txt"} {
		if p := filepath.Join(extDir, name); fileExists(p) {
			return p
		}
	}
	return filepath.Join(extDir, "notes.md")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// hasModularTargets reports whether a host list lives in targets/*.toml.
func hasModularTargets(extDir string) bool {
	entries, err := os.ReadDir(filepath.Join(extDir, "targets"))
	if err != nil {
		return false
	}
	for _, de := range entries {
		if !de.IsDir() && strings.HasSuffix(de.Name(), ".toml") && !strings.HasPrefix(de.Name(), ".") {
			return true
		}
	}
	return false
}

// Delete removes an extension and its directory. A missing one is not an error:
// the caller's intent — this name is gone — already holds.
func (s *Store) Delete(name string) error {
	p, err := s.configPath(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	err = s.removeLocked(p)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.notifyChange(name)
	return nil
}

// removeLocked deletes one template's file and the directory that held it. The
// caller holds s.mu and owns the change notification.
func (s *Store) removeLocked(p string) error {
	if err := retryFS(func() error { return os.Remove(p) }); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Best effort: leftover empty dirs are harmless, but a directory holding an
	// unrelated file must not be swept away by a delete.
	_ = os.Remove(filepath.Dir(p))
	return nil
}

// writeFileAtomic writes through a temp file and renames it into place, so no
// reader ever sees a partial template. 0600 because an extension can name
// private paths and a command line.
func writeFileAtomic(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-ext-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return retryFS(func() error { return os.Rename(tmpPath, p) })
}

// fsRetryAttempts and fsRetryDelay cover the window in which another goroutine
// holds a template open for reading. On Windows, syscall.Open does not pass
// FILE_SHARE_DELETE, so a rename or remove while a reader holds the handle fails
// with a sharing violation rather than waiting. The reconciler re-reads every
// template on a timer while an agent or the Web UI can save one, so this race is
// reachable in normal use. Both handles are transient, so a brief retry turns it
// back into the atomic swap it is everywhere else; on POSIX the first attempt
// already succeeded.
const (
	fsRetryAttempts = 20
	fsRetryDelay    = 25 * time.Millisecond
)

func retryFS(op func() error) error {
	var err error
	for i := 0; i < fsRetryAttempts; i++ {
		if err = op(); err == nil {
			return nil
		}
		time.Sleep(fsRetryDelay)
	}
	return err
}
