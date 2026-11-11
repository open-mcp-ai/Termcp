package termview

import (
	"strings"
	"testing"
)

// A FULL-SCREEN PROGRAM PAINTS ON ITS OWN BUFFER (#314).
//
// The transcript model deliberately records nothing of the alternate screen — vim exits
// and the terminal puts back the rows that were there before, so none of the program's
// output belongs to the log. That decision is right for the rail and useless for a caller
// asking what the screen says while vim is up, which is the whole reason for a second
// grid: the program's characters land in cells, and the cells are what a screen is.

func altGridFor(t *testing.T, seq string) *Layout {
	t.Helper()
	g := New(80, 24)
	g.Feed(0, []byte("\x1b[?1049h"))
	if seq != "" {
		g.Feed(0, []byte(seq))
	}
	return g
}

// WHERE THE PROGRAM PUTS A CHARACTER IS THE ANSWER, not the order it wrote it.
func TestAltGridPlacesCharactersWhereTheProgramAddressedThem(t *testing.T) {
	g := altGridFor(t, "\x1b[3;5Hhello")
	lines := g.AltLines()
	if lines == nil {
		t.Fatalf("a program holds the screen but there is no grid")
	}
	if len(lines) < 3 {
		t.Fatalf("the grid has %d rows, want at least 3", len(lines))
	}
	if !strings.HasPrefix(lines[2], "    hello") {
		t.Fatalf("row 3 is %q, want the text at column 5", lines[2])
	}
	if strings.TrimSpace(lines[0]) != "" {
		t.Fatalf("row 1 is %q, want it blank", lines[0])
	}
}

// A REDRAW IS NOT A LOGCAL PROBLEM: `\r` overwrites and `CSI K` erases, so the grid must
// hold what stands there now, not every byte the program wrote.
func TestAltGridOverwritesAndErases(t *testing.T) {
	g := altGridFor(t, "\x1b[1;1Hdownload 10%\rdownload 99%\x1b[K")
	if got := g.AltLines()[0]; got != "download 99%" {
		t.Fatalf("row 1 is %q, want the repaint and not its history", got)
	}

	g = altGridFor(t, "\x1b[1;1Hkeep this\x1b[2J\x1b[1;1Hgone")
	lines := g.AltLines()
	if strings.Contains(strings.Join(lines, "\n"), "keep this") {
		t.Fatalf("CSI 2 J did not clear the screen: %q", lines)
	}
	if !strings.HasPrefix(lines[0], "gone") {
		t.Fatalf("after the clear the program's text went to %q, want row 1", lines[0])
	}
}

// A LINE FEED AT THE BOTTOM OF THE SCREEN SCROLLS IT, which is how a pager works.
func TestAltGridScrollsAtTheBottom(t *testing.T) {
	g := New(80, 4)
	g.Feed(0, []byte("\x1b[?1049h"))
	g.Feed(0, []byte("one\r\ntwo\r\nthree\r\nfour\r\nfive"))
	lines := g.AltLines()
	if len(lines) != 4 {
		t.Fatalf("the grid has %d rows, want the screen's 4", len(lines))
	}
	if strings.TrimSpace(lines[0]) != "two" || !strings.HasPrefix(lines[3], "five") {
		t.Fatalf("the screen did not scroll: %q", lines)
	}
}

// DECSTBM SETS WHERE SCROLLING HAPPENS, and a full-screen program that keeps a status line
// at the top or a prompt at the bottom depends on it: the margin area moves, the rest of
// the screen stands still.
func TestAltGridScrollsOnlyInsideTheMargins(t *testing.T) {
	g := New(80, 5)
	g.Feed(0, []byte("\x1b[?1049h"))
	// Header stays, region rows 2..4 scroll, footer stays.
	g.Feed(0, []byte("\x1b[1;1HHEADER"))
	g.Feed(0, []byte("\x1b[5;1HFOOTER"))
	g.Feed(0, []byte("\x1b[2;4r"))
	g.Feed(0, []byte("\x1b[2;1Ha\r\nb\r\nc\r\nd"))
	lines := g.AltLines()
	if got := strings.TrimSpace(lines[0]); got != "HEADER" {
		t.Fatalf("row 1 is %q, want the header untouched by the region's scroll", got)
	}
	if got := strings.TrimSpace(lines[4]); got != "FOOTER" {
		t.Fatalf("row 5 is %q, want the footer untouched", got)
	}
	if got := strings.TrimSpace(lines[1]); got != "b" {
		t.Fatalf("row 2 is %q, want the region to have scrolled by one", got)
	}
	if got := strings.TrimSpace(lines[3]); got != "d" {
		t.Fatalf("row 4 is %q, want the newest line of the region", got)
	}
}

// LEAVING THE SCREEN THROWS THE PROGRAM'S BUFFER AWAY, exactly as a terminal does.
func TestAltGridIsDiscardedOnExit(t *testing.T) {
	g := altGridFor(t, "\x1b[1;1Hprogram output")
	if g.AltLines() == nil {
		t.Fatalf("setup: no grid while a program holds the screen")
	}
	g.Feed(0, []byte("\x1b[?1049l"))
	if lines := g.AltLines(); lines != nil {
		t.Fatalf("the program's screen survived its exit: %q", lines)
	}
}

// AND THE TRANSCRIPT IS UNTOUCHED BY ANY OF IT, which is the property the rail depends on
// and the reason the two buffers are separate.
func TestAltGridDoesNotReachTheTranscript(t *testing.T) {
	g := New(80, 24)
	g.Feed(0, []byte("before\r\n"))
	before := g.RowCount()
	g.Feed(80, []byte("\x1b[?1049h\x1b[1;1H"+strings.Repeat("noise ", 40)))
	if got := g.RowCount(); got != before {
		t.Fatalf("painting the program's screen added %d transcript rows", got-before)
	}
}
