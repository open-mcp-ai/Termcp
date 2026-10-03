package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// railBody is the rail endpoint's response as a client sees it.
type railBody struct {
	Cols      int             `json:"cols"`
	Top       int             `json:"top"`
	Spans     []*shellRowSpan `json:"spans"`
	TotalRows int             `json:"total_rows"`
}

// serveRail writes `log` into a store, restores it, and answers one rail request
// against the real handler. The endpoint is the only thing that decides how much
// of the log to replay and which `height` to hand the model, so a test that calls
// newRowLayout directly cannot see those choices at all — it would pass with the
// endpoint replaying the wrong bytes or rewriting the screen height.
func serveRail(t *testing.T, log []byte, query string) railBody {
	t.Helper()
	dir := t.TempDir()
	store := storage.New(dir)
	const sessID, shellID = "s-rail", "sh-rail"
	if _, err := store.AppendLog(sessID, shellID, log); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendMark(sessID, shellID, api.LogMark{Status: api.LogOutput, Time: 1758499205123, Offset: 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSession(api.Session{ID: sessID, Name: "rail", Status: api.SessionExited}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveShell(sessID, api.Session{ID: shellID, Name: "shell-1", Status: api.SessionExited}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store2 := storage.New(dir)
	sessMgr := session.NewManager(message.NewManager(store2), store2, nil)
	if err := sessMgr.RestoreDead(); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Sessions: sessMgr}
	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/shells/"+shellID+"/rail?"+query, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET rail?%s = %d (%s), want 200", query, rr.Code, rr.Body.String())
	}
	var body railBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode rail body: %v (%s)", err, rr.Body.String())
	}
	return body
}

// railLog builds `lines` numbered lines followed by a clear and one line after it.
func railLog(lines int) []byte {
	var log []byte
	for i := 0; i < lines; i++ {
		log = append(log, []byte(fmt.Sprintf("L%03d\r\n", i))...)
	}
	log = append(log, []byte("\x1b[3J\x1b[H\x1b[2J")...)
	log = append(log, []byte("after\r\n")...)
	return log
}

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
					size, row, g.rows[row].Start, g.rows[row].End,
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

