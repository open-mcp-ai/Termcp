package extensions

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
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

// Store persists extensions as a directory per name holding two files: the
// template, and the values this installation filled into its holes.
//
// TWO FILES, AND NEITHER NAME IS A CALLER'S CONCERN. A template is edited as a
// document; a value is set by name. Which file either lands in is storage, and the
// only reason the split exists is that one of them is replaced when the extension is
// upgraded and the other must survive that.
type Store struct {
	dataDir  string
	mu       sync.Mutex
	onChange atomic.Value // func(name string), fired after Save/Delete/Rename
	profiles atomic.Pointer[sshconfig.Store]
}

const profileBindingsFileName = "profile-identities.toml"

type profileBindings struct {
	Targets map[string]string `toml:"targets,omitempty"`
}

// SetOnChange registers a callback fired after a template changes, naming it.
//
// THE NAME IS THE POINT. Without it the only thing a subscriber can do is list the
// whole store and compare it against what it holds — a walk, in other words, with a
// diff in place of a clock. With it, the write says which template moved, and the
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

func (s *Store) dir(name string) (string, error) {
	if err := sshconfig.ValidateName(name); err != nil {
		return "", err
	}
	return filepath.Join(s.root(), name), nil
}

func (s *Store) configPath(name string) (string, error) {
	dir, err := s.dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.toml"), nil
}

func (s *Store) localPath(name string) (string, error) {
	dir, err := s.dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, LocalFileName), nil
}

func (s *Store) profileBindingsPath(name string) (string, error) {
	dir, err := s.dir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, profileBindingsFileName), nil
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

// Load builds one extension: the template, filled with this installation's values,
// validated.
//
// A TEMPLATE WHOSE HOLES ARE NOT ALL FILLED DOES NOT LOAD, and the error names the
// holes. That is the honest answer — the extension is not configured yet — and it is
// what lets the reconciler report "still needs a value for {{targets}}" instead of
// starting something that cannot work.
func (s *Store) Load(name string) (*Extension, error) {
	template, err := s.Template(name)
	if err != nil {
		return nil, err
	}
	values, err := s.Values(name)
	if err != nil {
		return nil, err
	}
	rendered, err := Render(template, values)
	if err != nil {
		return nil, fmt.Errorf("extension %q: %w", name, err)
	}
	e, err := Parse(rendered)
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
	if err := s.bindProfileReferences(name, e, template); err != nil {
		return nil, err
	}
	return e, nil
}

// SetSSHConfigStore gives the extension store the profile identities its
// templates are bound to. It is separate from NewStore to avoid a package
// cycle with the composition root.
func (s *Store) SetSSHConfigStore(profiles *sshconfig.Store) {
	s.profiles.Store(profiles)
}

func (s *Store) readProfileBindings(name string) (profileBindings, error) {
	path, err := s.profileBindingsPath(name)
	if err != nil {
		return profileBindings{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return profileBindings{Targets: map[string]string{}}, nil
		}
		return profileBindings{}, err
	}
	var out profileBindings
	if err := toml.Unmarshal(data, &out); err != nil {
		return profileBindings{}, fmt.Errorf("extension %q profile identities: %w", name, err)
	}
	if out.Targets == nil {
		out.Targets = map[string]string{}
	}
	for target, identity := range out.Targets {
		if err := sshconfig.ValidateName(target); err != nil {
			return profileBindings{}, fmt.Errorf("extension %q profile identity target: %w", name, err)
		}
		id, err := uuid.Parse(identity)
		if err != nil || id == uuid.Nil || id.String() != identity {
			return profileBindings{}, fmt.Errorf("extension %q profile identity for %q is invalid", name, target)
		}
	}
	return out, nil
}

func (s *Store) writeProfileBindings(name string, bindings profileBindings) error {
	path, err := s.profileBindingsPath(name)
	if err != nil {
		return err
	}
	if len(bindings.Targets) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := toml.Marshal(bindings)
	if err != nil {
		return fmt.Errorf("encode extension %q profile identities: %w", name, err)
	}
	return writeFileAtomic(path, data)
}

