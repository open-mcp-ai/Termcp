package termview

// The alternate screen gets a cell grid of its own (#314).
//
// THE TRANSCRIPT MODEL AND A FULL-SCREEN PROGRAM WANT DIFFERENT THINGS. The rail needs
// "which byte of the log is on which row", and it deliberately throws the alternate
// screen away: vim paints, exits, and the terminal puts back the rows that were there
// before, so nothing of the program belongs to the transcript. A caller asking `read
// --screen` while vim is up wants the opposite — what the program is painting NOW.
//
// So there are two buffers, exactly as a terminal has. The primary one is the row spans
// the rail indexes; the alternate one is this grid: characters at (row, col), no byte
// ranges, no scrollback, sized to the screen. `setAlt` switches which one Text answers
// from, and the primary state is untouched while a program holds the screen.
//
// WHAT IT MODELS: where characters land — cursor addressing, line feed at the bottom
// margin, scrolling within the margins DECSTBM sets, and the erase operations a redraw
// uses — plus how many columns a character occupies, because a Chinese row that counts
// its characters instead of its columns puts every cursor address after it one cell off.
// WHAT IT DOES NOT: colours, attributes, character sets. Those decide how a cell looks,
// not which cell holds what, and a caller reading a screen needs the second.

// grid is one fixed-size screen of characters.
type grid struct {
	rows, cols  int
	cells       [][]rune
	row, col    int
	top, bottom int // scroll margins, 0-based and inclusive
}

func newGrid(rows, cols int) *grid {
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	g := &grid{rows: rows, cols: cols, bottom: rows - 1}
	g.cells = make([][]rune, rows)
	for i := range g.cells {
		g.cells[i] = blankRow(cols)
	}
	return g
}

func blankRow(cols int) []rune {
	out := make([]rune, cols)
	for i := range out {
		out[i] = ' '
	}
	return out
}

func (g *grid) clampRow(r int) int {
	if r < 0 {
		return 0
	}
	if r >= g.rows {
		return g.rows - 1
	}
	return r
}

func (g *grid) clampCol(c int) int {
	if c < 0 {
		return 0
	}
	if c >= g.cols {
		return g.cols - 1
	}
	return c
}

// put writes one character at the cursor and advances by the columns it occupies. A wide
// character marks its second cell as occupied-but-empty, so a later cursor address into
// it lands where the terminal would.
func (g *grid) put(r rune) {
	w := runeWidth(r)
	if g.col+w > g.cols {
		g.col = 0
		g.lf()
	}
	g.cells[g.row][g.col] = r
	if w == 2 && g.col+1 < g.cols {
		g.cells[g.row][g.col+1] = wideTail
	}
	g.col += w
}

func (g *grid) cr() { g.col = 0 }

func (g *grid) back() {
	if g.col > 0 {
		g.col--
	}
}

func (g *grid) tab(stop int) {
	if stop <= 0 {
		stop = 8
	}
	g.col = min((g.col/stop+1)*stop, g.cols-1)
}

// lf moves down one row, scrolling the margin area when it is already at the bottom.
func (g *grid) lf() {
	if g.row == g.bottom {
		g.scrollUp(1)
		return
	}
	g.row = g.clampRow(g.row + 1)
}

// scrollUp drops `n` rows out of the top of the margin area.
func (g *grid) scrollUp(n int) {
	if n <= 0 {
		return
	}
	top, bottom := g.top, g.bottom
	if top < 0 {
		top = 0
	}
	if bottom >= g.rows {
		bottom = g.rows - 1
	}
	n = min(n, bottom-top+1)
	copy(g.cells[top:bottom-n+1], g.cells[top+n:bottom+1])
	for i := bottom - n + 1; i <= bottom; i++ {
		g.cells[i] = blankRow(g.cols)
	}
}

// scrollDown inserts `n` rows at the top of the margin area.
func (g *grid) scrollDown(n int) {
	top, bottom := g.top, g.bottom
	if top < 0 {
		top = 0
	}
	if bottom >= g.rows {
		bottom = g.rows - 1
	}
	n = min(n, bottom-top+1)
	copy(g.cells[top+n:bottom+1], g.cells[top:bottom-n+1])
	for i := top; i < top+n; i++ {
		g.cells[i] = blankRow(g.cols)
	}
}

func (g *grid) cup(row, col int) {
	g.row = g.clampRow(row)
	g.col = g.clampCol(col)
}

func (g *grid) eraseLine(mode int) {
	switch mode {
	case 1: // to the left, cursor included
		for c := 0; c <= g.col && c < g.cols; c++ {
			g.cells[g.row][c] = ' '
		}
	case 2: // the whole line
		g.cells[g.row] = blankRow(g.cols)
	default: // to the right
		for c := g.col; c < g.cols; c++ {
			g.cells[g.row][c] = ' '
		}
	}
}

func (g *grid) eraseDisplay(mode int) {
	switch mode {
	case 1:
		for r := 0; r < g.row; r++ {
			g.cells[r] = blankRow(g.cols)
		}
		g.eraseLine(1)
	case 2, 3:
		for r := range g.cells {
			g.cells[r] = blankRow(g.cols)
		}
	default:
		g.eraseLine(0)
		for r := g.row + 1; r < g.rows; r++ {
			g.cells[r] = blankRow(g.cols)
		}
	}
}

