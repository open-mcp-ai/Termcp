package sshconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/open-mcp-ai/termcp/internal/fsutil"
)

// ErrNotFound reports that a named ssh config does not exist. Callers can
// branch on it with errors.Is instead of matching the message.
var ErrNotFound = errors.New("not found")

// ErrInvalid marks a profile whose CONTENT is unusable — a missing host, no
// credential, a port outside the range.
//
// It exists so the surface can answer 400 instead of 500. Those checks already
// refuse the write, but they returned bare fmt.Errorf values, so nothing upstream
// could tell "the caller wrote an incomplete profile" from "the write failed" and
// every one of them surfaced as operation_failed.
var ErrInvalid = errors.New("invalid ssh config")

// ErrReserved reports that a name denotes the built-in internal profile, which
// is virtual and cannot be deleted. It is a sentinel so a batch caller can turn
// it into its own per-name code without re-testing the name or restating the
// message that Delete already carries.
var ErrReserved = errors.New("reserved ssh config")

// Store manages dataDir/ssh_configs/<name>/config.toml for remote profiles.
// The built-in "internal" profile is virtual: it is never written to disk.
type Store struct {
	dataDir string

	// temporary holds process-local profiles. A sync.Map: each entry is one name's
	// whole state and no rule spans two names.
	temporary sync.Map // name string -> []byte

	// nameTokens serialises the few operations that are a check-and-write on ONE
	// name (collision test, then file or temporary entry). The key is the lowercased
	// name, because uniqueness is case-insensitive — so `prod` and `PROD` share a
	// token while `prod` and `staging` do not. That is the whole point of splitting
	// what used to be one lock: unrelated profiles save in parallel.
	nameTokens sync.Map // lower(name) string -> chan struct{} (1-slot token)

	onChange atomic.Value // func(), set via SetOnChange, called after Save/Delete/Rename
}

// lockName takes the token for one name and returns its release. Tokens are 1-slot
// channels rather than mutexes so the acquisition is a plain send, and a caller that
// already holds another name's token can never deadlock by ordering.
func (s *Store) lockName(name string) func() {
	key := strings.ToLower(name)
	tok := make(chan struct{}, 1)
	tok <- struct{}{}
	actual, _ := s.nameTokens.LoadOrStore(key, tok)
	ch := actual.(chan struct{})
	<-ch
	return func() { ch <- struct{}{} }
}

// lockNames takes several tokens in sorted order, so two callers that need the same
// pair (a rename) cannot each hold what the other wants.
func (s *Store) lockNames(names ...string) func() {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	releases := make([]func(), 0, len(sorted))
	for _, n := range sorted {
		releases = append(releases, s.lockName(n))
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
}

// SetOnChange registers a callback fired after Save, Delete, or Rename succeeds.
func (s *Store) SetOnChange(fn func()) {
	s.onChange.Store(fn)
}

func (s *Store) notifyChange() {
	fn, _ := s.onChange.Load().(func())
	if fn != nil {
		fn()
	}
}

// NewStore returns a Store rooted at dataDir (always absolute path when Abs succeeds).
func NewStore(dataDir string) *Store {
	d := filepath.Clean(strings.TrimSpace(dataDir))
	if d == "" {
		d = "."
	}
	abs, err := filepath.Abs(d)
	if err != nil {
		abs = d
	}
	return &Store{dataDir: abs}
}

func (s *Store) root() string {
	return filepath.Join(s.dataDir, "ssh_configs")
}

// ConfigDir returns the directory containing config.toml for a named profile.
// For the virtual internal profile this path is not used for I/O.
func (s *Store) ConfigDir(name string) string {
	return filepath.Join(s.root(), name)
}

func (s *Store) configPath(name string) (string, error) {
	if err := ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.root(), name, "config.toml"), nil
}

// Load reads and validates a named config.
// name "internal" is the built-in entry, optionally overridden by a stored
// internal/config.toml (see ParseInternalOverride).
func (s *Store) Load(name string) (*Entry, error) {
	name = strings.TrimSpace(name)
	if IsInternalName(name) {
		base := InternalEntry()
		data, err := os.ReadFile(s.internalOverridePath())
		if err != nil {
			if os.IsNotExist(err) {
				return base, nil // no override: the defaults ARE the profile
			}
			return nil, err
		}
		ent, err := ParseInternalOverride(data, base)
		if err != nil {
			// A broken override is reported, not silently ignored: falling back
			// to the defaults would run the session UNGATED after the user asked
			// for a gate, which is the one direction this must never fail.
			return nil, fmt.Errorf("internal profile override: %w", err)
		}
		return ent, nil
	}
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	if v, inMemory := s.temporary.Load(name); inMemory {
		return ParseAndValidate(v.([]byte))
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("ssh config %q %w (use data-dir/ssh_configs/%s/config.toml)", name, ErrNotFound, name)
		}
		return nil, err
	}
	return ParseAndValidate(data)
}

