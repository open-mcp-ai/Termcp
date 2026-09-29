package webui

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/width"
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

// rowLayout replays a byte log into terminal rows.
//
// The state is the cursor (row, column) and whatever escape sequence is half-parsed
// at the read boundary. Reading the log in pieces is what keeps a long log
// affordable: only the rows are kept, and the parser carries its state across them.
//
// The rows tile the log. Every byte the terminal consumes is recorded on the row
// the cursor was on when it arrived — the text, the line ending that closes the
// row, the escape that colours it — so no byte falls between two rows and a mark
// cannot either. That is what a row's range is for: it says which bytes that row is
// the record of, and the rail colours it by the marks over them. A row the cursor
// comes back to (a repainted prompt, a progress bar) therefore holds the union of
// what was written on it, which is the honest answer for a model that keeps rows
// rather than cells: the bytes that reached that row are exactly the union.
//
// A row number is absolute and only ever grows, the way it does in a terminal's
// own buffer: printing at the foot of the screen moves the cursor to a new row
// and pushes nothing — the scrollback coordinate of every line above is settled
// and stays what it was. Relabelling rows as the screen scrolls (dropping the top
// row and decrementing the cursor) would be a different coordinate space than the
// one the terminal reports `viewportY` in, and it loses rows: the cursor would
// come back down onto a row it had already passed and overwrite the prompt and
// command standing on it.
type rowLayout struct {
	cols   int
	height int
	rows   []shellRowSpan
	first  int // absolute row number of rows[0]
	row    int // the cursor's row
	col    int
	// pending says the row the cursor is on is about to be rewritten, so the next
	// printable rune starts a new version of the row instead of extending the old one.
	// A row therefore holds what is standing on it now: a prompt the shell repainted
	// is the new prompt, not the old one plus the new one, and one mark cannot colour
	// two rows.
	//
	// Only text restarts a version. A control byte — the carriage return of a line
	// ending, the escape that colours a line — is part of the line it is on and just
	// extends the row. That is what keeps a submitted line's input mark, which sits on
	// the very carriage return that ended it, inside the row a reader sees it on.
	pending bool
	// top is the absolute row at the top of the screen. The screen scrolls when the
	// cursor is printed past its last row, and that is a window moving over the
	// buffer — not a row being dropped from it, which only happens at the
	// scrollback cap. Keeping it explicit is what lets cursor addressing count
	// from the screen while row numbers stay absolute.
	top int
	// alt says the alternate screen is up: a full-screen program (vim, an editor, a
	// pager) has taken the terminal over, and its bytes belong to a screen the
	// transcript does not contain. While it is up nothing is recorded on the primary
	// rows and the cursor does not advance over them, because the terminal restores
	// both when the program exits — recording the program's screenful where the
	// prompt and its history were puts every row below it out by a screenful, which
	// is the rail sliding out of step with the terminal the moment a full-screen
	// program quits.
	alt bool
	// altRow, altCol, altTop and altPending are the primary screen's state while the
	// alternate one is up. xterm keeps a cursor per buffer and hands the normal one
	// back as it was, so these are copied back on the way out rather than
	// recomputed: what the program did to the cursor in between is not the
	// transcript's business.
	altRow     int
	altCol     int
	altTop     int
	altPending bool
	// at is the log offset of data[0] in the current read. A byte's absolute offset is
	// at + its index within data, which is how a row's range is recorded.
	at int64
	// Parser state.
	esc int
	// tail is a rune split across two reads and csiBody the parameters of a CSI
	// sequence split across reads. Both are carried rather than re-derived: the
	// sequence `\x1b[4;17H` arriving as three pieces has to move the cursor
	// to row 4 column 17, and a parser that rescanned from the start of each read
	// would see only the last piece and put the text somewhere else.
	tail    []byte
	csiBody []byte
	// escStart is the absolute offset of the escape sequence now being parsed. It
	// is recorded on the row the cursor is on *before* the sequence is applied: a
	// CSI that erases a row must not put its own bytes back on it, and one that
	// moves the cursor belongs to the row it started on.
	escStart int64
}

const (
	escNone = iota
	escEsc
	escCSI
	escOSC
	escOSCEsc
	escSkip
)

func newRowLayout(cols, height int) *rowLayout {
	if height < 1 {
		height = 1
	}
	return &rowLayout{cols: cols, height: height}
}

// reached reports whether the layout has passed row `r`, so the rows above it are
// settled and no later byte can change them.
func (g *rowLayout) reached(r int) bool {
	return g.first+len(g.rows) >= r
}

