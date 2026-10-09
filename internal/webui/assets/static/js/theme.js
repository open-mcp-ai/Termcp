/* Shared by the workbench and docs. All discovery uses the static file server. */
var _themeActive = document.documentElement.getAttribute('data-theme') || 'default-light';
var _themeCallbacks = [];
var _themePending = null;
var _themeRevision = 0;
var _themeMenuRevision = 0;
var _themeStrings = {};
var _themeInitialResources = null;
var _themeTerminal = {};

function validThemeChoice(id) {
  return typeof id === 'string' && id.length > 0 && id.charAt(0) !== '.' && !/[\/\\:\x00-\x1f\x7f]/.test(id);
}

function themeText(key) {
  if (typeof t === 'function') {
    switch (key) {
      case 'theme.label': return t('theme.label');
      case 'theme.loading': return t('theme.loading');
      case 'theme.discoverFailed': return t('theme.discoverFailed');
      case 'theme.loadFailed': return t('theme.loadFailed');
      case 'theme.light': return t('theme.light');
      case 'theme.dark': return t('theme.dark');
      case 'theme.cyberpunk': return t('theme.cyberpunk');
    }
  }
  var overrides = _themeStrings.en;
  if (overrides && Object.prototype.hasOwnProperty.call(overrides, key)) return overrides[key];
  var english = { 'theme.label': 'Theme', 'theme.loading': 'Loading themes…', 'theme.discoverFailed': 'Unable to load themes', 'theme.loadFailed': 'Theme could not be loaded', 'theme.light': 'Light', 'theme.dark': 'Dark', 'theme.cyberpunk': 'Cyberpunk' };
  return english[key] || key;
}

/* Optional per-package copy: language -> existing i18n key -> plain text. */
function normalizeThemeStrings(value) {
  var result = Object.create(null);
  if (!value || typeof value !== 'object' || Array.isArray(value)) return result;
  ['en', 'zh-Hans', 'zh-Hant'].forEach(function (lang) {
    var source = Object.prototype.hasOwnProperty.call(value, lang) ? value[lang] : null;
    if (!source || typeof source !== 'object' || Array.isArray(source)) return;
    var messages = Object.create(null);
    Object.keys(source).forEach(function (key) {
      if (typeof source[key] === 'string') messages[key] = source[key];
    });
    result[lang] = messages;
  });
  return result;
}

function loadThemeJSON(id, filename, normalize) {
  var controller = typeof AbortController === 'function' ? new AbortController() : null;
  var finish;
  var timer;
  var promise = new Promise(function (resolve) {
    var settled = false;
    finish = function (catalog) {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      if (controller) controller.abort();
      resolve(catalog);
    };
    timer = setTimeout(function () { finish({}); }, 3000);
    var options = { cache: 'no-store' };
    if (controller) options.signal = controller.signal;
    fetch('themes/' + encodeURIComponent(id) + '/' + filename, options).then(function (response) {
      return response.ok ? response.json() : {};
    }).then(function (value) { finish(normalize(value)); }).catch(function () { finish({}); });
  });
  return { promise: promise, cancel: function () { finish({}); } };
}

