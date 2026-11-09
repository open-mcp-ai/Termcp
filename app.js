/* Page chrome: the install tabs, the language picker, the screenshot swap and
   the GitHub badge. Loaded last, so it also runs the initialisers at the end. */

// Tab switcher
const tabs = document.querySelectorAll('.cmd-tab');
const display = document.getElementById('cmd-display');
const cmdPrompt = document.querySelector('.cmd-text .prompt');
const cmdNote = document.getElementById('cmd-note');

/* WHICH BINARY IS THIS MACHINE? Six files are published and the command names
   one of them. Chromium answers `getHighEntropyValues` with a real
   architecture; Safari and Firefox freeze the platform string, so they get the
   OS and keep `<arch>` as a placeholder — a wrong guess here is a 404 the reader
   has to debug, which is worse than a hole they can see. */
let hostTarget = '<os>-<arch>';
let hostFilled = false;
const osFromUA = () => {
  const ua = navigator.userAgent;
  if (/Windows/i.test(ua)) return 'windows';
  if (/Mac OS X|Macintosh/i.test(ua)) return 'darwin';
  if (/Linux|X11|CrOS/i.test(ua)) return 'linux';
  return '';
};
async function detectHost() {
  let os = '', arch = '';
  const d = navigator.userAgentData;
  if (d && d.getHighEntropyValues) {
    try {
      const v = await d.getHighEntropyValues(['architecture', 'bitness', 'platform']);
      os = { macOS: 'darwin', Windows: 'windows', Linux: 'linux', 'Chrome OS': 'linux' }[v.platform] || '';
      arch = v.architecture === 'arm' ? 'arm64'
           : (v.architecture === 'x86' && v.bitness === '64') ? 'amd64' : '';
    } catch (err) { /* optional API; a refusal just leaves the placeholder */ }
  }
  if (!os) os = osFromUA();
  if (!os) return;
  hostFilled = !!arch;
  hostTarget = os + '-' + (arch || '<arch>') + (os === 'windows' ? '.exe' : '');
  renderCmd(activeCmd);
}
let activeCmd = 'ai';
/* The exact bytes the copy button hands over. Kept apart from the rendered
   text so the quotes the AI tab draws can never travel with it. */
let currentCmdText = '';

/* One renderer for both kinds of tab: a shell line from `commands`, or the
   sentence the AI tab shows from the catalog. The AI tab hides the `$`,
   because nothing there is a shell command. */
function renderCmd(key) {
  activeCmd = key;
  const isAI = key === 'ai';
  cmdPrompt.hidden = isAI;
  if (isAI) {
    currentCmdText = t('cmd_ai');
  } else {
    currentCmdText = (commands[key] || '').replace('{{target}}', hostTarget);
    /* chmod is not a thing on Windows; the rest of the block is one file
       either way, so dropping the line is the whole difference. */
    if (key === 'binary' && hostTarget.indexOf('windows') === 0) {
      currentCmdText = currentCmdText.split('\n').filter(l => l.indexOf('chmod') !== 0).join('\n');
    }
  }
  display.textContent = currentCmdText;
  display.classList.toggle('quoted', isAI);
  /* A tab may carry a caveat that belongs with its own command. */
  const noteKey = { docker: 'cmd_note_docker', binary: hostFilled ? 'cmd_note_binary_ok' : 'cmd_note_binary' }[key];
  /* innerHTML, not textContent: the followup line is marked up so it reads as
     the next step rather than as a footnote. */
  const note = noteKey ? t(noteKey).replace('{{target}}', hostTarget) : '';
  cmdNote.innerHTML = note;
  cmdNote.hidden = !note;
}
tabs.forEach(tab => {
  tab.addEventListener('click', () => {
    tabs.forEach(t => t.classList.remove('active'));
    tab.classList.add('active');
    renderCmd(tab.getAttribute('data-cmd'));
  });
});

