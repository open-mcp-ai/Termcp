/* termcp Web UI i18n engine.
 *
 * Plain script, no IIFE, no let/const: like every other module here, the
 * declarations below land in the shared global scope so the other modules can
 * call t()/applyI18n()/onLangChange(). Loading this module performs the first
 * applyI18n() immediately (the <script> sits at the end of <body>, so the
 * static markup already exists), which is what keeps the first paint in the
 * right language: no fetch, no DOMContentLoaded hop, no English flash.
 *
 * Language resolution is fixed by design:
 *   1. localStorage['termcp.lang'] in {auto, en, zh-Hans, zh-Hant}; default auto
 *   2. auto -> first match walking navigator.languages IN ORDER
 *        /^zh-(hant|tw|hk|mo)/i -> zh-Hant, /^zh/i -> zh-Hans, /^en/i -> en
 *   3. no match -> en
 * navigator.languages (plural) is required: a Hong Kong or Taiwan browser
 * often leads with en-US, so reading only navigator.language would label the
 * user English.
 */
var _termcpLang;
var _langReappliers = [];
var I18N_LANGS = ['en', 'zh-Hans', 'zh-Hant'];
var I18N_AUTO = 'auto';
var I18N_STORAGE_KEY = 'termcp.lang';

/** True for a value the selector can hold: the auto sentinel or a concrete code. */
function isLangChoice(value) {
  if (value === I18N_AUTO) return true;
  for (var i = 0; i < I18N_LANGS.length; i++) {
    if (I18N_LANGS[i] === value) return true;
  }
  return false;
}

/** The stored preference, or "auto" when it is missing/unreadable (private mode throws). */
function storedLangChoice() {
  var v = null;
  try { v = localStorage.getItem(I18N_STORAGE_KEY); } catch (e) {}
  return isLangChoice(v) ? v : I18N_AUTO;
}

/** Resolve a stored choice to a concrete language code. Read-only: never writes. */
function resolveLang(stored) {
  if (stored && stored !== I18N_AUTO && isLangChoice(stored)) return stored;
  var prefs = (navigator.languages && navigator.languages.length)
    ? navigator.languages
    : [navigator.language || ''];
  for (var i = 0; i < prefs.length; i++) {
    var tag = String(prefs[i] || '');
    if (/^zh-(hant|tw|hk|mo)/i.test(tag)) return 'zh-Hant';
    if (/^zh/i.test(tag)) return 'zh-Hans';
    if (/^en/i.test(tag)) return 'en';
  }
  return 'en';
}

/** One catalog entry, or undefined when the language or the key is absent. */
function i18nLookup(lang, key) {
  var cat = (typeof I18N_CATALOG !== 'undefined' && I18N_CATALOG) ? I18N_CATALOG[lang] : null;
  if (!cat || !key) return undefined;
  return cat[key];
}

/** Replace {name} placeholders; a placeholder with no param is left verbatim. */
function i18nInterpolate(str, params) {
  if (!params) return str;
  return str.replace(/\{(\w+)\}/g, function (whole, name) {
    return Object.prototype.hasOwnProperty.call(params, name) ? String(params[name]) : whole;
  });
}

/* Translate key. Missing keys fall back current language -> en -> the key
   itself, so a typo shows up on screen instead of rendering blank. */
function t(key, params) {
  var s = i18nLookup(_termcpLang, key);
  if (s === undefined) s = i18nLookup('en', key);
  if (s === undefined) return String(key);
  return i18nInterpolate(s, params);
}

/* Singular/plural key picker: tCount(oneKey, otherKey, {count: n}).
   Deliberately not an ICU/plural-rule engine — it only chooses between two
   catalog keys, and both keys stay visible at the call site. Languages that do
   not inflect fill both keys with the same sentence. */
function tCount(oneKey, otherKey, params) {
  var n = params ? Number(params.count) : NaN;
  return t(n === 1 ? oneKey : otherKey, params);
}

function hasI18nAttrs(el) {
  return el.hasAttribute('data-i18n') || el.hasAttribute('data-i18n-title') ||
         el.hasAttribute('data-i18n-placeholder') || el.hasAttribute('data-i18n-aria');
}

/* Refresh every marked node under root (default document) to the current
   language. Only these four attributes are supported on purpose: an
   innerHTML path (data-i18n-html) would be an XSS surface, and dynamic markup
   calls t() at render time instead of being observed by a MutationObserver. */
