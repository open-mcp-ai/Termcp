package message

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Manager handles persistence of session messages.
type Manager struct {
	store *storage.Store
	// session holds per-session index locks (string → *sync.Mutex).
	session sync.Map
}

// NewManager creates a Manager backed by the given Store.
func NewManager(store *storage.Store) *Manager {
	return &Manager{store: store}
}

func (m *Manager) sessionLock(sessionID string) *sync.Mutex {
	if v, ok := m.session.Load(sessionID); ok {
		return v.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := m.session.LoadOrStore(sessionID, mu)
	return actual.(*sync.Mutex)
}

// ForgetSession releases in-memory state associated with a closed session.
// Disk-backed messages are intentionally kept.
func (m *Manager) ForgetSession(sessionID string) {
	m.session.Delete(sessionID)
}

// Append records a new message and persists it. The shell id is empty, meaning
// the primary/legacy shell of the session.
func (m *Manager) Append(sessionID string, typ api.MsgType, content string) (*api.Message, error) {
	return m.AppendShell(sessionID, "", typ, content)
}

// AppendShell records a new message tagged with the originating shell id.
// An empty shellID identifies the primary/legacy shell.
func (m *Manager) AppendShell(sessionID string, shellID string, typ api.MsgType, content string) (*api.Message, error) {
	msg := api.Message{
		ID:        uuid.New().String()[:12],
		SessionID: sessionID,
		ShellID:   shellID,
		Type:      typ,
		Content:   content,
		CreatedAt: time.Now().UTC(),
		ByteSize:  len(content),
	}
	if err := m.store.SaveMessage(sessionID, msg); err != nil {
		return nil, err
	}

	mu := m.sessionLock(sessionID)
	mu.Lock()
	defer mu.Unlock()

	entries, err := m.store.LoadMessageIndex(sessionID)
	if err != nil {
		entries = []api.MessageIndexEntry{}
	}
	entries = append(entries, api.MessageIndexEntry{
		ID:        msg.ID,
		ShellID:   msg.ShellID,
		Type:      msg.Type,
		CreatedAt: msg.CreatedAt,
		ByteSize:  msg.ByteSize,
	})
	if err := m.store.SaveMessageIndex(sessionID, entries); err != nil {
		return nil, err
	}
	return &msg, nil
}

// List returns the message index for a session.
func (m *Manager) List(sessionID string) ([]api.MessageIndexEntry, error) {
	return m.store.LoadMessageIndex(sessionID)
}

// Get returns a single message by ID.
func (m *Manager) Get(sessionID, msgID string) (*api.Message, error) {
	return m.store.LoadMessage(sessionID, msgID)
}

// GetMany returns multiple messages by ID.
func (m *Manager) GetMany(sessionID string, msgIDs []string) ([]api.Message, error) {
	return m.store.LoadMessages(sessionID, msgIDs)
}

// OutputByteRange returns raw output bytes [start, start+max) of the persisted
// output stream for one shell (shellID != "") or the whole merged session
// stream (shellID == ""), plus the total persisted length. It reads the index
// only to compute offsets, then loads just the message files that overlap the
// requested window, so a large transcript is never fully materialised.
//
// This is the read path for DEAD and restart-restored sessions, which have no
// live in-memory buffer.
func (m *Manager) OutputByteRange(sessionID, shellID string, start int64, max int) ([]byte, int64, error) {
	if m.store == nil {
		return nil, 0, nil
	}
	entries, err := m.store.LoadMessageIndex(sessionID)
	if err != nil {
		return nil, 0, err
	}
	var total int64
	for i := range entries {
		e := &entries[i]
		if e.Type != api.MsgOutput || (shellID != "" && e.ShellID != shellID) {
			continue
		}
		total += int64(e.ByteSize)
	}
	if start < 0 {
		start = 0
	}
	if start >= total || max <= 0 {
		return nil, total, nil
	}
	end := start + int64(max)
	if end > total {
		end = total
	}
	out := make([]byte, 0, end-start)
	var pos int64
	for i := range entries {
		e := &entries[i]
		if e.Type != api.MsgOutput || (shellID != "" && e.ShellID != shellID) {
			continue
		}
		n := int64(e.ByteSize)
		lo, hi := pos, pos+n
		pos = hi
		if hi <= start || lo >= end {
			continue
		}
		msg, err := m.store.LoadMessage(sessionID, e.ID)
		if err != nil || msg == nil {
			continue
		}
		content := msg.Content
		s := lo
		if s < start {
			s = start
		}
		ePos := hi
		if ePos > end {
			ePos = end
		}
		relStart := s - lo
		relEnd := ePos - lo
		if relStart < int64(len(content)) {
			if relEnd > int64(len(content)) {
				relEnd = int64(len(content))
			}
			out = append(out, content[relStart:relEnd]...)
		}
	}
	return out, total, nil
}
