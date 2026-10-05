package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// hostAttributionEnv is a live in-process instance (real loopback SSH server +
// manager + mux) with one stored remote-shaped profile. The profile is not
// dialed: these tests are about which host a session is attributed to, and the
// attribution is decided before any dial happens.
type hostAttributionEnv struct {
	mux     *http.ServeMux
	store   *sshconfig.Store
	sessMgr *session.Manager
}

func newHostAttributionEnv(t *testing.T) *hostAttributionEnv {
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
	sessMgr := session.NewManager(message.NewManager(store), store, srv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		srv.Stop()
	})
	cfgStore := sshconfig.NewStore(dir)
	h := &Handler{Sessions: sessMgr, SSH: cfgStore}
	mux := http.NewServeMux()
	h.Register(mux)
	return &hostAttributionEnv{mux: mux, store: cfgStore, sessMgr: sessMgr}
}

func (e *hostAttributionEnv) sessions(t *testing.T) []map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/sessions = %d: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Sessions []map[string]any `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Sessions
}

func (e *hostAttributionEnv) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions", strings.NewReader(body)))
	return rr
}

// TestSessionReportsItsProfile pins the field NetHub attributes by. A session's
// Name is whatever the caller typed and can be renamed at any moment; the card
// needs the profile separately or a renamed session drops off its host.
func TestSessionReportsItsProfile(t *testing.T) {
	e := newHostAttributionEnv(t)

	rr := e.post(t, `{"ssh_config":"internal","name":"renamed-by-hand","rows":24,"cols":80}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rr.Code, rr.Body.String())
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	for _, s := range e.sessions(t) {
		if s["id"] != created.SessionID {
			continue
		}
		if got := s["name"]; got != "renamed-by-hand" {
			t.Fatalf("name = %v, want the caller's display name", got)
		}
		if got := s["ssh_config"]; got != "internal" {
			t.Fatalf("ssh_config = %v, want the profile the session was created from", got)
		}
		return
	}
	t.Fatal("created session missing from GET /api/sessions")
}

// TestRenamingASessionKeepsItsProfile is the regression that produced the wrong
// lamp: renaming a session rewrote the only field the card could attribute it
// by, so a live session under a host read as offline.
func TestRenamingASessionKeepsItsProfile(t *testing.T) {
	e := newHostAttributionEnv(t)

	rr := e.post(t, `{"ssh_config":"internal","rows":24,"cols":80}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", rr.Code, rr.Body.String())
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	rename := httptest.NewRecorder()
	e.mux.ServeHTTP(rename, httptest.NewRequest(http.MethodPatch, "/api/sessions/"+created.SessionID,
		strings.NewReader(`{"name":"something-else"}`)))
	if rename.Code != http.StatusOK {
		t.Fatalf("rename = %d: %s", rename.Code, rename.Body.String())
	}

	info := e.sessMgr.Get(created.SessionID).Info()
	if info.Name != "something-else" {
		t.Fatalf("name = %q, want the renamed value", info.Name)
	}
	if info.SSHConfig != "internal" {
		t.Fatalf("ssh_config = %q after rename, want the profile to survive it", info.SSHConfig)
	}
}

// TestRenamingAProfileFollowsLiveSessions pins the other half: the profile name
// is the label the card shows, so a rename must move the running sessions with
// it or they keep pointing at a name that no longer exists.
func TestRenamingAProfileFollowsLiveSessions(t *testing.T) {
	e := newHostAttributionEnv(t)

	profile := "kind = \"remote\"\nhost = \"example.invalid\"\nuser = \"tester\"\npassword = \"placeholder\"\n"
	if err := e.store.Save("old-name", []byte(profile)); err != nil {
		t.Fatalf("save profile: %v", err)
	}
	sess, err := e.sessMgr.Create(session.Config{Mode: api.ModePTY, Rows: 24, Cols: 80, Name: "custom-session", SSHConfig: "old-name"})
	if err != nil {
		t.Fatal(err)
	}

	body := "kind = \"remote\"\nhost = \"example.invalid\"\nuser = \"tester\"\npassword = \"placeholder\"\n"
	rr := httptest.NewRecorder()
	e.mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPut,
		"/api/connections/new-name?from=old-name", strings.NewReader(body)))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("rename profile = %d: %s", rr.Code, rr.Body.String())
	}

	if got := sess.Info().SSHConfig; got != "new-name" {
		t.Fatalf("ssh_config = %q after the profile was renamed, want new-name", got)
	}
	if got := sess.Info().Name; got != "custom-session" {
		t.Fatalf("session display name changed during profile rename: %q", got)
	}
}
