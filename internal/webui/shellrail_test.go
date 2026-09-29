package webui

import (
	"strings"
	"testing"
)

// TestShellRailLayoutWrapsAndMarksRows pins the row<->byte mapping the rail is
// drawn from.
//
// The mapping is derived from the log and the terminal width, and every way of
// getting it wrong looks the same on screen: cells a row or two off their text,
// which is invisible in a unit test that only counts cells. So the layout is
// checked on the bytes it assigns, for the shapes a shell actually writes — a
// line that fits, one that wraps, a carriage return that repaints a line, a wide
// character that takes two columns.
func TestShellRailLayoutWrapsAndMarksRows(t *testing.T) {
	// Eight columns, four rows: a width small enough that every rule is visible.
	lay := func(cols int, log string) *rowLayout {
		g := newRowLayout(cols, 4)
		g.feed(0, []byte(log))
		return g
	}
	span := func(g *rowLayout, row int) (int64, int64, bool) {
		spans := g.window(row, 1)
		if spans[0] == nil {
			return 0, 0, false
		}
		return spans[0].Start, spans[0].End, true
	}

	// One short line: the row holds its text and the newline that ends it, and the
	// next row starts straight after — no gap between them.
	g := lay(8, "abc\ndef\n")
	if s, e, ok := span(g, 0); !ok || s != 0 || e != 4 {
		t.Errorf("row 0 of a short line is [%d,%d) ok=%v, want its text and newline [0,4)", s, e, ok)
	}
	if s, e, ok := span(g, 1); !ok || s != 4 || e != 8 {
		t.Errorf("row 1 is [%d,%d) ok=%v, want [4,8)", s, e, ok)
	}
	// The newline belongs to the line it ends, so the row it opens holds no bytes:
	// the shell has printed nothing there yet. A blank line *between* lines of output
	// is the case below, and that one does draw a cell.
	if _, _, ok := span(g, 2); ok {
		t.Error("the row after a trailing newline reports bytes; the newline belongs to the line it ended")
	}

	// A blank line between two others is a row with content of its own — the line
	// break — so the rail colours it by the mark covering it rather than leaving a
	// hole where the user can see a line of text.
	g = lay(8, "a\n\nb\n")
	for row, want := range map[int][2]int64{0: {0, 2}, 1: {2, 3}, 2: {3, 5}} {
		if s, e, ok := span(g, row); !ok || s != want[0] || e != want[1] {
			t.Errorf("row %d of a log with a blank line is [%d,%d) ok=%v, want [%d,%d)",
				row, s, e, ok, want[0], want[1])
		}
	}

	// The rows tile the log: consecutive rows meet exactly and together cover every
	// byte. This is the invariant the rail rests on — a gap is a byte no mark can
	// reach, which is a cell that never appears.
	tiled := "one\ntwo\n\nthree\n"
	g = lay(8, tiled)
	var at int64
	for row := 0; row < int(g.rowCount()); row++ {
		s, e, ok := span(g, row)
		if !ok {
			break // the empty row a trailing newline opens holds no bytes
		}
		if s != at {
			t.Errorf("row %d starts at %d, the row above ended at %d: the rows leave a gap", row, s, at)
		}
		at = e
	}
	if at != int64(len(tiled)) {
		t.Errorf("the rows cover %d bytes, the log is %d", at, len(tiled))
	}

	// A line longer than the screen wraps, and the wrap point is the width — this
	// is the whole reason the layout is recomputed per width rather than stored.
	g = lay(8, "abcdefghij")
	if s, e, ok := span(g, 0); !ok || s != 0 || e != 8 {
		t.Errorf("the first wrapped row is [%d,%d) ok=%v, want [0,8)", s, e, ok)
	}
	if s, e, ok := span(g, 1); !ok || s != 8 || e != 10 {
		t.Errorf("the wrapped remainder is [%d,%d) ok=%v, want [8,10)", s, e, ok)
	}
	// The same bytes at a wider terminal are one row: a stored mapping would still
	// say two, which is the bug this layout exists to avoid.
	g = lay(20, "abcdefghij")
	if s, e, ok := span(g, 0); !ok || s != 0 || e != 10 {
		t.Errorf("at 20 columns the line is [%d,%d) ok=%v, want [0,10)", s, e, ok)
	}
	if _, _, ok := span(g, 1); ok {
		t.Error("a line that fits still wrapped; the width was not applied")
	}

	// A carriage return repaints the row: what follows replaces what was standing on
	// it, so the row holds the version the reader can see and not the union with the
	// one it replaced. The bytes of the replaced version belong to no row — they are
	// not on the screen any more, and a rail that coloured them would put a cell on a
	// line whose text no longer exists.
	bar := "loading 10%\rloading 99%"
	g = lay(20, bar)
	wantBar := int64(strings.Index(bar, "\r") + 1)
	if s, e, ok := span(g, 0); !ok || s != wantBar || e != int64(len(bar)) {
		t.Errorf("a repainted row is [%d,%d) ok=%v, want the version standing on it [%d,%d)",
			s, e, ok, wantBar, len(bar))
	}
	// And the row below holds no bytes: a carriage return does not move down.
	if _, _, ok := span(g, 1); ok {
		t.Error("a carriage return started a new row; it only returns to column one")
	}

	// A wide character takes two columns: five of them fill a ten-column row, and
	// counting them as one would wrap the line two characters late.
	g = lay(10, "你好世界啊x")
	if s, e, ok := span(g, 0); !ok || s != 0 || e != 15 {
		t.Errorf("a row of wide characters is [%d,%d) ok=%v, want [0,15)", s, e, ok)
	}
	if s, e, ok := span(g, 1); !ok || s != 15 || e != 16 {
		t.Errorf("the rune after five wide ones is on row 1: [%d,%d) ok=%v, want [15,16)", s, e, ok)
	}

	// Escape sequences are not text, but they are bytes the terminal consumed on
	// that row, so the row's range covers them: a mark can sit on the sequence that
	// coloured a line and must still colour the line.
	coloured := "\x1b[32mok\x1b[0m\n"
	g = lay(20, coloured)
	if s, e, ok := span(g, 0); !ok || s != 0 || e != int64(len(coloured)) {
		t.Errorf("a coloured line's row is [%d,%d) ok=%v, want every byte on it [0,%d)",
			s, e, ok, len(coloured))
	}

	// A tab advances to the next tab stop; eight columns in, it wraps.
	g = lay(8, "ab\tcd")
	if s, e, ok := span(g, 0); !ok || s != 0 || e != 3 {
		t.Errorf("the row before a tab is [%d,%d) ok=%v, want its text and the tab [0,3)", s, e, ok)
	}
	if s, e, ok := span(g, 1); !ok || s != 3 || e != 5 {
		t.Errorf("the row after a wrapping tab is [%d,%d) ok=%v, want [3,5)", s, e, ok)
	}

	// A clear erases the rows it covers: the text is gone, so the rail has nothing
	// to colour there.
	g = lay(20, "one\ntwo\nthree\n\x1b[2J")
	for row := 0; row < 3; row++ {
		if _, _, ok := span(g, row); ok {
			t.Errorf("row %d still holds bytes after a screen clear", row)
		}
	}

	// A read split mid-sequence and mid-rune must resume exactly: the parser and
	// the rune are carried across the pieces, so the same log laid out in pieces
	// and in one go gives the same rows.
	full := newRowLayout(8, 4)
	full.feed(0, []byte("héllo\x1b[3"))
	part := newRowLayout(8, 4)
	part.feed(0, []byte("héllo"))
	part.feed(6, []byte("\x1b[3"))
	if a, b := full.rowCount(), part.rowCount(); a != b {
		t.Errorf("a log split across reads has %d rows, whole it has %d", b, a)
	}
	for row := 0; row < int(full.rowCount()); row++ {
		s1, e1, ok1 := span(full, row)
		s2, e2, ok2 := span(part, row)
		if ok1 != ok2 || s1 != s2 || e1 != e2 {
			t.Errorf("a split read gives row %d as [%d,%d) ok=%v, whole it is [%d,%d) ok=%v",
				row, s2, e2, ok2, s1, e1, ok1)
		}
	}
}

