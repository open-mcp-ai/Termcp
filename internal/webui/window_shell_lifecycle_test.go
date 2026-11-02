package webui

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// Closing a window releases the shell resource it owns. The internal primary
// uses the same archive route as every other channel: that ends the shell, but
// leaves its parent session/transport available for SFTP, forwards, and new shells.
func TestArchivePrimaryShellReleasesChannelWithoutTerminatingSession(t *testing.T) {
	if testing.Short() {
		t.Skip("starts an SSH server")
	}
	srv := sshserver.New()
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	store := storage.New(dir)
	sessions := session.NewManager(nil, store, srv)
	t.Cleanup(func() {
		for _, sess := range sessions.ListAll() {
			_ = sessions.Delete(sess.ID)
		}
		srv.Stop()
	})

	sess, err := sessions.Create(session.Config{Mode: api.ModePTY, Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	primary := sess.PrimaryShellID()
	if primary == "" {
		t.Fatal("session has no primary shell")
	}

	h := &Handler{Sessions: sessions, SSH: sshconfig.NewStore(dir)}
	mux := http.NewServeMux()
	h.Register(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/shells/"+primary+"/archive", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("archive primary = %d (%s), want 204", response.Code, response.Body.String())
	}
	if got := sess.GetChildShell(primary); got != nil {
		t.Fatal("primary shell still registered after its owning window closed")
	}
	if got := sess.Info().Status; got != api.SessionRunning {
		t.Fatalf("session status = %q, want running after one shell closes", got)
	}
	var retained bool
	for _, sh := range sess.SnapshotShells() {
		if sh.ID == primary && sh.Status == api.SessionExited {
			retained = true
			break
		}
	}
	if !retained {
		t.Fatalf("closed primary %q was not retained as an exited shell", primary)
	}

	child, err := sess.CreateChildShell("", nil, true, 24, 80, "")
	if err != nil {
		t.Fatalf("session transport should remain available for a new shell: %v", err)
	}
	if err := sess.CloseChildShell(child.ID); err != nil {
		t.Fatal(err)
	}
}
