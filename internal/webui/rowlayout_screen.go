package webui

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

// ensure grows the layout up to and including `row`, filling the rows in between
// with empty spans: a cursor can address a row far below the text (a clear, a
// cursor move), and those rows hold no bytes.
func (g *rowLayout) ensure(row int) {
	if g.alt {
		return
	}
	if row < 0 {
		return
	}
	for len(g.rows) <= row {
		g.rows = append(g.rows, shellRowSpan{})
	}
	// Past the retention limit the oldest rows fall off and every row number slides
	// down with them, exactly as they do in the terminal’s own trimmed buffer. This
	// is the only thing that renumbers rows: the screen scrolling is a window moving
	// over the buffer, not a row leaving it. The cursor and the screen top keep
	// pointing at the same text, so they move by the same amount.
	//
	// The limit is `scrollback + height`, not `scrollback`: xterm sizes its line
	// buffer to hold a screenful on top of the scrollback it was configured with
	// (`getCorrectBufferLength`), so trimming one screenful early would put every
	// row number off by the screen height for the rest of the session — a rail that
	// is right until the assumption wears off, then wrong by a fixed amount, which is
	// the hardest kind of misalignment to see.
	if over := len(g.rows) - g.capacity(); over > 0 {
		g.trimFront(over)
	}
}

// capacity is how many rows the buffer holds before the oldest falls off.
func (g *rowLayout) capacity() int {
	if g.height >= shellRailScrollback {
		return g.height
	}
	return shellRailScrollback + g.height
}

// trimToScrollback applies an erase-saved-lines sequence (CSI 3 J): the terminal
// throws away everything in the scrollback and keeps the screen, and every row
// number above it slides down by what was dropped.
//
// This is the sequence `clear` actually sends, and its effect on the numbering is
// total: xterm trims to exactly one screenful and renumbers from there, so a model
// that ignored it would keep describing a buffer the terminal no longer has —
// measured on one real session, 122 modelled rows against xterm's 25, which puts
// every cell about four screens away from the text it is supposed to be beside.
//
// The trim only reaches the scrollback: rows on the screen stay, and so do the
// cursor and the screen top, which are already inside the last `height` rows.
func (g *rowLayout) trimToScrollback() {
	if g.alt {
		// A full-screen program erasing its own screen's saved lines changes nothing
		// about the transcript's buffer; the rows it is painting are not recorded and
		// the primary screen keeps its own. saveState/restoreState carry the top back.
		return
	}
	drop := len(g.rows) - g.height
	if drop <= 0 {
		return
	}
	g.trimFront(drop)
}

// trimFront drops `drop` rows off the top of the buffer, which is what the
// terminal does to both of its ends: the row numbers above the dropped ones slide
// down, so the cursor and the screen top move with them and the rows left behind
// keep pointing at the same text.
//
// Reslicing is intentional. Copying the survivors with
// `append(rows[:0], rows[drop:]...)` runs once per row at the retention limit, so
// a buffer at its 100k-row capacity memmoves 1.6 MB for every line that scrolls
// off: a 20 MB log measured 7.2s of pure copying, during which the rail simply
// does not answer and the cells sit where the previous response left them. A
// reslice makes the trim constant work. Appending new rows eventually exhausts
// the remaining capacity and Go allocates a fresh backing array, copying the live
// rows once; that is amortized over the rows which filled that capacity, rather
// than paid on every trim. The live slice remains `rows[0:]`, so all row-indexed
// accesses continue to use the terminal's numbering directly.
func (g *rowLayout) trimFront(drop int) {
	if drop <= 0 {
		return
	}
	if drop > len(g.rows) {
		drop = len(g.rows)
	}
	g.rows = g.rows[drop:]
	g.row -= drop
	if g.row < 0 {
		g.row = 0
	}
	g.top -= drop
	if g.top < 0 {
		g.top = 0
	}
}

// screenTop is the row a cursor-addressing sequence counts from: the top of the
// screen, which is the window the cursor has scrolled into view.
func (g *rowLayout) screenTop() int {
	if g.top < 0 {
		return 0
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
	if from < 0 {
		from = 0
	}
	if to > len(g.rows) {
		to = len(g.rows)
	}
	for r := from; r < to; r++ {
		g.ensure(r)
		g.rows[r] = shellRowSpan{}
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
		g.ensure(g.row)
		g.rows[g.row] = shellRowSpan{}
		g.pending = true
		return
	}
	// Erase to the right of the cursor. The row keeps what it holds up to the
	// cursor; when the cursor is at the start of the row that is the whole row.
	if g.col <= 0 {
		g.ensure(g.row)
		g.rows[g.row] = shellRowSpan{}
		g.pending = true
	}
}