// TestShellRailSplitReadsAgree checks that the layout does not depend on how the log
// was chopped up when it was read.
//
// The endpoint reads the log in 256 KiB pieces, so a sequence can land on either side
// of a read boundary — and a CSI sequence naming a row and a column is the one that
// matters, because a parser that lost half of it would put the text somewhere else on
// the screen. That is exactly the bug this pins: the parser consumed a partial
// sequence and then rescanned from the start of the next piece, so \x1b[4;17H arriving
// as \x1b[4, then ";1", then "7H" was applied as "7H" and moved the cursor to column 7
// of the current row instead of row 4 column 17 — 13 extra rows from a 140-byte log.
//
// Reading a log whole and reading it in pieces must give the same rows, so the check
// is that every piece size agrees with the reference layout byte for byte.
func TestShellRailSplitReadsAgree(t *testing.T) {
	log := "\x1b[2J\x1b[H\r\n\x1b[4;17Hscoped\x1b[?25h done\r\n" +
		"\x1b[m\x1b[1;1Htop\x1b[3;5Hmid\x1b[32mgreen\x1b[0m tail\r\n"
	ref := newRowLayout(80, 24)
	ref.feed(0, []byte(log))
	for _, size := range []int{1, 2, 3, 5, 7, 11, 64, 4096} {
		g := newRowLayout(80, 24)
		for off := 0; off < len(log); off += size {
			end := off + size
			if end > len(log) {
				end = len(log)
			}
			g.feed(int64(off), []byte(log[off:end]))
		}
		if g.rowCount() != ref.rowCount() {
			t.Fatalf("reading in %d-byte pieces gives %d rows, one read gives %d", size, g.rowCount(), ref.rowCount())
		}
		for row := 0; row < ref.rowCount(); row++ {
			if g.rows[row] != ref.rows[row] {
				t.Errorf("reading in %d-byte pieces puts row %d at [%d,%d), one read puts it at [%d,%d)",
					size, ref.first+row, g.rows[row].Start, g.rows[row].End,
					ref.rows[row].Start, ref.rows[row].End)
			}
		}
	}
}

