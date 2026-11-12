package mcp

// The two modelling gaps the second dogfood round found, both in how a row's bytes become
// the line a caller reads.

import (
	"strings"
	"testing"
)

// A SHELL REPAINTS ITS INPUT LINE, and the row's span holds every paint. Returning the span
// verbatim gave a palimpsest: the command line concatenated with its own previous versions.
// The row's own CR and CSI K are what turn the paints back into the characters that survived.
func TestScreenRowAppliesItsOwnRepaints(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "carriage returns keep the last paint",
			raw:  "alpha\rbeta\rgamma",
			want: "gamma",
		},
		{
			// What a shell actually does when it repaints: CR to column one, erase to the
			// end of the line, write the new text.
			name: "cr then erase then rewrite keeps the rewrite",
			raw:  "old text\r\x1b[Knew",
			want: "new",
		},
		{
			// EL does not move the cursor, so `new` lands where the cursor already was.
			name: "erase to end of line leaves the cursor put",
			raw:  "old text\x1b[Knew",
			want: "old textnew",
		},
		{
			name: "erase whole line clears without moving the cursor",
			raw:  "gone\x1b[2Kkept",
			want: "    kept",
		},
		{
			name: "colour is dropped, characters stay",
			raw:  "\x1b[31mred\x1b[m and plain",
			want: "red and plain",
		},
		{
			name: "backspace moves back without erasing",
			raw:  "abc\b\bd",
			want: "adc",
		},
		{
			name: "a newline inside a span ends the row",
			raw:  "first\nsecond",
			want: "first",
		},
		{
			name: "wide runes occupy one column each here, not two",
			raw:  "中文",
			want: "中文",
		},
	} {
		if got := renderScreenRow([]byte(tc.raw)); got != tc.want {
			t.Errorf("%s: renderScreenRow(%q) = %q, want %q", tc.name, tc.raw, got, tc.want)
		}
	}
}

// A WRAPPED ROW AND ITS CONTINUATION CAN SHARE A SPAN. Emitting each span from its own start
// printed the text twice, which is what "the screen repeats the session" looked like from
// the outside. Bytes are rendered once, in order.
func TestScreenRowsDoNotReEmitTheirNeighbour(t *testing.T) {
	s := newTestServer(t)
	h := openPty(t, s)
	defer termText(t, s, map[string]any{"cmd": "close " + h})
	awaitPtyReady(t, s, h)

	// Long enough to wrap at 80 columns, with a unique marker at the front.
	termText(t, s, map[string]any{"cmd": "run " + h + " $1",
		"args": []any{`echo WRAP""MARK-` + strings.Repeat("x", 200)}, "timeout": 10})
	screenUntil(t, s, h, "WRAPMARK")

	// Every page height, because the duplication only appeared once a page covered the
	// whole screen.
	for _, rows := range []string{"5", "10", "20", "24", "40"} {
		got := termText(t, s, map[string]any{
			"cmd": "read " + h + " --screen --top 0 --rows " + rows, "timeout": 0})
		_, body, _ := strings.Cut(got, "\n\n")
		if n := strings.Count(body, "WRAPMARK"); n > 1 {
			t.Fatalf("--rows %s printed the marker %d times:\n%s", rows, n, body)
		}
	}
}
