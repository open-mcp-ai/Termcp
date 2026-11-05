/** installSettingsMenu owns the workspace preference menu. It is deliberately
 * separate from approval.js: approvals are an operational queue, while theme,
 * language, permissions, login security, encryption and connected clients are settings. */
(function installSettingsMenu() {
  function mount() {
    if (document.getElementById('settings-menu')) return;
    var nav = document.querySelector('.app-header-nav');
    if (!nav) return;
    var menu = document.createElement('details');
    menu.className = 'clients-menu settings-menu';
    menu.id = 'settings-menu';
    menu.innerHTML =
      '<summary class="clients-btn settings-btn lang-btn app-control app-menu-trigger" title="Settings" data-i18n-title="settings.title" aria-label="Settings" data-i18n-aria="settings.title">' +
        '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="8" cy="8" r="2.2"/><path d="M8 1.8v1.4M8 12.8v1.4M1.8 8h1.4M12.8 8h1.4M3.6 3.6l1 1M11.4 11.4l1 1M12.4 3.6l-1 1M4.6 11.4l-1 1"/></svg>' +
        '<span class="clients-label" data-i18n="settings.title">Settings</span>' +
      '</summary>' +
      '<div class="clients-panel settings-panel lang-menu app-menu-panel">' +
        '<div class="settings-head"><strong data-i18n="settings.title">Settings</strong><p data-i18n="settings.hint">Appearance, permissions, login security and connections.</p></div>' +
        '<div id="settings-links" class="settings-links"></div>' +
      '</div>';
    nav.appendChild(menu);

    function move(id, className) {
      var node = document.getElementById(id);
      if (!node || node.closest('#settings-links')) return;
      if (className) node.classList.add(className);
      var control = node.matches('summary, button, a') ? node : node.querySelector('summary, button, a');
      if (control) control.classList.add('app-control', 'app-menu-trigger');
      var panel = node.querySelector('.clients-panel, .theme-menu, .lang-menu');
      if (panel) panel.classList.add('app-menu-panel');
      document.getElementById('settings-links').appendChild(node);
    }
    document.querySelectorAll('#lang-menu .lang-opt').forEach(function (item) {
      item.classList.add('app-menu-item');
    });
    move('theme-switch', 'settings-theme');
    var language = document.querySelector('.app-header-nav .lang-switch');
    if (language && !language.closest('#settings-links')) {
      language.classList.add('settings-lang');
      document.getElementById('settings-links').appendChild(language);
    }
    move('clients-menu', 'settings-clients');

    // Three top-level entries, in the requested order.
    var approval = document.getElementById('approval-menu');
    var api = document.querySelector('.app-header-nav .api-nav-link');
    if (approval) nav.appendChild(approval);
    if (api) {
      api.classList.add('app-control');
      nav.appendChild(api);
    }
    nav.appendChild(menu);
    if (typeof applyI18n === 'function') applyI18n(menu);
    menu.addEventListener('toggle', function () {
      if (!menu.open) return;
      if (typeof closeHeaderMenus === 'function') closeHeaderMenus('settings-menu');
    });
    document.addEventListener('click', function (e) {
      if (menu.open && !menu.contains(e.target)) menu.open = false;
    });
  }
  // Scripts load after the page DOM, so the header controls already exist here.
  // Mount synchronously: config-storage.js follows immediately and must find
  // #settings-links when it runs.
  mount();
})();
