// TOML shown when creating a new remote profile. Lives here because the web UI
// is its only consumer: the server never writes this to disk (saving a profile
// always goes through PUT /api/connections/{name} with user-supplied TOML).
// Every field is commented out: it is a set of hints, not values. A real value
// here gets saved verbatim by anyone who forgets to edit it, producing a profile
// that dials a host that does not exist. The server rejects an empty host, so
// the user has to fill these in, which is the point.
var _connTemplateRemote = [
  '# termcp SSH config (TOML)',
  'kind = "remote"',
  'description = ""',
  '',
  '# Required: host and user of the machine to connect to.',
  '# host = "example.com"',
  '# user = "root"',
  '# port = 22',
  '',
  '# Authentication: set exactly one of password / private_key.',
  '# password = ""',
  '# private_key = """',
  '# -----BEGIN OPENSSH PRIVATE KEY-----',
  '# ...',
  '# -----END OPENSSH PRIVATE KEY-----',
  '# """',
  '# key_passphrase = ""',
  '',
  '# Host key checking. trust_unknown_host accepts any key on first contact;',
  '# pin known_hosts to verify against a specific key instead.',
  '# trust_unknown_host = false',
  '# known_hosts = ""',
  '# dial_timeout_seconds = 30',
  '',
  '# Optional: tunnel SSH through a SOCKS5 proxy.',
  '# proxy = "socks5://user:pass@127.0.0.1:1080"',
  '',
  '# Optional: review every AI write by default. With this on, sessions started',
  '# from this profile begin with review mode enabled — each command waits for a',
  '# human to accept it before any byte reaches the shell.',
  '# default_approval = true',
  '',
  '# Optional: bastion / ProxyJump chain (self-contained, inline).',
  '# [jump]',
  '# host = "bastion.example"',
  '# port = 22',
  '# user = ""',
  '# password = ""',
  '# proxy = ""',
  '# trust_unknown_host = true',
  '# [jump.jump]   # deeper hop',
  '# host = "..."',
  ''
].join('\n');

