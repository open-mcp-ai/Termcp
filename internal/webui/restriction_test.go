package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/extensions"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/notify"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// restProbe is one restricted session reachable over the real routes: the mux
// that serves it, the session, and its primary shell id.
type restProbe struct {
	mux        *http.ServeMux
	sess       *session.Session
	sessMgr    *session.Manager
	shellID    string
	dataDir    string
	forwardMgr *forward.ForwardManager
	notifyMgr  *notify.Manager
}

// withExtensions swaps in a handler that carries the extension reconciler, so the
// template routes are live. It exists because most probes only need the session
// surface.
func (p *restProbe) withExtensions(t *testing.T) {
	t.Helper()
	rec := extensions.NewReconciler(extensions.NewStore(p.dataDir), p.sessMgr, nil, sshconfig.NewStore(p.dataDir), nil)
	h := &Handler{Sessions: p.sessMgr, Extensions: rec}
	mux := http.NewServeMux()
	h.Register(mux)
	p.mux = mux
}

// newRestProbe builds a running session whose capabilities were trimmed the way
// an extension template trims them, wired into a real handler. It is the REST
// half of the guard tests: the same whitelist has to hold on the HTTP surface,
// because every capability an extension preset names is reachable there too.
func newRestProbe(t *testing.T, allow, paths []string) restProbe {
	t.Helper()
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})

	r, err := session.NewRestriction(allow, paths)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := sessMgr.Create(session.Config{
		Mode:        api.ModePTY,
		Rows:        24,
		Cols:        80,
		Restriction: r,
	})
	if err != nil {
		t.Fatal(err)
	}
	fwdMgr := forward.NewForwardManager()
	notifyMgr := notify.NewManager(nil)
	h := &Handler{Sessions: sessMgr, MsgMgr: msgMgr, ForwardMgr: fwdMgr, NotifyMgr: notifyMgr}
	mux := http.NewServeMux()
	h.Register(mux)
	return restProbe{mux: mux, sess: sess, sessMgr: sessMgr, shellID: sess.PrimaryShellID(), dataDir: dir, forwardMgr: fwdMgr, notifyMgr: notifyMgr}
}

func (p restProbe) post(t *testing.T, path, body string) int {
	t.Helper()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	p.mux.ServeHTTP(rr, req)
	return rr.Code
}

func (p restProbe) get(t *testing.T, path string) int {
	t.Helper()
	rr := httptest.NewRecorder()
	p.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr.Code
}

func (p restProbe) del(t *testing.T, path string) int {
	t.Helper()
	rr := httptest.NewRecorder()
	p.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, path, nil))
	return rr.Code
}

