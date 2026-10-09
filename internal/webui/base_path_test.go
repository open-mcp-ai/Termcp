package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Web UI is often reached through a reverse proxy mounted on a sub-path
// (`https://host/termcp/`). Every URL the page emits is then resolved by the
// browser against that mount, so a root-absolute `/api/...` escapes it and hits
// the parent site — the API calls silently go somewhere else while the page
// itself renders fine.
//
// util.js derives the prefix from the document's own path and routes every URL
// through apiPath(); these tests pin the pure resolution logic, the assets that
// must use it, and the invariant that the root deployment keeps byte-identical
// URLs (prefix "" must be a no-op).
func TestAPIPathResolvesAgainstDeploymentPath(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; cannot exercise the path resolver")
	}
	util := readAssetLF(t, "static/js/util.js")
	// util.js touches window/document at load time, so only the resolver pair is
	// extracted — same slice-the-source approach the other asset tests use.
	helpers := between(t, util, "function uiBasePath() {", "/** /api/sessions/")

	script := `
const fs = require('fs');
const vm = require('vm');
const helpers = fs.readFileSync(process.argv[2], 'utf8');

// The resolvers only read location.pathname.
function run(pathname, expr) {
  const sandbox = { location: { pathname: pathname }, console: console };
  vm.createContext(sandbox);
  vm.runInContext(helpers, sandbox);
  return vm.runInContext(expr, sandbox);
}
function resolve(pathname, probe) {
  return run(pathname, 'apiPath(' + JSON.stringify(probe) + ')');
}
function base(pathname) {
  return run(pathname, 'uiBasePath()');
}

let bad = 0;
function check(name, got, want) {
  if (got !== want) { console.log('FAIL ' + name + ': got ' + JSON.stringify(got) + ' want ' + JSON.stringify(want)); bad++; }
  else console.log('PASS ' + name);
}

// Root deployment: "" prefix, URLs unchanged.
check('root base is empty', base('/'), '');
check('root index.html base is empty', base('/index.html'), '');
check('root api URL unchanged', resolve('/', '/api/version'), '/api/version');
check('root ws URL unchanged', resolve('/', '/api/ui/ws'), '/api/ui/ws');

// Sub-path deployment: the mount is prepended.
check('sub base from trailing slash', base('/termcp/'), '/termcp');
check('sub base from index.html', base('/termcp/index.html'), '/termcp');
check('nested base', base('/a/b/termcp/'), '/a/b/termcp');
check('sub api URL', resolve('/termcp/', '/api/version'), '/termcp/api/version');
check('sub static URL', resolve('/termcp/', '/static/css/app.css'), '/termcp/static/css/app.css');

// Idempotence: an already-prefixed value must not be prefixed twice.
check('idempotent', resolve('/termcp/', '/termcp/api/version'), '/termcp/api/version');
// Non-absolute inputs pass through untouched (callers may hand over a full URL).
check('absolute URL untouched', resolve('/termcp/', 'https://x/y'), 'https://x/y');
check('relative path untouched', resolve('/termcp/', 'icons/x.svg'), 'icons/x.svg');

console.log(bad === 0 ? 'BASE PATH OK' : bad + ' failure(s)');
process.exit(bad === 0 ? 0 : 1);
`
	tmp := t.TempDir()
	utilPath := filepath.Join(tmp, "util-helpers.js")
	if err := os.WriteFile(utilPath, []byte(helpers), 0o600); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(tmp, "probe.js")
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, scriptPath, utilPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("base path probe failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "BASE PATH OK") {
		t.Fatalf("base path probe reported failures:\n%s", out)
	}
}

// Every asset that emits a termcp-root-absolute URL must route it through
// apiPath. A missed call site is invisible in the root deployment and only
// breaks under a proxy mount, which is exactly the failure this guards.
func TestAssetsRouteAbsoluteURLsThroughAPIPath(t *testing.T) {
	js := readAssetLF(t, "static/js/util.js")
	for _, want := range []string{"function uiBasePath()", "function apiPath(", "if (p.slice(-11) === '/index.html')"} {
		if !strings.Contains(js, want) {
			t.Fatalf("util.js must define the deployment prefix helpers (missing %q)", want)
		}
	}
	if !strings.Contains(js, "return apiPath('/api/sessions/'") {
		t.Error("sessionAPI must build its URL through apiPath")
	}

	// Files that issue requests. Root-absolute string literals in these are the
	// bug; comparisons, regexes and DOM selectors are not, so the check is
	// narrowed to the request-shaped call sites.
	requesters := []string{
		"static/js/approval.js",
		"static/js/conn-form.js",
		"static/js/dialogs.js",
		"static/js/forward-modal.js",
		"static/js/sessions.js",
		"static/js/shell-windows.js",
		"static/js/terminal-panels.js",
		"static/js/terminal-view.js",
		"static/js/terminal-window.js",
		"static/js/timeline.js",
		"static/js/ui-socket.js",
		"static/js/util.js",
	}
	for _, rel := range requesters {
		body := readAssetLF(t, rel)
		for i, line := range strings.Split(body, "\n") {
			if !strings.Contains(line, "/api/") && !strings.Contains(line, "/static/") && !strings.Contains(line, "/icons/") {
				continue
			}
			// Bare literal that starts a URL and is not already wrapped.
			if (strings.Contains(line, "'/api/") || strings.Contains(line, `"/api/`) ||
				strings.Contains(line, "'/static/") || strings.Contains(line, `"/static/`) ||
				strings.Contains(line, "'/icons/") || strings.Contains(line, `"/icons/`)) &&
				!strings.Contains(line, "apiPath(") {
				t.Errorf("%s:%d emits a root-absolute URL without apiPath: %s", rel, i+1, strings.TrimSpace(line))
			}
		}
	}

	// The document itself is served under the mount, so its own links are
	// relative; a root-absolute one would leave the mount.
	html := readAssetLF(t, "index.html")
	if strings.Contains(html, `href="/api.html"`) {
		t.Error("index.html must link the API docs relatively so a sub-path mount works")
	}
	if !strings.Contains(html, `href="api.html"`) {
		t.Error("index.html must keep the API docs link")
	}
}
