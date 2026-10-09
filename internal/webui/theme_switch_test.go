package webui

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestThemeSwitchKeepsStateAndLatestChoice(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	engine := filepath.Join(dir, "theme.js")
	terminal := filepath.Join(dir, "terminal-theme.js")
	languages := filepath.Join(dir, "i18n-catalog.js")
	i18n := filepath.Join(dir, "i18n.js")
	util := filepath.Join(dir, "util.js")
	copies := filepath.Join(dir, "copies.json")
	for filename, asset := range map[string]string{languages: "static/js/i18n-catalog.js", i18n: "static/js/i18n.js", util: "static/js/util.js"} {
		if err := os.WriteFile(filename, []byte(readAssetLF(t, asset)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	messages := map[string]json.RawMessage{}
	configs := map[string]json.RawMessage{}
	for _, id := range []string{"default-light", "default-dark", "cyberpunk"} {
		messages[id] = json.RawMessage(readAssetLF(t, "themes/"+id+"/strings.json"))
		configs[id] = json.RawMessage(readAssetLF(t, "themes/"+id+"/theme.json"))
	}
	encoded, err := json.Marshal(map[string]any{"copies": messages, "configs": configs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copies, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(engine, []byte(readAssetLF(t, "static/js/theme.js")), 0600); err != nil {
		t.Fatal(err)
	}
	view := readAssetLF(t, "static/js/terminal-view.js")
	start := strings.Index(view, "function termcpXtermTheme()")
	if start < 0 {
		t.Fatal("missing terminal theme reader")
	}
	if err := os.WriteFile(terminal, []byte(view[start:]), 0600); err != nil {
		t.Fatal(err)
	}
	script := `
const assert = require('assert');
const fs = require('fs');
const vm = require('vm');
const data = JSON.parse(fs.readFileSync(process.argv[6], 'utf8'));
const copies = data.copies, configs = data.configs;
const delayed = {};
const delayedConfig = {};
const links = [];
const attrs = { 'data-theme': 'default-light' };
const storage = {};
const requests = [];
const callbacks = {};
const styles = {};
const labelAttrs = { 'data-i18n': 'nav.nethub', 'data-i18n-title': 'nav.nethub', 'data-i18n-aria': 'nav.nethub' };
const label = { textContent: '', getAttribute: k => labelAttrs[k], setAttribute: (k, v) => labelAttrs[k] = v };
const root = { getAttribute: k => attrs[k], setAttribute: (k, v) => attrs[k] = v, classList: { add() {}, remove() {}, toggle() {} },
 style: { setProperty: (k, v) => styles[k] = v, removeProperty: k => delete styles[k] } };
function cssValue(k) {
 const base = {
  '--xterm-foreground': attrs['data-theme'] === 'default-dark' ? '#eeeeee' : '#223344',
  '--text-primary': attrs['data-theme'] === 'default-dark' ? '#eeeeee' : '#223344',
  '--bg-card': '#ffffff', '--font-mono': 'Baseline CJK Mono',
  '--terminal-transparent': attrs['data-theme'] === 'cyberpunk' ? 'true' : 'false'
 };
 return (styles[k] || base[k] || '').replace(/var\((--[\w-]+)\)/g, (_, name) => cssValue(name));
}

const head = {
 appendChild(el) { links.push(el); el.parentNode = head; },
 insertBefore(el, next) { const i = links.indexOf(next); links.splice(i < 0 ? links.length : i, 0, el); el.parentNode = head; }
};
function link() { return { remove() { const i = links.indexOf(this); if (i >= 0) links.splice(i, 1); } }; }
const original = link(); original.id = 'termcp-theme'; head.appendChild(original);
/* The trigger's own label (#theme-current). It is the one place the CHOICE is
   visible to the user, so it is asserted rather than assumed. */
const themeCurrent = { textContent: '' };
const history = ['unchanged terminal output'];
const term = { options: {}, buffer: history };
const channels = { a: { term, streamDone: false }, b: { term: { options: {}, buffer: ['archived'] }, streamDone: true } };
const win = { _channels: channels };
/* The system's scheme, shared by every query object for that query the way a real
   matchMedia is kept live by the browser: the engine may ask more than once (boot
   resolution, then a flip), and a fresh object per call would freeze the answer at
   creation. Keyed by query string, because other modules query other media
   features (the glass probe asks about transparency) and one shared flag would
   answer those too. */
const mediaQueries = [];
const mediaAnswers = {};
const context = {
 console, Promise, setTimeout, clearTimeout, navigator: { languages: ['en'] },
 window: { matchMedia: q => { const mq = { query: q, get matches() { return !!mediaAnswers[q]; }, listeners: [], addEventListener(n, fn) { this.listeners.push(fn); } }; mediaQueries.push(mq); return mq; } },
 document: { documentElement: root, head, createElement: link,
  getElementById: id => id === 'theme-current' ? themeCurrent : (links.find(el => el.id === id) || null),
  querySelectorAll: selector => selector.includes('data-i18n') ? [label] : [], addEventListener: (name, fn) => callbacks[name] = fn },
 localStorage: { getItem: k => storage[k], setItem: (k, v) => storage[k] = v },
 allShellWins: () => [win],
 getComputedStyle: () => ({ getPropertyValue: cssValue }),
 fetch: async url => ({ ok: true, text: async () => '', json: async () => {
   const id = decodeURIComponent(url.split('/')[1]);
   if (url.endsWith('/theme.json')) {
    if (delayedConfig[id]) return new Promise(resolve => delayedConfig[id].resolve = resolve);
    return configs[id] || {};
   }
   if (delayed[id]) return new Promise(resolve => delayed[id].resolve = resolve);
   if (id === 'invalid-json') throw Error('invalid JSON');
   return copies[id] || {};
 } }),
 DOMParser: class { parseFromString() { return { querySelectorAll: () => ['cyberpunk/', 'default-light/', 'default-dark/', 'custom%20theme/', '../', 'https://invalid/', 'not-a-folder'].map(href => ({ getAttribute: () => href })) }; } }
};
Object.assign(context.window, context);
const fetchTheme = context.fetch;
context.fetch = (...args) => { requests.push(args[0]); return fetchTheme(...args); };
vm.createContext(context);
vm.runInContext(fs.readFileSync(process.argv[4], 'utf8'), context);
vm.runInContext(fs.readFileSync(process.argv[5], 'utf8'), context);
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), context);
vm.runInContext(fs.readFileSync(process.argv[7], 'utf8'), context);
vm.runInContext(fs.readFileSync(process.argv[3], 'utf8'), context);
(async () => {
 assert.equal(label.textContent, 'Connections');
 let dynamicLabel;
 context.onLangChange(() => { dynamicLabel = context.t('nethub.add'); });
 const ids = await context.discoverThemes();
 assert.deepEqual(Array.from(ids), ['default-light', 'default-dark', 'cyberpunk', 'custom theme']);
 assert.equal(context.themeLabel('constructor'), 'constructor');
 assert.equal(context.themeLabel('toString'), 'toString');
 const first = context.applyTheme('default-dark');
 const stale = links[1], staleLoad = stale.onload;
 assert.equal(attrs['data-theme'], 'default-light');
 assert.equal(links[0], original);
 const second = context.applyTheme('cyberpunk');
 assert.equal(await first, false);
 staleLoad();
 assert.equal(attrs['data-theme'], 'default-light');
 links[1].onload();
 assert.equal(await second, true);
 assert.equal(attrs['data-theme'], 'cyberpunk');
 assert.equal(storage['termcp.theme'], 'cyberpunk');
 assert.equal(label.textContent, 'NetHub');
 assert.equal(label.title, 'NetHub');
 assert.equal(labelAttrs['aria-label'], 'NetHub');
 assert.equal(dynamicLabel, 'Add access');
 context.termcpApplyLang('zh-Hans');
 assert.equal(label.textContent, '网络接入仓');
 assert.equal(links.length, 1);
 assert.equal(term.buffer, history);
 assert.equal(win._channels, channels);
 assert.equal(channels.b.streamDone, true);
 assert.equal(channels.b.term.options.theme.foreground, '#223344');
 const active = links[0];
 const missing = context.applyTheme('missing');
 links[1].onerror();
 await assert.rejects(missing);
 assert.equal(links[0], active);
 assert.equal(attrs['data-theme'], 'cyberpunk');
 assert.equal(label.textContent, '网络接入仓');
 context.localStorage.setItem = () => { throw Error('storage blocked'); };
 const final = context.applyTheme('default-dark');
 links[1].onload();
 assert.equal(await final, true);
 assert.equal(term.options.theme.foreground, '#eeeeee');
 assert.equal(label.textContent, '连接');
 assert.equal(dynamicLabel, '添加主机');
 assert.equal(context.t('nethub.rail.more', { count: 2 }), '另外 2 个主机');
 assert.equal(context.t('common.cancel'), '取消');
 assert.equal(attrs.lang, 'zh-Hans');
 assert.equal(storage['termcp.lang'], 'zh-Hans');
 assert.equal(term.buffer, history);
 await assert.rejects(context.applyTheme('../bad'));
 await assert.rejects(context.applyTheme('bad\x7f'));
 assert.equal(links.length, 1);
 // CSS and copy commit together. A late response from a cancelled choice
 // cannot rewrite the title or revive the old theme.
 delayed.cyberpunk = {};
 const late = context.applyTheme('cyberpunk');
 links[1].onload();
 await new Promise(resolve => setTimeout(resolve, 0));
 assert.equal(label.textContent, '连接');
 assert.equal(attrs['data-theme'], 'default-dark');
 const latest = context.applyTheme('default-light');
 links[1].onload();
 assert.equal(await latest, true);
 assert.equal(await late, false);
 delayed.cyberpunk.resolve(copies.cyberpunk);
 await new Promise(resolve => setTimeout(resolve, 0));
 assert.equal(attrs['data-theme'], 'default-light');
 assert.equal(label.textContent, '连接');
 delete delayed.cyberpunk;
 // The optional JSON may be absent or malformed; stale overrides must clear.
 const malformed = context.applyTheme('invalid-json');
 links[1].onload();
 assert.equal(await malformed, true);
 assert.equal(label.textContent, '连接');
 const plain = context.applyTheme('no-strings');
 links[1].onload();
 assert.equal(await plain, true);
 assert.equal(label.textContent, '连接');
 const safe = context.normalizeThemeStrings({ 'zh-Hans': { 'nav.nethub': '<b>自定义连接</b>', 'nethub.add': 9 } });
 context.setThemeStrings(safe);
 assert.equal(label.textContent, '<b>自定义连接</b>');
 assert.equal(dynamicLabel, '添加主机');
 context.termcpApplyLang('zh-Hant');
 assert.equal(label.textContent, '連線');
 assert.equal(term.buffer, history);
 assert.equal(term.options.allowTransparency, false);
 assert.equal(term.options.theme.background, '#ffffff');
 // Terminal options and window controls are committed with the stylesheet,
 // including already hidden channels. Font fallback remains the shared stack.
 configs.custom = { terminal: { fontFamily: 'Theme Mono, var(--font-mono)', fontSize: 20, fontWeight: 500,
  fontWeightBold: 700, lineHeight: 1.6, letterSpacing: 2, transparent: true, opacity: .6,
  background: '#123456', foreground: '#ffeedd', cursor: '#aabbcc', selectionBackground: '#112233',
  colors: { brightRed: '#ff7777' }, window: { background: '#123456', borderColor: '#778899', blur: 8 } } };
 const custom = context.applyTheme('custom');
 links[1].onload();
 assert.equal(await custom, true);
 assert.equal(term.options.fontFamily, 'Theme Mono, Baseline CJK Mono');
 assert.equal(term.options.fontSize, 20);
 assert.equal(term.options.fontWeight, 500);
 assert.equal(term.options.lineHeight, 1.6);
 assert.equal(term.options.letterSpacing, 2);
 assert.equal(term.options.allowTransparency, true);
 assert.equal(term.options.theme.foreground, '#ffeedd');
 assert.equal(term.options.theme.background, 'rgba(0, 0, 0, 0)');
 assert.equal(term.options.theme.brightRed, '#ff7777');
 assert.equal(channels.b.term.options.fontSize, 20);
 assert.equal(attrs['data-terminal-transparent'], 'true');
 assert.equal(styles['--terminal-window-blur'], '8px');
 assert.equal(styles['--terminal-background-opacity'], '0.6');
 assert.equal(term.buffer, history);
 const opaque = context.applyTheme('default-light');
 links[1].onload();
 assert.equal(await opaque, true);
 assert.equal(term.options.allowTransparency, false);
 assert.equal(term.options.fontSize, 13);
 assert.equal(term.options.fontFamily, 'Baseline CJK Mono');
 assert.equal(term.options.theme.background, '#ffffff');
 assert.equal(styles['--xterm-bright-red'], undefined);
 assert.equal(styles['--terminal-body-background'], undefined);
 assert.equal(attrs['data-terminal-transparent'], 'false');
 const invalid = context.normalizeThemeConfig({ terminal: { fontSize: -9, opacity: 2, lineHeight: 0, transparent: 'yes', foreground: 9 } });
 assert.equal(invalid.fontSize, undefined);
 assert.equal(invalid.opacity, undefined);
 assert.equal(invalid.transparent, undefined);
 delayedConfig.custom = {};
 const oldConfig = context.applyTheme('custom');
 links[1].onload();
 await new Promise(resolve => setTimeout(resolve, 0));
 assert.equal(term.options.fontSize, 13);
 const lastConfig = context.applyTheme('default-dark');
 links[1].onload();
 assert.equal(await lastConfig, true);
 assert.equal(await oldConfig, false);
 delayedConfig.custom.resolve(configs.custom);
 await new Promise(resolve => setTimeout(resolve, 0));
 assert.equal(term.options.fontSize, 13);
 assert.equal(term.options.allowTransparency, false);
 assert.equal(attrs['data-theme'], 'default-dark');
 // A packaged font is measured only after loading. Until then both new and
 // existing terminals use the installed stack, and an old load is discarded.
 delete delayedConfig.custom;
 let webFontReady = false, finishFont;
 context.document.fonts = {
  check: font => !font.includes('Theme Mono') || webFontReady,
  load: () => new Promise(resolve => finishFont = resolve)
 };
 const loadingFont = context.applyTheme('custom');
 links[1].onload();
 assert.equal(await loadingFont, true);
 assert.equal(term.options.fontFamily, context.TERMCP_MONO_FALLBACK);
 webFontReady = true;
 finishFont([]);
 await new Promise(resolve => setTimeout(resolve, 0));
 assert.equal(term.options.fontFamily, 'Theme Mono, Baseline CJK Mono');
 webFontReady = false;
 const stillLoading = context.applyTheme('custom');
 links[1].onload();
 assert.equal(await stillLoading, true);
 const afterFont = context.applyTheme('default-light');
 links[1].onload();
 assert.equal(await afterFont, true);
 webFontReady = true;
 finishFont([]);
 await new Promise(resolve => setTimeout(resolve, 0));
 assert.equal(term.options.fontFamily, 'Baseline CJK Mono');
 assert.equal(term.options.fontSize, 13);
 assert.equal(term.buffer, history);
 // ---- auto: the choice follows the OS, and a live flip is honoured ----
 // Storage was blocked above to prove a failed write cannot break a switch; this
 // section reads back what auto persists, so it needs the write working again.
 context.localStorage.setItem = (k, v) => storage[k] = v;
 // The menu offers auto even though no such folder exists on disk.
 const choices = await context.discoverThemes();
 assert.ok(choices.indexOf('auto') < 0, 'auto is not a theme folder and must not come from discovery');
 // Picking auto loads the theme the SYSTEM asks for, and stores the word "auto"
 // rather than the resolved id — storing the id would turn "follow the system"
 // into "stay exactly as you are".
 mediaAnswers['(prefers-color-scheme: dark)'] = true;
 const autoDark = context.applyTheme('auto');
 links[1].onload();
 assert.equal(await autoDark, true);
 assert.equal(attrs['data-theme'], 'default-dark');
 assert.equal(new URL(links[0].href, 'https://app.example/').pathname, '/themes/default-dark/theme.css');
 assert.equal(storage['termcp.theme'], 'auto');
 // The trigger keeps saying Auto while the OS decides; naming the resolved theme
 // there would read as if auto had been overridden by hand.
 assert.equal(context.themeLabel('auto'), '自動');
 // ...and the trigger shows that, not the theme the OS happened to resolve to:
 // "Auto" is the choice, and naming the resolved theme there would read as if
 // auto had been overridden by hand.
 assert.equal(themeCurrent.textContent, '自動');
 // The OS flips (a sunset switch, a scheduled dark mode): the page must follow
 // without a reload. This is the half that a boot-time-only resolution misses.
 // Every query is for the scheme preference, and the engine registered exactly
 // one listener on the query it will keep consulting — resolving the choice may
 // ask again, but only the listener is what makes a live flip work.
 const scheme = mediaQueries.filter(mq => mq.query === '(prefers-color-scheme: dark)');
 assert.ok(scheme.length >= 1, 'the engine must ask the system how it is themed');
 const watched = scheme.filter(mq => mq.listeners.length > 0);
 assert.equal(watched.length, 1);
 assert.equal(watched[0].listeners.length, 1);
 mediaAnswers['(prefers-color-scheme: dark)'] = false;
 watched[0].listeners[0]();
 await new Promise(resolve => setTimeout(resolve, 0));
 links[1].onload();
 await new Promise(resolve => setTimeout(resolve, 0));
 assert.equal(attrs['data-theme'], 'default-light');
 assert.equal(storage['termcp.theme'], 'auto');
 // An explicit choice must NOT be overridden by the OS: the listener is a
 // follow-the-system feature, not a second authority over the user's pick.
 const pinned = context.applyTheme('cyberpunk');
 links[1].onload();
 assert.equal(await pinned, true);
 mediaAnswers['(prefers-color-scheme: dark)'] = true;
 watched[0].listeners[0]();
 await new Promise(resolve => setTimeout(resolve, 0));
 // A flip only does something by starting a stylesheet load, so "nothing was
 // loaded" IS the assertion — waiting on a load event first would make this pass
 // for the wrong reason (there is no new link to load). The pinned theme and the
 // stored choice are checked too, so the intent survives a refactor that reaches
 // the same effect another way.
 assert.equal(links.length, 1, 'a pinned choice must not start a second stylesheet load when the OS flips');
 assert.equal(attrs['data-theme'], 'cyberpunk');
 assert.equal(storage['termcp.theme'], 'cyberpunk');

 // The same runtime works in the workbench and docs under a proxy mount.
 for (const page of ['https://app.example/', 'https://app.example/termcp/index.html', 'https://app.example/termcp/api.html']) {
  const prefix = new URL('.', page).pathname;
  for (const url of requests.concat(links[0].href, context.themeAssetURL('icons/terminal-shell.svg'))) {
   assert.ok(new URL(url, page).pathname.startsWith(prefix + 'themes/'), 'theme resource escaped mount: ' + url);
  }
 }
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	path := filepath.Join(dir, "switch-test.js")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, engine, terminal, languages, i18n, copies, util).CombinedOutput(); err != nil {
		t.Fatalf("theme switch regression: %v\n%s", err, out)
	}
}

func TestTerminalFitUsesModernThemeFontOptions(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	source := readAssetLF(t, "static/js/ui-socket.js")
	start := strings.Index(source, "function fitShellTerminal(")
	if start < 0 {
		t.Fatal("missing terminal fit function")
	}
	end := strings.Index(source[start:], "/** First wheel")
	if end < 0 {
		t.Fatal("missing end of terminal fit function")
	}
	dir := t.TempDir()
	engine := filepath.Join(dir, "fit.js")
	if err := os.WriteFile(engine, []byte(source[start:start+end]), 0600); err != nil {
		t.Fatal(err)
	}
	script := `
const assert = require('assert'), fs = require('fs'), vm = require('vm');
let measured, resizeCount = 0, remoteCount = 0;
const context = {
 document: {
  createElement: () => ({ style: {}, get offsetWidth() { return Number(this.style.cssText.match(/([\d.]+)px/)[1]) / 2; } }),
  body: { appendChild(el) { measured = el; }, removeChild() {} }
 },
 getComputedStyle: () => ({ getPropertyValue: () => '0' }),
 termcpMonoFontFamily: () => { throw Error('fit ignored the selected font'); },
 scheduleShellRemoteResize: () => remoteCount++
};
vm.createContext(context);
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), context);
const term = {
 options: { fontSize: 20, fontFamily: 'Theme Mono', fontWeight: 500, lineHeight: 1.5, letterSpacing: 2 },
 cols: 80, rows: 24,
 element: { querySelector: selector => selector === '.xterm-viewport' ? { clientWidth: 240 } : null },
 resize(cols, rows) { resizeCount++; this.cols = cols; this.rows = rows; }
};
const container = { isConnected: true, clientWidth: 240, clientHeight: 200 };
assert.equal(context.fitShellTerminal(term, container, {}, true), true);
assert.equal(term.cols, 20);
assert.equal(term.rows, 4);
assert.ok(measured.style.cssText.includes('font:500 20px Theme Mono'));
assert.equal(resizeCount, 1);
assert.equal(remoteCount, 1);
assert.equal(context.fitShellTerminal(term, container, {}, false), false);
assert.equal(resizeCount, 1);
`
	path := filepath.Join(dir, "fit-test.js")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, engine).CombinedOutput(); err != nil {
		t.Fatalf("terminal fit regression: %v\n%s", err, out)
	}
}

func TestThemeBootRestoresAndFallsBackUnderDeploymentPath(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	dir := t.TempDir()
	boot := filepath.Join(dir, "boot.js")
	if err := os.WriteFile(boot, []byte(readAssetLF(t, "static/theme-boot.js")), 0600); err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{}
	for _, page := range []string{"index.html", "api.html"} {
		html := readAssetLF(t, page)
		link := between(t, html, `<link id="termcp-theme"`, ">")
		_, tail, found := strings.Cut(link, `href="`)
		if !found {
			t.Fatalf("%s lacks a theme stylesheet href", page)
		}
		pages[page], _, _ = strings.Cut(tail, `"`)
	}
	encoded, err := json.Marshal(pages)
	if err != nil {
		t.Fatal(err)
	}
	references := filepath.Join(dir, "pages.json")
	if err := os.WriteFile(references, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	script := `
const assert = require('assert'), fs = require('fs'), vm = require('vm');
const source = fs.readFileSync(process.argv[2], 'utf8');
const pages = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));
for (const prefix of ['/', '/tools/termcp/']) {
 for (const page of ['index.html', 'api.html']) {
  const base = 'https://app.example' + prefix + page;
  const link = { href: pages[page] }, attrs = {};
  assert.equal(new URL(link.href, base).pathname, prefix + 'themes/default-light/theme.css');
  const context = {
   document: { documentElement: { setAttribute: (k, v) => attrs[k] = v, classList: { add() {}, remove() {} } },
    getElementById: () => link, dispatchEvent() {} },
   localStorage: { getItem: () => 'saved theme' },
   setTimeout: () => 1, clearTimeout() {}, CustomEvent: class {}
  };
  vm.createContext(context);
  vm.runInContext(source, context);
  assert.equal(attrs['data-theme'], 'saved theme');
  assert.equal(new URL(link.href, base).pathname, prefix + 'themes/saved%20theme/theme.css');
  link.onerror();
  assert.equal(attrs['data-theme'], 'default-light');
  assert.equal(new URL(link.href, base).pathname, prefix + 'themes/default-light/theme.css');
 }
}
`
	path := filepath.Join(dir, "boot-test.js")
	if err := os.WriteFile(path, []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, path, boot, references).CombinedOutput(); err != nil {
		t.Fatalf("theme boot regression: %v\n%s", err, out)
	}
}