function applyI18n(root) {
  var scope = root || document;
  var nodes = [];
  if (scope.nodeType === 1 && scope.hasAttribute && hasI18nAttrs(scope)) nodes.push(scope);
  if (scope.querySelectorAll) {
    var found = scope.querySelectorAll('[data-i18n],[data-i18n-title],[data-i18n-placeholder],[data-i18n-aria]');
    for (var i = 0; i < found.length; i++) nodes.push(found[i]);
  }
  nodes.forEach(function (el) {
    var key = el.getAttribute('data-i18n');
    if (key) el.textContent = t(key);
    key = el.getAttribute('data-i18n-title');
    if (key) el.title = t(key);
    key = el.getAttribute('data-i18n-placeholder');
    if (key) el.setAttribute('placeholder', t(key));
    key = el.getAttribute('data-i18n-aria');
    if (key) el.setAttribute('aria-label', t(key));
  });
}

/* Register a callback that redraws dynamic DOM after a language switch. See
   the registration at the bottom of each module: they re-render from in-memory
   snapshots, never from a fresh fetch, so switching cannot blank a list or
   flicker while offline. */
function onLangChange(fn) {
  if (typeof fn === 'function') _langReappliers.push(fn);
}

/* Reflect a stored choice on the header control: the trigger shows the name of
   the active choice and the menu marks it. Names are copied from the menu's own
   labels, so "Auto" follows the catalog (Auto/自动/自動) while the three
   language names stay written in their own script. */
function syncLangChoice(choice) {
  var opts = document.querySelectorAll('.lang-opt');
  var current = null;
  for (var i = 0; i < opts.length; i++) {
    var isOn = opts[i].getAttribute('data-lang') === choice;
    opts[i].classList.toggle('active', isOn);
    if (isOn) {
      opts[i].setAttribute('aria-current', 'true');
      current = opts[i];
    } else {
      opts[i].removeAttribute('aria-current');
    }
  }
  var label = document.getElementById('lang-current');
  if (label) label.textContent = current ? current.textContent : '';
}

/* --- the language menu ---
   A disclosure, not a custom listbox: the trigger is a real button and the items
   are real buttons, so Tab/Enter/Space come from the browser. Only the "close
   when the user looks away" behaviour is script. */
var _langMenuOpen = false;

function langMenuEl() { return document.getElementById('lang-menu'); }
function langBtnEl() { return document.getElementById('lang-btn'); }

/** Open or close the menu, keeping the trigger's aria state in step. */
function setLangMenuOpen(open) {
  var menu = langMenuEl(), btn = langBtnEl();
  if (!menu || !btn) return;
  _langMenuOpen = !!open;
  menu.classList.toggle('hidden', !_langMenuOpen);
  btn.setAttribute('aria-expanded', _langMenuOpen ? 'true' : 'false');
}

function toggleLangMenu() { setLangMenuOpen(!_langMenuOpen); }
function closeLangMenu() { setLangMenuOpen(false); }

/** A menu item: switch the language, close, and leave the focus where it was. */
function chooseLang(value) {
  termcpApplyLang(value);
  closeLangMenu();
  var btn = langBtnEl();
  if (btn) { try { btn.focus(); } catch (e) {} }
}

/* Registered once and inert while the menu is closed, so opening it does not
   accumulate listeners. A click on the trigger itself is inside .lang-switch and
   therefore cannot close the menu it just opened. */
function initLangMenu() {
  document.addEventListener('click', function (e) {
    if (!_langMenuOpen) return;
    if (e.target.closest && e.target.closest('.lang-switch')) return;
    closeLangMenu();
  });
  document.addEventListener('keydown', function (e) {
    if (!_langMenuOpen || e.key !== 'Escape') return;
    closeLangMenu();
    var btn = langBtnEl();
    if (btn) { try { btn.focus(); } catch (err) {} }
  });
}

/* Language switch entry point (the radios' onchange). value: auto | en | zh-Hans | zh-Hant.
   Order matters: store, resolve, relabel the document, static DOM, then the
   dynamic redraws. The per-callback try/catch is required: one module failing
   to redraw must not stop the others. */
function termcpApplyLang(value) {
  var choice = isLangChoice(value) ? value : I18N_AUTO;
  try { localStorage.setItem(I18N_STORAGE_KEY, choice); } catch (e) {}
  _termcpLang = resolveLang(choice);
  document.documentElement.setAttribute('lang', _termcpLang);
  if (document.body) document.body.classList.toggle('lang-hant', _termcpLang === 'zh-Hant');
  applyI18n(document);
  _langReappliers.forEach(function (fn) {
    try { fn(); } catch (e) { console.error(e); }
  });
  syncLangChoice(choice);
}

/* First application, at load time. The selector keeps the STORED value (which
   may be "auto"), not the resolved code, so it shows what the user chose. */
function initI18n() {
  var choice = storedLangChoice();
  _termcpLang = resolveLang(choice);
  document.documentElement.setAttribute('lang', _termcpLang);
  if (document.body) document.body.classList.toggle('lang-hant', _termcpLang === 'zh-Hant');
  applyI18n(document);
  syncLangChoice(choice);
}
initI18n();
initLangMenu();
