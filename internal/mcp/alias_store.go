package mcp

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/BurntSushi/toml"
	"github.com/open-mcp-ai/termcp/internal/fsutil"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

// savedAliases manages the persistent half of the alias table: one file per alias
// under `<data-dir>/aliases/<name>.toml`.
//
// ZERO MUTEXES BY DESIGN (see AGENTS.md concurrency rules). An alias table is a
// key-value store with no multi-key invariant: every lookup, insert, update and
// removal touches exactly one entry. sync.Map provides atomic Load, Store, Delete,
// and Range without locks to manage or forget, and WriteFileAtomic commits each
// file without holding any in-memory synchronization.
//
// ONE FILE PER ALIAS (<name>.toml): an alias is an independent unit of configuration,
// matching the per-entry layout used by ssh_configs/<name>/ and extensions/<name>/.
// Editing or deleting one alias touches only its own file, so concurrent writes
// never collide or overwrite each other.
type aliasStore struct {
	dir    string
	loaded atomic.Bool
	m      sync.Map // name string -> expansion string
}

// aliasFile is the TOML structure stored on disk.
type aliasFile struct {
	Expansion string `toml:"expansion"`
}

// newAliasStore returns the store for one data directory. A blank dir means
// "this instance keeps no aliases on disk", which leaves the table process-local.
func newAliasStore(dataDir string) *aliasStore {
	dir := filepath.Clean(dataDir)
	if dir == "" || dir == "." {
		return nil
	}
	return &aliasStore{dir: filepath.Join(dir, "aliases")}
}

// filePath returns the on-disk path for one alias, validating the name to prevent
// directory traversal.
func (a *aliasStore) filePath(name string) (string, error) {
	if err := sshconfig.ValidateName(name); err != nil {
		return "", fmt.Errorf("invalid alias name %q: %w", name, err)
	}
	return filepath.Join(a.dir, name+".toml"), nil
}

// ensureLoaded reads all `<dir>/*.toml` files on first access.
// atomic.CompareAndSwap ensures exactly one reader performs the disk scan.
func (a *aliasStore) ensureLoaded() {
	if a == nil || a.loaded.Load() {
		return
	}
	if !a.loaded.CompareAndSwap(false, true) {
		return
	}
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		name := entry.Name()[:len(entry.Name())-5]
		if err := sshconfig.ValidateName(name); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(a.dir, entry.Name()))
		if err != nil {
			continue
		}
		var f aliasFile
		if _, err := toml.Decode(string(data), &f); err == nil && f.Expansion != "" {
			a.m.Store(name, f.Expansion)
		}
	}
}

// get reads one saved entry.
func (a *aliasStore) get(name string) (string, bool) {
	if a == nil {
		return "", false
	}
	a.ensureLoaded()
	v, ok := a.m.Load(name)
	if !ok {
		return "", false
	}
	s, _ := v.(string)
	return s, true
}

// set writes one alias to `<dir>/<name>.toml` and updates the sync.Map.
//
// ZERO LOCKS: the disk write uses WriteFileAtomic (write-to-sibling + rename), and
// the map is updated only after the file is successfully durable on disk.
func (a *aliasStore) set(name, expansion string) error {
	if a == nil {
		return nil
	}
	a.ensureLoaded()
	p, err := a.filePath(name)
	if err != nil {
		return err
	}
	// THE DIRECTORY IS CREATED ON FIRST SAVE, not at construction: a store that was
	// never written to should not leave a directory behind either.
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(aliasFile{Expansion: expansion}); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(p, buf.Bytes()); err != nil {
		return err
	}
	a.m.Store(name, expansion)
	return nil
}

// drop removes `<dir>/<name>.toml` and drops the entry from the sync.Map.
func (a *aliasStore) drop(name string) (bool, error) {
	if a == nil {
		return false, nil
	}
	a.ensureLoaded()
	p, err := a.filePath(name)
	if err != nil {
		return false, err
	}
	_, existed := a.m.LoadAndDelete(name)
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return existed, err
	}
	return existed, nil
}

// sorted returns the saved entries in name order.
func (a *aliasStore) sorted() [][2]string {
	if a == nil {
		return nil
	}
	a.ensureLoaded()
	var out [][2]string
	a.m.Range(func(k, v any) bool {
		name, _ := k.(string)
		expansion, _ := v.(string)
		out = append(out, [2]string{name, expansion})
		return true
	})
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
