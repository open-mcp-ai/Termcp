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
        if (state && state.mode === 'managed' && (response.status === 401 || response.headers.get('X-Termcp-Reauthenticate') === '1')) location.assign('/login');
        return response;
      });
    });
  };
  context.then(function (state) {
    if (!state || !state.authenticated) return;
    function explainSharedPassword() {
      var form = document.getElementById('config-storage-password-form');
      if (!form || document.getElementById('managed-master-password-note')) return;
      var note = document.createElement('p');
      note.id = 'managed-master-password-note';
      note.className = 'conn-check-hint';
      note.textContent = /^zh/i.test(navigator.language)
        ? '此处修改的是同一主密码：同时更新 Admin 登录密码，并使所有登录会话失效。'
        : 'This changes the shared master password, including Admin login, and signs out all sessions.';
      form.insertBefore(note, form.firstChild);
    }
    if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', explainSharedPassword, { once: true });
    else explainSharedPassword();
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