// setMargins applies DECSTBM. A degenerate or inverted pair resets to the whole screen,
// which is what a terminal does with it.
func (g *grid) setMargins(top, bottom int) {
	if top < 0 || bottom <= top || bottom >= g.rows {
		g.top, g.bottom = 0, g.rows-1
		return
	}
	g.top, g.bottom = top, bottom
	g.row = g.clampRow(g.row)
}

// applyCSI runs the sequences that decide which cell holds what. Everything else — SGR,
// character sets, mouse modes — describes how a cell looks, which this model does not
// claim to know.
func (g *grid) applyCSI(params []int, final byte) {
	arg := func(i, def int) int {
		if i < len(params) && params[i] > 0 {
			return params[i]
		}
		return def
	}
	switch final {
	case 'A':
		g.row = g.clampRow(g.row - arg(0, 1))
	case 'B':
		g.row = g.clampRow(g.row + arg(0, 1))
	case 'C':
		g.col = g.clampCol(g.col + arg(0, 1))
	case 'D':
		g.col = g.clampCol(g.col - arg(0, 1))
	case 'E':
		g.row = g.clampRow(g.row + arg(0, 1))
		g.col = 0
	case 'F':
		g.row = g.clampRow(g.row - arg(0, 1))
		g.col = 0
	case 'G':
		g.col = g.clampCol(arg(0, 1) - 1)
	case 'H', 'f':
		// Relative to the margin area, which is how a program inside DECSTBM addresses.
		g.cup(g.top+arg(0, 1)-1, arg(1, 1)-1)
	case 'd':
		g.cup(g.top+arg(0, 1)-1, g.col)
	case 'J':
		g.eraseDisplay(arg(0, 0))
	case 'K':
		g.eraseLine(arg(0, 0))
	case 'L':
		g.enterLines(arg(0, 1))
	case 'M':
		g.deleteLines(arg(0, 1))
	case 'P':
		g.deleteChars(arg(0, 1))
	case '@':
		g.insertChars(arg(0, 1))
	case 'r':
		// DECSTBM: the scrolling region, 1-based and inclusive.
		g.setMargins(arg(0, 1)-1, arg(1, g.rows)-1)
	case 'S':
		g.scrollUp(arg(0, 1))
	case 'T':
		g.scrollDown(arg(0, 1))
	}
}

func (g *grid) enterLines(n int) {
	if g.row < g.top || g.row > g.bottom {
		return
	}
	rows := g.bottom - g.row + 1
	n = min(n, rows)
	copy(g.cells[g.row+n:g.bottom+1], g.cells[g.row:g.bottom-n+1])
	for i := g.row; i < g.row+n; i++ {
		g.cells[i] = blankRow(g.cols)
	}
}

func (g *grid) deleteLines(n int) {
	if g.row < g.top || g.row > g.bottom {
		return
	}
	rows := g.bottom - g.row + 1
	n = min(n, rows)
	copy(g.cells[g.row:g.bottom-n+1], g.cells[g.row+n:g.bottom+1])
	for i := g.bottom - n + 1; i <= g.bottom; i++ {
		g.cells[i] = blankRow(g.cols)
	}
}

func (g *grid) deleteChars(n int) {
	n = min(n, g.cols-g.col)
	line := g.cells[g.row]
	copy(line[g.col:], line[g.col+n:])
	for i := g.cols - n; i < g.cols; i++ {
		line[i] = ' '
	}
}

func (g *grid) insertChars(n int) {
	n = min(n, g.cols-g.col)
	line := g.cells[g.row]
	copy(line[g.col+n:], line[g.col:])
	for i := g.col; i < g.col+n; i++ {
		line[i] = ' '
	}
}

// lines renders the grid the way a screen reads: the second cell of a wide character
// emits nothing (the character before it already occupies both columns), and trailing
// blanks are trimmed, because an 80-column row of spaces is a fact about the terminal,
// not about what the program said.
func (g *grid) lines() []string {
	out := make([]string, 0, g.rows)
	for _, row := range g.cells {
		var b []rune
		for _, r := range row {
			if r == wideTail {
				continue
			}
			b = append(b, r)
		}
		end := len(b)
		for end > 0 && b[end-1] == ' ' {
			end--
		}
		out = append(out, string(b[:end]))
	}
	return out
}

// AltLines is what a full-screen program is showing right now, or nil when no program
// holds the screen.
//
// THIS IS THE ONE THING THE TRANSCRIPT CANNOT ANSWER. The rail deliberately records
// nothing of the alternate screen, so `read --screen` while vim is up had no honest
// answer before the grid: it could say "a program is in control" and nothing else. These
// are the rows the program painted, trailing blanks trimmed, in screen order.
func (g *Layout) AltLines() []string {
	if !g.alt || g.altGrid == nil {
		return nil
	}
	return g.altGrid.lines()
}
