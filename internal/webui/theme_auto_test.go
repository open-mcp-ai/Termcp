package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Auto follows the system's light/dark preference instead of pinning one theme.
//
// Two halves have to agree for that to mean anything, and they run at different
// times: theme-boot.js resolves the stored choice in <head>, before the first
// paint (a wrong theme there is a flash of the wrong palette on every load), and
// theme.js owns the choice afterwards — including the case that makes "auto"
// different from "light": the OS flipping while the page is open. Either half
// alone leaves a page that is correct on reload and wrong in between, which is
// exactly the state a user would call "auto doesn't work".
//
// The choice and the resolved theme are deliberately different values, so most of
// this pins that they do not collapse into one: 'auto' must reach localStorage and
// the menu label, while a concrete id must reach the stylesheet, data-theme and the
// theme folder.
func TestThemeAutoFollowsTheSystemScheme(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	boot := filepath.Join(dir, "theme-boot.js")
	if err := os.WriteFile(boot, []byte(readAssetLF(t, "static/theme-boot.js")), 0600); err != nil {
		t.Fatal(err)
	}
	script := `
const assert = require('assert'), fs = require('fs'), vm = require('vm');
const source = fs.readFileSync(process.argv[2], 'utf8');

/* Runs the real boot script against one (stored choice, system scheme) pair and
   reports what it decided. The boot script is what the browser runs in <head>,
   so this is the resolution the first paint depends on. */
function boot(stored, dark, opts) {
  opts = opts || {};
  const attrs = {};
  let mediaCalls = [];
  const link = { href: 'themes/default-light/theme.css' };
  const context = {
    document: { documentElement: { setAttribute: (k, v) => attrs[k] = v, classList: { add() {}, remove() {} } },
      getElementById: () => link, dispatchEvent() {} },
    localStorage: { getItem: () => stored },
    window: { matchMedia: q => { mediaCalls.push(q); return { matches: dark }; } },
    setTimeout: () => 1, clearTimeout() {}, CustomEvent: class {}
  };
  if (opts.noMatchMedia) delete context.window.matchMedia;
  if (opts.storageThrows) context.localStorage.getItem = () => { throw Error('blocked'); };
  vm.createContext(context);
  vm.runInContext(source, context);
  return { theme: attrs['data-theme'], href: link.href, mediaCalls };
}

/* The first paint's theme for each combination. A fresh browser (nothing stored)
   must follow the OS: the <link> in the markup is hardcoded to the light theme,
   so defaulting to "no choice = light" would leave every dark-mode machine on a
   light page until the user went and picked one. */
assert.equal(boot(null, true).theme, 'default-dark');
assert.equal(boot(null, false).theme, 'default-light');
assert.equal(boot('auto', true).theme, 'default-dark');
assert.equal(boot('auto', false).theme, 'default-light');
/* The resolved theme, not the word "auto", is what the stylesheet URL may carry:
   there is no themes/auto/ folder, so a link built from the choice would 404 and
   the page would fall back to unstyled. */
assert.equal(boot('auto', true).href, 'themes/default-dark/theme.css');
assert.equal(boot('auto', false).href, 'themes/default-light/theme.css');
assert.equal(boot(null, true).href, 'themes/default-dark/theme.css');
/* An explicit choice wins over the OS in both directions, or "follow the system"
   would be the only thing the picker could ever do. */
assert.equal(boot('default-light', true).theme, 'default-light');
assert.equal(boot('default-dark', false).theme, 'default-dark');
assert.equal(boot('cyberpunk', false).theme, 'cyberpunk');
assert.equal(boot('cyberpunk', false).href, 'themes/cyberpunk/theme.css');
assert.equal(boot('custom theme', true).theme, 'custom theme');
/* A browser without matchMedia, or with storage blocked, still has to boot
   somewhere instead of throwing before the body is parsed. */
assert.equal(boot('auto', true, { noMatchMedia: true }).theme, 'default-light');
assert.equal(boot(null, true, { storageThrows: true }).theme, 'default-dark');
/* A hostile stored value is rejected at boot exactly as it is by the picker. */
assert.equal(boot('../../etc', false).theme, 'default-light');
assert.equal(boot('bad\x7f', false).theme, 'default-light');
/* The preference is only asked for where it is used: a concrete choice must not
   consult the OS at all. */
assert.equal(boot('cyberpunk', true).mediaCalls.length, 0);
assert.equal(boot('auto', true).mediaCalls.length, 1);
console.log('boot: ok');
`
	path := filepath.Join(dir, "auto-boot-test.js")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, boot).CombinedOutput(); err != nil {
		t.Fatalf("theme auto boot regression: %v\n%s", err, out)
	}
}
