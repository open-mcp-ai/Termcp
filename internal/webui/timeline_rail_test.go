package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestRailDistinctCues checks each status has its own colour and that the strip
// carries the pieces a reader needs: a cell, a hover label, a detail card and the
// badge for a reviewed row.
//
// The colours are read out of the stylesheet rather than asserted in full, so a
// theme override can restyle the rail (assets can be replaced) while a status that
// lost its colour — or two statuses that ended up sharing one — still fails. At
// twelve pixels wide on a dark background, two statuses with one colour are one
// status as far as the reader is concerned.
func TestRailDistinctCues(t *testing.T) {
	css := readAppCSS(t)

	// A status colour is declared on a bare status rule (`.term-rail-output {
	// background: … }`) and nowhere else: the compound rules a cell also carries
	// (hover, focus, the swatch inside the card) must not be read as the status's
	// own colour. The declaration follows the opening brace on the same line, so
	// the rule name is checked up to that brace and not to the end of the line.
	hues := map[string]string{}
	for _, line := range strings.Split(css, "\n") {
		trimmed := strings.TrimSpace(line)
		for _, status := range []string{"output", "input", "agent", "review"} {
			rule := ".term-rail-" + status
			if !strings.HasPrefix(trimmed, rule) {
				continue
			}
			// The next character must be the brace, so `.term-rail-inputx` and
			// `.term-rail-inputs` (the swatch in a card) do not match.
			rest := trimmed[len(rule):]
			if !strings.HasPrefix(strings.TrimLeft(rest, " "), "{") {
				continue
			}
			open := strings.Index(rest, "background: ")
			if open < 0 {
				t.Errorf(".term-rail-%s sets no background", status)
				continue
			}
			rest = rest[open+len("background: "):]
			if semi := strings.Index(rest, ";"); semi >= 0 {
				rest = rest[:semi]
			}
			hues[status] = strings.TrimSpace(rest)
		}
	}
	for _, status := range []string{"output", "input", "agent", "review"} {
		if hues[status] == "" {
			t.Errorf("app.css has no bare .term-rail-%s rule with a background", status)
		}
	}
	seen := map[string]string{}
	for status, hue := range hues {
		if hue == "" {
			continue
		}
		if other, dup := seen[hue]; dup {
			t.Errorf("%s and %s share the colour %s; the rail cannot be read by colour alone", status, other, hue)
		}
		seen[hue] = status
	}
	if len(seen) != 4 {
		t.Errorf("expected 4 distinct status colours, got %d (%v)", len(seen), hues)
	}

	for _, want := range []string{
		".term-rail {",
		".term-rail-cell {",
		".term-rail-pop {",
		".term-rail-card {",
		".term-rail-card-badge {",
		".term-rail-swatch {",
		".term-rail-cause-input",
		".term-rail-cause-agent",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css misses %q", want)
		}
	}
	// The old per-mark vocabulary must not come back: a mark was a tick placed by
	// byte share, and its styles are what a reader would see if the rail reverted
	// to plotting offsets. Cells are the rows.
	for _, gone := range []string{".term-rail-mark", ".term-rail-dot", ".term-rail-diamond"} {
		if strings.Contains(css, gone) {
			t.Errorf("app.css still styles %s; the rail draws one cell per terminal row", gone)
		}
	}
	// A cell is a button: the whole thing is hoverable and focusable, which a
	// filled div is not. It is positioned absolutely and stretched by its insets,
	// so the module only ever sets top/height on it.
	cell := between(t, css, "\n.term-rail-cell {", "\n}")
	for _, want := range []string{"position: absolute", "left: 2px", "right: 2px"} {
		if !strings.Contains(cell, want) {
			t.Errorf("a cell misses %q: %s", want, cell)
		}
	}
	// Cells tile flush. A radius, a background clip or a horizontal border on the
	// base cell is what made a run of same-status rows look like a string of beads —
	// one per row — instead of the one band it is, and a reader then counts beads to
	// learn what a colour already said. The hue changing is what marks a status
	// change. A zero radius is spelled out because a button carries the browser's
	// own otherwise.
	//
	// A cause frame is the one exception and it lives on its own classes, never on
	// the base cell: it outlines a run of output in the colour of the input that
	// produced it, and only the run's first and last row take a horizontal edge
	// (see .term-rail-cause-*).
	for _, unwanted := range []string{"background-clip", "border-top", "border-bottom"} {
		if strings.Contains(cell, unwanted) {
			t.Errorf("the base cell sets %q; cells must tile flush, with no radius and no gap: %s", unwanted, cell)
		}
	}
	if strings.Contains(cell, "border-radius") && !strings.Contains(cell, "border-radius: 0") {
		t.Errorf("a cell rounds its corners again; cells must tile flush: %s", cell)
	}
	// The frame's colour must be the input's own: the frame is how a reader ties a
	// stretch of output back to the command that produced it, and a frame in a
	// different blue is a new colour to learn rather than a reference to one that
	// is already on the rail. The value is read from the status rule itself rather
	// than repeated here, so the two cannot drift apart silently.
	for _, tc := range []struct{ rule, status string }{
		{".term-rail-cause-input", "input"},
		{".term-rail-cause-agent", "agent"},
	} {
		// The rule's own block, found by brace depth so a one-line rule and a
		// wrapped one both read the same: the shared block that lists
		// `.term-rail-cause-input,` / `.term-rail-cause-agent {` draws the edges
		// and names no colour, so its selector must not be mistaken for this one.
		frame := cssRule(t, css, tc.rule)
		want := "--rail-cause: " + hues[tc.status] + ";"
		if !strings.Contains(frame, want) {
			t.Errorf("%s is not %q; the frame must reuse the input's colour (%s)", tc.rule, want, frame)
		}
	}
	// The frame has to close: vertical edges alone are two lines, not a frame, so
	// the run's ends carry the horizontal ones.
	for _, want := range []string{
		".term-rail-cause-top {",
		".term-rail-cause-bottom {",
		"border-top-width: 1px",
		"border-bottom-width: 1px",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css misses %q; the cause frame would not close around its run", want)
		}
	}
}

