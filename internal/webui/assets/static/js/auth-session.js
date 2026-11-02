function initManagedSession() {
  'use strict';
  var original = window.fetch.bind(window);
  var context = original('/auth/status', { cache: 'no-store' }).then(function (response) {
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
        if (state && state.mode === 'managed' && response.status === 423) location.assign('/auth');
        if (state && state.mode === 'managed' && (response.status === 401 || response.headers.get('X-Termcp-Reauthenticate') === '1')) location.assign('/login');
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
        ? '此处仅修改配置加密密码，不修改登录密码。系统托管仅用于自动解锁配置。'
        : 'This changes only the configuration encryption password. Login passwords are independent; system protection enables optional automatic vault unlocking.';
      form.insertBefore(note, form.firstChild);
    }
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', explainIndependentPassword, { once: true });
    else explainIndependentPassword();
    function openEncryptionSettings(){if(new URLSearchParams(location.search).get('config-encryption')!=='1')return;var button=document.getElementById('config-storage-open');if(button){button.click();history.replaceState(null,'',location.pathname);}}
    if(document.readyState==='loading')document.addEventListener('DOMContentLoaded',function(){setTimeout(openEncryptionSettings,0);},{once:true});else setTimeout(openEncryptionSettings,0);
    var link = document.createElement('a');
    link.href = '/auth';
    link.textContent = state.actor.username;
    link.setAttribute('aria-label', /^zh/i.test(navigator.language) ? '账号与退出登录' : 'Account and sign out');
    link.className = 'api-nav-link';
    var header = document.querySelector('.app-header-nav') || document.querySelector('header');
    if (header) {
      var permissions = document.createElement('a');
      permissions.href = '/permissions';
      permissions.textContent = /^zh/i.test(navigator.language) ? '权限管理' : 'Permissions';
      permissions.className = 'api-nav-link';
      header.appendChild(permissions);
      header.appendChild(link);
    }
  });
}
initManagedSession();
