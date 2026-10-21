package webui

import (
	"strings"
	"testing"
)

// TestTabCarriesTheSessionActionsTheCardUsedTo pins that the per-session actions
// survive the removal of the session plate.
//
// The plate listed every running session as a card, and the card carried the
// things that are not "switch to it": the approval gate, the rename, the copy of
// the session URL. The plate is gone — the dock's tabs are the only listing — so
// a tab has to carry them or they are unreachable. That is a functional
// regression, not a styling one, which is why it is pinned here rather than left
// to a reviewer to notice.
func TestTabCarriesTheSessionActionsTheCardUsedTo(t *testing.T) {
	js := readAssetLF(t, "static/js/shell-windows.js")
	for _, want := range []string{
		"function openSessionTabMenu(",
		"function renameSessionById(",
		"function copySessionUrl(",
		"function sessionForTabWindow(",
		"addEventListener('contextmenu'",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the tab bar has lost %s — the card's actions would be unreachable", want)
		}
	}

	// The three verbs themselves, each present in the menu builder. A menu that
	// lost one would still open, and the missing action would look like it never
	// existed rather than like a bug.
	menu := between(t, js, "function openSessionTabMenu(", "\nfunction closeSessionTabMenu(")
	if menu == "" {
		t.Fatal("openSessionTabMenu is gone")
	}
	for _, want := range []string{
		"toggleSessionApproval(",
		"renameSessionById(",
		"copySessionUrl(",
	} {
		if !strings.Contains(menu, want) {
			t.Errorf("the tab menu must offer %s", want)
		}
	}
}

// TestTabMenuTrustsTheSnapshotNotTheWindowID pins the lookup.
//
// A shell window's `_sid` is not always a session id: a connecting window has
// none yet, and a status card carries the EXTENSION's name in the same slot.
// Resolving it as a session would offer "rename" and "gate" for something that
// is not a session, and the PATCH would 404. So the id is looked up in the last
// snapshot, and a window that matches nothing gets no menu at all.
func TestTabMenuTrustsTheSnapshotNotTheWindowID(t *testing.T) {
	js := readAssetLF(t, "static/js/shell-windows.js")
	fn := between(t, js, "function sessionForTabWindow(", "\n}\n")
	if fn == "" {
		t.Fatal("sessionForTabWindow is gone")
	}
	if !strings.Contains(fn, "_lastSessionsSnapshot") {
		t.Error("the tab's session must be resolved through the snapshot, not the window's _sid alone")
	}
	if !strings.Contains(fn, "s.id === sid") && !strings.Contains(fn, ".id === sid") {
		t.Error("the lookup must match the snapshot entry by id")
	}

	// An unmatched window must produce no menu: an empty menu is a worse answer
	// than none, because it claims there is nothing to do rather than that there
	// is nothing to do it TO.
	menu := between(t, js, "function openSessionTabMenu(", "\nfunction closeSessionTabMenu(")
	if !strings.Contains(menu, "if (!s) return;") {
		t.Error("a window with no session behind it must not open a menu")
	}
}

// TestTabMenuIsOneReusedElement pins the lifecycle.
//
// The menu is built on demand and torn down on open, on Escape, on a click
// outside, and on a resize. A per-tab menu would be one stale closure per window
// on a page that opens and closes windows all day, and a menu anchored to a tab
// that a resize moved would sit over the wrong window.
func TestTabMenuIsOneReusedElement(t *testing.T) {
	js := readAssetLF(t, "static/js/shell-windows.js")
	menu := between(t, js, "function openSessionTabMenu(", "\nfunction closeSessionTabMenu(")
	if !strings.Contains(menu, "closeSessionTabMenu()") {
		t.Error("opening the menu must tear down any previous one")
	}
	closeFn := between(t, js, "function closeSessionTabMenu(", "\n}\n")
	if !strings.Contains(closeFn, "sess-tab-menu") || !strings.Contains(closeFn, ".remove()") {
		t.Errorf("closing must remove the menu element; got %q", closeFn)
	}
	// The three dismissals, each on the document/window rather than per menu.
	for _, want := range []string{
		"closeSessionTabMenu();",
		"e.key === 'Escape'",
		"window.addEventListener('resize', closeSessionTabMenu)",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the menu has lost a dismissal path: %s is missing", want)
		}
	}
	// Placed after being attached, so it has a measured size to fit inside the
	// viewport; positioning before append would measure zero and never flip.
	if !strings.Contains(menu, "appendChild(menu)") ||
		strings.Index(menu, "appendChild(menu)") > strings.Index(menu, "getBoundingClientRect") {
		t.Error("the menu must be in the DOM before it is measured and placed")
	}
}
