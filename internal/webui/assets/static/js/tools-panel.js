function setForwardContext(sessionId, sshConfig) {
  _toolPanelSessionId = sessionId || '';
  _toolPanelSshCfg = sshConfig || 'internal';
}
function openForwardModal(sessionId, sshConfig) {
  var err = document.getElementById('modal-forward-err');
  if (err) { err.style.display = 'none'; err.textContent = ''; }
  if (!sessionId) {
    if (err) { err.textContent = 'No active session — open terminal first'; err.style.display = 'block'; }
    showModal('modal-forward');
    return;
  }
  setForwardContext(sessionId, sshConfig);
  var cfgEl = document.getElementById('fw-ssh-config-modal');
  if (cfgEl) cfgEl.value = _toolPanelSshCfg;
  document.getElementById('fw-remote-host').value = '';
  document.getElementById('fw-remote-port').value = '';
  document.getElementById('fw-local-host').value = '127.0.0.1';
  document.getElementById('fw-local-port').value = '0';
  document.getElementById('fw-direction').value = 'local';
  document.getElementById('fw-direction').dispatchEvent(new Event('change'));
  showModal('modal-forward');
}
function createForward(body) {
  if (!_toolPanelSessionId) {
    return Promise.reject(new Error('No active session — open terminal first'));
  }
  return fetch(sessionAPI(_toolPanelSessionId, '/forwards'), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
  }).then(function (r) {
    if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || r.status); });
    return r.json();
  });
}
function deleteForward(forwardId) {
  return fetch('/api/forwards/' + encodeURIComponent(forwardId), { method: 'DELETE' })
    .then(function (r) {
      if (!r.ok) return r.json().then(function (j) { throw new Error(j.error || r.status); });
      return r.json();
    })
    .then(function () { return loadForwards(); });
}
var _toolsPanelReady = false;
function setupToolsPanel() {
  if (_toolsPanelReady) return;
  _toolsPanelReady = true;
  var win = document.getElementById('panel-tools');
  if (!win) return;
  // Drag
  var header = win.querySelector('.shell-window-header');
  setupShellWindowDrag(win, header);
  // Resize
  setupShellWindowResize(win);
  // Collapse
  var collapseBtn = document.getElementById('panel-tools-collapse');
  if (collapseBtn) {
    collapseBtn.addEventListener('mousedown', function(e) { e.stopPropagation(); });
    collapseBtn.addEventListener('click', function(e) {
      e.preventDefault(); e.stopPropagation();
      var collapsing = !win.classList.contains('shell-window-collapsed');
      if (collapsing) {
        win._savedH = win.style.height || (win.getBoundingClientRect().height + 'px');
        win.style.height = 'auto';
        win.style.minHeight = '0';
      } else {
        win.style.height = win._savedH || '';
        win.style.minHeight = '';
      }
      win.classList.toggle('shell-window-collapsed');
      var icD = collapseBtn.querySelector('.ic-d');
      var icU = collapseBtn.querySelector('.ic-u');
      if (icD && icU) {
        icD.style.display = win.classList.contains('shell-window-collapsed') ? 'none' : '';
        icU.style.display = win.classList.contains('shell-window-collapsed') ? '' : 'none';
      }
      collapseBtn.title = win.classList.contains('shell-window-collapsed') ? 'Expand' : 'Collapse';
    });
  }
  // Close
  var closeBtn = document.getElementById('panel-tools-close');
  bindShellWindowCloseButton(win, closeBtn);
  if (closeBtn) {
    closeBtn.addEventListener('click', function() { win.style.display = 'none'; });
  }
  // Copy session ID
  var copyBtn = document.getElementById('panel-tools-copy-sid');
  if (copyBtn) {
    copyBtn.addEventListener('mousedown', function(e) { e.stopPropagation(); });
    copyBtn.addEventListener('click', function(e) {
      e.preventDefault(); e.stopPropagation();
      copyTextToClipboard(resourceUrlSession(_toolPanelSessionId || '')).then(function() { showCopyToast(); }).catch(function() { showCopyToast('Copy failed'); });
    });
  }
  // z-order on mousedown
  win.addEventListener('mousedown', function() { bringShellWindowToFront(win); });
}