// TestRESTRespectsSessionRestriction pins that a template's whitelist is enforced
// on the HTTP surface, not only through MCP.
//
// Every capability the extension presets name exists here too: terminal input and
// keys, opening a channel, terminating the session, and the file routes. An agent
// holding an extension handle and the deployment token could reach all of them,
// so a whitelist only the MCP guard applies is a whitelist with a documented way
// around it.
func TestRESTRespectsSessionRestriction(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	p := newRestProbe(t, []string{"file_read"}, []string{"~/skills/foo/"})
	sid, shellID := p.sess.ID, p.shellID

	// A read-only skill window must not accept the operator's terminal input.
	if code := p.post(t, "/api/shells/"+shellID+"/input", `{"text":"id","press_enter":true}`); code != http.StatusForbidden {
		t.Errorf("REST input on a read-only extension session = %d, want 403", code)
	}
	if code := p.post(t, "/api/shells/"+shellID+"/key", `{"key":"enter"}`); code != http.StatusForbidden {
		t.Errorf("REST key on a read-only extension session = %d, want 403", code)
	}
	if code := p.post(t, "/api/shells/"+shellID+"/resize", `{"rows":40,"cols":120}`); code != http.StatusForbidden {
		t.Errorf("REST resize on a read-only extension session = %d, want 403", code)
	}
	// A second channel would carry the container's whole environment.
	if code := p.post(t, "/api/sessions/"+sid+"/shells", `{}`); code != http.StatusForbidden {
		t.Errorf("REST shell_open on a read-only extension session = %d, want 403", code)
	}
	// Terminating the shared connection takes down every extension on the host.
	if code := p.post(t, "/api/sessions/"+sid+"/terminate", ``); code != http.StatusForbidden {
		t.Errorf("REST terminate on a restricted session = %d, want 403", code)
	}
	if code := p.del(t, "/api/sessions/"+sid); code != http.StatusForbidden {
		t.Errorf("REST delete on a restricted session = %d, want 403", code)
	}
	// The file routes are gated by tool name and by path.
	if code := p.get(t, "/api/sessions/"+sid+"/files?path=/etc"); code != http.StatusForbidden {
		t.Errorf("REST list-files outside the whitelist = %d, want 403", code)
	}
	if code := p.get(t, "/api/sessions/"+sid+"/files/download?path=/etc/passwd"); code != http.StatusForbidden {
		t.Errorf("REST download outside the whitelist = %d, want 403", code)
	}
	if code := p.post(t, "/api/sessions/"+sid+"/files/upload?path=/tmp/x", "payload"); code != http.StatusForbidden {
		t.Errorf("REST upload on a read-only session = %d, want 403", code)
	}
	// The batch form refuses wholesale, so a partial refusal cannot read as a
	// partial success.
	if code := p.post(t, "/api/sessions/"+sid+",plain-id/terminate", ``); code != http.StatusForbidden {
		t.Errorf("REST batch terminate including a restricted session = %d, want 403", code)
	}
	// A tunnel is a capability of the session it rides. This template granted no
	// forwards at all, so neither opening nor closing one on its connection is
	// reachable — the MCP forward tool answers the same way.
	if code := p.post(t, "/api/sessions/"+sid+"/forwards", `{"direction":"dynamic","local_port":0}`); code != http.StatusForbidden {
		t.Errorf("REST forward on a session that granted none = %d, want 403", code)
	}

	// Closing a forward and removing a notification rule name their own ids, not a
	// session, so both routes have to look the owner up. A caller showing up with
	// only the resource id must meet the same whitelist as one naming the session.
	fw, err := p.forwardMgr.CreateDynamic(sid, "restricted", "", 0, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if code := p.del(t, "/api/forwards/"+fw.ForwardID); code != http.StatusForbidden {
		t.Errorf("REST forward close on a session that granted no forwards = %d, want 403", code)
	}
	rule, err := p.notifyMgr.Register(sid, p.shellID, notify.ChannelResource, notify.EventOutput, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code := p.del(t, "/api/notifications/"+rule.ID); code != http.StatusForbidden {
		t.Errorf("REST rule delete on a session that granted no shell_notify = %d, want 403", code)
	}
}

// TestRESTGatesTranscriptReadsAndShellClose pins the other half of the same rule:
// reading a transcript and closing a channel are capabilities too.
//
// These routes are the REST twins of shell_output, message(action=list) and
// shell_close, and the whitelist is about reachability rather than direction — a
// read of a session's bytes is as gated as writing to it. Leaving them open made
// the refusal a matter of which transport a caller picked, which is the whole
// thing the extension whitelist claims is not true.
//
// Each id is also passed in the spellings transcript accepts (the shell id and the
// session id), because a gate that resolved fewer of them than the reader would be
// walked around by respelling the target.
func TestRESTGatesTranscriptReadsAndShellClose(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	p := newRestProbe(t, []string{"file_read"}, []string{"~/skills/foo/"})
	sid, shellID := p.sess.ID, p.shellID

	for _, id := range []string{shellID, sid} {
		for _, route := range []string{"/marks", "/lines?from=1", "/rail?cols=80&count=1&height=1"} {
			if code := p.get(t, "/api/shells/"+id+route); code != http.StatusForbidden {
				t.Errorf("GET /api/shells/%s%s on a read-only extension session = %d, want 403", id, route, code)
			}
		}
		if code := p.get(t, "/api/sessions/"+id+"/output-range?start=0"); code != http.StatusForbidden {
			t.Errorf("output-range by %s on a read-only extension session = %d, want 403", id, code)
		}
		if code := p.del(t, "/api/shells/"+id); code != http.StatusForbidden {
			t.Errorf("DELETE /api/shells/%s on a read-only extension session = %d, want 403", id, code)
		}
		if code := p.post(t, "/api/sessions/"+id+"/close-shell", ``); code != http.StatusForbidden {
			t.Errorf("close-shell by %s on a read-only extension session = %d, want 403", id, code)
		}
	}
}

// TestRESTTranscriptReadsStillWorkWhenGranted pins the other side: an extension
// that DID grant the read capabilities keeps its transcript, so the gate is a
// whitelist and not a blanket ban on reading.
//
// Both names are granted because the routes do not all speak one: the byte windows
// are the twins of shell_output, and the marks index is the twin of message.
func TestRESTTranscriptReadsStillWorkWhenGranted(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	p := newRestProbe(t, []string{"shell_output", "message"}, nil)

	if code := p.get(t, "/api/shells/"+p.shellID+"/marks"); code == http.StatusForbidden {
		t.Error("a granted session must still serve its marks")
	}
	if code := p.get(t, "/api/sessions/"+p.sess.ID+"/output-range?start=0"); code == http.StatusForbidden {
		t.Error("a granted session must still serve its output range")
	}
}

// TestRESTLeavesUnrestrictedSessionsAlone pins the backward-compatibility half:
// an ordinary session (no Restriction) keeps the whole REST surface, so the
// enforcement is invisible to every existing caller.
func TestRESTLeavesUnrestrictedSessionsAlone(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	p := newRestProbe(t, nil, nil)

	if code := p.post(t, "/api/shells/"+p.shellID+"/resize", `{"rows":40,"cols":120}`); code != http.StatusOK {
		t.Errorf("REST resize on an unrestricted session = %d, want 200", code)
	}
	if code := p.get(t, "/api/sessions/"+p.sess.ID+"/files?path="+t.TempDir()); code == http.StatusForbidden {
		t.Error("an unrestricted session must not be refused by the capability whitelist")
	}
}

// TestRestRefusalBodyNamesTheArgument pins the refusal's shape: the REST answer
// carries the same error_code the MCP surface uses, so a client branches on the
// code rather than on prose.
func TestRestRefusalBodyNamesTheArgument(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	p := newRestProbe(t, []string{"file_read"}, []string{"~/skills/foo/"})

	w := httptest.NewRecorder()
	if restRefuseTool(w, nil, "file_write") {
		t.Error("a nil session is unrestricted and must pass")
	}
	if !restRefusePaths(w, p.sess, [2]string{"path", "/etc/passwd"}) {
		t.Fatal("a path outside the whitelist must be refused")
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if body["error_code"] != "forbidden" {
		t.Errorf("error_code = %v, want forbidden", body["error_code"])
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "path") {
		t.Errorf("the refusal should name the argument, got %q", msg)
	}
}

// TestWebSocketHonoursSessionRestriction pins the transport twin of the REST
// gate: the browser terminal talks over the socket, not the REST endpoints, so a
// whitelist enforced only on REST would be a matter of which transport a caller
// picked.
func TestWebSocketHonoursSessionRestriction(t *testing.T) {
	p := newRestProbe(t, []string{"file_read"}, []string{"~/skills/foo/"})
	shell := p.sess.GetChildShell(p.shellID)
	if shell == nil {
		t.Fatal("session has no primary shell")
	}

	ws := &uiWS{h: &Handler{Sessions: p.sessMgr}}
	if !ws.refuseWSCapability(shell, "shell_input") {
		t.Error("a remote-input frame on a read-only extension session must be dropped")
	}
	if !ws.refuseWSCapability(shell, "shell_resize") {
		t.Error("a resize frame on a read-only extension session must be dropped")
	}

	// The same session on a call it WAS granted still passes.
	if ws.refuseWSCapability(shell, "file_read") {
		t.Error("a granted capability must not be refused on the socket path")
	}
	// And an ordinary session is untouched, so the rule is invisible to every
	// caller that predates it.
	plain := newRestProbe(t, nil, nil)
	plainShell := plain.sess.GetChildShell(plain.shellID)
	wsPlain := &uiWS{h: &Handler{Sessions: plain.sessMgr}}
	if wsPlain.refuseWSCapability(plainShell, "shell_input") {
		t.Error("an unrestricted session must not be gated on the socket path")
	}
}

// TestCreateRepairsAStaleTemplate pins the one way out of a broken file.
//
// A config.toml that no longer validates fails Load, so the update path cannot
// reach it. If create refused the name too, a broken template would be fixable
// only by someone with shell access to the data directory — which is exactly where
// a first attempt at "don't overwrite what exists" landed.
func TestCreateRepairsAStaleTemplate(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	p := newRestProbe(t, nil, nil)
	p.withExtensions(t)

	// A hand-written file that fails Validate (kind=skill with no paths).
	stale := filepath.Join(p.dataDir, "extensions", "stale")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "config.toml"), []byte("kind = \"skill\"\ntarget = \"internal\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/extensions",
		strings.NewReader(`{"name":"stale","kind":"prompt","target":"internal"}`))
	req.Header.Set("Content-Type", "application/json")
	p.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create over a stale template = %d, want 201 (that is the repair): %s", rr.Code, rr.Body)
	}

	// A WORKING template of the same name is still protected.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/extensions",
		strings.NewReader(`{"name":"stale","kind":"prompt","target":"internal"}`))
	req.Header.Set("Content-Type", "application/json")
	p.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("create over a working template = %d, want 409", rr.Code)
	}
}