// ReadRaw returns the raw config.toml bytes for a name.
//
// For the internal profile it returns the stored override when one exists and
// the built-in template otherwise, so the editor round-trips what is actually
// in effect rather than a constant it cannot change.
func (s *Store) ReadRaw(name string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if IsInternalName(name) {
		data, err := os.ReadFile(s.internalOverridePath())
		if err == nil {
			return data, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		return InternalTemplate(), nil
	}
	p, err := s.configPath(name)
	if err != nil {
		return nil, err
	}
	if v, inMemory := s.temporary.Load(name); inMemory {
		return append([]byte(nil), v.([]byte)...), nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if _, err := ParseAndValidate(data); err != nil {
		return nil, err
	}
	return data, nil
}

// List returns sorted config names: virtual "internal", temporary profiles, and profiles on disk.
// Leftover ssh_configs/internal/ directories are ignored.
func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.root())
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	names := []string{"internal"}
	seen := map[string]bool{"internal": true}
	s.temporary.Range(func(k, _ any) bool {
		name := k.(string)
		names = append(names, name)
		seen[name] = true
		return true
	})
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		if IsInternalName(name) || seen[name] {
			continue // disk leftover; virtual entry already listed
		}
		cfg := filepath.Join(s.root(), name, "config.toml")
		if st, err := os.Stat(cfg); err == nil && !st.IsDir() {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Save writes config for a name (validates first).
//
// The internal profile is saved as an override file that Load layers over the
// built-in defaults, so setting one field does not mean restating the rest.
func (s *Store) Save(name string, data []byte) error {
	return s.SaveWithOptions(name, data, false)
}

// SaveWithOptions stores a remote profile only in memory when temporary is true.
// Switching an existing profile between modes removes its previous copy.
func (s *Store) SaveWithOptions(name string, data []byte, temporary bool) error {
	if IsInternalName(name) {
		if temporary {
			return fmt.Errorf("internal profile cannot be temporary")
		}
		if _, err := ParseInternalOverride(data, InternalEntry()); err != nil {
			return err
		}
		p := s.internalOverridePath()
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			return err
		}
		if err := fsutil.WriteFileAtomic(p, data); err != nil {
			return err
		}
		s.notifyChange()
		return nil
	}
	if _, err := ParseAndValidate(data); err != nil {
		return err
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	unlock := s.lockName(name)
	defer unlock()
	if err := s.checkNameCollision(name, name); err != nil {
		return err
	}
	p := filepath.Join(s.root(), name, "config.toml")
	if temporary {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		_ = os.Remove(filepath.Dir(p))
		s.temporary.Store(name, append([]byte(nil), data...))
		s.notifyChange()
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(p, data); err != nil {
		return err
	}
	s.temporary.Delete(name)
	s.notifyChange()
	return nil
}

// IsTemporary reports whether the profile exists only in this process.
func (s *Store) IsTemporary(name string) bool {
	_, ok := s.temporary.Load(name)
	return ok
}

// checkNameCollision reports whether the name is taken, case-insensitively. The
// caller holds that name's token, which is what makes the answer usable: without it
// another saver could claim the name between this check and the write.
func (s *Store) checkNameCollision(name, exclude string) error {
	var clash string
	s.temporary.Range(func(k, _ any) bool {
		existing := k.(string)
		if existing != exclude && strings.EqualFold(existing, name) {
			clash = existing
			return false
		}
		return true
	})
	if clash != "" {
		return fmt.Errorf("config already exists: %s", clash)
	}
	entries, err := os.ReadDir(s.root())
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && entry.Name() != exclude && strings.EqualFold(entry.Name(), name) {
			return fmt.Errorf("config already exists: %s", entry.Name())
		}
	}
	return nil
}

// internalOverridePath is where a stored internal-profile override lives. The
// name is dot-prefixed so the directory scan that builds the profile list skips
// it (that scan treats any dot-directory as private).
func (s *Store) internalOverridePath() string {
	return filepath.Join(s.root(), ".internal", "config.toml")
}

// writeFileAtomic writes data through a temp file and renames it into place, so
// a crash cannot leave a half-written config behind. The file is 0600: profiles
// hold credentials.
// Rename moves a remote config directory to a new validated name.
func (s *Store) Rename(oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == newName {
		return nil
	}
	if IsInternalName(oldName) || IsInternalName(newName) {
		return fmt.Errorf("cannot rename reserved ssh config %q", "internal")
	}
	oldPath, err := s.configPath(oldName)
	if err != nil {
		return err
	}
	newPath, err := s.configPath(newName)
	if err != nil {
		return err
	}

	// BOTH NAMES, TAKEN IN SORTED ORDER. A rename moves two names at once, so it is
	// the one operation here that needs two tokens; sorting is what stops a concurrent
	// rename of the pair in the other direction from deadlocking against it.
	unlock := s.lockNames(oldName, newName)
	defer unlock()
	if err := s.checkNameCollision(newName, oldName); err != nil {
		return err
	}
	if v, ok := s.temporary.Load(oldName); ok {
		if _, exists := s.temporary.Load(newName); exists {
			return fmt.Errorf("config already exists: %s", newName)
		}
		if _, err := os.Stat(newPath); err == nil {
			return fmt.Errorf("config already exists: %s", newName)
		} else if !os.IsNotExist(err) {
			return err
		}
		s.temporary.Store(newName, v)
		s.temporary.Delete(oldName)
		s.notifyChange()
		return nil
	}

	oldDir := filepath.Dir(oldPath)
	newDir := filepath.Dir(newPath)
	if st, err := os.Stat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("ssh config %q %w", oldName, ErrNotFound)
		}
		return err
	} else if st.IsDir() {
		return fmt.Errorf("ssh config %q is not a file", oldName)
	}
	// Case-insensitive collision check (Windows/macOS).
	entries, err := os.ReadDir(s.root())
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != oldName && strings.EqualFold(e.Name(), newName) {
			return fmt.Errorf("config already exists: %s", filepath.Join(s.root(), e.Name(), "config.toml"))
		}
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return err
	}
	s.notifyChange()
	return nil
}

// Delete removes a remote config; the virtual internal profile cannot be deleted.
func (s *Store) Delete(name string) error {
	if IsInternalName(name) {
		return fmt.Errorf("cannot delete reserved ssh config %q: %w", name, ErrReserved)
	}
	p, err := s.configPath(name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	unlock := s.lockName(name)
	defer unlock()
	s.temporary.Delete(name)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(dir)
	s.notifyChange()
	return nil
}
