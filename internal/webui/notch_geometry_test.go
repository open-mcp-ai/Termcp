package webui

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ruleContaining returns the rule that holds the given declaration — its selector
// list and declaration block — located by scanning outward from the marker to its
// enclosing braces. The notch knobs, their clip-path and their background layers live
// in shared multi-selector rules, which the single-line cssRule helper cannot reach.
// The selector is included so a test can assert which plates a rule applies to.
func ruleContaining(t *testing.T, css, marker string) string {
	t.Helper()
	i := strings.Index(css, marker)
	if i < 0 {
		t.Fatalf("app.css has no declaration %q", marker)
	}
	open := strings.LastIndex(css[:i], "{")
	close := strings.Index(css[i:], "}")
	if open < 0 || close < 0 {
		t.Fatalf("could not find the block around %q", marker)
	}
	// Step back over the blank lines and comments between the previous rule and
	// this selector.
	start := strings.LastIndex(css[:open], "}") + 1
	return css[start : i+close+1]
}

// TestNotchGeometryMatchesItsKnobs pins the numbers a reader cannot see are linked.
//
// The notch is a fold: the left wall steps in at 45° over --notch-run and drops
// straight to the bottom edge. That step is exactly the main diagonal of the square
// --notch-run box, which is why --notch-line can be a plain 45deg gradient on a
// square layer — the angle belongs to the layer box, so it stays on the step at
// every panel size. (The shape this replaced sloped a share of the panel's height,
// so its angle followed the aspect ratio and had to be derived per box.) This check
// pins what makes that true — the 45deg direction, the square layer, the absolute
// stops, the position correction — because a percentage band on a diagonal gradient
// is not 1px, and a percentage *offset* silently measures against (area - image)
// and slides the stroke off the fold, where the clip then hides it entirely.
//
// The stroke's size and position are derived from the knobs, so retuning the notch
// means moving the knobs and nothing else.
func TestNotchGeometryMatchesItsKnobs(t *testing.T) {
	css := readAppCSS(t)

	knob := func(name string) float64 {
		t.Helper()
		re := regexp.MustCompile(regexp.QuoteMeta(name) + `:\s*([0-9.]+)px`)
		m := re.FindStringSubmatch(css)
		if m == nil {
			// --notch-left is a share of the panel's height, so it is the one knob
			// that is a percentage rather than a length.
			re = regexp.MustCompile(regexp.QuoteMeta(name) + `:\s*([0-9.]+)%`)
			m = re.FindStringSubmatch(css)
		}
		if m == nil {
			t.Fatalf("app.css declares no %s", name)
		}
		v, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return v
	}
	left, run := knob("--notch-left"), knob("--notch-run")
	// The fold needs a positive inset to step over and a fold line to sit above
	// the bottom edge, or the clip and the stroke describe different shapes.
	if run <= 0 || left <= 0 {
		t.Fatalf("the knobs (%v%%, %vpx) do not describe a notch: --notch-left and "+
			"--notch-run must both be positive", left, run)
	}

	notchLine := regexp.MustCompile(`(?s)--notch-line:\s*linear-gradient\(([^;]*);`).FindStringSubmatch(css)
	if notchLine == nil {
		t.Fatal("app.css declares no --notch-line gradient")
	}
	if !strings.Contains(notchLine[1], "45deg") {
		t.Errorf("--notch-line is %q; the step must be drawn at 45deg, the only angle that "+
			"stays on a run-shaped step at every panel size", strings.TrimSpace(notchLine[1]))
	}

	// Any percentage stop on a diagonal gradient measures along a line whose
	// length depends on the box, so the band would stop being 1px. Both stops have
	// to be the 50% centre offset by a length.
	for _, want := range []string{
		"transparent calc(50% - .5px)",
		"transparent calc(50% + .5px)",
	} {
		if !strings.Contains(notchLine[1], want) {
			t.Errorf("--notch-line has no %q stop, so its band is not a 1px stroke on the step", want)
		}
	}

	// The strokes must be anchored so they are measured from the panel's own edges.
	// A percentage in background-position is a share of (positioning area - image):
	// `top: var(--notch-left)` therefore lands the step's square half its own height
	// too high, off the fold and — since it is also off the clip — invisible. The
	// half-run restores the absolute reading. The wall's stroke is anchored from the
	// bottom edge for the same reason, and both offsets are lengths.
	strokes := ruleContaining(t, css, "background-position:")
	for _, want := range []string{
		"top calc(var(--notch-left) + var(--notch-run) / 2)",
		"bottom 0",
	} {
		if !strings.Contains(strokes, want) {
			t.Errorf("the notch strokes must be anchored with %q so they are measured from the "+
				"bottom edge, not from (container - image)", want)
		}
	}
	if strings.Contains(strokes, "calc(100% - var(--notch") {
		t.Error("a notch stroke is positioned with calc(100% - ...), which measures the offset " +
			"against (container - image) and lands the line too high")
	}

	// The 45deg band only lands on the step while its layer is that square, and the
	// wall's stroke is 1px wide and as tall as the wall.
	sizes := ruleContaining(t, css, "background-size:")
	for _, want := range []string{
		"var(--notch-run) var(--notch-run)",
		"1px calc(var(--notch-left) - var(--notch-run))",
	} {
		if !strings.Contains(sizes, want) {
			t.Errorf("the stroke layers have no %q size, so a stroke is not the edge it draws", want)
		}
	}

	// The clip must actually remove the notch the knobs describe, and the plates
	// that carry it must all be named by the SAME rule — keeping the corner geometry
	// in one place is what stops a plate from silently keeping an older shape, which
	// is exactly how the cut-only and notched rules drifted apart once already.
	//
	// The scope is deliberate: a notch belongs on a wide plate whose height is
	// bounded. A near-square session card, an approval row and a jump editor whose
	// heights follow their content are NOT notched, and neither is the host drawer
	// (#sec-entries-body), which is full-height. They stay rectangular, so they must
	// not be listed here.
	notchRule := ruleContaining(t, css, "--notch-run:")
	strokeRule := ruleContaining(t, css, "background-position:")
	for _, plate := range []string{
		".workspace-sessions", ".modal", ".conn-tile.entry-card",
		".conn-load-banner", ".jump-card.jump-summary", ".drawer-toolbar .host-batch-btn",
	} {
		if !strings.Contains(notchRule, plate) {
			t.Errorf("the notched plate rule is missing %s", plate)
		}
		if !strings.Contains(strokeRule, plate) {
			t.Errorf("%s is notched but draws no stroke, so its clip has no visible edge", plate)
		}
	}
	// Content-height plates and the full-height drawer stay rectangular on purpose.
	for _, plate := range []string{".conn-tile.sess-tile", ".approval-item", ".ui-notify-card", "#sec-entries-body"} {
		for _, rule := range []string{notchRule, strokeRule} {
			if strings.Contains(rule, plate) {
				t.Errorf("%s has a content-driven or full height, so the notch would clip its "+
					"own content", plate)
			}
		}
	}
	if strings.Contains(notchRule, ".jump-card,") || strings.Contains(notchRule, ".jump-card {") {
		t.Error("the notch must be scoped to .jump-card.jump-summary; the expanded editor's " +
			"height follows its fields, so a notch would cut into them")
	}
	clip := regexp.MustCompile(`clip-path:\s*polygon\(([^;]*)\);`).FindStringSubmatch(notchRule)
	if clip == nil {
		t.Fatal("the notched plates declare no clip-path")
	}
	for _, want := range []string{
		"var(--notch-run) 100%", // the notch's width: where the step's run ends
		"var(--notch-run) calc(100% - var(--notch-left) + var(--notch-run))", // the step
		"0 calc(100% - var(--notch-left))",                                   // the fold
	} {
		if !strings.Contains(clip[1], want) {
			t.Errorf("the clip-path does not contain %q, so the notch it removes is not the "+
				"one the knobs describe", want)
		}
	}
}