// TestShellRailWindowAndMarks checks the response's own arithmetic: the rows
// asked for come back at their absolute row numbers, and the marks cover exactly
// the bytes those rows hold.
//
// The window is what makes the endpoint affordable on a long-lived shell — the
// rows on screen are a few hundred bytes of a log that may be megabytes — and an
// off-by-one in the byte window would silently drop the mark that colours the
// first visible row, which reads as a rail that starts one row too late.
func TestShellRailWindowAndMarks(t *testing.T) {
	g := newRowLayout(8, 4)
	g.feed(0, []byte("aaaaaaaa\nbbbbbbbb\ncccccccc\ndddddddd\n"))

	// A window in the middle: absolute rows, and each span the row's own bytes.
	spans := g.window(1, 2)
	if len(spans) != 2 {
		t.Fatalf("asked for 2 rows, got %d", len(spans))
	}
	if spans[0] == nil || spans[0].Start != 9 || spans[0].End != 18 {
		t.Errorf("row 1 is %+v, want its text and newline [9,18)", spans[0])
	}
	if spans[1] == nil || spans[1].Start != 18 || spans[1].End != 27 {
		t.Errorf("row 2 is %+v, want [18,27)", spans[1])
	}

	// The byte window is the union of the rows', and a row with no bytes does not
	// narrow it.
	start, end, ok := spanWindow(spans)
	if !ok || start != 9 || end != 27 {
		t.Errorf("the byte window is [%d,%d) ok=%v, want [9,27)", start, end, ok)
	}
	holed := []*shellRowSpan{nil, {Start: 9, End: 17}, nil}
	if s, e, ok := spanWindow(holed); !ok || s != 9 || e != 17 {
		t.Errorf("a row with no bytes changed the window to [%d,%d) ok=%v", s, e, ok)
	}
	if _, _, ok := spanWindow([]*shellRowSpan{nil, nil}); ok {
		t.Error("a window of empty rows reports a byte range; there is nothing to fetch")
	}

	// A row past the end of the layout comes back null rather than clamped: the
	// viewport can be parked below the last line, and a borrowed span would colour
	// a row with another row's status.
	if spans := g.window(90, 2); spans[0] != nil || spans[1] != nil {
		t.Errorf("rows past the layout report spans: %+v", spans)
	}
}

