package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/open-mcp-ai/termcp/internal/fsutil"
)

// savedAliases is the persisted half of the alias table: one JSON object under the
// data directory, name -> expansion.
//
// WHY A FILE AND NOT THE EXTENSION TEMPLATE. An alias has two possible owners and
// they are not the same thing. The ones a PACKAGE ships belong in its template,
// because they travel with the recipe (the `alias` table in internal/extensions).
// The ones the operator or an agent invents belong to this installation, like
// `create host` and `create ext` do, and they must survive a template being
// replaced by an upgrade. Putting them in the template would mean the next upgrade
// silently deletes them; putting them here means the installer and the user edit
// different files, which is the same split extensions already draw between
// config.toml and config.local.toml.
//
// WHY JSON. The value is a map of strings and nothing else — no nesting, no types,
// no comments. One stdlib marshal, one stdlib unmarshal, no dependency, and a file
// a human can read and fix by hand.
type aliasStore struct {
	path string

	// ONE SMALL LOCK OVER MEMORY ONLY. The critical sections copy the map or swap a
	// key; the file write happens after the lock is released, because a disk write
	// is an unbounded external event and holding a lock across it is how a slow
	// filesystem turns into a stalled server (see AGENTS.md).
	mu     sync.Mutex
	m      map[string]string
	loaded bool
}

// newAliasStore returns the store for one data directory. A blank dir means "this
// instance keeps no aliases on disk", which is what an embedder that passed no
// directory gets.
func newAliasStore(dataDir string) *aliasStore {
	dir := filepath.Clean(dataDir)
	if dir == "" || dir == "." {
		return nil
	}
	return &aliasStore{path: filepath.Join(dir, "aliases.json")}
}

// load returns the table, reading the file once. A missing file is an empty table,
// not an error: an installation that has never saved an alias is the normal first
// run.
func (a *aliasStore) load() map[string]string {
	a.mu.Lock()
	if a.loaded {
		m := a.m
		a.mu.Unlock()
		return m
	}
	a.mu.Unlock()
	// THE FIRST READ RUNS WITHOUT THE LOCK. Two first callers may both read the
	// file; the guard below keeps exactly one winner, so this is a duplicated read,
	// not a lost one. Holding the lock across os.ReadFile would put an unbounded
	// disk event inside the critical section.
	raw, err := os.ReadFile(a.path)
	onDisk := map[string]string{}
	if err == nil {
		if m := map[string]string{}; json.Unmarshal(raw, &m) == nil {
			onDisk = m
		}
		// A CORRUPT FILE IS NOT FATAL AND NOT SILENT: the aliases it held are gone
		// either way. Refusing to start would take the instance down over a
		// convenience file, and pretending it parsed would resurrect half a table,
		// so a parse failure lands as an empty table and the next save rewrites it.
	}
	a.mu.Lock()
	if !a.loaded {
		a.m = onDisk
		a.loaded = true
	}
	m := a.m
	a.mu.Unlock()
	return m
}

// get reads one saved entry.
func (a *aliasStore) get(name string) (string, bool) {
	if a == nil {
		return "", false
	}
	m := a.load()
	a.mu.Lock()
	defer a.mu.Unlock()
	v, ok := m[name]
	return v, ok
}

// set writes one saved entry and returns the write error, if any.
//
// THE MAP IS UPDATED BEFORE THE FILE IS, and the file write is best-effort by
// contract: the caller reports the error, but the entry is live in this process
// either way. The alternative — refuse the alias because the disk is read-only —
// would make `alias` depend on a filesystem the caller cannot see.
func (a *aliasStore) set(name, expansion string) error {
	if a == nil {
		return nil
	}
	a.load()
	a.mu.Lock()
	a.m[name] = expansion
	m := make(map[string]string, len(a.m))
	for k, v := range a.m {
		m[k] = v
	}
	a.mu.Unlock()
	return fsutil.WriteFileAtomic(a.path, marshalAliases(m))
}

// drop removes one saved entry, reporting whether it existed.
func (a *aliasStore) drop(name string) (bool, error) {
	if a == nil {
		return false, nil
	}
	a.load()
	a.mu.Lock()
	if _, ok := a.m[name]; !ok {
		a.mu.Unlock()
		return false, nil
	}
	delete(a.m, name)
	m := make(map[string]string, len(a.m))
	for k, v := range a.m {
		m[k] = v
	}
	a.mu.Unlock()
	return true, fsutil.WriteFileAtomic(a.path, marshalAliases(m))
}

// sorted returns the saved entries as a name-ordered slice, so a listing reads the
// same way twice.
func (a *aliasStore) sorted() [][2]string {
	if a == nil {
		return nil
	}
	m := a.load()
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([][2]string, 0, len(m))
	for name, expansion := range m {
		out = append(out, [2]string{name, expansion})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// marshalAliases renders one alias table as indented JSON. encoding/json sorts map
// keys, so a diff of this file after one alias changes is one line.
func marshalAliases(m map[string]string) []byte {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return []byte("{}\n")
	}
	return append(raw, '\n')
}
