package webui

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/open-mcp-ai/termcp/internal/message"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshserver"
	"github.com/open-mcp-ai/termcp/internal/storage"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// readDeadline bounds one wait for a frame while watching, and is also the
// yardstick for "did that timeout arrive when it should have": a quiet connection
// reports one only after the full deadline, a dead one reports immediately.
const readDeadline = 2 * time.Second

// TestWatchAddActuallySubscribes drives the Web UI socket the way a browser tab
// does and asks the only question that matters to a user: once a tab says it is
// watching a channel, does that channel's output reach the tab?
//
// It exists because watch_add has no server-side acknowledgement: readPump drops
// addWatch's error (`_ = c.addWatch(id)`), so a failed subscription is completely
// silent — the tab simply never receives bytes and nothing says why. Any handle
// the browser holds but addWatch cannot resolve therefore looks exactly like a
// quiet terminal. This test pins the working path so a regression cannot hide
// behind that silence.
func TestWatchAddActuallySubscribes(t *testing.T) {
	sshSrv := sshserver.New()
	if err := sshSrv.Start(); err != nil {
		t.Fatal(err)
	}
	store := storage.New(t.TempDir())
	msgMgr := message.NewManager(store)
	sessMgr := session.NewManager(msgMgr, store, sshSrv)
	t.Cleanup(func() {
		for _, s := range sessMgr.ListAll() {
			_ = sessMgr.Delete(s.ID)
		}
		sshSrv.Stop()
	})

	h := &Handler{Sessions: sessMgr, MsgMgr: msgMgr, Version: "test"}
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sess, err := sessMgr.Create(session.Config{
		Command:   shellForTest(),
		Args:      shellArgsForTest(),
		Mode:      api.ModePipe,
		Name:      "ws-watch",
		SSHConfig: "internal",
		Rows:      24,
		Cols:      80,
	})
	if err != nil {
		t.Fatal(err)
	}
	shell := sess.PrimaryShell()
	if shell == nil {
		t.Fatal("session has no primary shell")
	}

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ui/ws"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Drain the pushed panel(s) so the reads below are about terminal frames.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for the sessions panel: %v", err)
		}
		if m["type"] == "sessions" {
			break
		}
	}

	if err := conn.WriteJSON(map[string]any{"type": "watch_add", "id": shell.ID}); err != nil {
		t.Fatalf("watch_add: %v", err)
	}
	marker := "watch-marker-" + time.Now().Format("150405.000000")
	time.Sleep(200 * time.Millisecond)
	if err := conn.WriteJSON(map[string]any{"type": "input", "id": shell.ID, "d": "echo " + marker + "\n"}); err != nil {
		t.Fatalf("input: %v", err)
	}

	// The output pump coalesces, so the marker may arrive split across frames;
	// accumulate the payloads rather than requiring one frame to hold it all.
	deadline := time.Now().Add(20 * time.Second)
	var got strings.Builder
	early := 0 // consecutive timeouts that arrived before their deadline
	for time.Now().Before(deadline) {
		started := time.Now()
		_ = conn.SetReadDeadline(started.Add(readDeadline))
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			// A read timeout means the channel is quiet, which is expected while
			// waiting for output — but it is NOT the only way ReadJSON fails, and
			// treating every failure as "quiet" re-reads a dead connection. gorilla
			// panics on that ("repeated read on failed websocket connection"), which
			// turned a slow CI runner into a panic instead of a test failure.
			if !isTimeout(err) {
				t.Fatalf("reading the watch channel: %v", err)
			}
			// isTimeout ALONE STILL SPINS ON A DEAD CONNECTION. Once the peer is
			// gone gorilla caches the failure and every read returns at once, so the
			// loop no longer waits out its two seconds — it retries thousands of times
			// inside the same 20s budget and trips the same panic through the door that
			// was left open.
			//
			// A healthy idle connection waits the WHOLE deadline before reporting a
			// timeout, so a timeout that arrives early is not "quiet" — it is usually
			// broken. But ONE early timeout is not proof: the deadline is armed per
			// read, and the first read after the drain can surface a deadline left
			// over from before it was reset (6.9µs on a loaded macOS runner, with the
			// connection perfectly alive). Two in a row, each against a freshly armed
			// deadline, cannot be a leftover — that is the cached failure.
			if elapsed := time.Since(started); elapsed < readDeadline/2 {
				early++
				if early >= 2 {
					t.Fatalf("the watch channel stopped answering (%d early timeouts, last after %v); got %q",
						early, elapsed, got.String())
				}
				continue
			}
			early = 0
			continue
		}
		if m["type"] != "terminal" {
			continue
		}
		if id, _ := m["id"].(string); id != shell.ID {
			continue
		}
		d, _ := m["d"].(string)
		got.WriteString(d)
		if strings.Contains(got.String(), marker) {
			return // the tab received the channel's live output
		}
	}
	t.Fatalf("watch_add did not deliver the channel's output; got %q", got.String())
}

// shellForTest and shellArgsForTest pick a shell that stays alive and echoes,
// which is what an interactive terminal test needs. Kept local to this file so it
// does not collide with other test helpers in the package.
func shellForTest() string {
	if runtime.GOOS == "windows" {
		return "cmd.exe"
	}
	return "/bin/sh"
}

func shellArgsForTest() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	return []string{"-i"}
}

// isTimeout reports whether err is a read deadline, as opposed to the
// connection being gone. Only the first kind means "keep waiting": re-reading a
// closed WebSocket panics inside gorilla rather than returning again.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