// rowCount is the number of rows the log has.
func (g *rowLayout) rowCount() int { return len(g.rows) }

// window returns the spans of rows [top, top+count), with a null for every row
// holding no bytes or lying outside the layout.
func (g *rowLayout) window(top, count int) []*shellRowSpan {
	out := make([]*shellRowSpan, count)
	for i := 0; i < count; i++ {
		r := top + i - g.first
		if r < 0 || r >= len(g.rows) || g.rows[r].End <= g.rows[r].Start {
			continue
		}
		sp := g.rows[r]
		out[i] = &sp
	}
	return out
}

// feed replays one piece of the log; `base` is the log offset of data[0].
func (g *rowLayout) feed(base int64, data []byte) {
	g.at = base
	if len(g.tail) > 0 {
		data = append(g.tail, data...)
		g.at = base - int64(len(g.tail))
		g.tail = nil
	}
	for i := 0; i < len(data); {
		if g.esc != escNone {
			// The sequence records its own bytes on the row the cursor is on before
			// it is applied (feedEscape), because its final byte is what moves the
			// cursor: the sequence belongs to the row it started on.
			i += g.feedEscape(g.at+int64(i), data[i:])
			continue
		}
		b := data[i]
		switch {
		case b == 0x1b:
			g.esc, g.escStart, i = escEsc, g.at+int64(i), i+1
		case b == '\n':
			// The newline belongs to the row it ends, which is what gives a blank
			// line a range of its own: a line break and nothing else is still a
			// lineful of output.
			g.touch(g.row, g.at+int64(i), g.at+int64(i)+1)
			g.newRow()
			i++
		case b == '\r':
			g.touch(g.row, g.at+int64(i), g.at+int64(i)+1)
			// A carriage return sends the cursor back to the first column. What follows
			// either repaints this row (a progress bar, a prompt the shell rewrote) or ends
			// it, and the byte after it says which: a newline means the carriage return was
			// the first half of a CRLF line ending, so the line is finished and its text
			// stays on the row. A bare one is a repaint, so the text that follows replaces
			// what is on the row. Treating every carriage return as a repaint would leave
			// every line of a normal shell holding nothing but its own line ending.
			g.col = 0
			if i+1 >= len(data) || data[i+1] != '\n' {
				g.armRepaint()
			}
			i++
		case b == '\b':
			g.touch(g.row, g.at+int64(i), g.at+int64(i)+1)
			if g.col > 0 {
				g.col--
			}
			i++
		case b == '\t':
			g.touch(g.row, g.at+int64(i), g.at+int64(i)+1)
			g.col = (g.col/shellRailTabStop + 1) * shellRailTabStop
			if g.col >= g.cols {
				g.newRow()
			}
			i++
		case b < 0x20 || b == 0x7f:
			g.touch(g.row, g.at+int64(i), g.at+int64(i)+1)
			i++
		default:
			r, size := utf8.DecodeRune(data[i:])
			if r == utf8.RuneError && size <= 1 {
				// A rune split across two reads, or a byte that is not UTF-8 at all.
				// Held back rather than guessed at: the next read decides which it is,
				// and a stray byte costs one column either way.
				if len(data)-i < utf8.UTFMax {
					g.tail = append([]byte(nil), data[i:]...)
					return
				}
				r, size = rune(b), 1
			}
			g.write(int64(i), r)
			i += size
		}
	}
}

// write places one printable rune, wrapping at the right edge. `off` is its byte
// offset within the current read.
func (g *rowLayout) write(off int64, r rune) {
	if g.alt {
		return
	}
	w := 1
	if kind := width.LookupRune(r).Kind(); kind == width.EastAsianWide || kind == width.EastAsianFullwidth {
		// A wide character takes two columns. Counting it as one would slide every
		// wrap after the first Chinese character by a column, which is the drift
		// that puts cells on the wrong rows.
		w = 2
	}
	if g.col+w > g.cols {
		g.newRow()
	}
	from := g.at + off
	if g.pending {
		// Text after a carriage return or a cursor move replaces what was on the row,
		// so the row's range starts here. The bytes it replaced belong to no row:
		// they are not on screen any more, which is what the rail must not colour.
		g.pending = false
		sp := g.span(g.row)
		sp.Start, sp.End = from, from
	}
	g.touch(g.row, from, from+int64(utf8.RuneLen(r)))
	g.col += w
}