// Copy Button
const copyBtn = document.getElementById('copy-btn');
copyBtn.addEventListener('click', () => {
  const text = currentCmdText;
  navigator.clipboard.writeText(text).then(() => {
    copyBtn.textContent = '✓ ' + t('copied');
    setTimeout(() => { copyBtn.textContent = t('copy_btn'); }, 1800);
  });
});

/* --- Header controls: Theme & Language ---
   Mirrors termcp's own UI (../termcp/internal/webui/assets):
   1. Stored value is the USER CHOICE ('auto' | 'light' | 'dark'), not the resolved id.
   2. 'auto' resolves live via prefers-color-scheme so a sunset switch works without refresh.
   3. Both are button+menu disclosures, not native <select>s, so dropdowns stay themed.
   4. Opening one menu closes the other. */

let themeChoice = (() => {
  const v = localStorage.getItem('termcp_theme');
  return (v === 'light' || v === 'dark') ? v : 'auto';
})();

const systemThemeMQ = window.matchMedia && window.matchMedia('(prefers-color-scheme: light)');

function resolveTheme(choice) {
  return choice === 'auto'
    ? ((systemThemeMQ && systemThemeMQ.matches) ? 'light' : 'dark')
    : choice;
}

function applyTheme(choice) {
  themeChoice = choice;
  try { localStorage.setItem('termcp_theme', choice); } catch (e) {}
  document.documentElement.dataset.theme = resolveTheme(choice);
  syncThemeMenu();
}

if (systemThemeMQ && systemThemeMQ.addEventListener) {
  systemThemeMQ.addEventListener('change', () => {
    if (themeChoice === 'auto') {
      document.documentElement.dataset.theme = resolveTheme('auto');
    }
  });
}

const themeBtn = document.getElementById('theme-btn');
const themeMenu = document.getElementById('theme-menu');
const themeCurrent = document.getElementById('theme-current');

function syncThemeMenu() {
  const dict = i18nData[currentLang] || i18nData['en'];
  const labels = {
    auto: dict.theme_auto || '自动',
    light: dict.theme_light || '浅色',
    dark: dict.theme_dark || '深色'
  };
  if (themeCurrent) themeCurrent.textContent = labels[themeChoice] || labels.auto;
  if (themeMenu) {
    themeMenu.querySelectorAll('[data-theme-choice]').forEach(item => {
      const choice = item.getAttribute('data-theme-choice');
      item.textContent = labels[choice] || choice;
      item.setAttribute('aria-selected', String(choice === themeChoice));
    });
  }
}

function closeThemeMenu() {
  if (!themeMenu) return;
  themeMenu.hidden = true;
  if (themeBtn) themeBtn.setAttribute('aria-expanded', 'false');
}

if (themeBtn && themeMenu) {
  themeBtn.addEventListener('click', () => {
    const opening = themeMenu.hidden;
    closeLangMenu();
    themeMenu.hidden = !opening;
    themeBtn.setAttribute('aria-expanded', String(opening));
    if (opening) syncThemeMenu();
  });
  themeMenu.addEventListener('click', (e) => {
    const item = e.target.closest('button[data-theme-choice]');
    if (!item) return;
    applyTheme(item.getAttribute('data-theme-choice'));
    closeThemeMenu();
    try { themeBtn.focus(); } catch (err) {}
  });
}

const langSelect = document.getElementById('lang-select');
const langMenu = document.getElementById('lang-menu');
const langBtn = document.getElementById('lang-icon-btn');
const langCurrent = document.getElementById('lang-current');

function applyShot(lang) {
  const pick = SHOTS[lang] || SHOTS['en'];
  [['console', 'shot-link'], ['ext', 'ext-link']].forEach(([kind, linkId]) => {
    const [id, file] = pick[kind];
    const shown = document.getElementById(id);
    if (!shown) return;
    if (!shown.getAttribute('src')) shown.src = shown.dataset.src;
    shown.hidden = false;
    document.querySelectorAll(kind === 'ext' ? '[id^="ext-"]' : '[id^="shot-"]').forEach(node => {
      if (node.tagName === 'IMG' && node !== shown) node.hidden = true;
    });
    document.getElementById(linkId).href = file;
  });
}