// TestShellRailReplaysPastTheWindow checks that the endpoint lays out the whole log
// before it answers, not a prefix of it.
//
// Two independent things are pinned here, both of which made the rail describe rows
// that no longer exist:
//
//   - The read must advance by the bytes it received. `OutputByteRange` reports its
//     second value as the log's *total* size, and assigning that to the offset moved
//     the cursor to the end of the log after the first chunk, so every log larger
//     than shellRailReadChunk was laid out from its first 256 KiB alone.
//   - The replay must run to the end. Rows are not settled when the cursor leaves
//     them: `CSI 3 J` (what `clear` sends) drops the scrollback and renumbers every
//     surviving row, so a window answered before it names rows the clear erased.
//
// The log is deliberately larger than one read chunk, because a single-chunk log
// cannot tell the two apart.
func TestShellRailReplaysPastTheWindow(t *testing.T) {
	log := railLog(40000) // comfortably more than one shellRailReadChunk
	if len(log) <= shellRailReadChunk {
		t.Fatalf("the log is %d bytes, not enough to need more than one read chunk", len(log))
	}

	// After the clear the buffer holds one screenful: row 0 is the line printed after
	// it, and rows 1.. are null. A replay that stopped early would instead answer the
	// numbered lines the clear erased, and one that never left the first chunk would
	// answer them too — the clear is past the first chunk.
	body := serveRail(t, log, "cols=20&top=0&count=25&height=4")
	if body.TotalRows != 4 {
		t.Errorf("the layout holds %d rows, want the clear's one screenful (the whole log was not replayed)", body.TotalRows)
	}
	if len(body.Spans) == 0 || body.Spans[0] == nil {
		t.Fatalf("row 0 is not answered: %+v", body.Spans)
	}
	if got := log[body.Spans[0].Start:body.Spans[0].End]; string(got) != "after\r\n" {
		t.Errorf("row 0 holds %q, want the line printed after the clear", got)
	}
	for i := 1; i < len(body.Spans); i++ {
		if body.Spans[i] != nil {
			t.Errorf("row %d holds %q, but the clear left only one screenful; the window was answered before the end of the log",
				i, log[body.Spans[i].Start:body.Spans[i].End])
		}
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

// TestShellRailHeightIsTheScreenNotTheWindow pins the parameter that decides where
// every screen-relative sequence lands.
//
// The client asks for more rows than the screen — the screen plus a screen of
// margin on each side, so a scroll inside the margin is a repaint instead of a
// request — and the endpoint used to widen the modelled screen to fit that window.
// The model's screen height is not a window size: `ESC[H` counts from the top of the
// screen, `ESC[2J` erases a screenful, and the buffer trims at `scrollback + screen`,
// so a screen widened to 121 rows made `clear` erase and renumber against a screen
// the terminal does not have — cells landing rows away from their text.
//
// This is checked through the endpoint, because `height` is the endpoint's to pass
// on: it is the handler that reads the query and builds the layout, and asserting on
// a layout the test built itself would stay green while the handler rewrote the
// value. The tell is the response's own numbers: after a clear the layout holds one
// screenful and the rows above the surviving screen answer null.
func TestShellRailHeightIsTheScreenNotTheWindow(t *testing.T) {
	log := railLog(16) // four screens of output on a 4-row screen

	// The height is taken as given; a wider window changes nothing about the layout.
	for _, count := range []int{4, 25, 121} {
		body := serveRail(t, log, fmt.Sprintf("cols=20&top=0&count=%d&height=4", count))
		if body.TotalRows != 4 {
			t.Errorf("count=%d: the layout holds %d rows, want the screen (4)", count, body.TotalRows)
		}
		if len(body.Spans) != count {
			t.Errorf("count=%d: the response has %d spans", count, len(body.Spans))
		}
		// Row 0 of the surviving screen is the line printed after the clear. It is
		// the whole point of the sequence: a screen widened to the window would erase
		// against an origin 121 rows up and leave the answer somewhere else entirely.
		if len(body.Spans) == 0 || body.Spans[0] == nil {
			t.Fatalf("count=%d: row 0 has no span; the clear left no screen", count)
		}
		if got := log[body.Spans[0].Start:body.Spans[0].End]; string(got) != "after\r\n" {
			t.Errorf("count=%d: row 0 holds %q, want the line printed after the clear", count, got)
		}
		// Every row past the surviving screen is null: those rows were dropped, not
		// blanked, so they are not in the buffer to be answered at all.
		for i := 4; i < len(body.Spans); i++ {
			if body.Spans[i] != nil {
				t.Errorf("count=%d: row %d has a span past the buffer (%+v)", count, i, body.Spans[i])
			}
		}
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
			t.Errorf("row %d reaches into the full-screen program's screen: [%d,%d)", r, sp.Start, sp.End)
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
			row = r
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
				r, sp.Start, sp.End)
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
					size, r, pieces.rows[r].Start, pieces.rows[r].End, g.rows[r].Start, g.rows[r].End)
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
// TestShellRailBufferCapIsScrollbackPlusScreen pins where the terminal stops keeping
// rows, because a cap one screenful out puts every row number off by the screen
// height for the rest of the session — right until the assumption wears off, then
// wrong by a fixed amount.
//
// xterm sizes its line buffer to the configured scrollback *plus* the screen it is
// showing (`getCorrectBufferLength`), so the oldest row falls off at `scrollback +
// rows`. The model has to drop at the same point and say so with the same numbers.
func TestShellRailBufferCapIsScrollbackPlusScreen(t *testing.T) {
	// The cap itself, exercised at the boundary instead of by filling 100k rows: one
	// row over `scrollback + height` drops exactly one row off the front, and the rows
	// that survive keep their own bytes — which is the whole point of moving them down
	// rather than dropping them all. A cap that ignored the height would trim one
	// screenful early; one that used the height as the cap would throw the scrollback
	// away entirely.
	const height = 4
	g := newRowLayout(20, height)
	// A buffer filled to the cap, with a byte range on each row so a moved row can be
	// told from an empty one.
	g.rows = make([]shellRowSpan, shellRailScrollback+height)
	for i := range g.rows {
		g.rows[i] = shellRowSpan{Start: int64(i), End: int64(i) + 1}
	}
	g.row, g.top = shellRailScrollback+height-1, shellRailScrollback+height-height

	// One row past the cap: the oldest goes and the rest slide down by one.
	g.ensure(shellRailScrollback + height)
	if got := len(g.rows); got != shellRailScrollback+height {
		t.Errorf("past the cap the buffer holds %d rows, want it held at %d", got, shellRailScrollback+height)
	}
	if g.rows[0].Start != 1 {
		t.Errorf("the oldest surviving row starts at %d, want 1: the front row should have been dropped", g.rows[0].Start)
	}
	if got := g.row; got != shellRailScrollback+height-2 {
		t.Errorf("the cursor is on row %d after one row was dropped above it, want %d: it must stay on the same text",
			got, shellRailScrollback+height-2)
	}
	if got := g.top; got != shellRailScrollback+height-height-1 {
		t.Errorf("the screen top is %d after one row was dropped, want %d", got, shellRailScrollback+height-height-1)
	}

	// A buffer well inside the cap is untouched: the cap is a limit, not a size the
	// layout is padded to. Printing `n` lines on a small screen leaves one row per line
	// plus the one the final newline opened, and none of them is dropped however small
	// the screen is.
	for _, lines := range []int{3, 7, 9} {
		short := newRowLayout(20, height)
		var log []byte
		for i := 0; i < lines; i++ {
			log = append(log, []byte("L\r\n")...)
		}
		short.feed(0, log)
		if got, want := short.rowCount(), lines+1; got != want {
			t.Errorf("%d lines on a %d-row screen hold %d rows, want %d", lines, height, got, want)
		}
	}
}

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
					t.Errorf("mark %s at %d is on two rows: %d and %d", m.Status, m.Start, hit, row)
				}
				hit = row
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
				row, sp.Start, at)
		}
		at = sp.End
	}
	if at != int64(len(log)) {
		t.Errorf("the live rows end at %d, the log ends at %d", at, len(log))
	}
}