// touch extends row `row`'s range over the absolute bytes [start, end).
//
// Every byte the terminal consumes belongs to the row the cursor is on when it
// arrives: the text, the line ending that closes the row, the carriage return that
// repaints it, the escape sequence that only colours it. That is what keeps the
// rows tiling the log with no gaps for a mark to fall into, and it is why a blank
// line — a newline on an otherwise empty row — is a row with content rather than a
// hole.
//
// Control bytes are included deliberately. A carriage return writes nothing a
// reader can see, but it is a byte the terminal consumed on that row, and an input
// mark is recorded at exactly such an offset: the keystroke ended at the carriage
// return that submitted the line. Leaving control bytes out would put the mark in
// a gap between two rows and the input bar would be missing from the strip.
func (g *rowLayout) touch(row int, start, end int64) {
	if g.alt {
		return
	}
	if row < g.first || end <= start {
		return // its row has aged out of the scrollback
	}
	sp := g.span(row)
	if sp.End <= sp.Start {
		sp.Start = start
	}
	if end > sp.End {
		sp.End = end
	}
}

// armRepaint marks the row the cursor is on to be replaced by whatever text comes
// next — but only when the cursor is at column one. Text written at a later column
// covers just those columns, so the text to its left is still on the row: that is
// how a prompt survives the inline prediction a shell writes over the tail of the
// line, and resetting the whole row there would throw the prompt away.
func (g *rowLayout) armRepaint() {
	if g.alt {
		return
	}
	g.pending = g.col == 0
}

// newRow closes the row being written and moves the cursor to the next one.
func (g *rowLayout) newRow() {
	if g.alt {
		// A line break inside a full-screen program advances its cursor, not the
		// transcript's: the program has a screen of its own, and the cursor the
		// terminal hands back on exit is the one saved on the way in.
		return
	}
	g.row++
	g.col = 0
	g.pending = true
	g.ensure(g.row)
	// Printing past the last row of the screen scrolls it down by as much as the
	// cursor overshot. The rows above keep their numbers: the buffer is not trimmed,
	// the window over it moved, exactly as in the terminal the log came from.
	if over := g.row - (g.top + g.height - 1); over > 0 {
		g.top += over
	}
}

// span returns one row's span, growing the layout so the row exists.
func (g *rowLayout) span(row int) *shellRowSpan {
	g.ensure(row)
	return &g.rows[row-g.first]
}

// ensure grows the layout up to and including `row`, filling the rows in between
// with empty spans: a cursor can address a row far below the text (a clear, a
// cursor move), and those rows hold no bytes.
func (g *rowLayout) ensure(row int) {
	if g.alt {
		return
	}
	if row < g.first {
		return
	}
	for g.first+len(g.rows) <= row {
		g.rows = append(g.rows, shellRowSpan{})
	}
	// Past the scrollback cap the oldest rows fall off, and every row number slides
	// down with them, exactly as they do in the terminal’s own trimmed buffer. This is
	// the only thing that renumbers rows: the screen scrolling is a window moving over
	// the buffer, not a row leaving it. The cursor and the screen top keep pointing at
	// the same text, so they move by the same amount.
	if over := len(g.rows) - shellRailScrollback; over > 0 {
		g.rows = append(g.rows[:0], g.rows[over:]...)
		g.first += over
		g.top -= over
		g.row -= over
	}
}

// screenTop is the row a cursor-addressing sequence counts from: the top of the
// screen, which is the window the cursor has scrolled into view.
func (g *rowLayout) screenTop() int {
	if g.top < g.first {
		return g.first
	}
	return g.top
}

// erase drops the bytes of rows [from, to): their text is gone, so the rail has
// nothing to colour there. It is the one place a row is left holding no bytes
// while the bytes it held are still in the log, which is exactly what erasing a
// line means.
func (g *rowLayout) erase(from, to int) {
	if g.alt {
		return
	}
	if from < g.first {
		from = g.first
	}
	if to > g.first+len(g.rows) {
		to = g.first + len(g.rows)
	}
	for r := from; r < to; r++ {
		g.ensure(r)
		g.rows[r-g.first] = shellRowSpan{}
	}
	if g.row >= from && g.row < to {
		g.pending = true
	}
}

