package message

import (
	"errors"
	"sync"

	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// ErrSessionForgotten is returned by an append that arrives after the session's
// in-memory state was released. Bytes already written stay on disk; the caller is
// expected to stop writing, which is what the transcript loop does on any error.
var ErrSessionForgotten = errors.New("session state was released")

// Manager persists a session's transcript as a byte log per shell plus an index
// of status marks (see docs/design/session-storage.md).
//
// The byte log is the single source of truth: a byte's offset is its position in
// log.bin and is never recomputed from record lengths. The mark index only says
// which status produced which span, so it can be missing, stale, or incomplete
// without affecting where a byte lives.
//
// Appends are serialized by ownership, not by a lock. Each session has exactly
// one writer goroutine (see sessionWriter) and every append is a request to it:
// that goroutine performs both halves of an append - the bytes and the mark that
// describes them - so nothing can land between them. It also owns the remembered
// status per shell, so that map needs no lock of its own.
//
// A per-session mutex used to live in a map and be deleted by ForgetSession. An
// append that loaded the mutex before the delete and one that loaded it after
// held two different mutexes for the same log, so the byte write and its mark
// could interleave. Releasing state must not be able to split a lock in two: the
// writer here is owned by the session, its lifetime is the session's lifetime,
// and ForgetSession stops and joins it before the id can be reused.
type Manager struct {
	store *storage.Store
	// sessions holds one *sessionWriter per session id. "One writer for one
	// session" is the whole design, and it is enforced by an invariant worth
	// stating once:
	//
	//	an entry exists if and only if its writer is still running
	//
	// Both directions are what keep a second writer from appearing. Creation uses
	// LoadOrStore, so it stores a writer only where there is no entry, and there is
	// no entry only where no writer is running. Removal is done by the writer's own
	// goroutine as its last act, so an entry is never removed while its writer is
	// alive. Together they mean no two writers for one id can overlap, and nothing
	// outside this file has to preserve that by ordering its calls correctly.
	sessions sync.Map // string → *sessionWriter
}

// NewManager creates a Manager backed by the given Store.
func NewManager(store *storage.Store) *Manager {
	return &Manager{store: store}
}

// ForgetSession releases in-memory state for a session. On-disk data is kept and
// is removed only by DeleteSession.
//
// The session's writer is stopped and joined, so when this returns nothing is in
// flight and no remembered status survives. A later append for the same id starts
// a fresh writer; joining is what makes that safe, since a retired writer still
// holding the status map would otherwise share it with the new one.
//
// Note what this does not do: it does not delete the map entry. The writer removes
// its own entry as the last thing its goroutine does (see run), which is what makes
// "one writer per session" hold without this function having to sequence anything.
// If the entry were deleted here, the order of deleting against stopping would
// decide whether a concurrent append could find an empty slot and create a second
// writer for the same log:
//
//	stop then delete  - correct, but only because the order happened to be right
//	delete then stop  - an append in the window finds no entry, creates a
//	                    successor, and both writers append the same log
//
// Making that a rule the caller has to remember is what the entry-owns-itself
// arrangement avoids: an entry exists exactly while its writer is alive, so a
// successor cannot be created no matter how this function is ordered. What remains
// here is only the join, which is a liveness property (release the goroutine and
// its state) rather than a correctness one.
func (m *Manager) ForgetSession(sessionID string) {
	v, ok := m.sessions.Load(sessionID)
	if !ok {
		return
	}
	v.(*sessionWriter).stop()
}

// AppendOutput appends shell output to its byte log and returns the absolute
// offset the bytes were written at.
//
// This is the one place a byte's position is established. Callers use the
// returned offset to verify their own view of the stream, so a byte cannot be
// counted by one component and missed by another without being detected.
func (m *Manager) AppendOutput(sessionID, shellID string, data []byte) (int64, error) {
	if m.store == nil || len(data) == 0 {
		return 0, nil
	}
	w := m.writer(sessionID)
	return w.append(shellID, data)
}

// AppendMarkOnly records a status change that produced no bytes (for example an
// input the remote terminal never echoes). The mark points at the current end of
// the log so the span is empty.
func (m *Manager) AppendMarkOnly(sessionID, shellID string, status api.LogStatus) error {
	if m.store == nil {
		return nil
	}
	w := m.writer(sessionID)
	return w.markOnly(shellID, status)
}

// OutputSize returns the length of a shell's byte log, i.e. the offset just past
// its last byte. It reads the file size directly: asking a byte-range reader for
// a zero-length window to learn the size would work, but it makes the size
// depend on the reader's clamping rules rather than on the file.
func (m *Manager) OutputSize(sessionID, shellID string) (int64, error) {
	if m.store == nil {
		return 0, nil
	}
	return m.store.LogSize(sessionID, shellID)
}

// OutputByteRange returns raw bytes [start, start+max) of a shell's byte stream
// plus the stream's total size. A `start` at or past the end, or max <= 0,
// returns no bytes but still reports the true total, so a caller can tell
// "empty" from "past the end" without a second call.
//
// Both come straight from log.bin: `total` is the file size and the window is a
// positional read, so the two can never disagree.
func (m *Manager) OutputByteRange(sessionID, shellID string, start int64, max int) ([]byte, int64, error) {
	if m.store == nil {
		return nil, 0, nil
	}
	total, err := m.store.LogSize(sessionID, shellID)
	if err != nil {
		return nil, 0, err
	}
	if start < 0 {
		start = 0
	}
	if start >= total || max <= 0 {
		return nil, total, nil
	}
	data, err := m.store.ReadLog(sessionID, shellID, start, max)
	if err != nil {
		return nil, total, err
	}
	return data, total, nil
}

// Marks returns a shell's status marks (the log.jsonl index). The index is
// advisory: an empty or damaged index does not affect what bytes exist.
func (m *Manager) Marks(sessionID, shellID string) ([]api.LogMark, error) {
	if m.store == nil {
		return nil, nil
	}
	return m.store.ReadMarks(sessionID, shellID)
}

// MarksWindow returns the part of a shell's status index that decides the byte
// window [start, end): every span overlapping it, and separately the offset of
// the mark that closes the last one (0 when the window runs to the end of the
// log). It is the same index Marks returns, read through the store's sparse seek
// table, so a reader that only needs one screen of a long log does not pay for
// the whole of it.
func (m *Manager) MarksWindow(sessionID, shellID string, start, end int64) ([]api.LogMark, int64, error) {
	if m.store == nil {
		return nil, 0, nil
	}
	return m.store.ReadMarksWindow(sessionID, shellID, start, end)
}