// TestShellRailParsesCursorAddressing checks the sequences a full-screen program
// uses to place text.
//
// A pager or editor repaints rows it already passed, so the model has to follow
// where the cursor is put rather than where the bytes happen to fall. The rows it
// paints are the ones the rail shows, and a model that ignored cursor addressing
// would pile a whole screenful of redraws onto the last line.
func TestShellRailParsesCursorAddressing(t *testing.T) {
	g := newRowLayout(10, 4)
	// Write three lines, then repaint the middle one by addressing it directly.
	first := "one\ntwo\nthree\n"
	addr := "\x1b[2;1HXX"
	g.feed(0, []byte(first))
	g.feed(int64(len(first)), []byte(addr))
	spans := g.window(1, 1)
	if spans[0] == nil {
		t.Fatal("the addressed row reports no bytes")
	}
	// The row now holds the addressed text, not the line it replaced: addressing a row
	// and writing there is a repaint, so the row is the text standing on it.
	wantStart := int64(len(first) + len("\x1b[2;1H"))
	if spans[0].Start != wantStart || spans[0].End != wantStart+2 {
		t.Errorf("the addressed row is [%d,%d), want the text written there [%d,%d)",
			spans[0].Start, spans[0].End, wantStart, wantStart+2)
	}
	// The rows above and below keep their own bytes.
	if sp := g.window(0, 1)[0]; sp == nil || sp.Start != 0 || sp.End != 4 {
		t.Errorf("the row above the addressed one moved: %+v", sp)
	}

	// Cursor up then write: the bytes land on the row above, which is how a
	// progress line is redrawn without addressing.
	up := "first\nsecond\n\x1b[1A\rREDONE"
	g = newRowLayout(10, 4)
	g.feed(0, []byte(up))
	spans = g.window(1, 1)
	// The bytes land on the row above, replacing what stood there: the row is the
	// text written last, which is what the reader sees on it.
	wantUp := int64(strings.Index(up, "REDONE"))
	if spans[0] == nil || spans[0].Start != wantUp || spans[0].End != int64(len(up)) {
		t.Errorf("a cursor-up write did not land on the row above: %+v, want [%d,%d)",
			spans[0], wantUp, len(up))
	}
}

