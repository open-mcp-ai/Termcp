function openForwardModal(sessionId, sshConfig) {
  var err = document.getElementById('modal-forward-err');
  if (err) { err.style.display = 'none'; err.textContent = ''; }
  if (!sessionId) {
    if (err) { err.textContent = t('modal.forward.err.noSession'); err.style.display = 'block'; }
    showModal('modal-forward');
    return;
  }
  _fwdSessionId = sessionId;
  _fwdSshCfg = sshConfig || 'internal';
  var cfgEl = document.getElementById('fw-ssh-config-modal');
  if (cfgEl) cfgEl.value = _fwdSshCfg;
  document.getElementById('fw-remote-host').value = '';
  document.getElementById('fw-remote-port').value = '';
  document.getElementById('fw-local-host').value = '127.0.0.1';
  document.getElementById('fw-local-port').value = '0';
  document.getElementById('fw-direction').value = 'local';
  document.getElementById('fw-direction').dispatchEvent(new Event('change'));
  showModal('modal-forward');
}
function createForward(body) {
  if (!_fwdSessionId) {
    return Promise.reject(new Error(t('modal.forward.err.noSession')));
  }
  return fetch(sessionAPI(_fwdSessionId, '/forwards'), {
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
var _fwClose = document.getElementById('modal-forward-close');
if (_fwClose) _fwClose.addEventListener('click', function() { hideModal('modal-forward'); });
var _fwCreate = document.getElementById('fw-create');
if (_fwCreate) _fwCreate.onclick = function () {
  var err = document.getElementById('modal-forward-err');
  err.style.display = 'none';
  var dir = document.getElementById('fw-direction').value;
  var remoteHost = document.getElementById('fw-remote-host').value.trim();
  var remotePort = parseInt(document.getElementById('fw-remote-port').value) || 0;
  if (dir !== 'dynamic' && (!remoteHost || !remotePort)) { err.textContent = t('modal.forward.err.required'); err.style.display = 'block'; return; }
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
  if (pl) pl.textContent = isDynamic ? t('modal.forward.socksPort') : t('modal.forward.listenPort');
};

function forwardMatchesConfig(f, sshConfig) {
  if (!sshConfig) return true;
  var cfg = f.ssh_config || '';
  return cfg === sshConfig || cfg.indexOf(sshConfig) >= 0 || sshConfig.indexOf(cfg) >= 0;
}
