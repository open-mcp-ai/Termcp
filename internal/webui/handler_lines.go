package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/message"
)

// GET /api/shells/{id}/lines: line-addressed reads over a shell's byte log.
//
// It is the REST twin of the MCP shell_output(from_line=...) parameter: the
// same line index (log.lines), the same absolute 1-based numbering, the same
// rule that a trailing partial line takes no number. Where output-range answers
// bytes, this answers lines - each with its raw byte range, so a caller that
// needs the exact bytes (or wants them stream-filtered via output-range) can
// address them without pulling a megabyte-sized line just to see where it ends.

// lineJSON is one entry of the lines response. Start/End are raw log.bin
// offsets; content is the raw line (newline included) and only present when
// the caller asked for it, so a huge line costs nothing until requested.
type lineJSON struct {
	N       int64  `json:"n"`
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	Content string `json:"content,omitempty"`
}

type linesResponse struct {
	Lines       []lineJSON `json:"lines"`
	TotalLines  int64      `json:"total_lines"`
	TotalBytes  int64      `json:"total_bytes"`
	SessionID   string     `json:"session_id"`
	RequestedID string     `json:"requested_id"`
}

// handleShellLines answers GET /api/shells/{id}/lines.
//
// Query: from (1-based, default 1), count (default 50, capped), content=1 to
// include the raw text of each line. from past the end is an error, not a
// silent page from elsewhere - the same contract the MCP parameter keeps.
func (h *Handler) handleShellLines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	sessionID, shellID, ok := h.marksTarget(w, r, id, capability.ShellOutput)
	if !ok {
		http.Error(w, "shell not found: "+id, http.StatusNotFound)
		return
	}

	q := r.URL.Query()
	from, err := strconv.ParseInt(strings.TrimSpace(q.Get("from")), 10, 64)
	if err != nil || from < 0 {
		from = 1
	}
	if from < 1 {
		from = 1
	}
	count, err := strconv.Atoi(strings.TrimSpace(q.Get("count")))
	if err != nil || count <= 0 {
		count = 50
	}
	const hardMaxCount = 1000
	if count > hardMaxCount {
		count = hardMaxCount
	}
	withContent := strings.TrimSpace(q.Get("content")) == "1"

	totalLines, err := h.Sessions.LineCount(sessionID, shellID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if from > totalLines {
		lerr := message.LineOutOfRangeError{Line: from, Total: totalLines}
		http.Error(w, lerr.Error(), http.StatusBadRequest)
		return
	}
	to := from + int64(count) - 1

	spans, err := h.Sessions.LineSpans(sessionID, shellID, from, to)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	lines := make([]lineJSON, 0, len(spans))
	for _, sp := range spans {
		line := lineJSON{N: sp.N, Start: sp.Start, End: sp.End}
		if withContent {
			raw, _, err := h.Sessions.OutputByteRange(sessionID, shellID, sp.Start, int(sp.End-sp.Start))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			line.Content = string(raw)
		}
		lines = append(lines, line)
	}
	totalBytes, err := h.Sessions.OutputSize(sessionID, shellID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, linesResponse{
		Lines:       lines,
		TotalLines:  totalLines,
		TotalBytes:  totalBytes,
		SessionID:   sessionID,
		RequestedID: id,
	})
}
