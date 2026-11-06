package webui

import (
	"strings"
	"testing"
)

// TestTheArrangementKeyIsAMenuNotANativeSelect pins the shape of the dock's
// arrangement control.
//
// A <select> was wrong here for two reasons, and both are visible to the reader
// rather than internal: the browser drops the operating system's own list (a
// white slab with system chrome) into a bar built from the terminal's palette,
// so it is the one control in the page that does not look like the page; and it
// cannot show the current choice while closed without a label as wide as the
// widest one, in a bar with no width to spare.
//
// The replacement is markup, and it borrows the header's language menu — the
// same .lang-menu panel and .lang-opt rows — so the two menus cannot drift into
// two different looks. Only the trigger is dock-shaped. This is a source
// assertion because the property is about which elements exist; the behaviour
// (choose one, the grid changes) is covered by the arrangement tests.
func TestTheArrangementKeyIsAMenuNotANativeSelect(t *testing.T) {
	js := readAssetLF(t, "static/js/shell-windows.js")
	if strings.Contains(js, "createElement('select')") {
		t.Error("the arrangement key is a native <select> again: its popup is the OS's, not the page's")
	}
	for _, want := range []string{
		// The trigger, its caret and its label.
		"'session-tabbar-strategy'",
		"session-tabbar-caret",
		"session-tabbar-strategy-label",
		// The panel reuses the header menu's rules rather than its own look.
		"'lang-menu session-tabbar-menu hidden'",
		`class="lang-opt"`,
		// Open state and the two ways out, so the menu is not a trap.
		"function setPaneStrategyOpen(",
		"e.key !== 'Escape'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the arrangement menu has lost %s", want)
		}
	}

	// The closed trigger and the checked row are drawn from ONE variable, so the
	// menu cannot claim an arrangement the grid is not using.
	if !strings.Contains(js, "function paintPaneStrategy(") {
		t.Error("nothing paints the trigger and the checked row from _paneStrategy")
	}
	if n := strings.Count(js, "_paneStrategy = "); n < 2 {
		t.Errorf("_paneStrategy is assigned in %d places; the menu and the stored value must both feed it", n)
	}

	// The panel opens UPWARD: the dock is the page's last row, so a list dropping
	// below the trigger would open off the bottom of the window.
	css := readAssetLF(t, "static/css/terminal.css")
	if !strings.Contains(css, ".session-tabbar-menu {") {
		t.Fatal("the arrangement menu has no panel rule")
	}
	panel := css[strings.Index(css, ".session-tabbar-menu {"):]
	panel = panel[:strings.Index(panel, "}")]
	if !strings.Contains(panel, "bottom: calc(100% + 4px);") || !strings.Contains(panel, "top: auto;") {
		t.Errorf("the arrangement menu must open upward from the dock, got:%s", panel)
	}
}