function normalizeThemeConfig(value) {
  var source = value && value.terminal;
  var terminal = {};
  if (!source || typeof source !== 'object' || Array.isArray(source)) return terminal;
  if (typeof source.transparent === 'boolean') terminal.transparent = source.transparent;
  var ranges = { fontSize: [6, 72], lineHeight: [1, 3], letterSpacing: [0, 10], opacity: [0, 1] };
  Object.keys(ranges).forEach(function (key) {
    var n = source[key];
    if (typeof n === 'number' && isFinite(n) && n >= ranges[key][0] && n <= ranges[key][1]) terminal[key] = n;
  });
  if (typeof source.fontFamily === 'string' && source.fontFamily.trim() &&
      (typeof CSS === 'undefined' || CSS.supports('font-family', source.fontFamily))) terminal.fontFamily = source.fontFamily;
  ['fontWeight', 'fontWeightBold'].forEach(function (key) {
    var weight = source[key];
    if (weight === 'normal' || weight === 'bold' || (typeof weight === 'number' && weight >= 100 && weight <= 900)) terminal[key] = weight;
  });
  function color(value) {
    return typeof value === 'string' && value.trim() && (typeof CSS === 'undefined' || CSS.supports('color', value));
  }
  ['background', 'foreground', 'cursor', 'cursorAccent', 'selectionBackground'].forEach(function (key) {
    if (color(source[key])) terminal[key] = source[key];
  });
  terminal.colors = {};
  ['black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white',
   'brightBlack', 'brightRed', 'brightGreen', 'brightYellow', 'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite'].forEach(function (key) {
    if (source.colors && color(source.colors[key])) terminal.colors[key] = source.colors[key];
  });
  terminal.window = {};
  var win = source.window || {};
  ['background', 'foreground', 'headerBackground', 'borderColor'].forEach(function (key) {
    if (color(win[key])) terminal.window[key] = win[key];
  });
  if (typeof win.blur === 'number' && isFinite(win.blur) && win.blur >= 0 && win.blur <= 64) terminal.window.blur = win.blur;
  return terminal;
}

function loadThemeResources(id) {
  var strings = loadThemeJSON(id, 'strings.json', normalizeThemeStrings);
  var config = loadThemeJSON(id, 'theme.json', normalizeThemeConfig);
  return {
    promise: Promise.all([strings.promise, config.promise]).then(function (values) { return { strings: values[0], terminal: values[1] }; }),
    cancel: function () { strings.cancel(); config.cancel(); }
  };
}

function setThemeTerminalConfig(config) {
  _themeTerminal = config;
  var root = document.documentElement;
  // Remove only properties owned by the previous JSON. CSS remains the fallback.
  var properties = {
    fontFamily: '--terminal-font-family', fontSize: '--terminal-font-size',
    fontWeight: '--terminal-font-weight', fontWeightBold: '--terminal-font-weight-bold',
    lineHeight: '--terminal-line-height', letterSpacing: '--terminal-letter-spacing', opacity: '--terminal-background-opacity',
    background: '--terminal-background', foreground: '--xterm-foreground', cursor: '--xterm-cursor',
    cursorAccent: '--xterm-cursor-accent', selectionBackground: '--xterm-selection'
  };
  Object.keys(properties).forEach(function (key) {
    root.style.removeProperty(properties[key]);
    if (config[key] !== undefined) root.style.setProperty(properties[key], String(config[key]));
  });
  var colors = config.colors || {};
  ['black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white',
   'brightBlack', 'brightRed', 'brightGreen', 'brightYellow', 'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite'].forEach(function (key) {
    var property = '--xterm-' + key.replace(/[A-Z]/g, function (letter) { return '-' + letter.toLowerCase(); });
    root.style.removeProperty(property);
    if (colors[key] !== undefined) root.style.setProperty(property, colors[key]);
  });
  var win = config.window || {};
  var windowProperties = { background: '--terminal-window-background', foreground: '--terminal-window-foreground', headerBackground: '--terminal-header-background', borderColor: '--terminal-border-color', blur: '--terminal-window-blur' };
  Object.keys(windowProperties).forEach(function (key) {
    root.style.removeProperty(windowProperties[key]);
    if (win[key] !== undefined) root.style.setProperty(windowProperties[key], String(win[key]) + (key === 'blur' ? 'px' : ''));
  });
  var transparent = config.transparent;
  if (transparent === undefined) transparent = getComputedStyle(root).getPropertyValue('--terminal-transparent').trim() === 'true';
  _themeTerminal.transparent = transparent;
  root.style.removeProperty('--terminal-body-background');
  if (transparent && (config.background !== undefined || win.background !== undefined)) {
    root.style.setProperty('--terminal-body-background', 'transparent');
  }
  if (transparent && (config.background !== undefined || win.background !== undefined) && config.opacity === undefined) root.style.setProperty('--terminal-background-opacity', '.85');
  root.setAttribute('data-terminal-transparent', String(transparent));
  root.setAttribute('data-terminal-window-foreground', String(win.foreground !== undefined));
}