func sameProfileBindings(a, b profileBindings) bool {
	if len(a.Targets) != len(b.Targets) {
		return false
	}
	for target, identity := range a.Targets {
		if b.Targets[target] != identity {
			return false
		}
	}
	return true
}

func (s *Store) bindProfileReferences(name string, e *Extension, template []byte) error {
	profiles := s.profiles.Load()
	if profiles == nil {
		return nil
	}
	bindings, err := s.readProfileBindings(name)
	if err != nil {
		return err
	}
	original := profileBindings{Targets: maps.Clone(bindings.Targets)}

	resolve := func(host, identity string) (string, string, error) {
		host = strings.TrimSpace(host)
		identity = strings.TrimSpace(identity)
		if identity == "" {
			identity = bindings.Targets[host]
		}
		if identity == "" {
			id, err := profiles.Identity(host)
			if errors.Is(err, sshconfig.ErrNotFound) {
				return host, "", nil
			}
			if err != nil {
				return "", "", err
			}
			return host, id, nil
		}
		resolved, err := profiles.NameByIdentity(identity)
		if errors.Is(err, sshconfig.ErrNotFound) {
			return host, identity, nil
		}
		if err != nil {
			return "", "", err
		}
		for boundName, boundIdentity := range bindings.Targets {
			if boundName != resolved && boundIdentity == identity {
				delete(bindings.Targets, boundName)
			}
		}
		return resolved, identity, nil
	}

	if len(e.Targets) == 0 {
		host, identity, err := resolve(e.Target, e.TargetIdentity)
		if err != nil {
			return err
		}
		e.Target, e.TargetIdentity = host, identity
		if identity != "" {
			bindings.Targets[host] = identity
		}
	} else {
		for i := range e.Targets {
			host, identity, err := resolve(e.Targets[i].Host, e.Targets[i].TargetIdentity)
			if err != nil {
				return err
			}
			e.Targets[i].Host, e.Targets[i].TargetIdentity = host, identity
			if identity != "" {
				bindings.Targets[host] = identity
			}
		}
	}
	if !sameProfileBindings(original, bindings) {
		if err := s.writeProfileBindings(name, bindings); err != nil {
			return err
		}
	}
	if len(e.Targets) == 0 && e.TargetIdentity != "" {
		var present struct {
			Identity string `toml:"target_identity"`
		}
		if err := toml.Unmarshal(template, &present); err != nil {
			return err
		}
		if present.Identity == "" {
			bound := append([]byte(fmt.Sprintf("target_identity = %q\n", e.TargetIdentity)), template...)
			path, err := s.configPath(name)
			if err != nil {
				return err
			}
			if err := writeFileAtomic(path, bound); err != nil {
				return err
			}
		}
	}
	return nil
}

// Template returns the stored template document, holes and all.
//
// THIS IS WHAT AN AUTHOR EDITS. It is not the extension: it is the recipe the
// extension is made from, which is why a hole can stand where a value belongs.
func (s *Store) Template(name string) ([]byte, error) {
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
	if len(data) > MaxConfigSize {
		return nil, fmt.Errorf("extension %q template is too large (max %d bytes)", name, MaxConfigSize)
	}
	return data, nil
}

// Values returns this installation's values for a template's holes.
//
// AN ABSENT FILE IS AN EMPTY SET, not an error: a template with no holes needs no
// values, and one with holes reports them as missing when it is loaded. A file that
// does not parse IS an error — it is the only copy of what the user typed, so
// guessing at it is the one thing not to do.
func (s *Store) Values(name string) (map[string]any, error) {
	p, err := s.localPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var values map[string]any
	if err := toml.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("extension %q values: %w", name, err)
	}
	if values == nil {
		values = map[string]any{}
	}
	return values, nil
}

