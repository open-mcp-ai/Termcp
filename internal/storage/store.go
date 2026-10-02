package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

var (
	ErrInvalidID = errors.New("storage: invalid ID")
	validIDRe    = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
)

func validateID(id string) error {
	if !validIDRe.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidID, id)
	}
	return nil
}

// Store persists sessions under the layout described in docs/design/session-storage.md:
//
//	<data-dir>/sessions/<session_id>/manifest.json
//	<data-dir>/sessions/<session_id>/<shell_id>/manifest.json
//	<data-dir>/sessions/<session_id>/<shell_id>/log.bin
//	<data-dir>/sessions/<session_id>/<shell_id>/log.jsonl
//
// log.bin is the single source of truth for a shell's byte stream: it is only
// ever appended to, so a byte's offset is its file position and never changes.
// log.jsonl holds one mark per status transition and carries no payload.
type Store struct {
	dataDir string
	mu      sync.RWMutex

	// logSink owns every open append handle and performs every append; see
	// logsink.go. It replaces a mutex plus a handle map: a lock guarding handles
	// that are opened on first append and closed on delete has a lifetime shorter
	// than the files it protects, and deletion is exactly where that breaks.
	// Ownership by one goroutine removes the window instead of narrowing it.
	logSink *logSink

	// markIdx caches one sparse index per shell (see markIndex). It is memory the
	// files do not have to carry: the index is rebuilt from log.jsonl on demand and
	// dropped with the shell's directory.
	idxMu   sync.Mutex
	markIdx map[string]*markIndex
}

// New creates a Store rooted at dataDir.
func New(dataDir string) *Store {
	s := &Store{
		dataDir: dataDir,
		markIdx: make(map[string]*markIndex),
	}
	// One owner goroutine per Store, started here rather than on first append so
	// that "who may write a log" has one answer from construction onward.
	s.logSink = newLogSink(s)
	return s
}

func (s *Store) initDir(path string) error {
	return os.MkdirAll(path, 0700)
}

func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// --- paths -----------------------------------------------------------------

func (s *Store) sessionsDir() string { return filepath.Join(s.dataDir, "sessions") }

func (s *Store) sessionDir(sessionID string) string {
	return filepath.Join(s.sessionsDir(), sessionID)
}

// shellDir is the directory holding one shell's manifests and log files.
func (s *Store) shellDir(sessionID, shellID string) string {
	return filepath.Join(s.sessionDir(sessionID), shellID)
}

// LogPath returns the path of a shell's byte log.
func (s *Store) LogPath(sessionID, shellID string) string {
	return filepath.Join(s.shellDir(sessionID, shellID), "log.bin")
}

// MarkPath returns the path of a shell's mark index.
func (s *Store) MarkPath(sessionID, shellID string) string {
	return filepath.Join(s.shellDir(sessionID, shellID), "log.jsonl")
}

// --- session manifests -----------------------------------------------------

// SaveSession writes one session's manifest.
func (s *Store) SaveSession(sess api.Session) error {
	if err := validateID(sess.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.sessionDir(sess.ID)
	if err := s.initDir(dir); err != nil {
		return err
	}
	return s.writeManifest(dir, sess)
}

// SaveShell writes one shell's manifest. A shell manifest is the same shape as a
// session manifest (see api.Session) minus the nested shell list, so restoring a
// DEAD tab needs no second schema.
func (s *Store) SaveShell(sessionID string, sh api.Session) error {
	if err := validateID(sessionID); err != nil {
		return err
	}
	if err := validateID(sh.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	dir := s.shellDir(sessionID, sh.ID)
	if err := s.initDir(dir); err != nil {
		return err
	}
	sh.Shells = nil // a shell manifest never nests further shells
	return s.writeManifest(dir, sh)
}

// DeleteShell removes a shell's directory (manifest + logs).
func (s *Store) DeleteShell(sessionID, shellID string) error {
	if err := validateID(sessionID); err != nil {
		return err
	}
	if err := validateID(shellID); err != nil {
		return err
	}
	s.logSink.closeShell(sessionID, shellID)
	s.dropMarkIndex(sessionID, shellID)

	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(s.shellDir(sessionID, shellID))
}

// DeleteSession removes the whole session directory. Closing a session is a
// delete, not an archive, so this is the only teardown path.
func (s *Store) DeleteSession(sessionID string) error {
	if err := validateID(sessionID); err != nil {
		return err
	}
	s.logSink.closeSession(sessionID)
	s.dropSessionMarkIndex(sessionID)

	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(s.sessionDir(sessionID))
}

// LoadSessions reads every session manifest and the shell manifests beneath it.
//
// The session list is derived from the directory tree rather than a separate
// list file: one session is one directory, so there is no second copy of the
// list to fall out of sync with the data.
func (s *Store) LoadSessions() ([]api.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	root := s.sessionsDir()
	dirs, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return []api.Session{}, nil
		}
		return nil, err
	}

	out := make([]api.Session, 0, len(dirs))
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		sess, err := s.readManifest(filepath.Join(root, d.Name()))
		if err != nil {
			// A directory without a readable manifest is not a session we can
			// describe; skip it rather than failing the whole load.
			continue
		}
		sess.Shells = s.loadShellsLocked(sess.ID)
		out = append(out, *sess)
	}
	return out, nil
}

// loadShellsLocked reads the shell manifests under a session directory, ordered
// by creation time. Caller holds s.mu.
func (s *Store) loadShellsLocked(sessionID string) []api.Session {
	dir := s.sessionDir(sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var shells []api.Session
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sh, err := s.readManifest(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		sh.Shells = nil
		shells = append(shells, *sh)
	}
	sortShellsByCreation(shells)
	return shells
}

func (s *Store) writeManifest(dir string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(filepath.Join(dir, "manifest.json"), data, 0644)
}

func (s *Store) readManifest(dir string) (*api.Session, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var sess api.Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, err
	}
	if sess.ID == "" {
		return nil, fmt.Errorf("manifest in %s has no id", dir)
	}
	return &sess, nil
}

// --- byte log --------------------------------------------------------------

// Close releases every cached log handle. Call on shutdown so buffered writes
// are not lost.
//
// It closes the handles by asking the owner goroutine to, so a Close cannot race
// an append onto a just-closed file. The goroutine itself is left running: appends
// after Close used to reopen a handle and still do, which keeps Close meaning
// "release the handles" rather than "retire this store". The cost is one goroutine
// per Store for the life of the process, which is what the previous design spent a
// mutex to avoid and is worth it for having no shared handle map at all.
func (s *Store) Close() error {
	return s.logSink.closeAll()
}

// HasPersistedHistory reports whether a session has a manifest on disk, i.e.
// whether anything about it was ever persisted.
func (s *Store) HasPersistedHistory(sessionID string) bool {
	if err := validateID(sessionID); err != nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()

	_, err := os.Stat(filepath.Join(s.sessionDir(sessionID), "manifest.json"))
	return err == nil
}

// sortShellsByCreation orders shells by creation time, then ID so the order is
// total and stable. This is the order the "termcp://#<sid>:N" locator counts, so
// it must not depend on directory read order.
func sortShellsByCreation(shells []api.Session) {
	sort.SliceStable(shells, func(i, j int) bool {
		a, b := shells[i], shells[j]
		if a.CreatedAt == b.CreatedAt {
			return a.ID < b.ID
		}
		return a.CreatedAt < b.CreatedAt
	})
}
