/* Page chrome: the install tabs, the language picker, the screenshot swap and
   the GitHub badge. Loaded last, so it also runs the initialisers at the end. */

// Tab switcher
const tabs = document.querySelectorAll('.cmd-tab');
const display = document.getElementById('cmd-display');
const cmdPrompt = document.querySelector('.cmd-text .prompt');
const cmdNote = document.getElementById('cmd-note');
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
  currentCmdText = isAI ? t('cmd_ai') : (commands[key] || '');
  display.textContent = currentCmdText;
  display.classList.toggle('quoted', isAI);
  /* A tab may carry a caveat that belongs with its own command. */
  const note = key === 'docker' ? t('cmd_note_docker') : '';
  cmdNote.textContent = note;
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
  if (!e.target.closest('.lang-select-wrap')) closeLangMenu();
});
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') closeLangMenu();
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

detectLang();
fetchGitHubMeta();
fetchDonors();
