package session

import (
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Marks returns the status index for one shell (or, with an empty shellID, the
// session's primary shell). DEAD and restart-restored sessions have no in-memory
// buffer, so this index is what says which span of the byte log holds output
// versus input.
func (m *Manager) Marks(sessionID, shellID string) ([]api.LogMark, error) {
	if m.msgMgr == nil {
		return nil, nil
	}
	return m.msgMgr.Marks(sessionID, m.resolveShellID(sessionID, shellID))
}

// MarksWindow returns the marks deciding the byte window [start, end) of one
// shell (or, with an empty shellID, the session's primary shell), plus the offset
// of the mark that closes the last of them (0 when the window runs to the end of
// the log). The marks a window answers with are the ones that start inside it plus
// the one it starts inside, so a reader can ask for the screen it is showing
// rather than the whole history; a zero-byte window is still answered, since the
// mark on its start byte is what colours a just-printed line.
func (m *Manager) MarksWindow(sessionID, shellID string, start, end int64) ([]api.LogMark, int64, error) {
	if m.msgMgr == nil {
		return nil, 0, nil
	}
	return m.msgMgr.MarksWindow(sessionID, m.resolveShellID(sessionID, shellID), start, end)
}

// OutputByteRange reads a window of a shell's persisted byte log. It exists so
// callers that only have a session id (DEAD/restored sessions) still address the
// same offset space as the live path.
func (m *Manager) OutputByteRange(sessionID, shellID string, start int64, max int) ([]byte, int64, error) {
	if m.msgMgr == nil {
		return nil, 0, nil
	}
	return m.msgMgr.OutputByteRange(sessionID, m.resolveShellID(sessionID, shellID), start, max)
}

// OutputSize returns the current length of a shell's byte log, i.e. the offset
// just past the last byte.
func (m *Manager) OutputSize(sessionID, shellID string) (int64, error) {
	if m.msgMgr == nil {
		return 0, nil
	}
	return m.msgMgr.OutputSize(sessionID, m.resolveShellID(sessionID, shellID))
}
