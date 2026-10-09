package webui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The batch-select switch's expand/collapse has to be VISIBLE, which turns out to
// be a property of the companion keys rather than of the switch.
//
// The switch animates its own `flex` from `1 1 0%` (full row) to `0 0 34px` (a
// square) over 0.2s. That transition was measured in a real browser and 78% of
// its travel happened in the FIRST frame: the companion keys were `display: none`
// when collapsed, and `display` is not interpolable, so the moment they were
// revealed they claimed their 181px at once. The switch only gets the space the
// companions leave, so it was crushed from 283px to 90px before its transition
// ran a single step, and the remaining 56px covered the whole 0.2s. The row read
// as "no animation" while every declaration looked correct.
//
// So the companions must hold a length in BOTH states and animate that length:
// in the flow in both states (NOT `display: none`), zero length at rest, and the
// length transitioned. This test pins that arrangement, and deliberately not
// "does the switch have a transition" — the broken version had one.
func TestNetHubSelectToggleSlideNeedsTheCompanionsToReleaseLengthGradually(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("assets", "themes", "cyberpunk", "assets", "static", "css", "workspace.css"))
	if err != nil {
		t.Fatal(err)
	}
	rules := parseNethubRules(string(src))

	// The switch carries the motion itself, over the 0.2s the design asks for.
	toggle := nethubRule(t, rules, ".nethub-select-toggle")
	if !strings.Contains(toggle, "flex: 1 1 0%") {
		t.Errorf("the collapsed switch is no longer `flex: 1 1 0%%`, so it has no row to slide across; got:\n%s", toggle)
	}
	pressed := nethubRule(t, rules, `.nethub-select-toggle[aria-pressed="true"]`)
	if !strings.Contains(pressed, "flex: 0 0 34px") {
		t.Errorf("the pressed switch is no longer `flex: 0 0 34px`, so the slide has nowhere to travel; got:\n%s", pressed)
	}
	props := nethubTransitionProps(rules, ".nethub-select-toggle")
	if props == nil {
		t.Fatal("the switch has no transition, so the expand/collapse does not animate")
	}
	for _, want := range []string{"flex-basis", "flex-grow"} {
		if !props[want] {
			t.Errorf("the switch does not transition %s, so its slide is an instant jump", want)
		}
	}

	// The companions: in the flow in both states, zero length at rest, length
	// transitioned. Any one of these missing restores the first-frame crush.
	for _, sel := range []string{".nethub-batch-icon", ".nethub-open-split"} {
		base := nethubRule(t, rules, sel)
		if base == "" {
			t.Errorf("%s has no rule", sel)
			continue
		}
		// `display: none` is the specific regression: an element out of the flow
		// cannot animate its length back in, so its width arrives whole.
		if disp, ok := nethubDeclaration(base, "display"); ok && disp == "none" {
			t.Errorf("%s is `display: none` while collapsed. That cannot be interpolated, so the key "+
				"claims its width in the first frame and crushes the switch's slide — the switch then "+
				"teleports most of the way and animates only the sliver left over", sel)
			continue
		}
		// Zero length at rest, or the collapsed row is not actually collapsed.
		flex, ok := nethubDeclaration(base, "flex")
		if !ok {
			t.Errorf("%s declares no `flex`, so its collapsed length is unknown", sel)
		} else if !strings.HasPrefix(strings.TrimSpace(flex), "0 0") {
			t.Errorf("%s does not collapse to a zero basis (flex: %q), so it occupies the row at rest", sel, flex)
		}
		// The length has to be transitioned, or the release is a jump and the
		// switch is crushed just as surely as with a `display` flip.
		if p := nethubTransitionProps(rules, sel); p == nil {
			t.Errorf("%s has no transition, so its length arrives in one frame", sel)
		} else if !p["flex-basis"] {
			t.Errorf("%s does not transition flex-basis, so it claims its width in one frame and the "+
				"switch's slide is crushed to whatever is left", sel)
		}
	}

	// Pressing must reveal them over that same length, with the fade layered on
	// top so they do not pop at the ends of the slide.
	for _, sel := range []string{".nethub-selecting .nethub-batch-icon", ".nethub-selecting .nethub-open-split"} {
		body := nethubRule(t, rules, sel)
		if body == "" {
			t.Errorf("%s has no rule, so the key never appears", sel)
			continue
		}
		if disp, ok := nethubDeclaration(body, "display"); ok && disp == "none" {
			t.Errorf("%s does not reveal the key (display: none)", sel)
		}
		if p := nethubTransitionProps(rules, sel); p == nil || !p["flex-basis"] {
			t.Errorf("%s does not transition its length when pressed, so the collapse would not "+
				"release it gradually in reverse either", sel)
		}
	}

	// A container `gap` cannot be interpolated and is not part of any key's box,
	// so the row's spacing would appear and vanish in one frame. It has to live on
	// the keys as a margin, which does transition.
	if gap, ok := nethubDeclaration(nethubRule(t, rules, ".drawer-toolbar"), "gap"); ok && gap != "0" {
		t.Errorf("the toolbar sets `gap: %s`, which is not interpolable: the row's spacing would "+
			"appear in the first frame and eat part of the switch's slide", gap)
	}

	// Reduced motion must NOT stop this slide, and that is a deliberate decision
	// rather than an oversight. The setting is for motion nobody asked for — the
	// pulsing status dot, the breathing backdrop — while this slide is the answer
	// to a click: the switch IS the control that opens the row, so removing it
	// removes the only feedback that the press landed and the row just appears.
	//
	// It also has to be consistent with the panel it sits in: the NetHub panel's
	// own collapse (`sec-entries-body.transform`) is not reduced either, and a row
	// that snapped while its panel slid read as a broken button rather than as a
	// respected preference. Glass is unaffected either way — it keys off
	// `prefers-reduced-transparency`, a different media feature.
	theme, err := os.ReadFile(filepath.Join("assets", "themes", "cyberpunk", "assets", "static", "css", "theme.css"))
	if err != nil {
		t.Fatal(err)
	}
	reduced := string(theme)
	for _, m := range regexp.MustCompile(`@media[^{]*prefers-reduced-motion[^{]*\{`).FindAllStringIndex(reduced, -1) {
		block := balancedNethubBlock(reduced[m[1]-1:])
		for _, sel := range []string{".nethub-select-toggle", ".nethub-batch-icon", ".nethub-open-split"} {
			if strings.Contains(block, sel) {
				t.Errorf("prefers-reduced-motion still names %s; the toolbar's expand/collapse is the "+
					"answer to a click, so reducing it leaves the row with no feedback at all", sel)
			}
		}
	}
}

