package webui

import (
	"strings"
	"testing"
)

// TestCloseButtonHandlerIsExported ensures the close-button binder exists at
// top level so opening any terminal window can call it without throwing.
func TestCloseButtonHandlerIsExported(t *testing.T) {
	src := readAssetLF(t, "static/js/shell-windows.js")
	if !strings.Contains(src, "\nfunction bindShellWindowCloseButton(") {
		t.Fatal("shell-windows.js does not define bindShellWindowCloseButton at top level")
	}
	if !strings.Contains(src, "\nfunction closeSessionWindowFromUI(") {
		t.Fatal("shell-windows.js does not define closeSessionWindowFromUI at top level")
	}
}
