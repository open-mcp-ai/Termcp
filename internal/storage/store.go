package storage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	// logs caches one open append handle per shell. Output arrives in bursts,
	// so keeping the file open turns "one open+seek+write per 4096 bytes" into
	// a single write; see the cost measurements in the design doc.
	logsMu sync.Mutex
	logs   map[string]*os.File
}

// New creates a Store rooted at dataDir.
func New(dataDir string) *Store {
	return &Store{dataDir: dataDir, logs: make(map[string]*os.File)}
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
	s.closeLog(sessionID, shellID)

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
	s.closeSessionLogs(sessionID)

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

// AppendLog appends raw bytes to a shell's log.bin and returns the offset the
// data was written at.
//
// The returned offset is the file position, which is what makes an offset
// meaningful without any bookkeeping: it is a fact about the file, not a sum of
// lengths that can drift out of agreement with the bytes.
//
// The append happens through a cached handle so a burst of small writes does not
// pay an open() per write. Callers must AppendMark *after* this returns so a
// crash can only ever leave a trailing span with no mark (attributed to the
// previous status), never a mark pointing past the end of the data.
func (s *Store) AppendLog(sessionID, shellID string, data []byte) (int64, error) {
	if err := validateID(sessionID); err != nil {
		return 0, err
	}
	if err := validateID(shellID); err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return s.LogSize(sessionID, shellID)
	}

	f, err := s.logHandle(sessionID, shellID)
	if err != nil {
		return 0, err
	}

	s.logsMu.Lock()
	defer s.logsMu.Unlock()

	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	if _, err := f.Write(data); err != nil {
		return off, err
	}
	return off, nil
}

// AppendMark appends one status transition to a shell's log.jsonl.
//
// Append this after the corresponding AppendLog: the mark then always points at
// a byte that exists. The reverse order would produce marks pointing past EOF,
// which cannot be repaired.
func (s *Store) AppendMark(sessionID, shellID string, mark api.LogMark) error {
	if err := validateID(sessionID); err != nil {
		return err
	}
	if err := validateID(shellID); err != nil {
		return err
	}
	dir := s.shellDir(sessionID, shellID)
	if err := s.initDir(dir); err != nil {
		return err
	}
	line, err := json.Marshal(mark)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	// Marks are a handful of bytes per status change, so open/append/close keeps
	// their durability story simple and never holds a second handle open.
	f, err := os.OpenFile(filepath.Join(dir, "log.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// LogSize returns the current size of a shell's log.bin, i.e. the offset just
// past the last byte ever written.
func (s *Store) LogSize(sessionID, shellID string) (int64, error) {
	st, err := os.Stat(s.LogPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return st.Size(), nil
}

// ReadLog reads at most max bytes of a shell's log.bin starting at offset.
//
// This is a positional read: it never consults log.jsonl and never accumulates
// lengths, so a window is exactly the file's bytes at that position.
func (s *Store) ReadLog(sessionID, shellID string, offset int64, max int) ([]byte, error) {
	if offset < 0 {
		offset = 0
	}
	if max <= 0 {
		return nil, nil
	}
	f, err := os.Open(s.LogPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	buf := make([]byte, max)
	n, err := f.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

// ReadMarks reads a shell's status marks in file order.
func (s *Store) ReadMarks(sessionID, shellID string) ([]api.LogMark, error) {
	f, err := os.Open(s.MarkPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var marks []api.LogMark
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m api.LogMark
		if err := json.Unmarshal(line, &m); err != nil {
			// One malformed line (a torn append) must not discard the index.
			continue
		}
		marks = append(marks, m)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return marks, nil
}

// logHandle returns the cached append handle for a shell, opening it if needed.
func (s *Store) logHandle(sessionID, shellID string) (*os.File, error) {
	key := sessionID + "\x00" + shellID

	s.logsMu.Lock()
	defer s.logsMu.Unlock()
	if f, ok := s.logs[key]; ok {
		return f, nil
	}

	dir := s.shellDir(sessionID, shellID)
	if err := s.initDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.bin"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	s.logs[key] = f
	return f, nil
}

func (s *Store) closeLog(sessionID, shellID string) {
	key := sessionID + "\x00" + shellID
	s.logsMu.Lock()
	defer s.logsMu.Unlock()
	if f, ok := s.logs[key]; ok {
		f.Close()
		delete(s.logs, key)
	}
}

// closeSessionLogs closes every cached handle belonging to a session.
func (s *Store) closeSessionLogs(sessionID string) {
	prefix := sessionID + "\x00"
	s.logsMu.Lock()
	defer s.logsMu.Unlock()
	for k, f := range s.logs {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			f.Close()
			delete(s.logs, k)
		}
	}
}

// Close releases every cached log handle. Call on shutdown so buffered writes
// are not lost.
func (s *Store) Close() error {
	s.logsMu.Lock()
	defer s.logsMu.Unlock()
	var firstErr error
	for k, f := range s.logs {
		if err := f.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
		delete(s.logs, k)
	}
	return firstErr
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
