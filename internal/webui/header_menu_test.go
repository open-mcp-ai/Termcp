package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnlyOneHeaderMenuIsOpenAtATime pins the rule that keeps top-level header
// menus from covering each other, while allowing theme/language/clients to live
// inside the Settings menu without closing that parent.
//
// The theme switch, the language menu and the clients panel are all children of
// the header and share one rung of the z-ladder (--z-dropdown), so which of them
// paints on top is DOM order — and the clients panel is the LAST of the three.
// It was also the only one that never closed when the reader looked away, so it
// could sit open behind everything and win against any menu opened afterwards:
// the reader pressed the language trigger and its menu rendered UNDERNEATH a
// panel opened minutes earlier.
//
// The fix is not a higher rung for the newer menu; it is that two header menus
// open at once is a state worth nothing, so opening one closes the rest. This
// drives that function against a DOM stub for every pair.
func TestOnlyOneHeaderMenuIsOpenAtATime(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the header menus")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "menus.js")
	if err := os.WriteFile(script, []byte(headerMenuHarness), 0o600); err != nil {
		t.Fatal(err)
	}
	module := filepath.Join(dir, "i18n.js")
	if err := os.WriteFile(module, []byte(readAssetLF(t, "static/js/i18n.js")), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, script, module).CombinedOutput()
	if err != nil {
		t.Fatalf("the header menu check failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "HEADER MENUS OK") {
		t.Fatalf("the header menu harness did not report success:\n%s", out)
	}

	// Every opener has to call it, or the rule only holds for the pair that did.
	// i18n covers the language menu, theme the switch, clients.js the panel.
	for _, f := range []string{"static/js/theme.js", "static/js/clients.js"} {
		if !strings.Contains(readAssetLF(t, f), "closeHeaderMenus(") {
			t.Errorf("%s opens a header menu without closing the others: a menu opened "+
				"while another is up renders underneath it", f)
		}
	}
	i18n := readAssetLF(t, "static/js/i18n.js")
	if !strings.Contains(i18n, "active.closest('#' + id)") {
		t.Error("a nested clients/language menu must not close its Settings parent")
	}
}

// headerMenuHarness drives closeHeaderMenus against a DOM stub. The three menus
// are stubbed as the objects the real code touches: a <details> for the theme
// switch and the clients panel, a hidden-class span for the language menu.
const headerMenuHarness = `
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');

function classListOf(initial) {
  const on = new Set(initial || []);
  return {
    toggle(name, want) { want ? on.add(name) : on.delete(name); },
    contains: (name) => on.has(name),
    add: (name) => on.add(name), remove: (name) => on.delete(name)
  };
}
function el(id) {
  return {
    id: id || '', open: false, hidden: false, textContent: '', classList: classListOf(),
    setAttribute() {}, getAttribute() { return null; }, addEventListener() {}, style: {}
  };
}
const els = {
  'lang-menu': el('lang-menu'), 'lang-btn': el('lang-btn'),
  'theme-switch': el('theme-switch'), 'clients-menu': el('clients-menu')
};
els['lang-menu'].classList.toggle('hidden', true);
const document = {
  documentElement: { setAttribute() {}, getAttribute() { return null; } },
  body: { classList: classListOf() },
  getElementById: (id) => els[id] || null,
  querySelectorAll: () => [], querySelector: () => null,
  addEventListener() {}, readyState: 'complete'
};
const ctx = {
  console, document, JSON, Object, Array, String, Number,
  navigator: { languages: ['en'] },
  localStorage: { getItem: () => null, setItem() {} }
};
vm.createContext(ctx);
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), ctx);

const langOpen = () => !els['lang-menu'].classList.contains('hidden');
const openAll = () => {
  els['theme-switch'].open = true;
  els['clients-menu'].open = true;
  els['lang-menu'].classList.toggle('hidden', false);
};

/* Each menu opening must close the other two and leave itself up. */
openAll();
ctx.closeHeaderMenus('clients-menu');
assert.strictEqual(els['clients-menu'].open, true, 'the menu just opened must stay open');
assert.strictEqual(els['theme-switch'].open, false, 'opening the clients panel must close the theme menu');
assert.strictEqual(langOpen(), false, 'opening the clients panel must close the language menu');

openAll();
ctx.closeHeaderMenus('theme-switch');
assert.strictEqual(els['theme-switch'].open, true, 'the menu just opened must stay open');
assert.strictEqual(els['clients-menu'].open, false, 'opening the theme menu must close the clients panel');
assert.strictEqual(langOpen(), false, 'opening the theme menu must close the language menu');

/* The language menu is opened through setLangMenuOpen, which closes the two
   <details> itself — the assertion is on the path the trigger actually takes. */
els['theme-switch'].open = true;
els['clients-menu'].open = true;
ctx.setLangMenuOpen(true);
assert.strictEqual(langOpen(), true, 'the language menu must open');
assert.strictEqual(els['theme-switch'].open, false, 'opening the language menu must close the theme menu');
assert.strictEqual(els['clients-menu'].open, false, 'opening the language menu must close the clients panel');

console.log('HEADER MENUS OK');
`