function openToolPanel(sshConfig, sessionId) {
  setForwardContext(sessionId || '', sshConfig || 'internal');
  var win = document.getElementById('panel-tools');
  if (!win) return;
  setupToolsPanel();
  // Update title
  var sidEl = document.getElementById('panel-tools-sid');
  if (sidEl) sidEl.textContent = stripSessionPrefix(sessionId) || 'Tools';
  // Position if first time or hidden
  if (win.style.display === 'none') {
    win.style.left = Math.max(40, (window.innerWidth - 520) / 2) + 'px';
    win.style.top = Math.max(30, (window.innerHeight - 500) / 2) + 'px';
  }
  win.style.display = '';
  bringShellWindowToFront(win);
  // Hidden fields
  var el = document.getElementById('fw-ssh-config-modal');
  if (el) el.value = _toolPanelSshCfg;
  el = document.getElementById('fw-ssh-config');
  if (el) el.value = _toolPanelSshCfg;
  switchToolsTab('fw');
  refreshFwList();
}

function fileAPI(suffix) {
  if (!_toolPanelSessionId) return null;
  return '/api/sessions/' + encodeURIComponent(_toolPanelSessionId) + '/files' + (suffix || '');
}

function fileBrowse() {
  var path = document.getElementById('file-path').value.trim() || '/';
  var url = fileAPI('?path=' + encodeURIComponent(path));
  if (!url) { document.getElementById('file-listing').innerHTML = '<div style="padding:12px;color:#cf222e">No active session — open terminal first</div>'; return; }
  document.getElementById('file-listing').innerHTML = '<div style="padding:12px;color:#656d76">Loading...</div>';
  fetch(url).then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
    .then(function(j) { renderFileListing(j, path); })
    .catch(function(e) { document.getElementById('file-listing').innerHTML = '<div style="padding:12px;color:#cf222e">Error: ' + escapeHtml(String(e.message||e)) + '</div>'; });
}

function fileGoUp() {
  var path = document.getElementById('file-path').value.trim() || '/';
  if (path === '/') return;
  var parts = path.replace(/\/+$/, '').split('/');
  parts.pop();
  var parent = parts.join('/') || '/';
  document.getElementById('file-path').value = parent;
  fileBrowse();
}

function fileDownloadCurrent() {
  var path = document.getElementById('file-path').value.trim();
  if (!path) return;
  var url = fileAPI('/download?path=' + encodeURIComponent(path));
  if (!url) return;
  window.open(url, '_blank');
}

var _fileCtxPath = '';
function renderFileListing(data, currentPath) {
  document.getElementById('file-path').value = currentPath;
  var isDir = data && data.is_dir;
  var children = data && data.children ? data.children : [];
  if (!isDir) {
    var html = '<div style="padding:12px">';
    html += '<div style="font-weight:600;margin-bottom:8px">' + escapeHtml(data.name || currentPath) + '</div>';
    html += '<div>Size: ' + formatSize(data.size || 0) + '</div>';
    if (data.mod_time) html += '<div>Modified: ' + escapeHtml(fmtTime(data.mod_time)) + '</div>';
    html += '</div>';
    document.getElementById('file-listing').innerHTML = html;
    return;
  }
  if (!children.length) {
    document.getElementById('file-listing').innerHTML = '<div style="padding:12px;color:#656d76">Empty directory</div>';
    return;
  }
  var basePath = currentPath.replace(/\/+$/, '');
  var html = '';
  children.forEach(function(c) {
    var icon = c.is_dir ? '📁' : '📄';
    var fullPath = basePath + '/' + c.name;
    html += '<div class="file-row" data-path="' + escapeHtml(fullPath) + '" data-isdir="' + (c.is_dir ? '1' : '0') + '" data-name="' + escapeHtml(c.name) + '" style="display:flex;align-items:center;gap:6px;padding:5px 10px;cursor:pointer;border-bottom:1px solid #f0f0f0;white-space:nowrap">';
    html += '<span style="flex-shrink:0">' + icon + '</span>';
    html += '<span style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis">' + escapeHtml(c.name) + '</span>';
    if (!c.is_dir) html += '<span style="flex-shrink:0;color:#656d76;font-size:0.75rem">' + formatSize(c.size || 0) + '</span>';
    html += '</div>';
  });
  document.getElementById('file-listing').innerHTML = html;
  document.getElementById('file-listing').querySelectorAll('.file-row').forEach(function(row) {
    row.addEventListener('click', function(e) {
      var p = this.getAttribute('data-path');
      var d = this.getAttribute('data-isdir') === '1';
      document.getElementById('file-path').value = p;
      if (d) {
        fileBrowse();
      } else {
        // Show context menu for files
        _fileCtxPath = p;
        var menu = document.getElementById('file-ctx-menu');
        menu.style.display = 'block';
        menu.style.left = e.clientX + 'px';
        menu.style.top = e.clientY + 'px';
        e.stopPropagation();
      }
    });
  });
}