function applyLang(lang) {
  currentLang = lang;
  document.documentElement.lang = lang;
  const dict = i18nData[lang] || i18nData['en'];

  document.querySelectorAll('[data-i18n]').forEach(node => {
    const key = node.getAttribute('data-i18n');
    if (dict[key] !== undefined) node.textContent = dict[key];
  });
  document.querySelectorAll('[data-i18n-html]').forEach(node => {
    const key = node.getAttribute('data-i18n-html');
    if (dict[key] !== undefined) node.innerHTML = dict[key];
  });
  document.querySelectorAll('[data-i18n-alt]').forEach(node => {
    const key = node.getAttribute('data-i18n-alt');
    if (dict[key] !== undefined) node.alt = dict[key];
  });
  document.querySelectorAll('[data-i18n-placeholder]').forEach(node => {
    const key = node.getAttribute('data-i18n-placeholder');
    if (dict[key] !== undefined) node.placeholder = dict[key];
  });
  document.querySelectorAll('[data-i18n-title]').forEach(node => {
    const key = node.getAttribute('data-i18n-title');
    if (dict[key] !== undefined) node.title = dict[key];
  });

  /* The headline's glitch layers are pseudo-elements, so they read the
     `data-text` attribute rather than the node's own text — keep it in step
     with whatever language just landed in the h1. */
  const headline = document.querySelector('.hero h1');
  if (headline) headline.setAttribute('data-text', headline.textContent);

  copyBtn.textContent = dict.copy_btn;
  /* The AI tab's text IS copy, so it follows the language; the shell tabs are
     code and survive verbatim. */
  renderCmd(activeCmd);

  // The transcript's own wording lives in the catalog, so a language change
  // re-renders it; the MCP call lines are code and survive verbatim.
  if (termScreen) runTask(lastTask, { auto: true });
  reviewLock.textContent = t(reviewOn ? 'review_on' : 'review_off');
  setMode(mode);
  applyShot(lang);
  renderDonors();

  const autoOpt = langSelect.querySelector('option[value="auto"]');
  if (autoOpt) autoOpt.textContent = dict.auto_label;
  const autoItem = langMenu.querySelector('[data-lang="auto"]');
  if (autoItem) autoItem.textContent = dict.auto_label;
  syncThemeMenu();
  syncLangMenu();
}

function detectLang() {
  const saved = localStorage.getItem('termcp_lang');
  if (saved && saved !== 'auto') {
    langSelect.value = saved;
    applyLang(saved);
    return;
  }
  langSelect.value = 'auto';
  const navLang = (navigator.language || 'zh-CN').toLowerCase();
  if (navLang.startsWith('zh-tw') || navLang.startsWith('zh-hk')) {
    applyLang('zh-TW');
  } else if (navLang.startsWith('zh')) {
    applyLang('zh-CN');
  } else {
    applyLang('en');
  }
}

langSelect.addEventListener('change', (e) => {
  const val = e.target.value;
  localStorage.setItem('termcp_lang', val);
  if (val === 'auto') {
    detectLang();
  } else {
    applyLang(val);
  }
});

/* Narrow screens collapse the picker to the globe. The menu writes through
   to the <select> and fires its change event, so persistence, auto-detect
   and the screenshot swap all keep running through that one path. */
function syncLangMenu() {
  const current = langSelect.value;
  let label = '';
  langMenu.querySelectorAll('[data-lang]').forEach(item => {
    const lang = item.getAttribute('data-lang');
    const on = lang === current;
    item.setAttribute('aria-selected', String(on));
    if (on) label = item.textContent;
  });
  /* The button's own caption: the chosen item's name, minus the globe it
     already draws. */
  if (langCurrent) langCurrent.textContent = label.replace('🌐', '').trim();
}