// TestNotchedPlatesKeepContentClearOfTheNotch keeps content insets on the CONTENT
// boxes, not on the shape rule. The plate's clip is decoration; padding/margins are
// layout and belong to the things inside it.
func TestNotchedPlatesKeepContentClearOfTheNotch(t *testing.T) {
	css := readAppCSS(t)
	for _, want := range []string{
		".modal-body { padding: 8px 16px; }",
		".workspace-sessions .section-body { padding: 8px 16px; min-height: 180px; }",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css has no content inset %q; padding belongs on content, not the "+
				"notch container", want)
		}
	}
	// NetHub's resource list carries its own inset rather than inheriting one from
	// the plate rule: the sidebar is a grid column with a flat clip, but the rule
	// this test defends is the same one — spacing lives on the content box.
	list := between(t, css, "#sec-entries-body > #conn-grid {", "}")
	if !strings.Contains(list, "padding:") && !strings.Contains(list, "margin-left:") {
		t.Errorf("the resource list must carry its own content inset; got %q", list)
	}

	// The shape rule itself must not grow layout padding. Its declaration block is
	// intentionally limited to geometry tokens and clip-path.
	shape := ruleContaining(t, css, "--notch-run:")
	declarations := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(shape, "")
	if strings.Contains(declarations, "padding") || strings.Contains(declarations, "margin") {
		t.Error("the notched plate rule contains layout spacing; put padding/margins on content boxes")
	}
}

// TestPlateShapeDeclaredOnce guards the failure mode that produced this file: two
// rules describing the same corner with different selector lists, so some plates
// silently kept an older shape. Every element that draws the corner — the plates
// and the keys alike — has to read from ONE clip-path declaration.
func TestPlateShapeDeclaredOnce(t *testing.T) {
	css := readAppCSS(t)
	if n := strings.Count(css, "clip-path:"); n != 1 {
		t.Errorf("app.css declares %d clip-paths; the corner shape must be declared once "+
			"and shared through its knobs, or the copies drift apart", n)
	}
	// The keys must not go back to hardcoding a corner of their own.
	for _, sel := range []string{".btn {", ".icon-btn {"} {
		if strings.Contains(css, sel+" clip-path") || strings.Contains(css, sel+"clip-path") {
			t.Errorf("%s declares its own clip-path instead of using the shared knobs", sel)
		}
	}
}
