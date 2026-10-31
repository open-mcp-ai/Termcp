package webui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestPurgeArchivedSessionsSweepsOnlyEndedSessions pins the one property that
// makes a bulk sweep safe to expose at all: a running session is never in the
// target set. Everything else about the route (the count, the id list) is
// reporting; this is the line that would close somebody's live terminal.
func TestPurgeArchivedSessionsSweepsOnlyEndedSessions(t *testing.T) {
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

	live, err := sessMgr.Create(session.Config{Mode: api.ModePTY, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	ended, err := sessMgr.Create(session.Config{Mode: api.ModePTY, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	ended.Terminate(true, 0)

	h := &Handler{Sessions: sessMgr, SSH: sshconfig.NewStore(dir)}
	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions?status=dead", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE /api/sessions?status=dead = %d (%s), want 200", rr.Code, rr.Body.String())
	}

	var body struct {
		Deleted []string `json:"deleted"`
		Count   int      `json:"count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (%s)", err, rr.Body.String())
	}
	if body.Count != 1 || len(body.Deleted) != 1 || body.Deleted[0] != ended.ID {
		t.Fatalf("deleted = %v (count %d), want exactly [%s]", body.Deleted, body.Count, ended.ID)
	}
	if sessMgr.Get(ended.ID) != nil {
		t.Fatalf("ended session %s still in the registry after the sweep", ended.ID)
	}
	if sessMgr.Get(live.ID) == nil {
		t.Fatalf("running session %s was swept away; the route must only reach ended sessions", live.ID)
	}

	// Idempotent: a second sweep finds nothing and says so rather than erroring.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions?status=dead", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("second sweep = %d (%s), want 200", rr.Code, rr.Body.String())
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode second response: %v", err)
	}
	if body.Count != 0 || len(body.Deleted) != 0 {
		t.Fatalf("second sweep deleted %v, want nothing", body.Deleted)
	}
}

// TestPurgeArchivedSessionsRejectsUnknownStatus keeps the filter from being a
// silent no-op: "?status=bogus" must not read as "sweep nothing, exit 0", which
// a caller would take for a successful cleanup.
func TestPurgeArchivedSessionsRejectsUnknownStatus(t *testing.T) {
	h := &Handler{}
	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodDelete, "/api/sessions?status=bogus", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("DELETE /api/sessions?status=bogus = %d (%s), want 400", rr.Code, rr.Body.String())
	}
}
