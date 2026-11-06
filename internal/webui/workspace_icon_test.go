package webui

import (
	"strings"
	"testing"
)

// The host card's quick-start control is a launch action, not a second terminal
// icon. The terminal-window glyph was visually indistinguishable from the entry
// itself and made the two adjacent controls read as duplicate actions.
func TestConnectionQuickButtonKeepsPlayGlyph(t *testing.T) {
	utilJS := readAssetLF(t, "static/js/util.js")
	const playGlyph = `M5 3.5a.75.75 0 011.12-.65l7.5 4.5a.75.75 0 010 1.3l-7.5 4.5a.75.75 0 01-1.12-.65v-9z`
	if !strings.Contains(utilJS, playGlyph) {
		t.Fatal("connection quick-start button no longer uses the play glyph")
	}
	if strings.Contains(utilJS, `M4.5 6.5l2 1.5-2 1.5M8.5 9.5h3`) {
		t.Fatal("connection quick-start button regressed to the terminal-window glyph")
	}
}
