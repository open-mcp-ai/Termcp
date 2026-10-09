package webui

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/internal/storage"
	sshstd "golang.org/x/crypto/ssh"
)

// stallingSSHServer listens on a real TCP port and accepts any password, but
// waits before starting the SSH handshake. That gap is what makes the dial
// interruptible in a test: the client is committed (the socket is open) and
// cannot finish until the stall elapses, so the caller has a window in which to
// give up — and, once it has, a window in which the dial would otherwise
// complete successfully and register a session nobody asked for.
func stallingSSHServer(t *testing.T, stall time.Duration) (host, port string, stop func()) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := sshstd.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &sshstd.ServerConfig{
		PasswordCallback: func(sshstd.ConnMetadata, []byte) (*sshstd.Permissions, error) {
			return nil, nil // accept any credential: the point is the timing, not the auth
		},
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				time.Sleep(stall)
				sconn, chans, reqs, err := sshstd.NewServerConn(c, cfg)
				if err != nil {
					c.Close()
					return
				}
				defer sconn.Close()
				go sshstd.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "session" {
						nc.Reject(sshstd.UnknownChannelType, "session only")
						continue
					}
					ch, requests, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						for req := range requests {
							req.Reply(req.Type == "pty-req" || req.Type == "shell" || req.Type == "exec", nil)
						}
					}()
					go func() { io.Copy(io.Discard, ch); ch.Close() }()
				}
			}()
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	return h, p, func() { ln.Close() }
}

// Issue #79, first half: a connect that is still dialing is visible and
// interruptible. The second half is what this test covers — the interrupt is
// real. The dialog's close button is labelled "Close (cancel connect)", so a
// user who presses it believes the attempt is over; the button aborts the
// browser's fetch, and before this was fixed the server, which never saw that
// abort, dialed on to completion. A successful late dial then registered a
// running session that no window was showing: unreachable from the page (its
// placeholder was gone), but real — holding a remote shell, appearing in the
// session list, and visible to the agent through list_sessions. "Cancel" that
// leaves a live session behind is worse than no cancel at all, because the user
// has already been told it stopped.
//
// The dial here is deliberately one that WOULD succeed: the server stalls, then
// accepts. A test that pointed at an unreachable host would pass even with the
// bug, since the dial would fail on its own and register nothing.
func TestCanceledSessionRequestLeavesNoSession(t *testing.T) {
	host, port, stop := stallingSSHServer(t, 1200*time.Millisecond)
	defer stop()

	dir := t.TempDir()
	store := storage.New(dir)
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, nil)
	sshStore := sshconfig.NewStore(dir)
	body := []byte("kind = \"remote\"\nhost = \"" + host + "\"\nport = " + port +
		"\nuser = \"probe\"\npassword = \"x\"\ndial_timeout_seconds = 30\n")
	if err := sshStore.Save("slowhost", body); err != nil {
		t.Fatal(err)
	}

	h := &Handler{Sessions: sessMgr, SSH: sshStore}
	mux := http.NewServeMux()
	h.Register(mux)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/api/sessions",
		strings.NewReader(`{"ssh_config":"slowhost","command":"sleep","args":["30"],"mode":"pipe","rows":24,"cols":80}`)).
		WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	served := make(chan struct{})
	go func() {
		mux.ServeHTTP(rr, req)
		close(served)
	}()

	// Let the dial get past the TCP connect and into the stall, then close the
	// pending window: this is the browser aborting its fetch.
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return after its request was canceled; the dial is not interruptible")
	}

	// Give the stalled dial time to complete. Without the fix it does exactly
	// that, so this wait is what turns the race into a reliable failure.
	time.Sleep(2 * time.Second)

	if got := sessMgr.ListAll(); len(got) != 0 {
		for _, s := range got {
			t.Errorf("canceled request left session %s (status=%s profile=%s): the window was closed, so nothing on the page can reach it",
				s.ID, s.Status, s.SSHConfig)
		}
	}
}
