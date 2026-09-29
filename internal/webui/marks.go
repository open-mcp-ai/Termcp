package webui

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/pkg/api"
)

// GET /api/shells/{id}/marks: the status index of a shell's byte log.
//
// A shell's transcript is one byte stream (log.bin) plus an index of status
// marks (log.jsonl), where each mark says what produced the span starting at its
// offset. That index is what a consumer needs to draw a timeline — the spans say
// where output, human input and agent input sit — without downloading the bytes,
// which is why this endpoint exists beside output-range.
//
// It is the REST twin of the MCP message(action=list) tool: both read the same
// files through session.Manager.Marks / OutputSize, so the two surfaces cannot
// disagree about what a span is or what unit its time is in.

// markSpan is one entry of the marks response. The field names are spelled out
// (unlike the compact "s"/"t"/"i" on disk) because this is a public API body;
// the on-disk keys are an index format, not a document.
type markSpan struct {
	Status string `json:"status"` // LogStatus: o output, i API input, a MCP input, q/A review
	Time   int64  `json:"time"`   // Unix milliseconds, passed through from the mark
	Start  int64  `json:"start"`  // byte offset in log.bin where the span starts
	End    int64  `json:"end"`    // next mark's start; the last span ends at total_bytes
}

type marksResponse struct {
	Marks      []markSpan `json:"marks"`
	TotalBytes int64      `json:"total_bytes"`
	SessionID  string     `json:"session_id"`
}

// handleShellMarks answers GET /api/shells/{id}/marks.
//
// With no query parameters it returns the shell's whole index, which is the shape
// the MCP twin and every existing client read. `start` and `end` restrict it to a
// byte window, the same [start, end) an output-range request uses: a client
// drawing a terminal asks for the bytes its rows hold instead of the whole life of
// the shell, which for a long-running shell is the difference between a few
// kilobytes and megabytes. The spans of a windowed response are exact — the first
// one is the span the window starts inside and the response carries the mark that
// closes the last one — so `end` still derives to the next mark, or to total_bytes
// for the last span.
func (h *Handler) handleShellMarks(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sessionID, shellID, ok := h.marksTarget(id)
	if !ok {
		http.Error(w, "shell not found: "+id, http.StatusNotFound)
		return
	}
	start, end, windowed, ok := marksWindow(r)
	if !ok {
		http.Error(w, "start and end must be byte offsets with start <= end", http.StatusBadRequest)
		return
	}
	// total_bytes comes from the log file size, not from the marks: a shell whose
	// last bytes were written but whose mark was never appended still reports the
	// truth, and the last span then ends where the bytes do.
	total, err := h.Sessions.OutputSize(sessionID, shellID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var marks []api.LogMark
	// closer is the offset of the first mark at or past the window, i.e. where the
	// last overlapping span ends; 0 means the window runs to the end of the log.
	var closer int64
	if windowed {
		marks, closer, err = h.Sessions.MarksWindow(sessionID, shellID, start, end)
	} else {
		marks, err = h.Sessions.Marks(sessionID, shellID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, marksResponse{Marks: markSpans(marks, closer, total), TotalBytes: total, SessionID: sessionID})
}

// markSpans tiles a mark index into spans: each mark's span runs to the next
// mark's offset — `end` is derived, never stored. For the last mark it runs to
// the end of the log, or, in a window, to the mark that closes the window (the
// one mark past it is returned by the read for exactly that, and is not a span of
// it; closer of 0 means the window runs to the end of the log).
//
// Both the marks endpoint and the rail endpoint read the same manager and tile
// through here, so the two surfaces cannot disagree about what a span is.
func markSpans(marks []api.LogMark, closer, total int64) []markSpan {
	spans := make([]markSpan, 0, len(marks))
	for i, m := range marks {
		spanEnd := total
		if i+1 < len(marks) {
			spanEnd = marks[i+1].Offset
		} else if closer > m.Offset {
			spanEnd = closer
		}
		spans = append(spans, markSpan{
			Status: string(m.Status),
			Time:   m.Time,
			Start:  m.Offset,
			End:    spanEnd,
		})
	}
	return spans
}

// marksWindow reads the optional start/end byte-window parameters. Both must be
// present together and describe a valid half-open window; naming only one is a
// request the client cannot have meant, and answering it with the whole index
// would hide the bug rather than report it.
func marksWindow(r *http.Request) (start, end int64, windowed, ok bool) {
	q := r.URL.Query()
	sRaw := strings.TrimSpace(q.Get("start"))
	eRaw := strings.TrimSpace(q.Get("end"))
	if sRaw == "" && eRaw == "" {
		return 0, 0, false, true
	}
	if sRaw == "" || eRaw == "" {
		return 0, 0, false, false
	}
	start, err := strconv.ParseInt(sRaw, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false, false
	}
	end, err = strconv.ParseInt(eRaw, 10, 64)
	if err != nil || end < start {
		return 0, 0, false, false
	}
	return start, end, true, true
}

// marksTarget maps the path id to the (session_id, shell_id) pair that names a
// shell's log files. It accepts a shell_id — live, or a DEAD/restored one that
// only exists in the session's retained shell snapshot — and, like output-range,
// a session_id as a primary-shell fallback. ok is false only for an id this
// instance knows nothing about (404).
func (h *Handler) marksTarget(id string) (sessionID, shellID string, ok bool) {
	if id == "" {
		return "", "", false
	}
	// Searches live child shells and retained DEAD/restored snapshots alike, which
	// is what lets a closed session keep serving its own history.
	if sess := h.Sessions.GetSessionByShellID(id); sess != nil {
		return sess.ID, id, true
	}
	if sess := h.Sessions.Get(id); sess != nil {
		return sess.ID, "", true
	}
	return "", "", false
}