function _tomlEscape(v) { return v.replace(/\\/g,'\\\\').replace(/"/g,'\\"'); }
function _tomlVal(s) { return s ? ('"'+_tomlEscape(s)+'"') : '""'; }
function _hesc(s) { return String(s == null ? '' : s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;').replace(/'/g,'&#39;'); }

// section-aware TOML-ish parser: returns {root:{}, sections:{"jump":{}, "jump.jump":{}}}
function _parseConnTOML(toml) {
  var root = {}, sections = {}, cur = root;
  var lines = String(toml).replace(/\r/g,'').split('\n');
  var i = 0;
  while (i < lines.length) {
    var line = lines[i++].trim();
    if (line === '' || line.charAt(0) === '#') continue;
    var sec = line.match(/^\[([^\]]+)\]$/);
    if (sec) { cur = sections[sec[1].trim()] || (sections[sec[1].trim()] = {}); continue; }
    var kv = line.match(/^(\w+)\s*=\s*(.*)$/);
    if (!kv) continue;
    var key = kv[1], raw = kv[2];
    if (raw.charAt(0) === '"' && raw.charAt(1) === '"' && raw.charAt(2) === '"') {
      var body = raw.substring(3);
      if (body.indexOf('"""') >= 0) { cur[key] = body.replace(/""".*$/,'').replace(/\\"/g,'"'); continue; }
      var parts = [body];
      while (i < lines.length) {
        var l = lines[i++]; var idx = l.indexOf('"""');
        if (idx >= 0) { parts.push(l.substring(0, idx)); break; }
        parts.push(l);
      }
      cur[key] = parts.join('\n').replace(/\\"/g,'"').trim();
      continue;
    }
    var v = raw.trim();
    if (v === 'true') cur[key] = true;
    else if (v === 'false') cur[key] = false;
    else if (/^-?\d+$/.test(v)) cur[key] = parseInt(v, 10);
    else cur[key] = v.replace(/^"(.*)"$/,'$1').replace(/\\"/g,'"');
  }
  return { root: root, sections: sections };
}

function _newJumpCard() {
  return { host:'', port:22, user:'', auth:'password', password:'', pem:'', passphrase:'', proxy:'' };
}
function _sectionToJump(s) {
  return {
    host: s.host || '', port: s.port || 22, user: s.user || '',
    auth: s.private_key ? 'key' : 'password',
    password: s.password || '', pem: s.private_key || '',
    passphrase: s.key_passphrase || '', proxy: s.proxy || ''
  };
}
function _jumpsFromSections(sections) {
  var arr = [], name = 'jump';
  while (sections[name]) { arr.push(_sectionToJump(sections[name])); name += '.jump'; }
  return arr;
}

var jumpCards = [];
var jumpEditingIdx = -1; // index of the jump currently expanded in editor mode; -1 = none
var jumpEditingNew = false; // the editing jump is a brand-new, unsaved one (cancel removes it)
var connEntries = null;

function renderJumps() {
  var box = document.getElementById('conn-jumps');
  box.innerHTML = '';
  jumpCards.forEach(function(c, idx) {
    var card = document.createElement('div');
    card.className = 'jump-card';
    card.setAttribute('data-idx', idx);
    if (idx === jumpEditingIdx) {
      var authIsKey = c.auth === 'key';
      card.innerHTML =
        '<div class="jump-card-h"><span>' + escapeHtml(t('jump.label', { n: idx+1 })) + '</span>' +
          '<span class="jump-card-actions">' +
            '<button type="button" class="btn jump-save" style="font-size:0.72rem;padding:2px 8px" data-i18n="jump.save">save</button>' +
            '<button type="button" class="btn jump-cancel" style="font-size:0.72rem;padding:2px 8px" data-i18n="jump.cancel">cancel</button>' +
          '</span></div>' +
        '<div class="conn-field"><label data-i18n="jump.import">Import from host</label><select class="jump-import"><option value="" data-i18n="jump.import.select">— select entry —</option></select></div>' +
        '<div class="conn-row">' +
          '<div class="conn-field"><label data-i18n="modal.conn.field.host">Host</label><input type="text" data-f="host" value="' + _hesc(c.host) + '"></div>' +
          '<div class="conn-field"><label data-i18n="modal.conn.field.port">Port</label><input type="number" data-f="port" value="' + _hesc(c.port||22) + '"></div>' +
          '<div class="conn-field"><label data-i18n="modal.conn.field.user">User</label><input type="text" data-f="user" value="' + _hesc(c.user) + '"></div>' +
        '</div>' +
        '<div class="conn-field"><label data-i18n="modal.conn.field.auth">Auth</label><select data-f="auth"><option value="password" data-i18n="modal.conn.field.password"' + (authIsKey?'':' selected') + '>Password</option><option value="key" data-i18n="modal.conn.field.privateKey"' + (authIsKey?' selected':'') + '>Private Key</option></select></div>' +
        '<div class="conn-field j-pw" style="' + (authIsKey?'display:none':'') + '"><label data-i18n="modal.conn.field.password">Password</label><input type="password" data-f="password" value="' + _hesc(c.password) + '"></div>' +
        '<div class="conn-field j-pem" style="' + (authIsKey?'':'display:none') + '"><label data-i18n="modal.conn.field.privateKey">Private Key</label><textarea data-f="pem" rows="3">' + _hesc(c.pem) + '</textarea></div>' +
        '<div class="conn-field j-passphrase" style="' + (authIsKey?'':'display:none') + '"><label data-i18n="modal.conn.field.passphrase">Key Passphrase (optional)</label><input type="password" data-f="passphrase" value="' + _hesc(c.passphrase) + '"></div>' +
        '<div class="conn-field"><label data-i18n="jump.proxy">Proxy (optional)</label><input type="text" data-f="proxy" placeholder="socks5://..." value="' + _hesc(c.proxy) + '"></div>';
    } else {
      var host = c.host || t('jump.empty');
      var info = (c.user ? c.user + '@' : '') + host + (c.port && c.port !== 22 ? ':' + c.port : '');
      var tag = c.auth === 'key' ? 'key' : (c.password || c.pem ? 'pwd' : '');
      card.className = 'jump-card jump-summary';
      card.setAttribute('title', t('jump.editTip'));
      card.innerHTML =
        '<span class="jump-summary-idx">' + (idx+1) + '</span>' +
        '<span class="jump-summary-info">' + _hesc(info) + (tag ? ' <em>' + tag + '</em>' : '') + '</span>' +
        '<button type="button" class="btn jump-rm" style="font-size:0.72rem;padding:2px 8px" title="remove" data-i18n-title="jump.remove">remove</button>';
    }
    box.appendChild(card);
  });
  // Fill the data-i18n* markers baked into the cards above.
  applyI18n(box);
  _populateJumpImports();
}

function _populateJumpImports() {
  function fill() {
    document.querySelectorAll('.jump-import').forEach(function(sel) {
      var opts = '<option value="" data-i18n="jump.import.select">— select entry —</option>';
      (connEntries || []).forEach(function(c) {
        if (!c || c.kind !== 'remote') return;
        opts += '<option value="' + _hesc(c.name) + '">' + _hesc(c.name) + '</option>';
      });
      sel.innerHTML = opts;
      applyI18n(sel);
    });
  }
  if (connEntries) { fill(); return; }
  fetch(apiPath('/api/connections')).then(function(r){ return r.json(); }).then(function(j) {
    connEntries = j.connections || [];
    fill();
  }).catch(function(){});
}

function _jumpToLines(c, depth) {
  var sec = '', i;
  for (i = 0; i < depth; i++) { if (i) sec += '.'; sec += 'jump'; }
  var lines = ['[' + sec + ']'];
  lines.push('host = ' + _tomlVal(c.host));
  var p = parseInt(c.port, 10) || 22; if (p !== 22) lines.push('port = ' + p);
  lines.push('user = ' + _tomlVal(c.user));
  if (c.auth === 'key' || (c.pem && c.pem.trim())) lines.push('private_key = """\n' + c.pem + '\n"""');
  else lines.push('password = ' + _tomlVal(c.password));
  if (c.passphrase) lines.push('key_passphrase = ' + _tomlVal(c.passphrase));
  if (c.proxy) lines.push('proxy = ' + _tomlVal(c.proxy));
  return lines;
}

function _connFormToTOML() {
  // The internal profile's override carries only the settings it actually uses.
  // Emitting host/credentials for it would either be ignored on load (confusing)
  // or, worse, make a loopback profile look like it dials somewhere.
  var isInternal = editingConnName === 'internal';
  var lines = [isInternal ? 'kind = "internal"' : 'kind = "remote"'];
  if (!isInternal) {
    var h = document.getElementById('conn-f-host').value.trim(); lines.push('host = ' + _tomlVal(h));
    var p = parseInt(document.getElementById('conn-f-port').value, 10) || 22;
    if (p !== 22) lines.push('port = ' + p);
    lines.push('user = ' + _tomlVal(document.getElementById('conn-f-user').value.trim()));
    var auth = document.getElementById('conn-f-auth').value;
    if (auth === 'password') lines.push('password = ' + _tomlVal(document.getElementById('conn-f-password').value));
    else lines.push('private_key = """\n' + document.getElementById('conn-f-pem').value + '\n"""');
    var pp = document.getElementById('conn-f-passphrase').value;
    if (pp) lines.push('key_passphrase = ' + _tomlVal(pp));
  }
  var sh = document.getElementById('conn-f-shell').value.trim();
  if (sh) lines.push('default_shell = ' + _tomlVal(sh));
  // Written only when on: `default_approval = false` is the default, and a line
  // stating the default is noise in a file a human is meant to read.
  if (document.getElementById('conn-f-approval').checked) lines.push('default_approval = true');
  if (!isInternal) {
    var px = document.getElementById('conn-f-proxy').value.trim();
    if (px) lines.push('proxy = ' + _tomlVal(px));
  }
  lines.push('trust_unknown_host = true');
  for (var j = 0; j < jumpCards.length; j++) {
    lines = lines.concat(_jumpToLines(jumpCards[j], j + 1));
  }
  return lines.join('\n') + '\n';
}
function _connTOMLToForm(toml) {
  var parsed = _parseConnTOML(toml);
  var o = parsed.root;
  document.getElementById('conn-f-host').value = o.host || '';
  document.getElementById('conn-f-port').value = o.port || 22;
  document.getElementById('conn-f-user').value = o.user || '';
  document.getElementById('conn-f-password').value = o.password || '';
  document.getElementById('conn-f-pem').value = o.private_key || '';
  document.getElementById('conn-f-passphrase').value = o.key_passphrase || '';
  document.getElementById('conn-f-shell').value = o.default_shell || '';
  document.getElementById('conn-f-approval').checked = o.default_approval === true;
  document.getElementById('conn-f-proxy').value = o.proxy || '';
  document.getElementById('conn-f-auth').value = o.private_key ? 'key' : 'password';
  _connAuthChange();
  jumpCards = _jumpsFromSections(parsed.sections);
  jumpEditingIdx = -1; jumpEditingNew = false;
  renderJumps();
}
function _connAuthChange() {
  var a = document.getElementById('conn-f-auth').value;
  document.getElementById('conn-f-password-wrap').style.display = a === 'password' ? '' : 'none';
  document.getElementById('conn-f-pem-wrap').style.display = a === 'key' ? '' : 'none';
  document.getElementById('conn-f-passphrase-wrap').style.display = a === 'key' ? '' : 'none';
}
function _connToggleView() {
  var formView = document.getElementById('conn-form-view');
  var tomlView = document.getElementById('conn-config-view');
  var btn = document.getElementById('conn-toggle-view');
  var iconForm = btn.querySelector('.toggle-icon-form');
  var iconToml = btn.querySelector('.toggle-icon-toml');
  if (tomlView.style.display === 'none') {
    document.getElementById('conn-config').value = _connFormToTOML();
    formView.style.display = 'none';
    tomlView.style.display = '';
    iconForm.style.display = 'none';
    iconToml.style.display = '';
  } else {
    _connTOMLToForm(document.getElementById('conn-config').value);
    tomlView.style.display = 'none';
    formView.style.display = '';
    iconToml.style.display = 'none';
    iconForm.style.display = '';
  }
}
function _connGetBody() {
  if (document.getElementById('conn-config-view').style.display === 'none') {
    return _connFormToTOML();
  }
  return document.getElementById('conn-config').value;
}

function openConnModal(edit, name, kind) {
  // The internal profile is editable too: its settings are stored as an
  // override over the built-in defaults, which is how the loopback connection
  // gets a review default. It cannot be renamed or deleted (the name is how the
  // built-in connection is addressed), and the form hides those two controls
  // for it below.
  editingConnName = edit ? name : '';
  _connDirty = false;
  connEntries = null; // refresh import dropdown options
  document.getElementById('modal-conn-err').style.display = 'none';
  var isInternal = edit && kind === 'internal';
	var temporaryEl = document.getElementById('conn-temporary');
	var current = (window._lastConnections || []).find(function(c) { return c.name === name; });
	temporaryEl.checked = !!(edit && current && current.temporary);
	document.getElementById('conn-temporary-wrap').style.display = isInternal ? 'none' : '';
  document.getElementById('modal-conn-title').textContent = edit ? t('modal.conn.titleEdit') : t('modal.conn.titleAdd');
  document.getElementById('conn-delete').style.display = (edit && !isInternal) ? 'inline-block' : 'none';
  document.getElementById('conn-duplicate').style.display = (edit && !isInternal) ? 'inline-block' : 'none';
  var nameEl = document.getElementById('conn-name');
  nameEl.value = edit ? name : '';
  // Only the internal profile is pinned. Its name is the sole way to address the
  // built-in loopback connection, so renaming it would orphan every caller that
  // says ssh_config="internal"; the host/user/auth fields below are hidden for it
  // too, since the loopback dial ignores them. Every other profile CAN be
  // renamed — the name is the id used in `/api/connections/{name}`, and the save
  // handler below passes the old one as ?from= so the store moves the profile and
  // the sessions already holding it follow (see handler_conn.go and
  // TestRenamingAProfileFollowsLiveSessions). Left as isInternal, not !!edit:
  // pinning every edit is what removed renaming from the dialog.
  nameEl.readOnly = isInternal;
  var loopbackOnly = document.getElementById('conn-f-loopback-only');
  if (loopbackOnly) loopbackOnly.style.display = isInternal ? '' : 'none';
  var remoteFields = document.getElementById('conn-f-remote-fields');
  if (remoteFields) remoteFields.style.display = isInternal ? 'none' : '';
  // Proxy and the jump chain are the dial path; loopback dials this machine, so it
  // hides them too — but it keeps 默认 Shell / 默认审核, which are its own settings.
  var remoteExtra = document.getElementById('conn-f-remote-extra');
  if (remoteExtra) remoteExtra.style.display = isInternal ? 'none' : '';
  // Start in form view
  document.getElementById('conn-form-view').style.display = '';
  document.getElementById('conn-config-view').style.display = 'none';
  if (edit) {
    fetch(apiPath('/api/connections/') + encodeURIComponent(name)).then(function (r) {
      if (!r.ok) throw new Error(r.statusText);
      return r.text();
    }).then(function (t) {
      document.getElementById('conn-config').value = t;
      _connTOMLToForm(t);
    }).catch(function (err) {
      document.getElementById('modal-conn-err').textContent = String(err.message || err);
      document.getElementById('modal-conn-err').style.display = 'block';
    });
  } else {
    document.getElementById('conn-config').value = _connTemplateRemote;
    _connTOMLToForm(_connTemplateRemote);
  }
  resetConnPasswordVisibility();
  showModal('modal-conn');
}

document.getElementById('conn-f-auth').onchange = _connAuthChange;
document.getElementById('conn-toggle-view').onclick = function () { _connDirty = true; _connToggleView(); };
// Track unsaved edits inside the connection editor so leaving the page first asks.
var _connModalEl = document.getElementById('modal-conn');
if (_connModalEl) {
  _connModalEl.addEventListener('input', function () { _connDirty = true; });
  _connModalEl.addEventListener('change', function () { _connDirty = true; });
}

// ---- jump chain events ----
document.getElementById('conn-add-jump').onclick = function () {
  _connDirty = true;
  jumpCards.push(_newJumpCard());
  jumpEditingIdx = jumpCards.length - 1;
  jumpEditingNew = true;
  renderJumps();
};
var _jumpsBox = document.getElementById('conn-jumps');
_jumpsBox.addEventListener('input', function (e) {
  var t = e.target, f = t.getAttribute && t.getAttribute('data-f');
  if (!f) return;
  var card = t.closest('.jump-card');
  if (!card) return;
  var idx = parseInt(card.getAttribute('data-idx'), 10);
  if (isNaN(idx) || !jumpCards[idx]) return;
  var c = jumpCards[idx];
  if (t.type === 'checkbox') c[f] = t.checked;
  else c[f] = t.value;
  if (f === 'auth') {
    card.querySelector('.j-pw').style.display = t.value === 'password' ? '' : 'none';
    card.querySelector('.j-pem').style.display = t.value === 'key' ? '' : 'none';
    card.querySelector('.j-passphrase').style.display = t.value === 'key' ? '' : 'none';
  }
});
_jumpsBox.addEventListener('change', function (e) {
  if (!e.target.classList.contains('jump-import')) return;
  var name = e.target.value;
  e.target.value = '';
  if (!name) return;
  var card = e.target.closest('.jump-card');
  var idx = card ? parseInt(card.getAttribute('data-idx'), 10) : NaN;
  if (isNaN(idx)) return;
  fetch(apiPath('/api/connections/') + encodeURIComponent(name)).then(function (r) {
    if (!r.ok) throw new Error(r.statusText);
    return r.text();
  }).then(function (t) {
    var parsed = _parseConnTOML(t);
    var root = _sectionToJump(parsed.root);
    var chain = _jumpsFromSections(parsed.sections);
    jumpCards = jumpCards.slice(0, idx).concat([root], chain, jumpCards.slice(idx + 1));
    jumpEditingIdx = idx; // keep editor open on the imported hop
    jumpEditingNew = false;
    renderJumps();
  }).catch(function (err) {
    var e2 = document.getElementById('modal-conn-err');
    e2.textContent = String(err.message || err);
    e2.style.display = 'block';
  });
});
_jumpsBox.addEventListener('click', function (e) {
  var card = e.target.closest('.jump-card');
  var idx = card ? parseInt(card.getAttribute('data-idx'), 10) : NaN;
  if (isNaN(idx)) return;
  if (e.target.classList.contains('jump-rm')) {
    _connDirty = true;
    jumpCards.splice(idx, 1);
    if (jumpEditingIdx === idx) { jumpEditingIdx = -1; jumpEditingNew = false; }
    else if (jumpEditingIdx > idx) { jumpEditingIdx -= 1; }
    renderJumps();
    return;
  }
  if (e.target.classList.contains('jump-save')) {
    _connDirty = true;
    jumpEditingIdx = -1; jumpEditingNew = false;
    renderJumps();
    return;
  }
  if (e.target.classList.contains('jump-cancel')) {
    _connDirty = true;
    if (jumpEditingNew && jumpEditingIdx === idx) {
      jumpCards.splice(idx, 1);
    }
    jumpEditingIdx = -1; jumpEditingNew = false;
    renderJumps();
    return;
  }
  // click on a summary card (not on a button) → expand editor
  if (card.classList.contains('jump-summary') && jumpEditingIdx !== idx) {
    jumpEditingIdx = idx; jumpEditingNew = false;
    renderJumps();
  }
});

function _toggleEye(btn) {
  var inp = btn.parentElement.querySelector('input');
  var show = inp && inp.type === 'password';
  if (inp) inp.type = show ? 'text' : 'password';
  var openEl = btn.querySelector('.eye-open');
  var closedEl = btn.querySelector('.eye-closed');
  if (openEl) openEl.style.display = show ? 'none' : '';
  if (closedEl) closedEl.style.display = show ? '' : 'none';
}

/** 重置连接对话框内所有密码输入框为默认隐藏状态。
 *  眼睛按钮切换的是 input.type 与图标 display，这些状态保存在常驻 DOM 上；
 *  若不在重新打开对话框时复位，上次“显示明文”的选择会残留到下次打开。 */
function resetConnPasswordVisibility() {
  var modal = document.getElementById('modal-conn');
  if (!modal) return;
  modal.querySelectorAll('.psw-eye').forEach(function (btn) {
    var inp = btn.parentElement.querySelector('input');
    if (inp) inp.type = 'password';
    var openEl = btn.querySelector('.eye-open');
    var closedEl = btn.querySelector('.eye-closed');
    if (openEl) openEl.style.display = '';
    if (closedEl) closedEl.style.display = 'none';
  });
}
document.querySelectorAll('.psw-eye').forEach(function(btn) {
  btn.onclick = function() { _toggleEye(this); };
});

document.getElementById('modal-conn-close').onclick = function () { _connDirty = false; hideModal('modal-conn'); };
document.getElementById('conn-test').onclick = function () {
  var err = document.getElementById('modal-conn-err');
  var out = document.getElementById('conn-test-result');
  err.style.display = 'none';
  out.style.display = 'none';
  var btn = this;
  var body = _connGetBody();
  btn.disabled = true;
  btn.textContent = t('test.testing');
  fetch(apiPath('/api/connections/test'), { method: 'POST', headers: { 'Content-Type': 'text/plain; charset=utf-8' }, body: body })
    .then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error(t || r.status); });
      return r.json();
    })
    .then(function (j) {
      out.style.display = 'block';
      if (j.ok) {
        out.textContent = t('test.ok', { ms: j.duration_ms || 0 });
        out.style.color = '#1a7f37';
      } else {
        out.textContent = t('test.failed', { msg: j.error || t('test.connectionFailed') });
        out.style.color = '#cf222e';
      }
    })
    .catch(function (e) {
      out.style.display = 'block';
      out.textContent = t('test.failed', { msg: String(e.message || e) });
      out.style.color = '#cf222e';
    })
    .finally(function () {
      btn.disabled = false;
      btn.textContent = t('common.test');
    });
};
document.getElementById('conn-save').onclick = function () {
  var err = document.getElementById('modal-conn-err');
  err.style.display = 'none';
  var name = document.getElementById('conn-name').value.trim();
  var body = _connGetBody();
  if (!name) { err.textContent = t('err.name.required'); err.style.display = 'block'; return; }
  if (!/^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$/.test(name)) {
    err.textContent = t('err.name.invalid');
    err.style.display = 'block';
    return;
  }
  // Check for duplicate; when editing, exclude the original name.
  var tiles = document.querySelectorAll('.conn-tile');
  for (var i = 0; i < tiles.length; i++) {
    var n = tiles[i].getAttribute('data-conn-name');
    if (n && n.toLowerCase() === name.toLowerCase() && n.toLowerCase() !== editingConnName.toLowerCase()) {
      err.textContent = t('err.name.exists', { name: name });
      err.style.display = 'block';
      return;
    }
  }
  var url = apiPath('/api/connections/') + encodeURIComponent(name);
  var params = new URLSearchParams();
  params.set('temporary', document.getElementById('conn-temporary').checked ? 'true' : 'false');
  if (editingConnName && editingConnName !== name) params.set('from', editingConnName);
  url += '?' + params.toString();
  fetch(url, { method: 'PUT', headers: { 'Content-Type': 'text/plain; charset=utf-8' }, body: body })
    .then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error(t || r.status); });
      _connDirty = false;
      hideModal('modal-conn');
      loadConnections();
    })
    .catch(function (e) { err.textContent = String(e.message || e); err.style.display = 'block'; });
};
document.getElementById('conn-delete').onclick = function () {
  var name = document.getElementById('conn-name').value.trim();
  if (!name) return;
  confirmDialog({
    title: t('dialog.conn.delete.title'),
    message: t('dialog.conn.delete.msg', { name: name }),
    okText: t('common.delete'),
    danger: true
  }).then(function (ok) {
    if (!ok) return;
    doDeleteConnection(name);
  });
};
function doDeleteConnection(name) {
  fetch(apiPath('/api/connections/') + encodeURIComponent(name), { method: 'DELETE' })
    .then(function (r) {
    if (!r.ok) return r.text().then(function (t) { throw new Error(t || r.status); });
      _connDirty = false;
      hideModal('modal-conn');
      loadConnections();
    })
    .catch(function (e) {
      var err = document.getElementById('modal-conn-err');
    err.textContent = String(e.message || e);
    err.style.display = 'block';
  });
};
document.getElementById('conn-duplicate').onclick = function () {
  var nameEl = document.getElementById('conn-name');
  // Switch into create mode while keeping the current form/TOML values.
  editingConnName = '';
  nameEl.value = '';
  nameEl.readOnly = false;
  document.getElementById('modal-conn-title').textContent = t('modal.conn.titleAdd');
  document.getElementById('conn-delete').style.display = 'none';
  document.getElementById('conn-duplicate').style.display = 'none';
  var err = document.getElementById('modal-conn-err');
  if (err) { err.style.display = 'none'; err.textContent = ''; }
  try { nameEl.focus(); } catch (e) {}
};

