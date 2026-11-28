package extensions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/BurntSushi/toml"

	"github.com/open-mcp-ai/termcp/internal/fsutil"
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
	onChange atomic.Value // func(name string), fired after Save/Delete/Rename
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
	return e, nil
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
	if err := fsutil.WriteFileAtomic(p, data); err != nil {
		return err
	}
	// A template that now renders into something invalid is rolled back. A template
	// whose holes are merely unfilled is kept: that is the installed-but-unset state,
	// and Load reports it by naming the holes.
	if _, err := s.Load(name); err != nil && !isMissingValues(err) {
		if existed {
			_ = fsutil.WriteFileAtomic(p, before)
		} else {
			_ = fsutil.Retry(func() error { return os.Remove(p) })
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
	if err := fsutil.WriteFileAtomic(p, data); err != nil {
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
	if err := s.save(e); err != nil {
		return err
	}
	// ANNOUNCED WITHOUT ANY LOCK HELD: the subscriber reads the template back through
	// this same store, and a callback under a lock is what this file must never do.
	s.notifyChange(e.Name)
	return nil
}

// save writes one validated plain template.
//
// NO LOCK: one extension is one directory and one file, and WriteFileAtomic makes the
// write whole through write-to-sibling + rename. Two saves of different extensions are
// therefore independent — which is the point: a store-wide lock serialised every
// template behind whichever disk happened to be busy.
func (s *Store) save(e *Extension) error {
	p, err := s.configPath(e.Name)
	if err != nil {
		return err
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
	return fsutil.WriteFileAtomic(p, data)
}

// Delete removes an extension and its directory. A missing one is not an error: the
// caller's intent — this name is gone — already holds.
func (s *Store) Delete(name string) error {
	p, err := s.configPath(name)
	if err != nil {
		return err
	}
	err = s.remove(p)
	if err != nil {
		return err
	}
	s.notifyChange(name)
	return nil
}

// remove deletes one template's file and the directory that held it.
func (s *Store) remove(p string) error {
	if err := fsutil.Retry(func() error { return os.Remove(p) }); err != nil && !os.IsNotExist(err) {
		return err
	}
	// The values go with it. Leaving them behind would let a later extension of the
	// same name inherit a stranger's hosts — and a host list is the one thing here a
	// user cannot re-derive from the template.
	_ = fsutil.Retry(func() error { return os.Remove(filepath.Join(filepath.Dir(p), LocalFileName)) })
	// Best effort: leftover empty dirs are harmless, but a directory holding an
	// unrelated file must not be swept away by a delete.
	_ = os.Remove(filepath.Dir(p))
	return nil
}

// holeWord names what a template asks for, for the error a wrong value name gets.
func holeWord(written []string) string {
	if len(written) == 0 {
		return "nothing (it has no holes)"
	}
	return strings.Join(braceAll(written), ", ")
}
