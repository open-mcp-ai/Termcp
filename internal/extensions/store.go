package extensions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"

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
	profiles atomic.Pointer[sshconfig.Store]
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
func (s *Store) Load(name string) (*Extension, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	e.Normalize()
	if err := e.Validate(); err != nil {
		// A template that no longer validates is reported rather than silently
		// downgraded: the reconciler would otherwise keep failing to bring it
		// up with no indication of why.
		return nil, err
	}
	if profiles := s.profiles.Load(); profiles != nil {
		if e.TargetIdentity != "" {
			id, err := uuid.Parse(e.TargetIdentity)
			if err != nil || id == uuid.Nil || id.String() != e.TargetIdentity {
				return nil, errors.New("invalid target_identity")
			}
			if target, err := profiles.NameByIdentity(e.TargetIdentity); err == nil {
				e.Target = target
			}
		} else if id, err := profiles.Identity(e.Target); err == nil {
			var fields map[string]any
			if _, err := toml.Decode(string(data), &fields); err != nil {
				return nil, err
			}
			if _, present := fields["target_identity"]; present {
				return nil, errors.New("target_identity must not be empty")
			}
			// Add only the binding; retain the original template and its comments.
			bound := append([]byte("target_identity = "+strconv.Quote(id)+"\n"), data...)
			if err := writeFileAtomic(p, bound); err != nil {
				return nil, err
			}
			e.TargetIdentity = id
		}
	}
	return e, nil
}

func (s *Store) SetSSHConfigStore(profiles *sshconfig.Store) {
	s.profiles.Store(profiles)
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

// ReadRaw returns the file bytes for an editor round trip.
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
	if profiles := s.profiles.Load(); profiles != nil {
		var old Extension
		hadOld := false
		if raw, err := os.ReadFile(p); err == nil {
			hadOld = toml.Unmarshal(raw, &old) == nil
		}
		if e.TargetIdentity != "" {
			current, err := profiles.NameByIdentity(e.TargetIdentity)
			// An explicit edit to target selects a new host. A stale display name
			// in an unchanged template remains bound to its existing identity.
			if hadOld && e.TargetIdentity == old.TargetIdentity && e.Target != old.Target && e.Target != current {
				e.TargetIdentity = ""
			} else if err == nil {
				e.Target = current
			}
		}
		if e.TargetIdentity == "" {
			if id, err := profiles.Identity(e.Target); err == nil {
				e.TargetIdentity = id
			}
		}
	}
	data, err := toml.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal extension: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return writeFileAtomic(p, data)
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