document.getElementById('conn-import-open').onclick = function (e) {
  e.stopPropagation();
  document.getElementById('conn-import-file').value = '';
  document.getElementById('conn-import-temporary').checked = false;
  document.getElementById('conn-import-err').style.display = 'none';
  document.getElementById('conn-import-result').style.display = 'none';
  showModal('modal-conn-import');
};
document.getElementById('conn-import-close').onclick = function () { hideModal('modal-conn-import'); };
document.getElementById('conn-import-run').onclick = function () {
  var file = document.getElementById('conn-import-file').files[0];
  var err = document.getElementById('conn-import-err');
  err.style.display = 'none';
  document.getElementById('conn-import-result').style.display = 'none';
  if (!file) { err.textContent = t('conn.batch.selectFile'); err.style.display = 'block'; return; }
  var btn = this;
  btn.disabled = true;
  var temporary = document.getElementById('conn-import-temporary').checked;
  // The file is opaque to the UI; the backend parses and validates its TOML.
  fetch(apiPath('/api/connections/batch?temporary=') + temporary, {
    method: 'POST', headers: { 'Content-Type': 'application/toml' }, body: file
  }).then(function (r) {
    if (!r.ok) return r.text().then(function (message) { throw new Error(message || String(r.status)); });
    return r.json();
  }).then(function (result) {
    loadConnections();
    if (result.renamed && result.renamed.length) {
      var lines = [t('conn.batch.imported', { count: result.imported })];
      result.renamed.forEach(function (entry) {
        lines.push(t('conn.batch.renamed', { from: entry.from, to: entry.to }));
      });
      var output = document.getElementById('conn-import-result');
      output.textContent = lines.join('\n');
      output.style.display = 'block';
      document.getElementById('conn-import-file').value = '';
    } else {
      hideModal('modal-conn-import');
      showCopyToast(t('conn.batch.imported', { count: result.imported }));
    }
  }).catch(function (e) {
    err.textContent = String(e.message || e);
    err.style.display = 'block';
  }).finally(function () { btn.disabled = false; });
};
/* The TOML download shared by the batch bar's export. The backend composes the
   document either way (names filter or all), so the browser stays a pipe for
   opaque bytes. */