function closeLangMenu() {
  langMenu.hidden = true;
  langBtn.setAttribute('aria-expanded', 'false');
}

langBtn.addEventListener('click', () => {
  const opening = langMenu.hidden;
  closeThemeMenu();
  langMenu.hidden = !opening;
  langBtn.setAttribute('aria-expanded', String(opening));
  if (opening) syncLangMenu();
});

langMenu.addEventListener('click', (e) => {
  const item = e.target.closest('button[data-lang]');
  if (!item) return;
  langSelect.value = item.getAttribute('data-lang');
  langSelect.dispatchEvent(new Event('change'));
  closeLangMenu();
});

document.addEventListener('click', (e) => {
  if (!e.target.closest('.lang-select-wrap')) {
    closeLangMenu();
    closeThemeMenu();
  }
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    closeLangMenu();
    closeThemeMenu();
  }
});

// Auto fetch GitHub stars & release
// Opened straight from disk (file://) the GitHub API is unreliable (CORS / null origin),
// so we render fake values there to make layout & debugging deterministic.
const MOCK_META = {
  stargazers_count: 1234,
  tag_name: 'v9.9.9'
};

function isFileProtocol() {
  return location.protocol === 'file:';
}

function applyStars(count) {
  const el = document.getElementById('star-count');
  if (el) el.textContent = '★ ' + count;
}

function applyVersion(tagName) {
  const tag = document.getElementById('version-tag');
  if (!tag) return;
  tag.textContent = tagName;
  tag.hidden = false;
}

async function fetchGitHubMeta() {
  if (isFileProtocol()) {
    applyStars(MOCK_META.stargazers_count);
    applyVersion(MOCK_META.tag_name);
    return;
  }
  try {
    const repoRes = await fetch('https://api.github.com/repos/open-mcp-ai/termcp', { cache: 'no-store' });
    if (repoRes.ok) {
      const data = await repoRes.json();
      if (data.stargazers_count !== undefined) {
        applyStars(data.stargazers_count);
      }
    }        const relRes = await fetch('https://api.github.com/repos/open-mcp-ai/termcp/releases/latest', { cache: 'no-store' });
    if (relRes.ok) {
      const relData = await relRes.json();
      if (relData.tag_name) {
        applyVersion(relData.tag_name);
      }
    }
  } catch (err) {
    // Fallback gracefully to default
  }
}

/* Station strip: a table of contents, not a tab bar.
   The five panels are always on the page; the strip floats under the site
   header and marks whichever layer the reader is currently inside. Clicking a
   station is a plain anchor jump, so it works with JavaScript switched off. */
function initStopNav() {
  const stops = Array.from(document.querySelectorAll('.stops .stop'));
  if (!stops.length) return;

  const targets = stops.map(a => document.querySelector(a.getAttribute('href')));
  const strip = document.querySelector('.stops');
  function sync() {
    /* The line the reader's eye counts as "here": the bottom edge of the
       floating strip, measured live so a wrapped station row still lines up. */
    const line = strip ? strip.getBoundingClientRect().bottom + 24 : 180;
    let active = 0;
    for (let i = 0; i < targets.length; i++) {
      const el = targets[i];
      if (el && el.getBoundingClientRect().top <= line) active = i;
    }
    stops.forEach((a, i) => {
      if (i === active) a.setAttribute('aria-current', 'true');
      else a.removeAttribute('aria-current');
    });
  }

  /* Called straight from the scroll event: five rect reads per event is
     cheaper than a rAF hop, and virtual-time/headless environments do not
     always pump rAF after the load settles. */
  addEventListener('scroll', sync, { passive: true });

  sync();
}

detectLang();
applyTheme(themeChoice);
initStopNav();
fetchGitHubMeta();
fetchDonors();
detectHost();