// nethubRule returns the declaration block of the rule whose selector list names
// sel exactly, so a descendant rule mentioning the same class is not mistaken for
// the base rule.
func nethubRule(t *testing.T, rules []nethubCSSRule, sel string) string {
	t.Helper()
	for _, rule := range rules {
		for _, item := range rule.items {
			if item == sel {
				return rule.body
			}
		}
	}
	return ""
}

// parseNethubRules splits a stylesheet into rules. Comments are removed first: a
// comment sitting directly above a rule would otherwise be swallowed into that
// rule's selector text and the rule would silently fail to match. An at-rule's
// body is parsed recursively, and the scan resumes after the at-rule's closing
// brace — landing inside the body instead would leave that brace to be read as
// part of the next selector.
func parseNethubRules(css string) []nethubCSSRule {
	var rules []nethubCSSRule
	parseNethubInto(&rules, stripNethubComments(css))
	return rules
}

func parseNethubInto(rules *[]nethubCSSRule, css string) {
	i := 0
	for i < len(css) {
		rel := strings.IndexByte(css[i:], '{')
		if rel < 0 {
			return
		}
		open := i + rel
		head := strings.TrimSpace(css[i:open])
		body := balancedNethubBlock(css[open:])
		if body == "" {
			return
		}
		// Just past this block's closing brace.
		i = open + len(body) + 2
		if strings.HasPrefix(head, "@") {
			// A media query wraps real rules; they apply at top level here, which is
			// what the assertions want (reduced motion is checked separately).
			parseNethubInto(rules, body)
			continue
		}
		var items []string
		for _, item := range strings.Split(head, ",") {
			if s := strings.TrimSpace(item); s != "" {
				items = append(items, s)
			}
		}
		*rules = append(*rules, nethubCSSRule{items: items, body: body, order: len(*rules)})
	}
}

// stripNethubComments removes /* ... */ spans, preserving newlines so line
// numbers in failures still line up with the file.
func stripNethubComments(css string) string {
	var b strings.Builder
	for i := 0; i < len(css); {
		if strings.HasPrefix(css[i:], "/*") {
			end := strings.Index(css[i+2:], "*/")
			if end < 0 {
				break
			}
			for _, r := range css[i : i+2+end+2] {
				if r == '\n' {
					b.WriteByte('\n')
				}
			}
			i += 2 + end + 2
			continue
		}
		b.WriteByte(css[i])
		i++
	}
	return b.String()
}

type nethubCSSRule struct {
	items []string
	body  string
	order int
}

// nethubTransitionProps returns the property names a rule transitions, keyed by
// name.
func nethubTransitionProps(rules []nethubCSSRule, sel string) map[string]bool {
	var body string
	for _, rule := range rules {
		for _, item := range rule.items {
			if item == sel {
				body = rule.body
			}
		}
	}
	value, ok := nethubDeclaration(body, "transition")
	if !ok {
		return nil
	}
	props := map[string]bool{}
	// `transition: color .14s ease, flex-basis .2s ease` — the property is the
	// first token of each comma-separated part.
	for _, part := range strings.Split(value, ",") {
		fields := strings.Fields(part)
		if len(fields) > 0 {
			props[fields[0]] = true
		}
	}
	return props
}

// nethubDeclaration returns the value of a property inside a rule body.
func nethubDeclaration(body, prop string) (string, bool) {
	for _, line := range strings.Split(body, ";") {
		name, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		if strings.TrimSpace(name) == prop {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

// balancedNethubBlock returns the contents of the brace-delimited block that
// starts at the given `{`, counting nesting so a nested rule does not end it
// early.
func balancedNethubBlock(s string) string {
	if s == "" || s[0] != '{' {
		return ""
	}
	depth := 0
	for i, r := range s {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[1:i]
			}
		}
	}
	return ""
}
