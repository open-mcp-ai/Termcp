package message

import (
	"strings"
	"sync"

	"github.com/open-mcp-ai/termcp/internal/clock"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Manager persists a session's transcript as a byte log per shell plus an index
// of status marks (see docs/design/session-storage.md).
//
// The byte log is the single source of truth: a byte's offset is its position in
// log.bin and is never recomputed from record lengths. The mark index only says
// which status produced which span, so it can be missing, stale, or incomplete
// without affecting where a byte lives.
type Manager struct {
	store *storage.Store
	// session holds per-session locks so concurrent appends to one log cannot
	// interleave a byte write with a mark write.
	session sync.Map // string → *sync.Mutex

	// lastStatus remembers the status of each shell's most recent mark, so a mark
	// is only appended when the status actually changes. Without this the index
	// would grow one line per read chunk.
	lastMu     sync.Mutex
	lastStatus map[string]api.LogStatus
}

// NewManager creates a Manager backed by the given Store.
func NewManager(store *storage.Store) *Manager {
	return &Manager{store: store, lastStatus: make(map[string]api.LogStatus)}
}

func (m *Manager) sessionLock(sessionID string) *sync.Mutex {
	if v, ok := m.session.Load(sessionID); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := m.session.LoadOrStore(sessionID, mu)
	return actual.(*sync.Mutex)
}

// ForgetSession releases in-memory state for a session. On-disk data is kept and
// is removed only by DeleteSession.
func (m *Manager) ForgetSession(sessionID string) {
	m.session.Delete(sessionID)

	m.lastMu.Lock()
	prefix := sessionID + "\x00"
	for k := range m.lastStatus {
		if strings.HasPrefix(k, prefix) {
			delete(m.lastStatus, k)
		}
	}
	m.lastMu.Unlock()
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
	mu := m.sessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	offset, err := m.store.AppendLog(sessionID, shellID, data)
	if err != nil {
		return 0, err
	}
	if err := m.markIfChanged(sessionID, shellID, api.LogOutput, offset); err != nil {
		// The bytes are durable; only the status mark failed. Report it, but the
		// caller can keep going: a missing mark makes the span read as a
		// continuation of the previous status, which is a labelling inaccuracy, not
		// a lost byte or a moved offset.
		return offset, err
	}
	return offset, nil
}

// AppendMarkOnly records a status change that produced no bytes (for example an
// input the remote terminal never echoes). The mark points at the current end of
// the log so the span is empty.
func (m *Manager) AppendMarkOnly(sessionID, shellID string, status api.LogStatus) error {
	if m.store == nil {
		return nil
	}
	size, err := m.store.LogSize(sessionID, shellID)
	if err != nil {
		return err
	}
	mu := m.sessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()
	return m.markIfChanged(sessionID, shellID, status, size)
}

// markIfChanged appends a mark when the shell's status differs from the last one
// recorded, so log.jsonl holds one line per transition rather than one per write.
func (m *Manager) markIfChanged(sessionID, shellID string, status api.LogStatus, offset int64) error {
	key := sessionID + "\x00" + shellID

	m.lastMu.Lock()
	prev, seen := m.lastStatus[key]
	if seen && prev == status {
		m.lastMu.Unlock()
		return nil
	}
	m.lastStatus[key] = status
	m.lastMu.Unlock()

	return m.store.AppendMark(sessionID, shellID, api.LogMark{
		Status: status,
		Time:   clock.Now(),
		Offset: offset,
	})
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
