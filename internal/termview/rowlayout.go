package termview

import (
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
//
// The buffer's retention limits are the one exception, and they are not optional:
// past `scrollback + height` rows the terminal itself drops the oldest and slides
// every remaining row number down, so a model that kept them would be counting in
// a space the terminal has already left. See ensure for where that trim happens.
type Layout struct {
	cols   int
	height int
	// rows is the buffer, indexed by the row number the terminal itself would use:
	// rows[0] is the oldest row it still holds. That makes the index the number in
	// the API — the one `viewportY` is reported in — and it is why the slice is
	// trimmed from the front rather than kept whole with an offset: a trim moves
	// every row number down, so an offset would have to be subtracted from every
	// one of them on the way out, and any path that forgot would name a row the
	// terminal does not have.
	rows []Span
	row  int // the cursor's row
	col  int
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
	alt            bool
	cursorVisible  bool
	bracketedPaste bool
	// altGrid is the screen the full-screen program is painting (#314). The primary
	// buffer keeps its rows and its cursor untouched while a program holds the screen, because
	// that is what the terminal does; this grid is what the program sees instead.
	altGrid *grid
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

func New(cols, height int) *Layout {
	if height < 1 {
		height = 1
	}
	return &Layout{cols: cols, height: height, cursorVisible: true}
}

// rowCount is the number of rows the log has.
func (g *Layout) RowCount() int { return len(g.rows) }

// window returns the spans of rows [top, top+count), with a null for every row
// holding no bytes or lying outside the layout.
func (g *Layout) Window(top, count int) []*Span {
	out := make([]*Span, count)
	for i := 0; i < count; i++ {
		r := top + i
		if r < 0 || r >= len(g.rows) || g.rows[r].End <= g.rows[r].Start {
			continue
		}
		sp := g.rows[r]
		out[i] = &sp
	}
	return out
}

// feed replays one piece of the log; `base` is the log offset of data[0].
func (g *Layout) Feed(base int64, data []byte) {
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
		case g.alt && g.altGrid != nil && (b == '\n' || b == '\r' || b == '\b' || b == '\t' || b < 0x20 || b == 0x7f):
			// Line breaks, returns, tabs and backspaces inside a full-screen program are
			// the program's own cursor movement: they move the grid's cursor and record
			// nothing on the transcript (see setAlt).
			switch b {
			case '\n', '\v', '\f':
				g.altGrid.lf()
			case '\r':
				g.altGrid.cr()
			case '\b':
				g.altGrid.back()
			case '\t':
				g.altGrid.tab(TabStop)
			}
			i++
		case b == '\n' || b == '\v' || b == '\f':
			// The newline belongs to the row it ends, which is what gives a blank
			// line a range of its own: a line break and nothing else is still a
			// lineful of output. Vertical tab and form feed are the same thing to a
			// terminal — xterm wires VT and FF to its line-feed handler — so they are
			// treated as the break they are rather than as one more control byte.
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
			g.col = (g.col/TabStop + 1) * TabStop
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
func (g *Layout) write(off int64, r rune) {
	if g.alt {
		// The program's own screen: its characters go into the grid, and the transcript
		// keeps the rows it had before the screen was taken over.
		if g.altGrid != nil {
			g.altGrid.put(r)
		}
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
		sp := g.SpanAt(g.row)
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
func (g *Layout) touch(row int, start, end int64) {
	if g.alt {
		return
	}
	if row < 0 || end <= start {
		return // its row has aged out of the scrollback
	}
	sp := g.SpanAt(row)
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
func (g *Layout) armRepaint() {
	if g.alt {
		return
	}
	g.pending = g.col == 0
}

// newRow closes the row being written and moves the cursor to the next one.
func (g *Layout) newRow() {
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
func (g *Layout) SpanAt(row int) *Span {
	g.ensure(row)
	if row < 0 {
		row = 0
	}
	return &g.rows[row]
}

// Alt reports whether a full-screen program currently holds the screen (47 / 1047 /
// 1049). What the program painted there is NOT recorded — see setAlt — so this says
// "a TUI is in control", not "here is what it shows".
func (g *Layout) Alt() bool { return g.alt }

// Cursor is where the terminal's cursor stands: the row in the model's numbering and the
// column within it. A pty gives no other way to ask whether a prompt is ready.
func (g *Layout) Cursor() (row, col int) { return g.row, g.col }

// CursorVisible reports whether the terminal cursor is currently visible (?25h vs ?25l).
// A full-screen program turns it off while repainting; a shell turns it on at the prompt.
func (g *Layout) CursorVisible() bool { return g.cursorVisible }

// BracketedPaste reports whether the shell or program has bracketed paste enabled (?2004).
func (g *Layout) BracketedPaste() bool { return g.bracketedPaste }