// TestRailCallsTheRealTerminalGeometry pins the one thing that makes cells line
// up with text: the strip is measured to xterm's own screen box.
//
// The strip hangs off the channel body while the rows belong to `.xterm-screen`
// inside it, and those boxes differ by the terminal wrap's padding and by the
// scrollbar width. Cells placed as percentages of the wrong box sit a few pixels
// off every row — visible, and maddening to diagnose from a screenshot.
func TestRailCallsTheRealTerminalGeometry(t *testing.T) {
	js := readAssetLF(t, "static/js/timeline.js")
	for _, want := range []string{
		".xterm-screen",
		"getBoundingClientRect",
		"function railScreenBox(",
		"function railViewFor(",
		"function railViewMoved(",
		"function railRepaintWindow(",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("timeline.js misses %q; cells would be placed against the wrong box", want)
		}
	}
	// The percentages are what survive a resize without a repaint; a pixel top per
	// cell would need every cell rewritten on every resize.
	if !strings.Contains(js, "toFixed(4) + '%'") {
		t.Error("cells are no longer placed as percentages of the strip")
	}
	// xterm moves rows under the strip on scroll and on resize; both have to be
	// followed without re-fetching the marks.
	for _, want := range []string{"'scroll'", "onResize", "ResizeObserver"} {
		if !strings.Contains(js, want) {
			t.Errorf("timeline.js no longer watches %s; the strip would stop following the terminal", want)
		}
	}
	if !strings.Contains(js, "xterm-viewport") {
		t.Error("the scroll listener is no longer on the viewport element")
	}
}

// TestRailCoverageUsesAbsoluteTotalRows exercises the client's stale-tail guard.
//
// `spans.length` is the requested count, while `total_rows` is an absolute row
// number. During a streaming command the response can therefore contain 121 array
// slots while the log only has 1427 rows and the viewport already reaches row 1431.
// Treating the array length as the extent says the response covers the viewport and
// leaves the newest rows without cells. The JS path is run, not only searched, so a
// later refactor cannot silently reintroduce the off-by-top arithmetic.
func TestRailCoverageUsesAbsoluteTotalRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot execute rail coverage helper")
	}
	js := readAssetLF(t, "static/js/timeline.js")
	path := filepath.Join(t.TempDir(), "timeline.js")
	if err := os.WriteFile(path, []byte(js), 0o600); err != nil {
		t.Fatal(err)
	}
	const probe = `
const fs = require('fs'), vm = require('vm');
vm.runInThisContext(fs.readFileSync(process.argv[1], 'utf8'));
const body = { cols: 80, top: 1354, spans: Array(121).fill(null), total_rows: 1427 };
const staleTail = railCoversWindow({ lastBody: body }, { top: 1406, rows: 25 }, 80);
if (staleTail) throw new Error('a 121-slot response was treated as covering rows past absolute total_rows');
body.total_rows = 1450;
const covered = railCoversWindow({ lastBody: body }, { top: 1406, rows: 25 }, 80);
if (!covered) throw new Error('a viewport inside absolute total_rows was rejected');
console.log('ok');
`
	cmd := exec.Command(node, "-e", probe, path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rail coverage probe failed: %v\n%s", err, out)
	}
}