// TestShellRailRouteIsMounted pins the route and the required width parameter.
//
// The rail's layout depends on the terminal width, and a request without one
// cannot be answered correctly: laying the log out at an assumed width would put
// every cell on the wrong row and look like a rail that simply drifts. The width
// is therefore a parameter, not a default.
func TestShellRailRouteIsMounted(t *testing.T) {
	src := readGoSource(t, "handler.go")
	if !strings.Contains(src, `mux.HandleFunc("GET /api/shells/{id}/rail", h.handleShellRail)`) {
		t.Error("handler.go no longer mounts GET /api/shells/{id}/rail")
	}
	rail := readGoSource(t, "shellrail.go")
	if !strings.Contains(rail, `r.URL.Query().Get("cols")`) {
		t.Error("the rail endpoint no longer takes a terminal width; the layout needs it")
	}
	// The layout must be recomputed from the log, not recorded: no anchor or
	// stored mapping may come back.
	for _, gone := range []string{"railRecordAnchor", "railPushAnchor", "railDropAnchors", "SHELL_RAIL_MAX_ANCHORS"} {
		if strings.Contains(readAssetLF(t, "static/js/timeline.js"), gone) {
			t.Errorf("timeline.js still carries %s; the mapping is derived, not recorded", gone)
		}
	}
	// And the client must ask for the layout at the width the terminal has.
	js := readAssetLF(t, "static/js/timeline.js")
	if !strings.Contains(js, "/rail?cols=") {
		t.Error("the rail no longer asks the server for a layout at the terminal's width")
	}
	if !strings.Contains(js, "onResize") {
		t.Error("a resize no longer re-lays the log out; the rows in hand would name reflowed-away lines")
	}
}