function downloadConnectionsToml(names) {
  var path = apiPath('/api/connections/batch');
  if (names && names.length) path += '?names=' + names.map(encodeURIComponent).join(',');
  return fetch(path).then(function (r) {
    if (!r.ok) return r.text().then(function (message) { throw new Error(message || String(r.status)); });
    return r.blob();
  }).then(function (blob) {
    var url = URL.createObjectURL(blob);
    var link = document.createElement('a');
    link.href = url;
    link.download = 'termcp-connections.toml';
    document.body.appendChild(link);
    link.click();
    link.remove();
    setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
  });
}

/* ---- NetHub selection actions ---------------------------------------------
 * The toolbar's switch (nethub-select-toggle) re-aims the node cards: off, a
 * card click connects; on, it ticks the card and the batch bar appears. With a
 * selection, three actions exist — open a session per host, export exactly the
 * selected profiles (the same TOML shape as export-all, so it imports back),
 * and a comma-batch delete with per-name results. The selection state itself
 * lives in dialogs.js; this block binds the controls and does the work. */

function selectedNodeNames() {
  if (!nodeSelectModeOn()) return [];
  var names = selectableNodeNames().filter(function (n) { return _nodeSelIds.has(n); });
  // Name order, which is the order the grid shows them in.
  names.sort(function (a, b) { return a.localeCompare(b); });
  return names;
}