// Dismiss context menu on outside click
document.addEventListener('click', function(e) {
  var menu = document.getElementById('file-ctx-menu');
  if (!menu || menu.style.display === 'none') return;
  if (!menu.contains(e.target)) menu.style.display = 'none';
});

// Context menu actions
document.getElementById('file-ctx-menu').addEventListener('click', function(e) {
  var action = (e.target.closest('.file-ctx-item') || {}).dataset && e.target.closest('.file-ctx-item').dataset.action;
  this.style.display = 'none';
  if (!action || !_fileCtxPath) return;
  if (action === 'download') {
    var url = fileAPI('/download?path=' + encodeURIComponent(_fileCtxPath));
    if (url) window.open(url, '_blank');
  } else if (action === 'delete') {
    var url2 = fileAPI('?path=' + encodeURIComponent(_fileCtxPath));
    if (!url2) return;
    if (!confirm('Delete ' + _fileCtxPath + '?')) return;
    fetch(url2, { method: 'DELETE' })
      .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function() { fileBrowse(); })
      .catch(function(err) { console.error('Delete failed:', err); });
  }
});

function formatSize(bytes) {
  if (!bytes || bytes < 0) return '0 B';
  var units = ['B', 'KB', 'MB', 'GB'];
  var i = 0;
  var size = bytes;
  while (size >= 1024 && i < units.length - 1) { size /= 1024; i++; }
  return (i === 0 ? size : size.toFixed(1)) + ' ' + units[i];
}

/**
 * Render a Unix-millisecond timestamp as local time.
 *
 * Every timestamp on the wire is Unix ms: manifests,
 * log.jsonl marks, session and forward metadata, file mod_time. Formatting is
 * deliberately the client's job so the server never has to serialize a time
 * as a locale string, and every field shares one unit.
 */
function fmtTime(ms) {
  if (!ms) return '';
  var d = new Date(ms);
  if (isNaN(d.getTime())) return '';
  return d.toLocaleString();
}
function switchToolsTab(name) {
  document.querySelectorAll('.tools-tab').forEach(function(t) { t.classList.toggle('active', t.dataset.tab === name); });
  document.getElementById('tools-tab-fw').style.display = name === 'fw' ? '' : 'none';
  var ft = document.getElementById('tools-tab-file');
  ft.style.display = name === 'file' ? 'flex' : 'none';
}
document.querySelectorAll('.tools-tab').forEach(function(t) {
  t.addEventListener('click', function() { switchToolsTab(t.dataset.tab); });
});
var _panelClose = document.getElementById('panel-tools-close');
if (_panelClose) _panelClose.addEventListener('click', function() { document.getElementById('panel-tools').style.display = 'none'; });

// File panel bindings
var _fileLs = document.getElementById('file-ls');
if (_fileLs) _fileLs.addEventListener('click', fileBrowse);
var _fileDownload = document.getElementById('file-download');
if (_fileDownload) _fileDownload.addEventListener('click', fileDownloadCurrent);
var _fileUp = document.getElementById('file-up');
if (_fileUp) _fileUp.addEventListener('click', fileGoUp);
var _filePath = document.getElementById('file-path');
if (_filePath) _filePath.addEventListener('keydown', function(e) { if (e.key === 'Enter') fileBrowse(); });
var _fileUploadBtn = document.getElementById('file-upload-btn');
var _fileUploadInput = document.getElementById('file-upload-input');
if (_fileUploadBtn && _fileUploadInput) {
  _fileUploadBtn.addEventListener('click', function() { _fileUploadInput.click(); });
  _fileUploadInput.addEventListener('change', function() {
    var file = this.files && this.files[0];
    if (!file) return;
    var path = (document.getElementById('file-path').value.trim() || '/').replace(/\/+$/, '') + '/' + file.name;
    var url = fileAPI('/upload?path=' + encodeURIComponent(path));
    if (!url) return;
    fetch(url, { method: 'POST', body: file })
      .then(function(r) { if (!r.ok) throw new Error(r.status); return r.json(); })
      .then(function() { fileBrowse(); })
      .catch(function(e) { console.error('Upload failed:', e); });
  });
}