// eraseLine applies an erase-in-line sequence (CSI K): the row loses the bytes
// from the cursor's column onward, which is what a shell uses to clear the line it
// is about to repaint.
//
// The distinction from erase matters: a shell clearing its prompt row with
// \x1b[K before printing a shorter line has not erased the row's history, and the
// bytes still standing at the left of it are the row's content. Dropping the whole
// row would lose the prompt of every command whose line was repainted, which is
// most of them.
func (g *rowLayout) eraseLine(mode int) {
	if g.alt {
		return
	}
	sp := g.span(g.row)
	if sp.End <= sp.Start {
		return
	}
	if mode == 1 {
		// Erase to the left of the cursor: the bytes before it go, the rest stays.
		return
	}
	if mode == 2 {
		g.rows[g.row-g.first] = shellRowSpan{}
		g.pending = true
		return
	}
	// Erase to the right of the cursor. The row keeps what it holds up to the
	// cursor; when the cursor is at the start of the row that is the whole row.
	if g.col <= 0 {
		g.rows[g.row-g.first] = shellRowSpan{}
		g.pending = true
	}
}

// feedEscape consumes one escape sequence from the front of data and returns how
// many bytes it used. The sequences that move the cursor are applied; the ones
// that only paint are read and dropped, because the rail asks where the text is,
// not what colour it is.
func (g *rowLayout) feedEscape(base int64, data []byte) int {
	switch g.esc {
	case escEsc:
		g.esc = escNone
		// The introducer's own bytes belong to the row the cursor is on now; the
		// letter can move it, so the row is recorded first.
		g.touch(g.row, g.escStart, base+1)
		switch data[0] {
		case '[':
			g.esc = escCSI
			g.csiBody = g.csiBody[:0]
		case ']':
			g.esc = escOSC
		case '(', ')', '*', '+', '#':
			g.esc = escSkip
		case '7', '8':
			// Save/restore cursor. Restoring a cursor from a previous screenful is a
			// repaint of rows the model would have to have kept; the rows it already
			// has are what a transcript reads as.
		case 'D': // index: down one row, same column
			g.newRow()
		case 'E': // next line
			g.newRow()
		case 'M': // reverse index: up one row, same column
			if g.alt {
				// The program's cursor, not the transcript's.
				return 1
			}
			if g.row > g.first {
				g.row--
			}
			g.pending = true
		}
		return 1
	case escCSI:
		// The introducer is already consumed, so the sequence runs from data[0];
		// 0x40..0x7e ends it. The parameters accumulate across reads, so a sequence
		// split down the middle still names the same row and column.
		for i := 0; i < len(data); i++ {
			if data[i] >= 0x40 && data[i] <= 0x7e {
				body := append(g.csiBody, data[:i]...)
				g.touch(g.row, g.escStart, base+int64(i)+1)
				g.applyCSI(body, data[i])
				g.esc, g.csiBody = escNone, g.csiBody[:0]
				return i + 1
			}
		}
		// No final byte yet: hold the parameters for the next read, bounded so a
		// stream that never ends a sequence cannot grow the body without limit.
		if len(g.csiBody) < shellRailMaxCSIParams {
			g.csiBody = append(g.csiBody, data...)
		}
		// What has arrived is recorded now, so a read that splits mid-sequence leaves
		// no unowned bytes behind.
		g.touch(g.row, g.escStart, base+int64(len(data)))
		return len(data)
	case escOSC:
		// A title, terminated by BEL or by ST (ESC \).
		for i := 0; i < len(data); i++ {
			if data[i] == 0x07 {
				g.touch(g.row, g.escStart, base+int64(i)+1)
				g.esc = escNone
				return i + 1
			}
			if data[i] == 0x1b {
				g.touch(g.row, g.escStart, base+int64(i)+1)
				g.esc = escOSCEsc
				return i + 1
			}
		}
		g.touch(g.row, g.escStart, base+int64(len(data)))
		return len(data)
	case escOSCEsc:
		g.touch(g.row, g.escStart, base+1)
		g.esc = escNone
		return 1
	case escSkip:
		g.touch(g.row, g.escStart, base+1)
		g.esc = escNone
		return 1
	}
	g.esc = escNone
	return 1
}