function openSelectedNodes() {
  var names = selectedNodeNames();
  if (!names.length) return;
  /* In drawer mode the windows open under the panel, so the panel leaves the
     way a single card click's does — staying up would cover exactly what just
     launched. */
  if (netHubIsDrawer()) setNetHubCollapsed(true, false);
  var settled = 0;
  var failed = [];
  names.forEach(function (name, i) {
    /* Staggered, not simultaneous: each dial opens a window, and a bunch that
       lands in the same instant piles into one unusable heap. 150ms is a
       cascade, not a queue. */
    setTimeout(function () {
      startSessionAndOpenShell(name, null)
        .catch(function (err) {
          if (err && err.name === 'AbortError') return;
          console.error(err);
          failed.push(name);
        })
        .finally(function () {
          settled++;
          if (settled !== names.length) return;
          // The ticks are spent: clear the selection, keep the mode on so
          // another batch can be picked without reaching for the switch.
          _nodeSelIds.clear();
          renderConnGrid(window._lastConnections || [], connBannerText());
          if (failed.length) showCopyToast(t('nethub.batch.openFailed', { count: failed.length, msg: failed[0] }));
        });
    }, i * 150);
  });
}

/* The built-in profile rides along in any selection (open works for it), but
   export and delete have nothing to do with it — the server refuses both, so
   the buttons refuse first and say why instead of shipping a request that can
   only partly succeed. */
