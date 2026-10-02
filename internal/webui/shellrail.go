package webui

import (
	"net/http"
	"strconv"
	"strings"
)

// GET /api/shells/{id}/rail: which bytes of a shell's log sit on which terminal row.
//
// The web UI draws a timeline rail beside a terminal: one cell per row, coloured
// by what that row holds. To do that it needs the row<->byte mapping, and this
// endpoint answers it by replaying the log — nothing records the mapping as
// output arrives.
//
// A recorded mapping cannot survive the two things that happen to a terminal
// anyway. A reloaded channel delivers its whole transcript in one write, so there
// is one recording for the entire scrollback, and a window resize reflows every
// line, so every recorded row number is stale. The log and the width are the only
// inputs the mapping has, so it is derived from them per request: the same call
// serves a channel that just loaded, a window the user just resized, and a scroll.
//
// # What the model is
//
// The bytes are replayed through a small terminal model at the requested width. A
// line break or the end of a row starts a new one, a carriage return overwrites
// from the first column (progress bars), and the cursor-addressing sequences a
// full-screen program uses move the cursor. Each row ends up with the byte range
// it displays, so a mark covering those bytes decides that row's status.
//
// It models what puts text on a row and ignores what only paints it — colours and
// the modes that decide how a terminal behaves are parsed but not applied. The
// transcript of a normal shell (prompts, commands, their output) is therefore
// exact. A full-screen program's screen is not part of that transcript at all: it
// paints the alternate screen, and nothing of it is recorded, so the rows that
// come back when the program exits are the ones standing there before it started.
// What stays approximate is a program that repaints rows it already passed without
// the alternate screen — a model with no scrolling region cannot follow that
// faithfully.

// shellRowSpan is one terminal row's byte range in a shell's log. A null entry in
// a response is a row holding no bytes — an erased line, or a row no output ever
// reached — which draws no cell rather than borrowing a neighbour's status.
//
// The rows tile the log: every byte the terminal consumed belongs to the row the
// cursor was on when it arrived, the newline that ends a row included. A row is
// therefore never the blank between two others, and a mark cannot fall between
// them — a zero-length input mark sitting on the very newline that submitted the
// line colours that line, which is the row a reader looks at when asking where the
// input was. Blank lines come out of the same rule: a newline and nothing else is a
// row's content, because a blank line a program printed is still output.
//
// Erasing is the deliberate exception. A row the terminal cleared holds nothing,
// because its bytes are not on screen any more: that is what the erase meant, and
// it is why the row draws no cell.
type shellRowSpan struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// shellRailResponse is one screen of the rail: the spans of the rows asked for,
// and the marks covering their bytes.
type shellRailResponse struct {
	Cols      int             `json:"cols"`
	Top       int             `json:"top"`
	Spans     []*shellRowSpan `json:"spans"`
	Marks     []markSpan      `json:"marks"`
	TotalRows int             `json:"total_rows"`
}

// The model's limits. The scrollback cap is the xterm option the client sets
// (terminal-view.js): xterm drops the oldest lines past it, and because the rail
// indexes rows the way the terminal does, the layout has to drop them too or every
// row number below the trim point would be off by the difference.
const (
	shellRailScrollback = 100000
	shellRailTabStop    = 8
	shellRailMaxRows    = 2048
	shellRailMaxCols    = 1000
	shellRailReadChunk  = 256 * 1024
	// shellRailMaxCSIParams bounds the parameters of a CSI sequence still in
	// flight: the longest real one is a handful of bytes, and a stream that never
	// ends a sequence must not grow the body without limit.
	shellRailMaxCSIParams = 64
)

// handleShellRail answers GET /api/shells/{id}/rail.
//
// `cols` is the terminal's width and is required: it is half the input to the
// layout, and a wrong one puts every cell on the wrong row. `top` and `count` name
// the rows wanted in the numbering the terminal itself uses (viewportY), and
// `height` is its height, which the model needs to know where the top of the
// screen is when a program addresses the cursor.
func (h *Handler) handleShellRail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sessionID, shellID, ok := h.marksTarget(id)
	if !ok {
		http.Error(w, "shell not found: "+id, http.StatusNotFound)
		return
	}
	cols, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("cols")))
	if err != nil || cols < 2 || cols > shellRailMaxCols {
		http.Error(w, "cols must be a terminal width between 2 and "+strconv.Itoa(shellRailMaxCols), http.StatusBadRequest)
		return
	}
	top := queryInt(r, "top", 0, 0, shellRailScrollback)
	count := queryInt(r, "count", 0, 1, shellRailMaxRows)
	height := queryInt(r, "height", count, 1, shellRailMaxRows)
	if height < count {
		height = count
	}

	total, err := h.Sessions.OutputSize(sessionID, shellID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// A row's byte range is settled once the layout has moved past it, so the read
	// stops as soon as the rows wanted are: a rail drawn at the bottom of the log
	// pays for the log, not for the rows beyond the window.
	layout := newRowLayout(cols, height)
	for off := int64(0); off < total && !layout.reached(top+count); {
		chunk, end, err := h.Sessions.OutputByteRange(sessionID, shellID, off, shellRailReadChunk)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if len(chunk) == 0 {
			break
		}
		layout.feed(off, chunk)
		off = end
	}

	spans := layout.window(top, count)
	resp := shellRailResponse{
		Cols:      cols,
		Top:       top,
		Spans:     spans,
		TotalRows: layout.rowCount(),
	}
	// The marks the rail needs are the ones covering the bytes those rows hold, and
	// the server has both the spans and the index in hand: answering with them
	// spares the client the byte arithmetic and a second round trip. The spans
	// come out of the mark index through the same tiling the marks endpoint uses
	// (markSpans), so the two surfaces cannot disagree about what a span is.
	if start, end, ok := spanWindow(spans); ok {
		marks, closer, err := h.Sessions.MarksWindow(sessionID, shellID, start, end)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp.Marks = markSpans(marks, closer, total)
	}
	writeJSON(w, http.StatusOK, resp)
}

func queryInt(r *http.Request, name string, def, min, max int) int {
	n := atoiDefault(r.URL.Query().Get(name), def)
	if n < min || n > max {
		return def
	}
	return n
}

// spanWindow is the byte window a set of row spans holds: the union of the rows'
// ranges, or ok=false when no row in it holds bytes.
func spanWindow(spans []*shellRowSpan) (start, end int64, ok bool) {
	start = -1
	for _, s := range spans {
		if s == nil || s.End <= s.Start {
			continue
		}
		if start < 0 || s.Start < start {
			start = s.Start
		}
		if s.End > end {
			end = s.End
		}
	}
	if start < 0 || end <= start {
		return 0, 0, false
	}
	return start, end, true
}