var _fwAdd = document.getElementById('fw-show-add');
if (_fwAdd) _fwAdd.addEventListener('click', function() {
  openForwardModal(_toolPanelSessionId, _toolPanelSshCfg);
});
var _fwClose = document.getElementById('modal-forward-close');
if (_fwClose) _fwClose.addEventListener('click', function() { hideModal('modal-forward'); });
var _fwCreate = document.getElementById('fw-create');
if (_fwCreate) _fwCreate.onclick = function () {
  var err = document.getElementById('modal-forward-err');
  err.style.display = 'none';
  var dir = document.getElementById('fw-direction').value;
  var remoteHost = document.getElementById('fw-remote-host').value.trim();
  var remotePort = parseInt(document.getElementById('fw-remote-port').value) || 0;
  if (dir !== 'dynamic' && (!remoteHost || !remotePort)) { err.textContent = 'Target host and port are required'; err.style.display = 'block'; return; }
  var body = {
    direction: dir,
    remote_host: remoteHost,
    remote_port: remotePort,
    local_host: document.getElementById('fw-local-host').value.trim() || '127.0.0.1',
    local_port: parseInt(document.getElementById('fw-local-port').value) || 0
  };
  createForward(body)
    .then(function () { hideModal('modal-forward'); loadForwards(); })
    .catch(function (e) { err.textContent = String(e.message || e); err.style.display = 'block'; });
};
document.getElementById('fw-direction').onchange = function () {
  var v = this.value;
  document.getElementById('fw-dir-hint-local').style.display = v === 'local' ? '' : 'none';
  document.getElementById('fw-dir-hint-remote').style.display = v === 'remote' ? '' : 'none';
  document.getElementById('fw-dir-hint-dynamic').style.display = v === 'dynamic' ? '' : 'none';
  var isDynamic = v === 'dynamic';
  var tf = document.getElementById('fw-target-fields-modal') || document.getElementById('fw-target-fields');
  if (tf) tf.style.display = isDynamic ? 'none' : '';
  var lh = document.getElementById('fw-local-host');
  if (lh) { var lbl = lh.closest('label'); if (lbl) lbl.style.display = isDynamic ? 'none' : ''; lh.style.display = isDynamic ? 'none' : ''; }
  var pl = document.getElementById('fw-local-port-label');
  if (pl) pl.textContent = isDynamic ? 'SOCKS5 port (0 = random)' : 'Listen port (0 = random)';
};

function forwardMatchesConfig(f, sshConfig) {
  if (!sshConfig) return true;
  var cfg = f.ssh_config || '';
  return cfg === sshConfig || cfg.indexOf(sshConfig) >= 0 || sshConfig.indexOf(cfg) >= 0;
}
function refreshFwList() {
  var el = document.getElementById('tools-fw-list-items');
  if (!el) return;
  var fwds = (window._lastForwards || []).filter(function(f) { return forwardMatchesConfig(f, _toolPanelSshCfg); });
  if (!fwds.length) { el.innerHTML = 'No active forwards'; return; }
  el.innerHTML = fwds.map(function(f){
    return '<div style="display:flex;justify-content:space-between;align-items:center;padding:3px 0;border-bottom:1px solid #eee">' +
      '<span><b>' + escapeHtml(f.direction) + '</b> ' + escapeHtml(f.listen_addr) + ' \u2192 ' + escapeHtml(f.target_addr) + ' <span style="color:#656d76;font-size:0.7rem">' + escapeHtml(f.ssh_config) + '</span></span>' +
      '<button class="btn fw-del-btn" style="padding:2px 8px;font-size:0.7rem" data-fwid="' + escapeHtml(f.forward_id) + '">\u2715</button>' +
      '</div>';
  }).join('');
}