function selectionHasInternal(names) {
  for (var i = 0; i < names.length; i++) {
    if (String(names[i]).toLowerCase() === 'internal') return true;
  }
  return false;
}

function exportSelectedNodes() {
  var names = selectedNodeNames();
  if (!names.length) return;
  if (selectionHasInternal(names)) {
    showCopyToast(t('nethub.batch.exportInternal'));
    return;
  }
  downloadConnectionsToml(names).catch(function (e) {
    showCopyToast(t('conn.batch.exportFailed', { msg: String(e.message || e) }));
  });
}

// One request for every id (comma-separated path, like the session routes):
// the server reports per-id outcomes, and <resource>_not_found counts as
// cleared here — another client already got there. Sessions and connections
// share the shape, so they share the helper; only the resource segment differs.
function deleteResourcesBatch(resource, ids) {
  var path = apiPath('/api/') + resource + '/' + ids.map(encodeURIComponent).join(',');
  return fetch(path, { method: 'DELETE' }).then(function (r) {
    if (r.ok && r.status !== 204) {
      return r.json().then(function (j) { return (j && j.results) || []; });
    }
    if (r.status === 204 || r.status === 404) {
      return ids.map(function (id) { return { id: id, ok: true }; });
    }
    return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
  });
}

function deleteSelectedNodes() {
  var names = selectedNodeNames();
  if (!names.length) return;
  if (selectionHasInternal(names)) {
    showCopyToast(t('nethub.batch.deleteInternal'));
    return;
  }
  // Deleting a profile never closes the sessions dialed from it — a session
  // owns its already-open connection — so the dialog says so when the
  // selection carries any.
  var withSessions = 0;
  names.forEach(function (n) { if (runningSessionsForNode(n).length > 0) withSessions++; });
  var message = tCount('nethub.batch.deleteMsg.one', 'nethub.batch.deleteMsg.other', { count: names.length });
  if (withSessions > 0) message += ' ' + t('nethub.batch.deleteSessionsHint', { count: withSessions });
  confirmDialog({
    title: t('nethub.batch.deleteTitle'),
    message: message,
    okText: t('batch.del.ok', { count: names.length }),
    danger: true
  }).then(function (ok) {
    if (!ok) return;
    deleteResourcesBatch('connections', names).then(function (results) {
      var failed = [];
      (results || []).forEach(function (r) {
        if (r.ok || r.code === 'connection_not_found') {
          _nodeSelIds.delete(r.id);
        } else {
          failed.push(r.id + ': ' + (r.error || r.code || 'failed'));
        }
      });
      // loadConnections refetches, re-renders, prunes the selection and syncs
      // the batch bar — one refresh path, the same one every other editor
      // action already uses.
      loadConnections();
      if (failed.length) showCopyToast(t('toast.delete.failed', { msg: failed.join('; ') }));
    }).catch(function (err) {
      showCopyToast(t('toast.delete.failed', { msg: String(err.message || err) }));
    });
  });
}

// The play split-key's dropdown, as plain functions: the NetHub wiring's
// Escape handler folds it before it folds the selection mode, so the state has
// to be reachable from there.
function nodeOpenMenuIsOpen() {
  var menu = document.getElementById('nethub-open-menu');
  return !!(menu && !menu.hidden);
}

function closeNodeOpenMenu() {
  var wasOpen = nodeOpenMenuIsOpen();
  var menu = document.getElementById('nethub-open-menu');
  var caret = document.getElementById('nethub-open-caret');
  if (menu) menu.hidden = true;
  if (caret) caret.setAttribute('aria-expanded', 'false');
  return wasOpen;
}

function toggleNodeOpenMenu() {
  var menu = document.getElementById('nethub-open-menu');
  if (!menu) return;
  menu.hidden = !menu.hidden;
  var caret = document.getElementById('nethub-open-caret');
  if (caret) caret.setAttribute('aria-expanded', String(!menu.hidden));
}

