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
  fetch('/api/connections').then(function(r){ return r.json(); }).then(function(j) {
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
  document.getElementById('modal-conn-title').textContent = edit ? t('modal.conn.titleEdit') : t('modal.conn.titleAdd');
  document.getElementById('conn-delete').style.display = (edit && !isInternal) ? 'inline-block' : 'none';
  document.getElementById('conn-duplicate').style.display = (edit && !isInternal) ? 'inline-block' : 'none';
  var nameEl = document.getElementById('conn-name');
  nameEl.value = edit ? name : '';
  // The name is the profile's identity and the id used in `/api/connections/{name}`.
  // For internal it is also the only way to address the built-in connection, so
  // it is pinned; the host/user/auth fields below are hidden since the loopback
  // dial ignores them.
  nameEl.readOnly = !!edit;
  var loopbackOnly = document.getElementById('conn-f-loopback-only');
  if (loopbackOnly) loopbackOnly.style.display = isInternal ? '' : 'none';
  var remoteFields = document.getElementById('conn-f-remote-fields');
  if (remoteFields) remoteFields.style.display = isInternal ? 'none' : '';
  // Start in form view
  document.getElementById('conn-form-view').style.display = '';
  document.getElementById('conn-config-view').style.display = 'none';
  if (edit) {
    fetch('/api/connections/' + encodeURIComponent(name)).then(function (r) {
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
  fetch('/api/connections/' + encodeURIComponent(name)).then(function (r) {
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
  fetch('/api/connections/test', { method: 'POST', headers: { 'Content-Type': 'text/plain; charset=utf-8' }, body: body })
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
  var url = '/api/connections/' + encodeURIComponent(name);
  if (editingConnName && editingConnName !== name) {
    url += '?from=' + encodeURIComponent(editingConnName);
  }
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
  fetch('/api/connections/' + encodeURIComponent(name), { method: 'DELETE' })
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

function openStartModal(connName, clickEvent) {
  startConnName = connName;
  document.getElementById('modal-start-err').style.display = 'none';
  document.getElementById('start-ssh-config').value = connName;
  var startNameEl = document.getElementById('start-name');
  if (startNameEl) startNameEl.value = connName;
  document.getElementById('start-title').textContent = t('modal.start.titleConnect', { name: connName });
  document.getElementById('start-cmd').value = '';
  document.getElementById('start-mode').value = 'pty';
  showModal('modal-start');
  window._startClickEvt = clickEvent;
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
  startSessionAndOpenShell(document.getElementById('start-ssh-config').value || startConnName, window._startClickEvt, {
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

// Session selection toolbar actions
document.getElementById('btn-clear-dead').onclick = function (e) {
  e.stopPropagation();
  var dead = (window._lastSessionsSnapshot || []).filter(function (s) { return s && s.id && s.status !== 'running'; });
  if (!dead.length) { showCopyToast(t('toast.dead.none')); return; }
  confirmDialog({
    title: t('section.batch.clearDead'),
    message: tCount('clear.dead.msg.one', 'clear.dead.msg.other', { count: dead.length }),
    okText: t('clear.dead.ok', { count: dead.length }),
    danger: true
  }).then(function (ok) {
    if (!ok) return;
    var banner = document.getElementById('session-load-banner');
    if (banner) setLoadBanner(banner, tCount('banner.clearing.one', 'banner.clearing.other', { count: dead.length }));
    return dead.reduce(function (p, s) {
      return p.then(function () {
        return fetch('/api/sessions/' + encodeURIComponent(s.id), { method: 'DELETE' })
          .then(function (r) {
            if (r.ok || r.status === 204 || r.status === 404) {
              var w = getShellWindowBySid(s.id);
              if (w) closeShellWindow(w);
              _selectedSessionIds.delete(s.id);
              return;
            }
            return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
          });
      });
    }, Promise.resolve()).then(function () {
      if (banner) setLoadBanner(banner, '');
      showCopyToast(tCount('toast.dead.cleared.one', 'toast.dead.cleared.other', { count: dead.length }));
      loadForwards();
      startUIWebSocket();
      renderSessionGrid(window._lastSessionsSnapshot || [], '');
    }).catch(function (err) {
      if (banner) setLoadBanner(banner, t('toast.clear.failed', { msg: String(err.message || err) }));
      renderSessionGrid(window._lastSessionsSnapshot || [], '');
    });
  });
};

// Select-all / clear-selection button
var btnSelAll = document.getElementById('batch-sel-all');
if (btnSelAll) {
  btnSelAll.onclick = function (e) {
    e.stopPropagation();
    var snapshot = (window._lastSessionsSnapshot || []).filter(function (s) { return s && s.id; });
    var allSelected = snapshot.length > 0 && snapshot.every(function (s) { return _selectedSessionIds.has(s.id); });
    if (allSelected) snapshot.forEach(function (s) { _selectedSessionIds.delete(s.id); });
    else snapshot.forEach(function (s) { _selectedSessionIds.add(s.id); });
    renderSessionGrid(window._lastSessionsSnapshot || [], '');
  };
}
var btnBatchDel = document.getElementById('batch-del-btn');
if (btnBatchDel) {
  btnBatchDel.onclick = function () {
    var snapshot = window._lastSessionsSnapshot || [];
    var targets = snapshot.filter(function (s) { return s && s.id && _selectedSessionIds.has(s.id); });
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
      return targets.reduce(function (p, s) {
        return p.then(function () {
          return fetch('/api/sessions/' + encodeURIComponent(s.id), { method: 'DELETE' })
            .then(function (r) {
              if (r.ok || r.status === 204 || r.status === 404) {
                var w = getShellWindowBySid(s.id);
                if (w) closeShellWindow(w);
                _selectedSessionIds.delete(s.id);
                return;
              }
              return r.text().then(function (t) { throw new Error(t || ('HTTP ' + r.status)); });
            });
        });
      }, Promise.resolve()).then(function () {
        if (banner) setLoadBanner(banner, '');
        showCopyToast(tCount('toast.session.deleted.one', 'toast.session.deleted.other', { count: targets.length }));
        loadForwards();
        startUIWebSocket();
        renderSessionGrid(window._lastSessionsSnapshot || [], '');
      }).catch(function (err) {
        if (banner) setLoadBanner(banner, t('toast.delete.failed', { msg: String(err.message || err) }));
        renderSessionGrid(window._lastSessionsSnapshot || [], '');
      });
    });
  };
}
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

// Section collapse/expand with localStorage persistence
['entries', 'sessions'].forEach(function (key) {
  var header = document.getElementById('sec-' + key);
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

// ---- Entries drawer (touch devices) ----
// On touch devices the connection list becomes a left slide-in drawer instead of
// an expanded section: 22 entries expanded are ~2000px tall, which pushes the
// user's own sessions off-screen. The section header itself is the trigger (there
// is no separate hamburger), so the same row that expands in place on desktop
// opens the drawer on touch. Picking a connection closes it again.
(function () {
  var header = document.getElementById('sec-entries');
  var body = document.getElementById('sec-entries-body');
  if (!header || !body) return;

  var scrim = document.createElement('div');
  scrim.className = 'drawer-scrim';
  scrim.setAttribute('aria-hidden', 'true');
  document.body.appendChild(scrim);

  function isOpen() { return body.classList.contains('drawer-open'); }

  function setOpen(open) {
    body.classList.toggle('drawer-open', open);
    scrim.classList.toggle('drawer-open', open);
    header.setAttribute('aria-expanded', String(open));
    /* Prevent the page behind from scrolling under the drawer. */
    document.body.style.overflow = open ? 'hidden' : '';
  }

  /* In drawer mode the section header toggles the drawer instead of the inline
     collapse, so swallow the click before the shared collapse handler sees it.
     Capture phase: the collapse listener is on the same element. */
  header.addEventListener('click', function (e) {
    if (!isMobileViewport()) return;        // desktop keeps the inline collapse
    if (e.target.closest('button')) return;
    e.stopImmediatePropagation();
    e.preventDefault();
    setOpen(!isOpen());
  }, true);
  header.addEventListener('keydown', function (e) {
    if (!isMobileViewport()) return;
    if (e.key !== 'Enter' && e.key !== ' ') return;
    e.stopImmediatePropagation();
    e.preventDefault();
    setOpen(!isOpen());
  }, true);

  scrim.addEventListener('click', function () { setOpen(false); });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape' && isOpen()) setOpen(false);
  });
  /* Picking a connection from the drawer should reveal what it opened rather
     than leave the drawer covering the page. */
  body.addEventListener('click', function (e) {
    if (e.target.closest('.conn-tile')) setOpen(false);
  });
  window.addEventListener('resize', function () {
    if (isOpen() && !isMobileViewport()) setOpen(false);
  });

  /* On touch the drawer starts closed, so the chevron must show the collapsed
     state — the shared collapse logic above seeds aria-expanded from
     localStorage (default "true"), which would otherwise leave the icon saying
     "expanded" next to a closed drawer. */
  if (isMobileViewport()) header.setAttribute('aria-expanded', 'false');

  /* The terminal header's switcher button opens this drawer too, so a full-screen
     terminal can switch connection without going home first. Desktop has no
     drawer (entries expand in place), so the call is a no-op there. */
  window.termcpToggleEntriesDrawer = function (forceOpen) {
    if (!isMobileViewport()) return false;
    setOpen(forceOpen === undefined ? !isOpen() : !!forceOpen);
    return true;
  };
})();
