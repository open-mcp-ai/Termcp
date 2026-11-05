function initManagedSession() {
  'use strict';
  /* auth deployment path: preserve reverse-proxy mount for authentication routes. */
  function authBasePath() {
    var pathname = location.pathname || '/';
    if (pathname.slice(-11) === '/index.html') pathname = pathname.slice(0, -11);
    else if (pathname !== '/') {
      var slash = pathname.lastIndexOf('/');
      pathname = slash < 0 ? '' : pathname.slice(0, slash);
    }
    return pathname === '/' ? '' : pathname;
  }
  function authPath(path) {
    var value = String(path == null ? '' : path);
    if (!value || value.charAt(0) !== '/') return value;
    var base = authBasePath();
    if (!base || value === base || value.indexOf(base + '/') === 0) return value;
    return base + value;
  }
  /* end auth deployment path */
  var original = window.fetch.bind(window);
  var context = original(authPath('/auth/status'), { cache: 'no-store' }).then(function (response) {
    if (!response.ok) return null;
    return response.json();
  }).catch(function () { return null; });
  window.fetch = function (input, options) {
    var request = new Request(input, options);
    var local = new URL(request.url).origin === location.origin;
    if (!local) return original(request);
    return context.then(function (state) {
      if (state && state.mode === 'managed' && !/^(GET|HEAD|OPTIONS)$/i.test(request.method)) {
        var headers = new Headers(request.headers);
        headers.set('X-Termcp-CSRF', state.csrf);
        request = new Request(request, { headers: headers });
      }
      return original(request).then(function (response) {
        if (state && state.mode === 'managed' && response.status === 423) location.assign(authPath('/auth'));
        if (state && state.mode === 'managed' && (response.status === 401 || response.headers.get('X-Termcp-Reauthenticate') === '1')) location.assign(authPath('/login'));
        return response;
      });
    });
  };
  context.then(function (state) {
    if (!state || !state.authenticated) return;
    function explainIndependentPassword() {
      var form = document.getElementById('config-storage-password-form');
      if (!form || document.getElementById('managed-independent-password-note')) return;
      var note = document.createElement('p');
      note.id = 'managed-independent-password-note';
      note.className = 'conn-check-hint';
      note.textContent = /^zh/i.test(navigator.language)
        ? '此处仅修改解密密码，不修改登录密码。'
        : 'This changes only the decryption password. The login password is independent.';
      form.insertBefore(note, form.firstChild);
    }
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', explainIndependentPassword, { once: true });
    else explainIndependentPassword();
    function openEncryptionSettings(){if(new URLSearchParams(location.search).get('config-encryption')!=='1')return;var button=document.getElementById('config-storage-open');if(button){button.click();history.replaceState(null,'',location.pathname);}}
    if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',function(){setTimeout(openEncryptionSettings,0);},{once:true});else setTimeout(openEncryptionSettings,0);
    var settingsAttempts = 0;
    function mountManagedSettings() {
      var settings = document.getElementById('settings-links');
      if (!settings) {
        if (settingsAttempts++ < 20) setTimeout(mountManagedSettings, 25);
        return;
      }
      if (document.getElementById('managed-permissions-link')) return;
      var permissions = document.createElement('a');
      permissions.id = 'managed-permissions-link';
      permissions.href = authPath('/permissions');
      permissions.innerHTML = '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" aria-hidden="true"><path d="M3 4h10M3 8h10M3 12h10"/><circle cx="6" cy="4" r="1.4" fill="currentColor" stroke="none"/><circle cx="10" cy="8" r="1.4" fill="currentColor" stroke="none"/><circle cx="7" cy="12" r="1.4" fill="currentColor" stroke="none"/></svg><span></span>';
      permissions.className = 'api-nav-link settings-link app-menu-item settings-permissions';
      permissions.lastElementChild.textContent = /^zh/i.test(navigator.language) ? '权限管理' : 'Permissions';
      var link = document.createElement('a');
      link.id = 'managed-security-link';
      link.href = authPath('/auth');
      link.innerHTML = '<svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="8" cy="5.5" r="2.5"/><path d="M3.5 13c.6-2.2 2.1-3.3 4.5-3.3s3.9 1.1 4.5 3.3"/></svg><span></span>';
      link.setAttribute('aria-label', /^zh/i.test(navigator.language) ? '登录与安全' : 'Login and security');
      link.className = 'api-nav-link settings-link app-menu-item settings-security';
      link.lastElementChild.textContent = /^zh/i.test(navigator.language) ? '登录与安全' : 'Login & security';
      settings.appendChild(permissions);
      settings.appendChild(link);
    }
    mountManagedSettings();
  });
}
initManagedSession();