function setThemeStrings(catalog) {
  _themeStrings = catalog;
  if (typeof setI18nThemeCatalog === 'function') setI18nThemeCatalog(catalog);
  syncThemeControl();
}

function restoreThemeResources(id) {
  if (_themeInitialResources) _themeInitialResources.cancel();
  var revision = _themeRevision;
  var request = loadThemeResources(id);
  _themeInitialResources = request;
  request.promise.then(function (resources) {
    if (_themeInitialResources === request) _themeInitialResources = null;
    if (revision === _themeRevision && id === _themeActive) notifyThemeChange(id, resources.strings, resources.terminal);
  });
}

function themeLabel(id) {
  var names = { 'default-light': 'theme.light', 'default-dark': 'theme.dark', 'cyberpunk': 'theme.cyberpunk' };
  return Object.prototype.hasOwnProperty.call(names, id) ? themeText(names[id]) : id;
}

function themeAssetURL(path) {
  return 'themes/' + encodeURIComponent(_themeActive) + '/assets/' + path.split('/').map(encodeURIComponent).join('/');
}

function syncThemeAssets(root) {
  (root || document).querySelectorAll('[data-theme-asset]').forEach(function (node) {
    var url = themeAssetURL(node.getAttribute('data-theme-asset'));
    node.setAttribute(node.tagName === 'LINK' ? 'href' : 'src', url);
  });
}

function onThemeChange(fn) {
  if (typeof fn === 'function') _themeCallbacks.push(fn);
}

function notifyThemeChange(id, catalog, terminal) {
  _themeActive = id;
  if (terminal) setThemeTerminalConfig(terminal);
  if (catalog) setThemeStrings(catalog);
  syncThemeAssets();
  syncThemeControl();
  _themeCallbacks.forEach(function (fn) {
    try { fn(); } catch (e) { console.error(e); }
  });
}

function discoverThemes() {
  return fetch('themes/', { cache: 'no-store' }).then(function (response) {
    if (!response.ok) throw new Error('themes ' + response.status);
    return response.text();
  }).then(function (html) {
    var listing = new DOMParser().parseFromString(html, 'text/html');
    var ids = [];
    listing.querySelectorAll('a[href]').forEach(function (a) {
      var href = a.getAttribute('href');
      if (!href || href.charAt(href.length - 1) !== '/') return;
      try {
        var id = decodeURIComponent(href.slice(0, -1));
        if (validThemeChoice(id) && ids.indexOf(id) < 0) ids.push(id);
      } catch (e) {}
    });
    var builtinOrder = ['default-light', 'default-dark', 'cyberpunk'];
    return ids.sort(function (a, b) {
      var ai = builtinOrder.indexOf(a), bi = builtinOrder.indexOf(b);
      if (ai >= 0 || bi >= 0) return (ai < 0 ? 3 : ai) - (bi < 0 ? 3 : bi);
      return a.localeCompare(b);
    });
  });
}

