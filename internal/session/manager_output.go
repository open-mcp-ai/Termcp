package session

import (
	"github.com/open-mcp-ai/termcp/internal/message"
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

// LineCount returns the number of complete lines in a shell's byte log (or,
// with an empty shellID, the session's primary shell). It lives beside
// Marks/OutputSize because it is the same kind of read: transcript metadata
// that must survive a DEAD session and resolve the primary shell the same way.
func (m *Manager) LineCount(sessionID, shellID string) (int64, error) {
	if m.msgMgr == nil {
		return 0, message.ErrLinesUnavailable
	}
	return m.msgMgr.LineCount(sessionID, m.resolveShellID(sessionID, shellID))
}

// LineStartOffset returns the byte offset where a 1-based line begins, which is
// the position `read --from-line` starts from. Same shell resolution as LineCount.
//
// It is the counterpart of LineSpanOfOffsets and the reason spans can be addressed:
// a mark records where a COMMAND began, not where its first output line began, so a
// span shown with line numbers needs this to answer "which byte is line N".
func (m *Manager) LineStartOffset(sessionID, shellID string, line int64) (int64, error) {
	if m.msgMgr == nil {
		return 0, nil
	}
	return m.msgMgr.LineStartOffset(sessionID, m.resolveShellID(sessionID, shellID), line)
}

// LineSpanOfOffsets maps a byte range onto line numbers (see message.Manager);
// same shell resolution as LineCount.
func (m *Manager) LineSpanOfOffsets(sessionID, shellID string, start, end int64) (int64, int64, error) {
	if m.msgMgr == nil {
		return 0, 0, message.ErrLinesUnavailable
	}
	return m.msgMgr.LineSpanOfOffsets(sessionID, m.resolveShellID(sessionID, shellID), start, end)
}

// StderrRanges returns the stderr byte ranges of a shell's byte log (or, with
// an empty shellID, the session's primary shell) - the transport-layer index
// the stream filter consumes. Same shell resolution as LineCount.
func (m *Manager) StderrRanges(sessionID, shellID string) ([][2]int64, error) {
	if m.msgMgr == nil {
		return nil, nil
	}
	return m.msgMgr.StderrRanges(sessionID, m.resolveShellID(sessionID, shellID))
}

// LineSpans returns the line spans [from, to] (1-based, inclusive); same shell
// resolution as LineCount.
func (m *Manager) LineSpans(sessionID, shellID string, from, to int64) ([]message.LineSpan, error) {
	if m.msgMgr == nil {
		return nil, message.ErrLinesUnavailable
	}
	return m.msgMgr.LineSpans(sessionID, m.resolveShellID(sessionID, shellID), from, to)
}
