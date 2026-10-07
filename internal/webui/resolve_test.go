package webui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
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

// TestResolveShellIndexIsStableAcrossClose is the REST half of issue #73: the
// locator a copy button emits must keep naming the same channel after an earlier
// channel is closed. The index is assigned at creation, not derived from the
// current list, so the survivors keep their numbers and a closed channel's
// number stops resolving instead of sliding onto its neighbour.
func TestResolveShellIndexIsStableAcrossClose(t *testing.T) {
	mux, sess := resolveFixture(t)

	second, err := sess.CreateChildShell("", nil, true, 24, 80, "")
	if err != nil {
		t.Fatal(err)
	}
	third, err := sess.CreateChildShell("", nil, true, 24, 80, "")
	if err != nil {
		t.Fatal(err)
	}

	// Each channel resolves to itself at its own number.
	for _, tc := range []struct {
		index int
		id    string
	}{{1, sess.PrimaryShellID()}, {2, second.ID}, {3, third.ID}} {
		rr := getResolve(t, mux, fmt.Sprintf("termcp://#%s:%d", sess.ID, tc.index))
		if rr.Code != http.StatusOK {
			t.Fatalf("index %d resolve = %d: %s", tc.index, rr.Code, rr.Body.String())
		}
		resp := decodeResolve(t, rr)
		if resp.ShellID != tc.id || resp.Index != tc.index {
			t.Fatalf("index %d resolve = %+v, want shell_id=%s", tc.index, resp, tc.id)
		}
	}

	// Close the middle channel: a positional lookup would now answer :3 with
	// whatever slid into third place.
	if err := sess.CloseChildShell(second.ID); err != nil {
		t.Fatal(err)
	}

	rr := getResolve(t, mux, fmt.Sprintf("termcp://#%s:3", sess.ID))
	if rr.Code != http.StatusOK {
		t.Fatalf(":3 after the close = %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResolve(t, rr); resp.ShellID != third.ID {
		t.Fatalf(":3 after the close = %+v, want the untouched third channel %s", resp, third.ID)
	}

	// The closed channel's number is retired: resolving it must 404 rather than
	// address a different shell.
	if rr := getResolve(t, mux, fmt.Sprintf("termcp://#%s:2", sess.ID)); rr.Code != http.StatusNotFound {
		t.Errorf(":2 after its channel was closed = %d, want 404 (a locator must never slide)", rr.Code)
	}
}

// TestShellListCarriesChannelIndex pins that the REST shell list exposes the same
// number the locator resolves, which is what lets the Web UI label and copy a tab
// from server data instead of a client-side counter.
func TestShellListCarriesChannelIndex(t *testing.T) {
	mux, sess := resolveFixture(t)
	if _, err := sess.CreateChildShell("", nil, true, 24, 80, ""); err != nil {
		t.Fatal(err)
	}

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/shells", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("list shells = %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Shells []struct {
			ID    string `json:"id"`
			Index int    `json:"index"`
		} `json:"shells"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Shells) != 2 {
		t.Fatalf("shell count = %d, want 2", len(body.Shells))
	}
	for i, sh := range body.Shells {
		if want := i + 1; sh.Index != want {
			t.Errorf("shells[%d] (id=%s).index = %d, want %d", i, sh.ID, sh.Index, want)
		}
	}
}

// TestCreateSessionReturnsPrimaryChannelIndex pins the other end of the same
// contract: the session-creation response carries the primary channel's number,
// so the Web UI can label and copy the first tab from server data. It must be
// the same number the resolver answers for that shell, not a value the client
// assumes.
func TestCreateSessionReturnsPrimaryChannelIndex(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := storage.New(dir)
	sessMgr := session.NewManager(message.NewManager(store), store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})

	h := &Handler{Sessions: sessMgr, SSH: sshconfig.NewStore(dir)}
	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions",
		strings.NewReader(`{"ssh_config":"internal","rows":24,"cols":80}`)))
	if rr.Code != http.StatusOK {
		t.Fatalf("create session = %d: %s", rr.Code, rr.Body.String())
	}
	var created struct {
		SessionID string `json:"session_id"`
		ShellID   string `json:"shell_id"`
		Index     int    `json:"index"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Index != 1 {
		t.Errorf("create session index = %d, want 1 for the primary channel", created.Index)
	}

	// The number the client was handed must be the number the resolver returns
	// for that very shell.
	res := getResolve(t, mux, fmt.Sprintf("termcp://#%s:1", created.SessionID))
	if res.Code != http.StatusOK {
		t.Fatalf("resolve :1 = %d: %s", res.Code, res.Body.String())
	}
	resp := decodeResolve(t, res)
	if resp.ShellID != created.ShellID {
		t.Errorf("resolved :1 to shell %q, but create returned %q", resp.ShellID, created.ShellID)
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

// TestCreateSessionAcceptsEntryLocator pins that the ssh_config body field takes
// the spelling the Web UI's copy button actually produces.
//
// A user copies "termcp://rock64" from a connection card and pastes it as the
// connection; MCP's session_start accepts that, so the HTTP body must too, or the
// same string works in a chat and fails in curl. It is a JSON body field — not a
// URL path — so the '#' and ':' that keep locators out of paths are harmless here.
func TestCreateSessionAcceptsEntryLocator(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := storage.New(dir)
	sessMgr := session.NewManager(message.NewManager(store), store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})

	h := &Handler{Sessions: sessMgr, SSH: sshconfig.NewStore(dir)}
	mux := http.NewServeMux()
	h.Register(mux)

	post := func(sshConfig string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions",
			strings.NewReader(`{"ssh_config":"`+sshConfig+`","rows":24,"cols":80}`)))
		return rr
	}

	if rr := post("termcp://internal"); rr.Code != http.StatusOK {
		t.Fatalf("ssh_config=termcp://internal = %d: %s", rr.Code, rr.Body.String())
	}
	// The locator's entry is the profile name that gets resolved and reported.
	var created struct {
		SSHConfig string `json:"ssh_config"`
	}
	if rr := post("termcp://internal"); rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if created.SSHConfig != "internal" {
			t.Errorf("ssh_config = %q, want the resolved profile name %q", created.SSHConfig, "internal")
		}
	}

	// A session/shell locator is not a profile: rejected with a hint, not treated
	// as a profile name that happens to contain punctuation.
	rr := post("termcp://#abc123")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("ssh_config=termcp://#abc123 = %d, want 400", rr.Code)
	}
	if body := rr.Body.String(); !strings.Contains(body, "entry") {
		t.Errorf("rejection should point at entry locators, got %q", body)
	}
}
