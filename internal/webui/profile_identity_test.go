package webui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/storage"
)

func TestSessionProfileIdentityFollowsRenameAndPersists(t *testing.T) {
	e := newHostAttributionEnv(t)
	profile := []byte("kind = \"internal\"\n")
	if err := e.store.Save("original", profile); err != nil {
		t.Fatal(err)
	}
	id, err := e.store.Identity("original")
	if err != nil {
		t.Fatal(err)
	}
	sess, err := e.sessMgr.Create(session.Config{SSHConfig: "original", Name: "identity-session", NoShell: true})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Info().SSHConfigIdentity != id {
		t.Fatal("session creation did not capture the profile identity")
	}
	if err := e.store.Rename("original", "renamed"); err != nil {
		t.Fatal(err)
	}
	if info := sess.Info(); info.SSHConfigIdentity != id || info.SSHConfig != "renamed" {
		t.Fatal("profile rename broke session attribution")
	}
	if err := os.Rename(e.store.ConfigDir("renamed"), e.store.ConfigDir("moved")); err != nil {
		t.Fatal(err)
	}
	_ = e.sessMgr.ListAll()
	if info := sess.Info(); info.SSHConfigIdentity != id || info.SSHConfig != "moved" {
		t.Fatal("directory rename did not follow the existing identity")
	}
	root := filepath.Dir(filepath.Dir(e.store.ConfigDir("moved")))
	disk := storage.New(root)
	stored, err := disk.ReadSession(sess.ID)
	if err != nil || stored.SSHConfigIdentity != id || stored.SSHConfig != "moved" {
		t.Fatalf("persisted session identity error = %v", err)
	}
	if err := e.store.Delete("moved"); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Save("moved", profile); err != nil {
		t.Fatal(err)
	}
	freshID, err := e.store.Identity("moved")
	if err != nil || freshID == id || sess.Info().SSHConfigIdentity != id {
		t.Fatal("same-name recreation rebound an existing session")
	}
}

func TestHostCardsAssociateByIdentityAcrossRenameAndNameReuse(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH")
	}
	body := readAssetLF(t, "static/js/dialogs.js")
	fn := between(t, body, "function sessionBelongsToNode(", "\nfunction runningSessionsForNode(")
	script := `
const vm = require('vm');
const assert = require('assert');
const sandbox = {window: {_lastConnections: [{name: 'renamed', identity: 'original-id'}]}};
vm.createContext(sandbox);
vm.runInContext(process.argv[1], sandbox);
function belongs(session, name) { sandbox.session = session; sandbox.name = name; return vm.runInContext('sessionBelongsToNode(session, name, [])', sandbox); }
assert.strictEqual(belongs({ssh_config: 'old-name', ssh_config_identity: 'original-id'}, 'renamed'), true);
sandbox.window._lastConnections = [{name: 'old-name', identity: 'new-id'}];
assert.strictEqual(belongs({ssh_config: 'old-name', ssh_config_identity: 'original-id'}, 'old-name'), false);
assert.strictEqual(belongs({ssh_config: 'old-name', ssh_config_identity: 'new-id'}, 'old-name'), true);
assert.strictEqual(belongs({ssh_config: 'old-name'}, 'old-name'), false);
`
	if out, err := exec.Command(node, "-e", script, fn).CombinedOutput(); err != nil {
		t.Fatalf("identity attribution regression: %v\n%s", err, out)
	}
}