function applyTheme(id) {
  if (!validThemeChoice(id)) return Promise.reject(new Error('invalid theme'));
  var revision = ++_themeRevision;
  if (_themeInitialResources) { _themeInitialResources.cancel(); _themeInitialResources = null; }
  if (_themePending) _themePending.cancel();
  return new Promise(function (resolve, reject) {
    var next = document.createElement('link');
    next.rel = 'stylesheet';
    next.media = 'not all';
    next.href = 'themes/' + encodeURIComponent(id) + '/theme.css?reload=' + revision;
    var timer;
    var resourcesRequest = loadThemeResources(id);
    var cssReady = false;
    var resourcesReady = false;
    var resources;
    var done = false;
    function cleanup() {
      done = true;
      clearTimeout(timer);
      resourcesRequest.cancel();
      next.onload = next.onerror = null;
      if (_themePending && _themePending.link === next) _themePending = null;
    }
    _themePending = { link: next, cancel: function () {
      cleanup();
      next.remove();
      resolve(false);
    } };
    next.onerror = function () {
      cleanup();
      next.remove();
      reject(new Error(themeText('theme.loadFailed')));
    };
    function commit() {
      if (done || !cssReady || !resourcesReady || revision !== _themeRevision) return;
      cleanup();
      // Stop any late initial-load event from superseding a user choice.
      if (typeof termcpBootTimer !== 'undefined') clearTimeout(termcpBootTimer);
      var previous = document.getElementById('termcp-theme');
      if (previous) previous.onload = previous.onerror = null;
      next.media = 'all';
      if (previous) previous.remove();
      next.id = 'termcp-theme';
      document.documentElement.setAttribute('data-theme', id);
      document.documentElement.classList.remove('theme-loading');
      try { localStorage.setItem('termcp.theme', id); } catch (e) {}
      notifyThemeChange(id, resources.strings, resources.terminal);
      resolve(true);
    }
    next.onload = function () {
      cssReady = true;
      commit();
    };
    resourcesRequest.promise.then(function (value) {
      resources = value;
      resourcesReady = true;
      commit();
    });
    timer = setTimeout(function () { if (next.onerror) next.onerror(); }, 15000);
    var current = document.getElementById('termcp-theme');
    if (current) current.parentNode.insertBefore(next, current.nextSibling);
    else document.head.appendChild(next);
  });
}

function syncThemeControl() {
  var label = document.getElementById('theme-current');
  if (label) label.textContent = themeLabel(_themeActive);
  document.querySelectorAll('[data-theme-choice]').forEach(function (button) {
    button.textContent = themeLabel(button.getAttribute('data-theme-choice'));
    button.setAttribute('aria-pressed', String(button.getAttribute('data-theme-choice') === _themeActive));
  });
}

function loadThemeChoices() {
  var revision = ++_themeMenuRevision;
  var options = document.getElementById('theme-options');
  var status = document.getElementById('theme-status');
  if (!options || !status) return;
  status.textContent = themeText('theme.loading');
  discoverThemes().then(function (ids) {
    if (revision !== _themeMenuRevision) return;
    options.textContent = '';
    status.textContent = '';
    ids.forEach(function (id) {
      var button = document.createElement('button');
      button.type = 'button';
      button.className = 'theme-option';
      button.setAttribute('data-theme-choice', id);
      button.addEventListener('click', function () {
        status.textContent = themeText('theme.loading');
        applyTheme(id).then(function (applied) {
          if (!applied) return;
          status.textContent = '';
          document.getElementById('theme-switch').open = false;
        }).catch(function () { status.textContent = themeText('theme.loadFailed'); });
      });
      options.appendChild(button);
    });
    syncThemeControl();
  }).catch(function () { if (revision === _themeMenuRevision) status.textContent = themeText('theme.discoverFailed'); });
}

function initThemeControls() {
  var picker = document.getElementById('theme-switch');
  if (!picker) return;
  syncThemeAssets();
  syncThemeControl();
  setThemeTerminalConfig({});
  restoreThemeResources(_themeActive);
  picker.addEventListener('toggle', function () { if (picker.open) loadThemeChoices(); });
  document.addEventListener('click', function (e) { if (!picker.contains(e.target)) picker.open = false; });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && picker.open) { picker.open = false; picker.querySelector('summary').focus(); }
  });
  if (typeof onLangChange === 'function') onLangChange(syncThemeControl);
}

document.addEventListener('termcp:themechange', function (e) {
  notifyThemeChange(e.detail, {}, {});
  restoreThemeResources(e.detail);
});
initThemeControls();
