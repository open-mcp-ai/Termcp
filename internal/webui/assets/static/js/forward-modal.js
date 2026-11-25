/* The add-forward form.
 *
 * The ssh profile the new forward will be labelled with is NOT a parameter any
 * more: the server derives it from the session it creates the forward on
 * (handleCreateForward reads the session's own name/endpoint), so a value
 * collected here was only ever echoed into a hidden field nobody read — state
 * that outlives the dialog with no effect on what it does. The session id is
 * the one thing the modal has to remember.
 *
 * That id is rebound on EVERY open, including the one that opens the dialog just
 * to report that it has no session: leaving the previous session bound there let
 * "Create" on a dialog saying "no session" post a forward to the session the
 * user opened the modal from last time. */
function openForwardModal(sessionId) {
  _fwdSessionId = sessionId || '';
  var err = document.getElementById('modal-forward-err');
  if (err) { err.style.display = 'none'; err.textContent = ''; }
  if (!sessionId) {
    if (err) { err.textContent = t('modal.forward.err.noSession'); err.style.display = 'block'; }
    showModal('modal-forward');
    return;
  }
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
  return fetch(apiPath('/api/forwards/') + encodeURIComponent(forwardId), { method: 'DELETE' })
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
    local_port: parseInt(document.getElementById('fw-local-port').value) || 0
  };
  /* A SOCKS5 forward has no fixed target, so the hidden target fields are not sent
   * with it: leaving them in would post a destination the direction cannot carry,
   * and the server refuses that rather than dropping it on the floor. */
  if (dir !== 'dynamic') {
    body.remote_host = remoteHost;
    body.remote_port = remotePort;
  }
  /* THE LISTEN ADDRESS HAS TWO NAMES BECAUSE IT NAMES TWO MACHINES. For -L (and
   * for -D, which can only be loopback) the listener is on THIS host and the field
   * is bind_host; for -R the listener is on the TARGET and the field is local_host.
   * Sending local_host for every direction is what made this dialog's Listen host
   * do nothing at all for a local forward: the server reads bind_host there, so the
   * address was dropped and the tunnel came up on 127.0.0.1. */
  var listenHost = document.getElementById('fw-local-host').value.trim();
  if (dir === 'remote') {
    body.local_host = listenHost || '127.0.0.1';
  } else if (dir === 'local') {
    body.bind_host = listenHost || '127.0.0.1';
  }
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
  /* THE LABEL NAMES THE MACHINE, because the field does: the same box is "this
   * host" for a local forward and "the target" for a remote one. "Listen host"
   * named neither, which is how an address meant for the local side was read as the
   * remote side's (and then dropped) more than once. */
  var lhl = document.getElementById('fw-local-host-label');
  if (lhl) {
    /* BOTH BRANCHES SPELL THE KEY OUT. The catalog scan reads keys off the source
     * (data-i18n markers and t() arguments), so a key held in a variable is an
     * orphan to it — and the marker has to be rewritten here anyway, or a language
     * switch would relabel the box with whatever the direction was at load time. */
    if (v === 'remote') {
      lhl.setAttribute('data-i18n', 'modal.forward.listenHostTarget');
      lhl.textContent = t('modal.forward.listenHostTarget');
    } else {
      lhl.setAttribute('data-i18n', 'modal.forward.listenHostHere');
      lhl.textContent = t('modal.forward.listenHostHere');
    }
    lhl.style.display = isDynamic ? 'none' : '';
  }
  if (lh) lh.style.display = isDynamic ? 'none' : '';
  var pl = document.getElementById('fw-local-port-label');
  if (pl) pl.textContent = isDynamic ? t('modal.forward.socksPort') : t('modal.forward.listenPort');
};

function forwardMatchesConfig(f, sshConfig) {
  if (!sshConfig) return true;
  var cfg = f.ssh_config || '';
  return cfg === sshConfig || cfg.indexOf(sshConfig) >= 0 || sshConfig.indexOf(cfg) >= 0;
}
