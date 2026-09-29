package storage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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

	// markIdx caches one sparse index per shell (see markIndex). It is memory the
	// files do not have to carry: the index is rebuilt from log.jsonl on demand and
	// dropped with the shell's directory.
	idxMu   sync.Mutex
	markIdx map[string]*markIndex
}

// New creates a Store rooted at dataDir.
func New(dataDir string) *Store {
	return &Store{
		dataDir: dataDir,
		logs:    make(map[string]*os.File),
		markIdx: make(map[string]*markIndex),
	}
}

// markIndexStride is how many marks pass between two entries of a shell's sparse
// index. log.jsonl is a sequence of JSON lines, so a byte offset cannot be looked
// up in it without knowing where the lines around that offset begin; one entry
// every 64 marks is enough to land within a stride of any offset, while costing
// one entry per 64 marks in memory. Nothing is written to disk for it: the file
// stays the only copy of the index.
const markIndexStride = 64

// markRef is one entry of that index: the byte position a mark's line starts at
// in log.jsonl and the log offset its span begins on.
//
// The pair is what makes a seek possible — the position is where to read, the
// offset is what to compare against the requested window. A line's length is not
// recorded because it is never needed: reading forward from a position finds the
// next line's end.
type markRef struct {
	Offset int64
	Pos    int64
}

// markIndex is a shell's sparse index plus how far into log.jsonl it reaches.
//
// It is built by reading the file once, in order, and extended by reading only
// what has been appended since, so a shell that streams for hours pays one line's
// parse per mark ever written however many windows are read out of it. A file
// that shrank is read again from its start rather than mixing two generations of
// one path.
//
// mu guards both fields and is held only for the duration of a refresh, which
// reads the appended bytes and nothing else.
type markIndex struct {
	mu    sync.Mutex
	refs  []markRef
	size  int64 // bytes of log.jsonl already consumed
	count int64 // marks scanned so far; the stride's counter
}

// refresh brings the index up to the file's current length. A missing file is an
// empty index rather than an error: the mark index is advisory, and a shell that
// has written no marks yet is not a failure.
func (i *markIndex) refresh(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			i.reset()
			return nil
		}
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	// A smaller file is a different file (truncated or replaced): its positions and
	// offsets have nothing to do with the ones already indexed.
	if st.Size() < i.size {
		i.reset()
	}
	if grew := st.Size() - i.size; grew > 0 {
		chunk := make([]byte, grew)
		if _, err := f.ReadAt(chunk, i.size); err != nil && err != io.EOF {
			return err
		}
		i.consume(chunk)
	}
	return nil
}

// consume indexes the whole lines at the head of chunk, leaving a torn trailing
// line for the next call: half a mark must never be counted as one, or every
// entry after it would name the wrong line.
func (i *markIndex) consume(chunk []byte) {
	last := bytes.LastIndexByte(chunk, '\n')
	if last < 0 {
		return
	}
	chunk = chunk[:last+1]
	pos := i.size
	for len(chunk) > 0 {
		nl := bytes.IndexByte(chunk, '\n')
		line := chunk[:nl]
		chunk = chunk[nl+1:]
		// A malformed line is skipped without counting it, exactly as a read skips
		// it: the index and the read must agree on which lines are marks.
		if len(line) > 0 {
			var m api.LogMark
			if json.Unmarshal(line, &m) == nil {
				if i.count%markIndexStride == 0 {
					i.refs = append(i.refs, markRef{Offset: m.Offset, Pos: pos})
				}
				i.count++
			}
		}
		pos += int64(nl + 1)
	}
	i.size = pos
}