// The toolbar's wiring. dialogs.js owns the selection state; this block only
// binds the controls, the same split the drawer's collapse uses (see the
// NetHub wiring below).
(function () {
  var toggle = document.getElementById('nethub-select-toggle');
  if (!toggle) return;
  toggle.addEventListener('click', function (e) {
    e.stopPropagation();
    setNodeSelectMode(!nodeSelectModeOn());
  });
  var invert = document.getElementById('nethub-sel-invert');
  if (invert) invert.addEventListener('click', function (e) {
    e.stopPropagation();
    selectableNodeNames().forEach(function (n) {
      if (_nodeSelIds.has(n)) _nodeSelIds.delete(n);
      else _nodeSelIds.add(n);
    });
    renderConnGrid(window._lastConnections || [], connBannerText());
  });
  // The play split-key: the wide half opens every ticked host, the caret
  // unfolds export and delete. Any click outside the menu folds it; Escape
  // folds it here first, before the mode-exit handler below folds the mode.
  var caret = document.getElementById('nethub-open-caret');
  if (caret) caret.addEventListener('click', function (e) {
    e.preventDefault();
    e.stopPropagation();
    toggleNodeOpenMenu();
  });
  document.addEventListener('click', function (e) {
    if (!nodeOpenMenuIsOpen()) return;
    if (e.target.closest('#nethub-open-menu') || e.target.closest('#nethub-open-caret')) return;
    closeNodeOpenMenu();
  });
  var menuEl = document.getElementById('nethub-open-menu');
  if (menuEl) menuEl.addEventListener('click', function (e) { e.stopPropagation(); });
  var menuExport = document.getElementById('nethub-menu-export');
  if (menuExport) menuExport.addEventListener('click', function () {
    closeNodeOpenMenu();
    exportSelectedNodes();
  });
  var menuDelete = document.getElementById('nethub-menu-delete');
  if (menuDelete) menuDelete.addEventListener('click', function () {
    closeNodeOpenMenu();
    deleteSelectedNodes();
  });
  var openBtn = document.getElementById('nethub-batch-open');
  if (openBtn) openBtn.addEventListener('click', function (e) { e.stopPropagation(); openSelectedNodes(); });
})();

/* The host list's launch-options dialog. Its entry point is a host card, so the
   window it starts is centred like the card's own click — see dialogs.js. The
   dialog outlives the click that opened it, so a pointer position captured here
   would only ever be a stale coordinate from behind a modal backdrop. */
function openStartModal(connName) {
  startConnName = connName;
  document.getElementById('modal-start-err').style.display = 'none';
  document.getElementById('start-ssh-config').value = connName;
  var startNameEl = document.getElementById('start-name');
  if (startNameEl) startNameEl.value = connName;
  document.getElementById('start-title').textContent = t('modal.start.titleConnect', { name: connName });
  document.getElementById('start-cmd').value = '';
  document.getElementById('start-mode').value = 'pty';
  showModal('modal-start');
}
document.getElementById('modal-start-close').onclick = function () { hideModal('modal-start'); };
document.getElementById('start-run').onclick = function () {
  var err = document.getElementById('modal-start-err');
  err.style.display = 'none';
  var cmd = document.getElementById('start-cmd').value.trim();
  var sname = document.getElementById('start-name').value.trim();
  // Close immediately on submit: the pending terminal window already shows
  // connection progress (spinner). On failure the dialog reopens with the
  // error so inputs stay editable for a retry.
  hideModal('modal-start');
  startSessionAndOpenShell(document.getElementById('start-ssh-config').value || startConnName, null, {
    command: cmd,
    mode: document.getElementById('start-mode').value,
    name: sname || undefined
  })
    .catch(function (e) {
      err.textContent = String(e.message || e);
      err.style.display = 'block';
      showModal('modal-start');
    });
};

// Closes the terminal windows and drops the selection of every id that is gone,
// then hands (clearedCount, failedEntries) to onDone. failedEntries carry
// {id, error} for the failure toast.
function settleSessionDeletes(results, banner, onDone) {
  var failed = [];
  (results || []).forEach(function (r) {
    if (r.ok || r.code === 'session_not_found') {
      var w = getShellWindowBySid(r.id);
      if (w) closeShellWindow(w);
      _sessionRegions.forEach(function (region) { region.ids.delete(r.id); });
    } else {
      failed.push(r);
    }
  });
  if (banner) setLoadBanner(banner, '');
  onDone((results || []).length - failed.length, failed);
}

function sessionFailuresText(failed) {
  return failed.map(function (r) { return r.id + (r.error ? ': ' + r.error : ''); }).join('; ');
}

/** Delete one plate's selected sessions after confirming.
 *
 *  Both plates share this: the only difference is which ids they own, and the
 *  wording of the dialog is the same operation either way. Taking the ids from
 *  the caller (rather than from a global selection) is what keeps the two
 *  regions independent.
 */
function deleteSelectedInRegion(region) {
  var snapshot = window._lastSessionsSnapshot || [];
  var targets = sessionsForRegion(region, snapshot).filter(function (s) { return region.ids.has(s.id); });
  if (!targets.length) {
    showCopyToast(t('toast.sessions.none'));
    return;
  }
  var msg = tCount('batch.del.msg.one', 'batch.del.msg.other', { count: targets.length });
  confirmDialog({
    title: t('batch.del.dialog'),
    message: msg,
    okText: t('batch.del.ok', { count: targets.length }),
    danger: true
  }).then(function (ok) {
    if (!ok) return;
    var banner = document.getElementById('session-load-banner');
    if (banner) setLoadBanner(banner, tCount('banner.deleting.one', 'banner.deleting.other', { count: targets.length }));
    return deleteResourcesBatch('sessions', targets.map(function (s) { return s.id; })).then(function (results) {
      settleSessionDeletes(results, banner, function (cleared, failed) {
        if (failed.length) showCopyToast(t('toast.delete.failed', { msg: sessionFailuresText(failed) }));
        else showCopyToast(tCount('toast.session.deleted.one', 'toast.session.deleted.other', { count: cleared }));
        loadForwards();
        startUIWebSocket();
        renderSessionGrid('');
      });
    }).catch(function (err) {
      if (banner) setLoadBanner(banner, t('toast.delete.failed', { msg: String(err.message || err) }));
      renderSessionGrid('');
    });
  });
}