// Holes returns what a template asks its installation for, with what is already
// filled. It is the answer to "what is missing" for both the panel and an agent.
func (s *Store) Holes(name string) ([]Hole, error) {
	template, err := s.Template(name)
	if err != nil {
		return nil, err
	}
	values, err := s.Values(name)
	if err != nil {
		return nil, err
	}
	return HolesFor(template, values)
}

// Missing reports the holes this installation has not filled.
func (s *Store) Missing(name string) ([]string, error) {
	template, err := s.Template(name)
	if err != nil {
		return nil, err
	}
	values, err := s.Values(name)
	if err != nil {
		return nil, err
	}
	return MissingOf(template, values)
}

// SaveTemplate stores a template document and announces the extension.
//
// VERBATIM IS THE POINT. A template carries holes, comments and formatting the
// author chose; re-marshalling it from a struct would delete the holes — they are
// not fields — and with them every value the installation supplied. So the bytes go
// in as they arrived.
//
// A TEMPLATE IS ACCEPTED WHILE ITS HOLES ARE EMPTY. That is what installing a
// package means: the recipe arrives, the values come later. What is refused is a
// template that does not say what it wants — a hole with no [params] line, or one
// embedded in a longer string.
//
// THE VALUES ARE NEVER TOUCHED. Replacing a template must not discard the hosts this
// installation filled in; that is the whole reason the two files are separate.
func (s *Store) SaveTemplate(name string, data []byte) error {
	if _, _, err := TemplateOf(data); err != nil {
		return err
	}
	// A TEMPLATE DOES NOT NAME ITSELF: the directory is the name. An exported entry
	// carries one (so a document can be imported), and pasting that entry back as a
	// single template would store a key nothing reads under a name the caller did
	// not choose.
	var doc struct {
		Name string `toml:"name"`
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return err
	}
	if got := strings.TrimSpace(doc.Name); got != "" && got != name {
		return fmt.Errorf("the document names %q, but this is being saved as %q", got, name)
	}
	p, err := s.configPath(name)
	if err != nil {
		return err
	}
	before, readErr := os.ReadFile(p)
	existed := readErr == nil
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if err := writeFileAtomic(p, data); err != nil {
		return err
	}
	// A template that now renders into something invalid is rolled back. A template
	// whose holes are merely unfilled is kept: that is the installed-but-unset state,
	// and Load reports it by naming the holes.
	if _, err := s.Load(name); err != nil && !isMissingValues(err) {
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

// SetValues writes values for a template's holes, one call for one edit.
//
// IT MERGES, because that is what setting a value means: a caller that sets
// `command` is not also saying "forget the hosts". Clearing is explicit — a nil
// value removes the entry.
//
// THE VALUES ARE STORED EVEN WHEN THEY DO NOT RENDER YET. They are the user's input
// and the only copy of it; discarding a half-typed port because the extension cannot
// start would mean the panel deletes what someone was in the middle of writing. What
// is refused is applying them, and the refusal names the reason.
func (s *Store) SetValues(name string, set map[string]any) error {
	template, err := s.Template(name)
	if err != nil {
		return err
	}
	_, written, err := TemplateOf(template)
	if err != nil {
		return err
	}
	declared, _, err := TemplateOf(template)
	if err != nil {
		return err
	}
	for key := range set {
		if !containsString(written, key) {
			return fmt.Errorf("this extension has no value called %q: it asks for %s", key, holeWord(written))
		}
		if v := set[key]; v != nil {
			if err := checkParamValue(key, declared[key].Type, wholeNumbers(v)); err != nil {
				return err
			}
		}
	}
	values, err := s.Values(name)
	if err != nil {
		return err
	}
	for key, v := range set {
		if v == nil {
			delete(values, key)
			continue
		}
		values[key] = wholeNumbers(v)
	}
	return s.writeValues(name, values)
}

// SetAllValues replaces the whole value set. A create uses it: there is nothing to
// merge with yet, and a value left over from an earlier template would be a setting
// for a hole this template does not have.
func (s *Store) SetAllValues(name string, values map[string]any) error {
	template, err := s.Template(name)
	if err != nil {
		return err
	}
	_, written, err := TemplateOf(template)
	if err != nil {
		return err
	}
	declared, _, err := TemplateOf(template)
	if err != nil {
		return err
	}
	for key, value := range values {
		if !containsString(written, key) {
			return fmt.Errorf("this extension has no value called %q: it asks for %s", key, holeWord(written))
		}
		value = wholeNumbers(value)
		if err := checkParamValue(key, declared[key].Type, value); err != nil {
			return err
		}
		values[key] = value
	}
	return s.writeValues(name, values)
}

// NotApplied reports that a value set WAS STORED but does not yet produce a working
// extension. It is not a write failure: the caller's input is on disk, and what it
// needs is the reason it has not taken effect. Callers that treat it as a failure
// make a half-typed port look like a lost edit, which is why the two are told apart
// by type rather than by message.
type NotApplied struct{ Err error }

func (e *NotApplied) Error() string { return e.Err.Error() }
func (e *NotApplied) Unwrap() error { return e.Err }

// writeValues persists a value set, then reports whether the extension can be built
// from it. The file is written either way — see SetValues.
func (s *Store) writeValues(name string, values map[string]any) error {
	p, err := s.localPath(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	data, err := toml.Marshal(values)
	if err != nil {
		return fmt.Errorf("encoding values: %w", err)
	}
	if err := writeFileAtomic(p, data); err != nil {
		return err
	}
	s.notifyChange(name)
	// The values are stored either way; whether they complete the template is the
	// answer the caller asked for, and Load is where that question is settled.
	if _, err := s.Load(name); err != nil {
		return &NotApplied{Err: err}
	}
	return nil
}

// isMissingValues reports whether an error is only "holes are still empty", which is
// a state to store rather than a template to reject.
func isMissingValues(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no value for {{")
}

// NameState reports whether a name is already stored and whether what is stored
// still loads. It is the one answer to "may this create proceed", and both
// authoring surfaces (the MCP extension tool and the WebUI's routes) ask it, so the
// two cannot drift into one of them overwriting what the other protects.
//
//	taken=false            the name is free — or unusable, which the save reports
//	taken=true, loadable=true   a live template holds it: a create is a conflict
//	taken=true, loadable=false  a stale file holds it: a create REPAIRS it
//
// The third case is the one worth naming. A template that no longer parses or
// validates makes Load fail, and the update path (which loads, then patches) cannot
// reach it either. If create refused it too, a broken extension would be fixable
// only by someone with shell access to the data directory. Overwriting it is
// therefore not an accident to prevent — it is the documented way out.
//
// AN INSTALLED-BUT-UNSET TEMPLATE COUNTS AS TAKEN AND NOT LOADABLE, which is the
// right answer for both callers: a create may replace it (it is not working yet),
// and an update still has to reach it through the template rather than through the
// rendered extension.
func (s *Store) NameState(name string) (taken, loadable bool) {
	if s.configPathErr(name) != nil {
		return false, false
	}
	if _, err := s.Template(name); err != nil {
		return false, false
	}
	_, err := s.Load(name)
	return true, err == nil
}

// configPathErr reports whether a name can be addressed at all. An unusable name is
// not "taken": the reader would reject it before touching the filesystem, and
// answering "already exists" for it sends the caller looking for a template to delete
// instead of fixing their name. Those names fall through to Validate.
func (s *Store) configPathErr(name string) error {
	_, err := s.configPath(name)
	return err
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

// Save writes an extension as a struct — the path for a template that has no holes,
// where the struct IS the document.
//
// A TEMPLATE WITH HOLES IS NOT SAVED THIS WAY. Marshalling a struct would write the
// filled values into the template and delete the holes, so the next template update
// could not change them and the installation's values would be baked in. That path is
// SaveTemplate (the document) plus SetValues (the values); asking for this one is a
// programming error, and saying so is cheaper than losing a template.
func (s *Store) Save(e *Extension) error {
	e.Normalize()
	if err := e.Validate(); err != nil {
		return err
	}
	if existing, err := s.Template(e.Name); err == nil {
		if _, written, err := TemplateOf(existing); err == nil && len(written) > 0 {
			return fmt.Errorf("extension %q is a template with values (%s): edit it as a document and set its values",
				e.Name, strings.Join(braceAll(written), ", "))
		}
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

// saveLocked writes one validated plain template. The caller holds s.mu and owns the
// change notification, which is what lets an import write a whole document under one
// lock and announce it once rather than once per template.
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
		if len(e.Targets) == 0 {
			if e.TargetIdentity != "" {
				current, err := profiles.NameByIdentity(e.TargetIdentity)
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
		} else {
			oldTargets := old.EffectiveTargets()
			for i := range e.Targets {
				if e.Targets[i].TargetIdentity != "" {
					current, err := profiles.NameByIdentity(e.Targets[i].TargetIdentity)
					if hadOld && i < len(oldTargets) &&
						e.Targets[i].TargetIdentity == oldTargets[i].TargetIdentity &&
						e.Targets[i].Host != oldTargets[i].Host &&
						e.Targets[i].Host != current {
						e.Targets[i].TargetIdentity = ""
					} else if err == nil {
						e.Targets[i].Host = current
					}
				}
				if e.Targets[i].TargetIdentity == "" {
					if id, err := profiles.Identity(e.Targets[i].Host); err == nil {
						e.Targets[i].TargetIdentity = id
					}
				}
			}
		}
	}
	if err := e.Validate(); err != nil {
		return err
	}
	data, err := toml.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal extension: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	if err := writeFileAtomic(p, data); err != nil {
		return err
	}
	if profiles := s.profiles.Load(); profiles != nil {
		bindings, err := s.readProfileBindings(e.Name)
		if err != nil {
			return err
		}
		for _, target := range e.EffectiveTargets() {
			if targetIdentity := targetIdentityForSave(e, target); targetIdentity != "" {
				bindings.Targets[target.Host] = targetIdentity
			}
		}
		if err := s.writeProfileBindings(e.Name, bindings); err != nil {
			return err
		}
	}
	return nil
}

func targetIdentityForSave(e *Extension, target TargetDecl) string {
	if len(e.Targets) == 0 {
		return e.TargetIdentity
	}
	return target.TargetIdentity
}

// Delete removes an extension and its directory. A missing one is not an error: the
// caller's intent — this name is gone — already holds.
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
	// The values go with it. Leaving them behind would let a later extension of the
	// same name inherit a stranger's hosts — and a host list is the one thing here a
	// user cannot re-derive from the template.
	_ = retryFS(func() error { return os.Remove(filepath.Join(filepath.Dir(p), LocalFileName)) })
	_ = retryFS(func() error { return os.Remove(filepath.Join(filepath.Dir(p), profileBindingsFileName)) })
	// Best effort: leftover empty dirs are harmless, but a directory holding an
	// unrelated file must not be swept away by a delete.
	_ = os.Remove(filepath.Dir(p))
	return nil
}

// writeFileAtomic writes through a temp file and renames it into place, so no reader
// ever sees a partial template. 0600 because an extension can name private paths and
// a command line.
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

// fsRetryAttempts and fsRetryDelay cover the window in which another goroutine holds
// a template open for reading. On Windows, syscall.Open does not pass
// FILE_SHARE_DELETE, so a rename or remove while a reader holds the handle fails with
// a sharing violation rather than waiting. The reconciler re-reads every template on a
// timer while an agent or the Web UI can save one, so this race is reachable in normal
// use. Both handles are transient, so a brief retry turns it back into the atomic swap
// it is everywhere else; on POSIX the first attempt already succeeded.
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

// holeWord names what a template asks for, for the error a wrong value name gets.
func holeWord(written []string) string {
	if len(written) == 0 {
		return "nothing (it has no holes)"
	}
	return strings.Join(braceAll(written), ", ")
}