// TestShellRailAltScreenLeavesTheTranscriptAlone checks that a full-screen
// program's screenful is not recorded as transcript rows.
//
// vim, less and the pagers paint the alternate screen: a buffer of their own that
// the terminal throws away on exit, putting back the rows that were on screen
// before. The bytes are all in the log, so a model that records them puts the
// program's whole screenful where the prompt and its history were and pushes every
// row after it down by a screen — the rail sliding out of step with the terminal
// the moment the program quits, which is what a reader sees as cells that no
// longer line up with the text beside them.
//
// The check is on the shape that makes the drift visible: the rows before the
// program and after it must still be neighbours, with nothing of the program
// between them, because that is what the terminal shows once it exits.
func TestShellRailAltScreenLeavesTheTranscriptAlone(t *testing.T) {
	// What vim actually sends: 1049 takes the screen over, the program then clears
	// it and paints every row, and 1049 gives it back.
	vim := "\x1b[?1049h\x1b[?25l\x1b[H\x1b[2J"
	for i := 0; i < 24; i++ {
		vim += "~\r\n"
	}
	vim += "\x1b[24;1H:q!\r\n\x1b[?1049l"
	log := "before\r\n" + vim + "after\r\n"

	g := newRowLayout(80, 24)
	g.feed(0, []byte(log))

	// Two lines of transcript: the one before the program and the one after it, as
	// the terminal shows them once the alternate screen is gone.
	var live []*shellRowSpan
	for r := 0; r < g.rowCount(); r++ {
		if sp := &g.rows[r]; sp.End > sp.Start {
			live = append(live, sp)
		}
	}
	if len(live) != 2 {
		t.Fatalf("the log has %d rows with bytes, want 2: the lines before and after the program", len(live))
	}
	if got := log[live[0].Start:live[0].End]; got != "before\r\n" {
		t.Errorf("the first row holds %q, want the line printed before the program", got)
	}
	if got := log[live[1].Start:live[1].End]; got != "after\r\n" {
		t.Errorf("the row after the program holds %q; it must follow the line before it, not the program's screen", got)
	}
	// And the program's own bytes are owned by no row at all. The sequence that
	// opens the session is the one exception, and it is not special: it arrives on
	// the primary screen and belongs to the row it started on, the way every other
	// escape does. What must never happen is a row reaching *inside* the session,
	// because those are the bytes the terminal throws away on exit.
	iEnter := int64(strings.Index(log, "\x1b[?1049h"))
	iExit := int64(strings.Index(log, "\x1b[?1049l"))
	openEnd := iEnter + int64(len("\x1b[?1049h"))
	for r := 0; r < g.rowCount(); r++ {
		sp := &g.rows[r]
		if sp.End > sp.Start && sp.Start < iExit && sp.End > openEnd {
			t.Errorf("row %d reaches into the full-screen program's screen: [%d,%d)", g.first+r, sp.Start, sp.End)
		}
	}

	// The three ways to take the screen over all end the same way, so all three are
	// read off the parameters of one sequence rather than special-cased per program.
	for _, mode := range []string{"47", "1047", "1049"} {
		prog := "\x1b[?" + mode + "h" + strings.Repeat("x", 200) + "\x1b[?" + mode + "l"
		tail := "one\r\n" + prog + "two\r\n"
		gg := newRowLayout(80, 24)
		gg.feed(0, []byte(tail))
		var got []string
		for r := 0; r < gg.rowCount(); r++ {
			if sp := &gg.rows[r]; sp.End > sp.Start {
				got = append(got, tail[sp.Start:sp.End])
			}
		}
		if len(got) != 2 || got[0] != "one\r\n" || got[1] != "two\r\n" {
			t.Errorf("?%sh/?%sl put the program on the transcript: rows %q", mode, mode, got)
		}
	}

	// A program that fills its screen with line breaks — the shape of a pager, and
	// the one that grows the transcript rather than overwriting it — must leave the
	// cursor where it found it. Guarding only the rows and not the cursor is the
	// subtle half of this bug: the rows stop growing, so the count looks right, but
	// every line printed after the program lands a screenful too low. Here the shell
	// prints one line, the program advances the cursor over its whole screen, and the
	// next shell line must still be row 1.
	steps := "\x1b[?1049h" + strings.Repeat("page\r\n", 40) + "\x1b[?1049l"
	cursorLog := "first\r\n" + steps + "second\r\n"
	cg := newRowLayout(80, 24)
	cg.feed(0, []byte(cursorLog))
	wantSecond := int64(strings.Index(cursorLog, "second\r\n"))
	row := -1
	for r := 0; r < cg.rowCount(); r++ {
		sp := &cg.rows[r]
		if sp.End > sp.Start && sp.Start <= wantSecond && wantSecond < sp.End {
			row = cg.first + r
		}
	}
	if row != 1 {
		t.Errorf("the line after a full-screen program landed on row %d, want row 1: the program's cursor moved the transcript's", row)
	}

	// A full-screen program that never exits is still a program: nothing it paints
	// after taking the screen over is transcript. A shell killed while a pager is up
	// leaves a log ending inside one, and the rows above it must still be the shell's
	// own.
	paint := "\x1b[?1049h" + strings.Repeat("y\r\n", 40)
	open := "start\r\n" + paint
	gg := newRowLayout(80, 24)
	gg.feed(0, []byte(open))
	iOpen := int64(strings.Index(open, "\x1b[?1049h"))
	for r := 0; r < gg.rowCount(); r++ {
		sp := &gg.rows[r]
		if sp.End > sp.Start && sp.End > iOpen+int64(len("\x1b[?1049h")) {
			t.Errorf("row %d records a log that ends inside a full-screen program: [%d,%d)",
				gg.first+r, sp.Start, sp.End)
		}
	}
	if sp := gg.window(0, 1)[0]; sp == nil || open[sp.Start:sp.End] != "start\r\n" {
		t.Errorf("the line printed before the program is not row 0 any more: %+v", sp)
	}

	// Splitting the log anywhere — including through the middle of the mode
	// sequence itself — must not change the outcome: the prefix is read off the
	// body across read boundaries like any other parameter.
	for _, size := range []int{1, 3, 8, 64} {
		pieces := newRowLayout(80, 24)
		for off := 0; off < len(log); off += size {
			end := off + size
			if end > len(log) {
				end = len(log)
			}
			pieces.feed(int64(off), []byte(log[off:end]))
		}
		if pieces.rowCount() != g.rowCount() {
			t.Fatalf("reading in %d-byte pieces gives %d rows, one read gives %d", size, pieces.rowCount(), g.rowCount())
		}
		for r := 0; r < g.rowCount(); r++ {
			if pieces.rows[r] != g.rows[r] {
				t.Errorf("reading in %d-byte pieces puts row %d at [%d,%d), one read puts it at [%d,%d)",
					size, g.first+r, pieces.rows[r].Start, pieces.rows[r].End, g.rows[r].Start, g.rows[r].End)
			}
		}
	}
}