// Each plate's trash and select-all act on that plate's own selection.
_sessionRegions.forEach(function (region) {
  var delBtn = document.getElementById(region.delId);
  if (delBtn) {
    delBtn.onclick = function (e) {
      e.stopPropagation();
      deleteSelectedInRegion(region);
    };
  }
  var selAllBtn = document.getElementById(region.selAllId);
  if (selAllBtn) {
    selAllBtn.onclick = function (e) {
      e.stopPropagation();
      var members = sessionsForRegion(region, window._lastSessionsSnapshot || []);
      var allSelected = members.length > 0 && members.every(function (s) { return region.ids.has(s.id); });
      if (allSelected) members.forEach(function (s) { region.ids.delete(s.id); });
      else members.forEach(function (s) { region.ids.add(s.id); });
      renderSessionGrid('');
    };
  }
});

// Collapse/expand each section with localStorage persistence. The archive is a
// plate rather than a section header, so only its header id differs; its body is
// the same .section-body and it shares this one implementation.
['entries', 'sessions', 'archive'].forEach(function (key) {
  var header = document.getElementById(key === 'archive' ? 'sec-archive-header' : 'sec-' + key);
  var body = document.getElementById('sec-' + key + '-body');
  if (!header || !body) return;
  var stored = localStorage.getItem('termcp.section.' + key);
  var collapsed = stored
    ? stored === 'collapsed'
    : header.getAttribute('aria-expanded') !== 'true';
  header.setAttribute('aria-expanded', String(!collapsed));
  body.classList.toggle('collapsed', collapsed);
  header.addEventListener('click', function (e) {
    if (e.target.closest('.icon-btn')) return;
    var collapsed = header.getAttribute('aria-expanded') === 'false';
    header.setAttribute('aria-expanded', String(collapsed));  // toggle: now expanded
    if (collapsed) {
      body.classList.remove('collapsed');
      localStorage.setItem('termcp.section.' + key, 'expanded');
    } else {
      body.classList.add('collapsed');
      localStorage.setItem('termcp.section.' + key, 'collapsed');
    }
  });
  header.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); header.click(); }
  });
});

function reasonLabel(r) {
  if (!r) return '';
  switch (String(r)) {
    case 'explicit': return t('reason.explicit');
    case 'crash': return t('reason.crash');
    case 'exited': return t('reason.exited');
    case 'shutdown': return t('reason.shutdown');
    default: return String(r);
  }
}

loadForwards();
loadNotifications();
loadConnections();
startUIWebSocket();

// The header labels the build (the string `termcp -version` prints first). The
// page is static, so the number is fetched once; a failure just leaves it blank.
fetch(apiPath('/api/version'))
  .then(function (r) { return r.ok ? r.json() : null; })
  .then(function (j) {
    var el = document.getElementById('app-version');
    if (el && j && j.version) el.textContent = j.version;
  })
  .catch(function () {});

// ---- NetHub wiring -------------------------------------------------------
// The resource sidebar is a layout column on desktop and the same left overlay
// it always was on a phone; both states are one boolean in dialogs.js
// (setNetHubCollapsed), so this block only binds the controls. Nothing here
// decides a width, and no click opens a modal: collapsing is a layout state.
(function () {
  var trigger = document.getElementById('open-host-drawer');
  var body = document.getElementById('sec-entries-body');
  var add = document.getElementById('conn-add');
  if (!trigger || !body) return;

  var scrim = document.createElement('div');
  scrim.className = 'drawer-scrim';
  scrim.setAttribute('aria-hidden', 'true');
  document.body.appendChild(scrim);
  /* The scrim is created here, after dialogs.js applied the initial state at
     load (it cannot exist before this expression runs). Re-apply that state so
     a narrow first paint with a stored desktop preference shows the overlay
     AND its scrim together — otherwise the body opens behind a scrim that
     never learns it is open. */
  setNetHubCollapsed(currentNetHubCollapsed(), false);

  /* The header button and the rail's expand key run the same toggle, so the
     control is one decision with two doors. Capture phase keeps a click from
     leaking into a shared section listener. */
  trigger.addEventListener('click', function (e) {
    e.stopImmediatePropagation();
    e.preventDefault();
    toggleNetHub();
  }, true);
  trigger.addEventListener('keydown', function (e) {
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.stopImmediatePropagation();
    e.preventDefault();
    toggleNetHub();
  }, true);

  /* The scrim and Escape belong to the overlay state only; on desktop neither
     element is in the layout, so they cannot close a sidebar the user wants. */
  scrim.addEventListener('click', function () { setNetHubCollapsed(true, false); });
  document.addEventListener('keydown', function (e) {
    if (e.key !== 'Escape') return;
    /* Escape folds the innermost layer first: the open dropdown, then the
       selection mode, then the drawer. Leaving the mode is the smaller undo,
       and closing the panel would also take the batch keys out of sight while
       their selection is still being picked. */
    if (closeNodeOpenMenu()) return;
    if (nodeSelectModeOn()) { setNodeSelectMode(false); return; }
    if (netHubIsDrawer() && !netHubCollapsed()) setNetHubCollapsed(true, false);
  });
  /* Choosing a node from the overlay should reveal what it opened rather than
     leave the panel covering the page it just put a window on. A tick is not a
     choice of a window: in selection mode the panel stays up so several hosts
     can be ticked in one look. */
  body.addEventListener('click', function (e) {
    if (!e.target.closest('.conn-tile') || !netHubIsDrawer()) return;
    if (nodeSelectModeOn()) return;
    setNetHubCollapsed(true, false);
  });
  if (add) {
    add.setAttribute('aria-label', t('conn.aria.add'));
    add.title = t('conn.aria.add');
    add.addEventListener('click', function () { openConnModal(false, ''); });
    add.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); openConnModal(false, ''); }
    });
  }

  /* The terminal header's switcher button opens the panel too, so a full-screen
     terminal can reach another node without going home first. */
  window.termcpToggleEntriesDrawer = function (forceOpen) {
    setNetHubCollapsed(forceOpen === undefined ? !netHubCollapsed() : !forceOpen, false);
    return true;
  };

  /* A node's state is read from the session list and the pending windows, so the
     cards have to be repainted when either changes. The session frame is the
     shared wake signal for both (see ui-socket.js), and this is the hook that
     keeps a card from claiming "offline" under a running session. */
  window.renderNodeStates = function () {
    renderConnGrid(window._lastConnections || [], connBannerText());
  };
})();
