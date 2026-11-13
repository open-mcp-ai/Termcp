package termview

import "testing"

// rows`. The model has to drop at the same point and say so with the same numbers.
func TestLayoutBufferCapIsScrollbackPlusScreen(t *testing.T) {
	// The cap itself, exercised at the boundary instead of by filling 100k rows: one
	// row over `scrollback + height` drops exactly one row off the front, and the rows
	// that survive keep their own bytes — which is the whole point of moving them down
	// rather than dropping them all. A cap that ignored the height would trim one
	// screenful early; one that used the height as the cap would throw the scrollback
	// away entirely.
	const height = 4
	g := New(20, height)
	// A buffer filled to the cap, with a byte range on each row so a moved row can be
	// told from an empty one.
	g.rows = make([]Span, Scrollback+height)
	for i := range g.rows {
		g.rows[i] = Span{Start: int64(i), End: int64(i) + 1}
	}
	g.row, g.top = Scrollback+height-1, Scrollback+height-height

	// One row past the cap: the oldest goes and the rest slide down by one.
	g.ensure(Scrollback + height)
	if got := len(g.rows); got != Scrollback+height {
		t.Errorf("past the cap the buffer holds %d rows, want it held at %d", got, Scrollback+height)
	}
	if g.rows[0].Start != 1 {
		t.Errorf("the oldest surviving row starts at %d, want 1: the front row should have been dropped", g.rows[0].Start)
	}
	if got := g.row; got != Scrollback+height-2 {
		t.Errorf("the cursor is on row %d after one row was dropped above it, want %d: it must stay on the same text",
			got, Scrollback+height-2)
	}
	if got := g.top; got != Scrollback+height-height-1 {
		t.Errorf("the screen top is %d after one row was dropped, want %d", got, Scrollback+height-height-1)
	}

	// A buffer well inside the cap is untouched: the cap is a limit, not a size the
	// layout is padded to. Printing `n` lines on a small screen leaves one row per line
	// plus the one the final newline opened, and none of them is dropped however small
	// the screen is.
	for _, lines := range []int{3, 7, 9} {
		short := New(20, height)
		var log []byte
		for i := 0; i < lines; i++ {
			log = append(log, []byte("L\r\n")...)
		}
		short.Feed(0, log)
		if got, want := short.RowCount(), lines+1; got != want {
			t.Errorf("%d lines on a %d-row screen hold %d rows, want %d", lines, height, got, want)
		}
	}
}