func (i *markIndex) reset() {
	i.refs = nil
	i.size = 0
	i.count = 0
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
	s.closeSessionLogs(sessionID)
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
//
// This is the whole index, for a caller that reasons over all of it (the MCP
// message list). A reader that only needs one screen's worth of a long log asks
// for a window through ReadMarksWindow instead.
func (s *Store) ReadMarks(sessionID, shellID string) ([]api.LogMark, error) {
	marks, _, err := s.readMarksWindow(sessionID, shellID, math.MinInt64, math.MaxInt64)
	return marks, err
}

// ReadMarksWindow reads a shell's status marks restricted to the byte window
// [start, end).
//
// A span's end is derived from the next mark, so the marks a window needs are
// more than the ones that start inside it: the returned slice is the part of the
// index that decides those bytes, in file order. It begins with the span the
// window starts inside — without it the first rows on screen have no status — and
// ends with the first mark at or after end, whose offset is returned separately as
// `closer`. The closer is not itself a span of the window: it is the boundary the
// last real span ends at, and a caller that wanted it as a span too would have to
// invent an end for it (the next mark's offset, or total_bytes) that this read
// cannot know.
//
// The point of the window is cost: the index is read through a sparse in-memory
// seek table (markIndex), so a request reads the marks the window contains plus
// at most one stride, no matter how long log.jsonl has grown. Callers that want
// everything pass ReadMarks, which is this with an unbounded window.
func (s *Store) ReadMarksWindow(sessionID, shellID string, start, end int64) (marks []api.LogMark, closer int64, err error) {
	// An id that names no shell is an empty answer, not an error: the mark index is
	// advisory (an empty or damaged one does not change what bytes exist), and a
	// session with no shells yet asks about exactly this.
	if validateID(sessionID) != nil || validateID(shellID) != nil {
		return nil, 0, nil
	}
	return s.readMarksWindow(sessionID, shellID, start, end)
}

// readMarksWindow is the windowed read without the id check, so the full read can
// share it: one scan path is what keeps a window and a full read from disagreeing
// about a span.
func (s *Store) readMarksWindow(sessionID, shellID string, start, end int64) ([]api.LogMark, int64, error) {
	refs, err := s.markCursors(sessionID, shellID)
	if err != nil {
		return nil, 0, err
	}
	if len(refs) == 0 {
		return nil, 0, nil
	}
	f, err := os.Open(s.MarkPath(sessionID, shellID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, err
	}
	defer f.Close()

	// Starting at the last entry at or before the window is what bounds the scan:
	// a stride's worth of marks behind it, the marks the window contains, and the
	// one that closes them.
	at := refs[seekRef(refs, start)]
	if _, err := f.Seek(at.Pos, io.SeekStart); err != nil {
		return nil, 0, err
	}

	var out []api.LogMark
	var closer int64
	var below []api.LogMark // the marks at or before the window, newest offset group
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
		if m.Offset <= start {
			// Below the window. Only the newest offset group is kept, because a later
			// mark below the window means the window starts inside that one instead.
			// The group, not just the last mark: marks may share an offset (a submitted
			// line and the review decision about it are both zero-length at the same
			// byte), and dropping the rest of the group would drop the row's status.
			if len(below) > 0 && below[len(below)-1].Offset != m.Offset {
				below = below[:0]
			}
			below = append(below, m)
			continue
		}
		if m.Offset >= end {
			// At or past the window's end: this mark is the boundary the span before it
			// ends at, and nothing after it can touch the window. It is reported as the
			// boundary rather than returned as a span, because its own span does not
			// overlap the window and its end would have to be invented.
			closer = m.Offset
			break
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	if len(below) > 0 {
		out = append(below, out...)
	}
	return out, closer, nil
}

// seekRef is the index entry to start reading from for a window starting at
// start: the last entry at or before it, or the first entry when the window
// starts before every mark. The entries are in file order, so their offsets rise.
func seekRef(refs []markRef, start int64) int {
	i := sort.Search(len(refs), func(k int) bool { return refs[k].Offset > start })
	if i == 0 {
		return 0
	}
	return i - 1
}

// markCursors returns a snapshot of a shell's sparse index, extended to the
// file's current length.
//
// The returned slice is a view of entries that are only ever appended to or
// replaced wholesale: a refresh that runs while a read is scanning cannot change
// the entries that read already holds.
func (s *Store) markCursors(sessionID, shellID string) ([]markRef, error) {
	key := sessionID + "\x00" + shellID
	s.idxMu.Lock()
	idx := s.markIdx[key]
	if idx == nil {
		idx = &markIndex{}
		s.markIdx[key] = idx
	}
	s.idxMu.Unlock()

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if err := idx.refresh(s.MarkPath(sessionID, shellID)); err != nil {
		return nil, err
	}
	return idx.refs, nil
}

// dropMarkIndex forgets one shell's index. Called when the shell's files are
// removed: an index that outlived its file would answer a later shell that
// reused the id from positions in a log that no longer exists.
func (s *Store) dropMarkIndex(sessionID, shellID string) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	delete(s.markIdx, sessionID+"\x00"+shellID)
}

// dropSessionMarkIndex forgets every shell index under one session, for the same
// reason as dropMarkIndex. The separator in the key is what keeps a session id
// from matching a longer one that starts with it.
func (s *Store) dropSessionMarkIndex(sessionID string) {
	prefix := sessionID + "\x00"
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	for k := range s.markIdx {
		if strings.HasPrefix(k, prefix) {
			delete(s.markIdx, k)
		}
	}
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
