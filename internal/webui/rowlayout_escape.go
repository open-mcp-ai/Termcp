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
			if g.row > 0 {
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
		if g.row < 0 {
			g.row = 0
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
		if g.row < 0 {
			g.row = 0
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
		case 2:
			g.erase(top, bottom)
		case 3:
			// Erase saved lines: the scrollback goes, the screen stays, and every row
			// number above it slides down. This is not a repaint of the screen — it is
			// the buffer losing its history — so it renumbers rather than blanks.
			g.trimToScrollback()
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