// applyCSI applies one CSI sequence: `body` is its parameters (without the
// introducer or the final byte) and `final` the byte that ended it. Missing and
// zero parameters default as the sequences themselves define, which is usually 1.
func (g *rowLayout) applyCSI(body []byte, final byte) {
	if g.alt && !(len(body) > 0 && body[0] == '?') {
		// A full-screen program addressing its own screen: the rows and the cursor it
		// moves are its own, and the transcript's are the ones saved on the way in. Its
		// private modes are still read, because that is where the sequence giving the
		// screen back is.
		return
	}
	if len(body) > 0 && (body[0] == '?' || body[0] == '>' || body[0] == '=') {
		// Private modes (cursor visibility, mouse tracking, bracketed paste) change
		// nothing about where text is, with the one exception of the alternate
		// screen, which changes everything about it. The prefix is read off the body,
		// so it survives a read boundary like any other parameter.
		if body[0] == '?' {
			g.applyPrivateMode(body[1:], final)
		}
		return
	}
	params := parseCSIParams(body)
	arg := func(i, def int) int {
		if i < len(params) && params[i] > 0 {
			return params[i]
		}
		return def
	}
	switch final {
	case 'A':
		g.row -= arg(0, 1)
		if g.row < g.first {
			g.row = g.first
		}
		g.armRepaint()
	case 'B':
		g.row += arg(0, 1)
		g.armRepaint()
		g.ensure(g.row)
	case 'C':
		g.col += arg(0, 1)
		if g.col > g.cols {
			g.col = g.cols
		}
	case 'D':
		g.col -= arg(0, 1)
		if g.col < 0 {
			g.col = 0
		}
	case 'E':
		g.row += arg(0, 1)
		g.col = 0
		g.armRepaint()
		g.ensure(g.row)
	case 'F':
		g.row -= arg(0, 1)
		if g.row < g.first {
			g.row = g.first
		}
		g.col = 0
		g.armRepaint()
	case 'G':
		g.col = arg(0, 1) - 1
	case 'H', 'f':
		// Addressing is relative to the screen, so a row below it scrolls the content
		// up first: a full-screen program that prints a line at the foot and then
		// addresses the top of the screen relies on that scroll having happened.
		want := arg(0, 1) - 1
		if want >= g.height {
			// Addressing below the screen scrolls it: the row asked for has to be on the
			// screen being addressed.
			g.top += want - g.height + 1
		}
		g.row = g.screenTop() + want
		g.col = arg(1, 1) - 1
		g.armRepaint()
		g.ensure(g.row)
	case 'd':
		g.row = g.screenTop() + arg(0, 1) - 1
		g.armRepaint()
		g.ensure(g.row)
	case 'J':
		top, bottom := g.screenTop(), g.screenTop()+g.height
		// A screen clear reaches rows the layout has not grown to yet: the rows the
		// terminal blanks are the ones on screen, whether or not text ever landed on
		// them.
		g.ensure(bottom - 1)
		switch arg(0, 0) {
		case 1:
			g.erase(top, g.row+1)
		case 2, 3:
			g.erase(top, bottom)
		default:
			g.erase(g.row, bottom)
		}
	case 'K':
		g.eraseLine(arg(0, 0))
	}
}

// applyPrivateMode reads the private modes this model has to act on, which is the
// alternate screen and nothing else. Cursor visibility, mouse tracking and
// bracketed paste say how a terminal behaves, not which row text lands on.
func (g *rowLayout) applyPrivateMode(body []byte, final byte) {
	if final != 'h' && final != 'l' {
		return
	}
	for _, p := range parseCSIParams(body) {
		switch p {
		case 47, 1047, 1049:
			// The three ways to take the screen over, oldest first: 47 hands the
			// alternate screen over as it is, 1047 clears it on the way out, and 1049 —
			// the one vim, less and the pagers send — also saves the cursor. All three
			// end with the normal screen back on display, which is what this model has
			// to reproduce.
			g.setAlt(final == 'h')
		}
	}
}

// setAlt enters and leaves the alternate screen.
//
// The bytes of a full-screen program are not the transcript's: they are painted on
// a screen of its own and thrown away when it exits, while the terminal puts the
// rows that were on screen before it back. The primary state is saved on the way in
// and copied back on the way out, because that is what the terminal does — the
// normal buffer keeps its cursor, its screen top and its rows throughout, and a
// model that recomputed them would be guessing at what the program did in between.
func (g *rowLayout) setAlt(on bool) {
	if on == g.alt {
		return
	}
	g.alt = on
	if on {
		g.altRow, g.altCol, g.altTop, g.altPending = g.row, g.col, g.top, g.pending
		return
	}
	g.row, g.col, g.top, g.pending = g.altRow, g.altCol, g.altTop, g.altPending
}

// parseCSIParams reads the numeric parameters of a CSI sequence. A private prefix
// or an intermediate byte means the sequence is not one this model applies, and
// comes back empty.
func parseCSIParams(body []byte) []int {
	var out []int
	cur, has := 0, false
	for _, c := range body {
		switch {
		case c >= '0' && c <= '9':
			cur = cur*10 + int(c-'0')
			has = true
		case c == ';':
			out = append(out, cur)
			cur, has = 0, false
		default:
			return nil
		}
	}
	if has {
		out = append(out, cur)
	}
	return out
}