// TestShellRailTilesRowsSoEveryMarkLands checks the property the two rail bugs
// both broke: the rows tile the log, so a mark always lands on one of them.
//
// A mark is a point in the byte log — an input is recorded as a zero-length mark
// at the offset the keystroke ended at, which for a submitted line is the carriage
// return or newline that submitted it. When rows covered only the printable bytes,
// those offsets fell into the gaps between them: the mark reached no row, so no
// row was coloured and the input bar was missing from the strip entirely. The same
// gaps are what made a blank line — a newline on an otherwise empty row — draw no
// cell at all, though a program printing one has visibly printed a line.
//
// The log below is a shell's own output: a banner with blank lines in it, then a
// prompt the shell repaints (the bracketed-paste sequence, colour codes, a
// carriage return and an erase) before echoing a typed command.
func TestShellRailTilesRowsSoEveryMarkLands(t *testing.T) {
	prompt := "\x1b[?2004h\x1b[31mmain\x1b[0m$ "
	// A banner with a blank line, the prompt, a repaint of that prompt, the typed
	// command, and the line ending that submitted it.
	log := "banner line\r\n\r\n" + prompt + "pi\r\x1b[K" + prompt + "ping host\r\nPING host\r\n"
	submit := int64(strings.Index(log, "\r\nPING"))

	marks := []markSpan{
		{Status: "o", Start: 0, End: 0},
		// The input mark sits on the carriage return that submitted the line, which
		// is the offset the record carries: the keystroke ended there.
		{Status: "i", Start: submit, End: submit},
		{Status: "o", Start: submit, End: int64(len(log))},
	}

	g := newRowLayout(80, 24)
	g.feed(0, []byte(log))

	// Every mark lands inside exactly one row's span.
	for _, m := range marks {
		hit := -1
		for row := 0; row < int(g.rowCount()); row++ {
			sp := g.rows[row]
			if sp.End > sp.Start && sp.Start <= m.Start && m.Start < sp.End {
				if hit >= 0 {
					t.Errorf("mark %s at %d is on two rows: %d and %d", m.Status, m.Start, hit, g.first+row)
				}
				hit = g.first + row
			}
		}
		if hit < 0 {
			t.Errorf("mark %s at %d reaches no row: the input bar would be missing from the rail", m.Status, m.Start)
		}
	}

	// The blank line in the banner holds bytes, so the rail colours it. It is the
	// row after the first, and it is the line break that was printed.
	blank := g.rows[1]
	if blank.End <= blank.Start {
		t.Error("a blank output line holds no bytes; it must draw a cell like any other line")
	}
	// And it holds the line break itself, not a neighbour's text.
	if got := log[blank.Start:blank.End]; got != "\r\n" {
		t.Errorf("the blank line's row holds %q, want the line break that printed it", got)
	}

	// From the last erase to the end of the log the rows tile exactly: each starts
	// where the one above ended. The bytes before it are the prompt the shell erased,
	// and an erased line owns nothing — that is what erasing it means.
	last := int64(strings.LastIndex(log, "[K")) + int64(len("[K"))
	var at int64 = -1
	for row := 0; row < int(g.rowCount()); row++ {
		sp := g.rows[row]
		if sp.End <= sp.Start || sp.End <= last {
			continue
		}
		if at >= 0 && sp.Start != at {
			t.Errorf("row %d starts at %d but the row above ended at %d: the live rows leave a gap",
				g.first+row, sp.Start, at)
		}
		at = sp.End
	}
	if at != int64(len(log)) {
		t.Errorf("the live rows end at %d, the log ends at %d", at, len(log))
	}
}
