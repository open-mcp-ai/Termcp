package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// resolveFixture starts a real loopback session and returns a mux plus the
// session, so the resolver is exercised against the same objects the REST API
// serves (live session, real shell ids, real ordering).
func resolveFixture(t *testing.T) (*http.ServeMux, *session.Session) {
	t.Helper()
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
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

	sess, err := sessMgr.Create(session.Config{Mode: api.ModePTY, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{Sessions: sessMgr, SSH: sshconfig.NewStore(dir)}
	mux := http.NewServeMux()
	h.Register(mux)
	return mux, sess
}

func getResolve(t *testing.T, mux *http.ServeMux, locator string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/resolve?url="+url.QueryEscape(locator), nil))
	return rr
}

func decodeResolve(t *testing.T, rr *httptest.ResponseRecorder) resolveResponse {
	t.Helper()
	var resp resolveResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return resp
}

// TestResolveLocators covers the three locator shapes an agent receives from
// the Web UI copy buttons (entry / session / shell index).
func TestResolveLocators(t *testing.T) {
	mux, sess := resolveFixture(t)
	primary := sess.PrimaryShellID()

	// Session locator (short form, what the copy buttons emit).
	rr := getResolve(t, mux, "termcp://#"+sess.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("session resolve = %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResolve(t, rr)
	if resp.Kind != "session" || resp.SessionID != sess.ID {
		t.Fatalf("session resolve = %+v, want kind=session id=%s", resp, sess.ID)
	}

	// Shell locator without index = primary shell.
	rr = getResolve(t, mux, "termcp://#"+sess.ID)
	if resp := decodeResolve(t, rr); resp.Kind != "session" {
		t.Fatalf("bare session locator = %+v, want session kind", resp)
	}
	rr = getResolve(t, mux, "termcp://#"+sess.ID+":1")
	if rr.Code != http.StatusOK {
		t.Fatalf("shell resolve = %d: %s", rr.Code, rr.Body.String())
	}
	resp = decodeResolve(t, rr)
	if resp.Kind != "shell" || resp.ShellID != primary || resp.Index != 1 {
		t.Fatalf("shell resolve = %+v, want kind=shell shell_id=%s index=1", resp, primary)
	}

	// A second channel resolves at index 2 in creation order.
	if _, err := sess.CreateChildShell("", nil, true, 24, 80, "shell-2"); err != nil {
		t.Fatal(err)
	}
	shells := sess.ListChildShells()
	if len(shells) != 2 {
		t.Fatalf("shell count = %d, want 2", len(shells))
	}
	rr = getResolve(t, mux, "termcp://#"+sess.ID+":2")
	resp = decodeResolve(t, rr)
	if rr.Code != http.StatusOK || resp.ShellID != shells[1].ID || resp.Index != 2 {
		t.Fatalf("shell 2 resolve = %d %+v, want shell_id=%s index=2", rr.Code, resp, shells[1].ID)
	}
	if resp.Name != shells[1].Name {
		t.Errorf("shell 2 name = %q, want %q", resp.Name, shells[1].Name)
	}

	// Entry locator: the built-in loopback profile always exists.
	rr = getResolve(t, mux, "termcp://internal")
	if rr.Code != http.StatusOK {
		t.Fatalf("entry resolve = %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResolve(t, rr); resp.Kind != "entry" || resp.SSHConfig != "internal" {
		t.Fatalf("entry resolve = %+v, want kind=entry ssh_config=internal", resp)
	}
}

// TestResolveLocatorErrors pins the actionable failures: malformed locators are
// 400, unknown profiles/sessions/shell indexes are 404 with a hint.
func TestResolveLocatorErrors(t *testing.T) {
	mux, sess := resolveFixture(t)

	if rr := getResolve(t, mux, ""); rr.Code != http.StatusBadRequest {
		t.Errorf("empty url = %d, want 400", rr.Code)
	}
	if rr := getResolve(t, mux, "termcp://shells/xyz"); rr.Code != http.StatusBadRequest {
		t.Errorf("notification URI = %d, want 400", rr.Code)
	}
	if rr := getResolve(t, mux, "termcp://#"+sess.ID+":0"); rr.Code != http.StatusBadRequest {
		t.Errorf("index 0 = %d, want 400", rr.Code)
	}
	if rr := getResolve(t, mux, "termcp://no-such-profile"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown entry = %d, want 404", rr.Code)
	}
	if rr := getResolve(t, mux, "termcp://#no-such-session"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown session = %d, want 404", rr.Code)
	}
	if rr := getResolve(t, mux, "termcp://#"+sess.ID+":9"); rr.Code != http.StatusNotFound {
		t.Errorf("shell index out of range = %d, want 404", rr.Code)
	}
}
