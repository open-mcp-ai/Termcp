package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// TestClosedSessionRestGuards pins the REST side of the close-vs-delete model:
// after a session is closed (DEAD), the file and forward endpoints answer 409
// instead of touching a dead transport, and no forward is created.
func TestClosedSessionRestGuards(t *testing.T) {
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
	fm := forward.NewForwardManager()
	h := &Handler{Sessions: sessMgr, SSH: sshconfig.NewStore(dir), ForwardMgr: fm}
	mux := http.NewServeMux()
	h.Register(mux)

	// Close the session in place: the registry entry stays (DEAD), the transport
	// is gone.
	sess.Terminate(true, 0)
	if got := sess.Info().Status; got != api.SessionExited {
		t.Fatalf("session status after terminate = %s, want exited", got)
	}

	// File listing needs a live connection → 409, not a 500 SFTP error.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/sessions/"+sess.ID+"/files?path=/", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("GET files on closed session = %d (%s), want 409", rr.Code, rr.Body.String())
	}

	// Forward creation must refuse a closed session and create no listener.
	rr = httptest.NewRecorder()
	body := `{"direction":"local","remote_host":"localhost","remote_port":80}`
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/api/sessions/"+sess.ID+"/forwards", strings.NewReader(body)))
	if rr.Code != http.StatusConflict {
		t.Fatalf("POST forwards on closed session = %d (%s), want 409", rr.Code, rr.Body.String())
	}
	if got := len(fm.List()); got != 0 {
		t.Fatalf("closed session must not create a forward, got %d", got)
	}

	// The session locator still resolves (read-only); a shell locator is 409.
	rr = getResolve(t, mux, "termcp://#"+sess.ID)
	if rr.Code != http.StatusOK {
		t.Fatalf("session locator on closed session = %d, want 200", rr.Code)
	}
	if resp := decodeResolve(t, rr); resp.Status != "exited" {
		t.Fatalf("closed session locator status = %q, want exited", resp.Status)
	}
	rr = getResolve(t, mux, "termcp://#"+sess.ID+":1")
	if rr.Code != http.StatusConflict {
		t.Fatalf("shell locator on closed session = %d (%s), want 409", rr.Code, rr.Body.String())
	}
}
