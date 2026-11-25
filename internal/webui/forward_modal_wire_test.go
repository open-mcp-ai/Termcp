package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestForwardModalPostsTheAddressFieldOfTheDirection is the Web UI half of the
// same rule the server enforces: the "Listen host" box names the machine that
// listens, and that machine is a different one per direction.
//
// THE FIELD IT POSTS MUST FOLLOW THE DIRECTION. `bind_host` is this host (local,
// and the loopback-only dynamic); `local_host` is the target (remote). The dialog
// has one box and used to send one key, so for `ssh -L` — the default direction,
// the one a user picks without thinking — the address was posted as the field the
// server reads only for a remote forward. It was dropped, the request still
// succeeded, and the tunnel came up on 127.0.0.1.
//
// The probe runs the shipped module under node with a stub DOM, because which key
// reaches the wire is a fact about the code that runs, not about its text: a
// source scan passes on any file that mentions both names somewhere.
func TestForwardModalPostsTheAddressFieldOfTheDirection(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the forward dialog")
	}
	// The session id the dialog remembers is declared with the shell window; the
	// probe supplies it so the shipped module can run on its own.
	modal := "var _fwdSessionId = '';\n" + readAssetLF(t, "static/js/forward-modal.js")

	script := `
const fs = require('fs');
const vm = require('vm');

function makeEl(id) {
  return { id: id, style: {}, value: '', textContent: '', disabled: false,
           attributes: {},
           setAttribute(k, v) { this.attributes[k] = v; },
           getAttribute(k) { return this.attributes[k]; },
           addEventListener() {}, dispatchEvent() {} };
}

const sandbox = {
  document: {
    _els: {},
    getElementById(id) {
      if (!this._els[id]) this._els[id] = makeEl(id);
      return this._els[id];
    },
    querySelectorAll() { return []; },
    addEventListener() {},
  },
  t: k => k,
  window: {},
  sessionAPI: (id, p) => '/api/sessions/' + id + p,
  apiPath: p => p,
  encodeURIComponent: encodeURIComponent,
  Event: function (name) { this.type = name; },
  hideModal() {}, showModal() {},
  loadForwards() {},
  sent: [],
  Promise: Promise,
  JSON: JSON,
  parseInt: parseInt,
  console: console,
};
sandbox.fetch = function (url, opts) {
  sandbox.sent.push(JSON.parse(opts.body));
  return Promise.resolve({ ok: true, json: () => ({}) });
};
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], 'utf8'), sandbox);
// The dialog is opened for a session, the way the page opens it.
vm.runInContext("openForwardModal('sid')", sandbox);

const el = id => sandbox.document.getElementById(id);
const dir = el('fw-direction');
// What a user types: a target and a listen address that is NOT loopback, so a
// dropped field cannot hide behind the server's default.
function submit(direction) {
  dir.value = direction;
  el('fw-remote-host').value = 'db';
  el('fw-remote-port').value = '5432';
  el('fw-local-host').value = '0.0.0.0';
  el('fw-local-port').value = '8080';
  sandbox.sent.length = 0;
  vm.runInContext("document.getElementById('fw-create').onclick()", sandbox);
  return sandbox.sent[0];
}

let bad = 0;
function check(name, cond, detail) {
  if (!cond) { console.log('FAIL ' + name + ': ' + detail); bad++; }
  else console.log('PASS ' + name);
}

const local = submit('local');
check('local posts bind_host', local && local.bind_host === '0.0.0.0', JSON.stringify(local));
check('local does not post local_host', local && local.local_host === undefined, JSON.stringify(local));
check('local still carries its target', local && local.remote_host === 'db' && local.remote_port === 5432, JSON.stringify(local));

const remote = submit('remote');
check('remote posts local_host', remote && remote.local_host === '0.0.0.0', JSON.stringify(remote));
check('remote does not post bind_host', remote && remote.bind_host === undefined, JSON.stringify(remote));
check('remote keeps the port the TARGET binds', remote && remote.local_port === 8080, JSON.stringify(remote));

const dynamic = submit('dynamic');
check('dynamic posts no address at all', dynamic && dynamic.bind_host === undefined && dynamic.local_host === undefined, JSON.stringify(dynamic));
check('dynamic posts no target', dynamic && dynamic.remote_host === undefined && dynamic.remote_port === undefined, JSON.stringify(dynamic));

// The label follows the direction too: the box is "this machine" for -L and
// "the target" for -R, which is what the field actually means.
function label(direction) {
  dir.value = direction;
  vm.runInContext("document.getElementById('fw-direction').onchange.call(document.getElementById('fw-direction'))", sandbox);
  return el('fw-local-host-label').getAttribute('data-i18n');
}
check('the local label names this machine', label('local') === 'modal.forward.listenHostHere', label('local'));
check('the remote label names the target', label('remote') === 'modal.forward.listenHostTarget', label('remote'));

console.log(bad === 0 ? 'FORWARD WIRE OK' : bad + ' wire failure(s)');
process.exit(bad === 0 ? 0 : 1);
`
	tmp := t.TempDir()
	modalPath := filepath.Join(tmp, "forward-modal.js")
	if err := os.WriteFile(modalPath, []byte(modal), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(tmp, "forward_wire_test.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, scriptPath, modalPath).CombinedOutput()
	if err != nil {
		t.Fatalf("forward wire probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "FORWARD WIRE OK") {
		t.Fatalf("forward wire probe did not pass:\n%s", out)
	}
}