// TestRailReservesAColumnBeforeTheScrollbar pins the non-overlap contract. The
// terminal keeps its full viewport box, and therefore its scrollbar at the far
// right; fitShellTerminal gives the text grid fewer columns, while the rail is
// placed in the newly free column immediately before that scrollbar.
//
// The two halves have to agree. A rail placed beside a grid that still uses the
// full width paints over the last columns, and a grid narrowed without a rail
// leaves a gutter of dead space — so both the CSS variables and the fit arithmetic
// are pinned here.
func TestRailReservesAColumnBeforeTheScrollbar(t *testing.T) {
	css := readAppCSS(t)
	// The two variables are read as a set and from the rule that owns them: a bare
	// substring search would also match the coarse-pointer override, so a desktop
	// width change could pass while the media query silently stopped being the
	// override. Each variable is asserted in the declaration block it belongs to.
	body := between(t, css, "\n  .shell-channel-body {", "\n  }")
	for _, want := range []string{"--term-rail-w: 14px", "--term-rail-gap: 10px"} {
		if !strings.Contains(body, want) {
			t.Errorf(".shell-channel-body misses %q; the rail column would not be reserved beside the scrollbar: %s", want, body)
		}
	}
	coarse := between(t, css, "@media (pointer: coarse) and (hover: none) {\n  .shell-channel-body", "\n}")
	if !strings.Contains(coarse, "--term-rail-w: 22px") {
		t.Errorf("the coarse-pointer override no longer widens the reserved column; the strip would be a target over the text: %s", coarse)
	}
	// The strip itself sits in that column, immediately left of the scrollbar's
	// gutter — not at `right: 16px`, which was the overlay that covered the text.
	rail := between(t, css, "\n.term-rail {", "\n}")
	for _, want := range []string{"right: var(--term-rail-gap, 10px)", "width: var(--term-rail-w, 14px)"} {
		if !strings.Contains(rail, want) {
			t.Errorf(".term-rail misses %q; the strip would not sit in the reserved column: %s", want, rail)
		}
	}
	// The terminal instance must keep the body's full box. Insetting it before the
	// rail is the tempting mistake: it drags the scrollbar left with it, so the
	// scrollbar no longer sits at the far right where a reader reaches for it.
	inst := between(t, css, "\n  .shell-channel-instance {", "\n  }")
	if !strings.Contains(inst, "inset: 0;") {
		t.Errorf("the terminal instance no longer fills the channel body; the scrollbar would move: %s", inst)
	}

	js := readAssetLF(t, "static/js/ui-socket.js")
	for _, want := range []string{
		"getPropertyValue('--term-rail-w')",
		"getPropertyValue('--term-rail-gap')",
		"Math.floor((w - reserve) / charWidth)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("ui-socket.js misses %q; xterm would still paint text beneath the rail", want)
		}
	}
}

// TestRailFetchGapFollowsWindowedCost pins the gap between two marks fetches at a
// value the request can afford.
//
// The gap exists so a flooding shell cannot turn into a request storm, but it is
// also the rail's lag: marks that arrive just after a refresh wait out the rest of
// it. It was set to 800ms when a refresh meant the shell's whole index — hundreds
// of milliseconds and megabytes of JSON — and left there when the fetch became a
// few-kilobyte byte window, so the gap went on being the delay it was written to
// prevent. A value above a few hundred milliseconds is that bug again.
func TestRailFetchGapFollowsWindowedCost(t *testing.T) {
	js := readAssetLF(t, "static/js/timeline.js")
	m := regexp.MustCompile(`SHELL_RAIL_MIN_GAP_MS = (\d+);`).FindStringSubmatch(js)
	if m == nil {
		t.Fatal("timeline.js no longer defines SHELL_RAIL_MIN_GAP_MS")
	}
	gap, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse the fetch gap: %v", err)
	}
	if gap > 300 {
		t.Errorf("the fetch gap is %dms; a windowed fetch costs microseconds, so this is the rail's lag", gap)
	}
	if gap < 50 {
		t.Errorf("the fetch gap is %dms; continuous output would fetch faster than a terminal can refresh", gap)
	}
	// The window, not the gap, is what keeps the request small: a refresh asks for
	// the rows on screen plus a margin, and the server stops reading the log once
	// they are settled.
	if !strings.Contains(js, "SHELL_RAIL_MARGIN_ROWS") {
		t.Error("the refresh no longer asks for a bounded row window; the gap would have to protect a full log layout again")
	}
}

// cssRule returns the declaration block of the rule whose selector is exactly
// sel. A multi-selector rule that merely lists sel among others is skipped: the
// caller wants the rule that is sel's own. A one-line rule (selector, declaration
// and closing brace on one line, possibly followed by a comment) is accepted.
func cssRule(t *testing.T, css, sel string) string {
	t.Helper()
	lines := strings.Split(css, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, sel) {
			continue
		}
		// The next token must open the block; a comma here means the rule lists
		// several selectors and this is not the one that names the colour.
		rest := strings.TrimSpace(trimmed[len(sel):])
		if rest == "" || rest[0] != '{' {
			continue
		}
		// A rule whose selector was continued from the line above (`x,` then
		// `y {`) is a shared block, not this rule's own.
		if i > 0 && strings.HasSuffix(strings.TrimSpace(lines[i-1]), ",") {
			continue
		}
		var b strings.Builder
		for j := i; j < len(lines); j++ {
			b.WriteString(lines[j])
			b.WriteString("\n")
			if strings.Contains(lines[j], "}") {
				break
			}
		}
		return b.String()
	}
	t.Fatalf("app.css has no %s rule of its own", sel)
	return ""
}
