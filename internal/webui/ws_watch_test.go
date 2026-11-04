package webui

import (
	"context"
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

func TestReplacedWatchPumpCannotSendStaleFrames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws := &uiWS{
		ctx:   ctx,
		send:  make(chan []byte, 2),
		watch: map[string]*wsWatchEntry{"shell-1": {rid: 2}},
	}

	if ws.sendWatchTerminalPayload("shell-1", 1, []byte(`{"type":"terminal"}`)) {
		t.Fatal("stale terminal frame was accepted")
	}
	if ws.sendWatchControlPayload("shell-1", 1, []byte(`{"type":"terminal_done"}`)) {
		t.Fatal("stale terminal_done was accepted")
	}
	if len(ws.send) != 0 {
		t.Fatalf("stale frames reached the send queue: %d", len(ws.send))
	}
	if !ws.sendWatchTerminalPayload("shell-1", 2, []byte(`{"type":"terminal"}`)) {
		t.Fatal("current terminal frame was rejected")
	}
}

// TestWatchAddActuallySubscribes drives the Web UI socket the way a browser tab
// does and asks the only question that matters to a user: once a tab says it is
// watching a channel, does that channel's output reach the tab?
//
// The subscriber now receives a watch_ready acknowledgement carrying the first
// byte offset that can appear in a terminal frame. The test also requires every
// terminal frame to describe a contiguous half-open byte range, so a reconnect
// or browser consumer can distinguish a complete handoff from a gap.
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

	missingID := shell.ID + "-missing"
	if err := conn.WriteJSON(map[string]any{"type": "watch_add", "id": missingID}); err != nil {
		t.Fatalf("bad watch_add: %v", err)
	}
	for {
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for watch_error: %v", err)
		}
		if m["type"] == "watch_error" {
			if id, _ := m["id"].(string); id != missingID {
				t.Fatalf("watch_error id = %q", id)
			}
			break
		}
	}

	if err := conn.WriteJSON(map[string]any{"type": "watch_add", "id": shell.ID}); err != nil {
		t.Fatalf("watch_add: %v", err)
	}
	var expectedOffset int64
	ready := false
	for !ready {
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for watch_ready: %v", err)
		}
		switch m["type"] {
		case "watch_error":
			t.Fatalf("watch_add failed: %v", m["error"])
		case "watch_ready":
			if id, _ := m["id"].(string); id != shell.ID {
				t.Fatalf("watch_ready id = %q", id)
			}
			cursor, ok := m["cursor"].(float64)
			if !ok || cursor < 0 {
				t.Fatalf("watch_ready cursor = %#v", m["cursor"])
			}
			expectedOffset = int64(cursor)
			ready = true
		}
	}
	marker := "watch-marker-" + time.Now().Format("150405.000000")

	// THE SHELL IS A PIPE, SO THE LINE ENDS WITH '\n'.
	//
	// Enter is '\r' only on a pty — `shell_key enter` branches on exactly that
	// (internal/session/keys.go) — and a pipe has no line discipline to translate
	// anything, so '\r' is just a byte in the middle of the command and the line never
	// ends. This test creates ModePipe, so '\n' is the separator. Changing it to '\r'
	// made the command never execute at all.
	//
	// THEN LET THE SHELL START, WITHOUT DECIDING HOW LONG THAT TAKES.
	//
	// `handleWSInput` looks the shell up and RETURNS SILENTLY when the session's child
	// shell is not registered yet, so input sent too early is dropped and the test then
	// waits for output nobody asked for. A fixed 200ms sleep sat here and was
	// load-bearing for that reason.
	//
	// A shell that prints a banner gives us something to wait FOR. A silent pipe gives
	// nothing, so the wait is bounded and its expiry is not an error. It cannot share
	// this connection: gorilla caches the first read error and never clears it (see the
	// deadline comment below), so a timeout here would poison the reads that matter.
	// Hence the throwaway connection — it is the only way to ask "has the shell said
	// anything yet" without spending the real one.

	// THE SHELL MUST BE REGISTERED, OR THE INPUT IS DROPPED IN SILENCE.
	//
	// `handleWSInput` resolves the channel with `getTerminalShell` and returns without
	// a word when it is not there yet, so input sent too early simply disappears and
	// the test then waits for output nobody asked for. A fixed 200ms sleep used to sit
	// here for that reason; on a loaded runner it was a coin flip.
	//
	// The wait polls the SAME lookup the input path uses, so it is not a guess about
	// timing, and it costs nothing when the shell is already registered (the usual
	// case). No websocket read is involved: gorilla caches the first read error, so a
	// probe read that times out would poison the connection the assertions need.
	deadline := time.Now().Add(10 * time.Second)
	for sessMgr.GetChildShell(shell.ID) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("channel %s never registered with the session manager", shell.ID)
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := conn.WriteJSON(map[string]any{"type": "input", "id": shell.ID, "d": "echo " + marker + "\n"}); err != nil {
		t.Fatalf("input: %v", err)
	}

	// ONE DEADLINE FOR THE WHOLE WAIT, NOT A POLL PER FRAME.
	//
	// gorilla caches the first read error on a connection and NEVER clears it: one
	// expired read deadline poisons the socket for the rest of its life, and every
	// later read returns that same error in microseconds without touching the wire.
	// A poll loop with a 2s deadline per iteration therefore kills its own connection
	// the first time it waits for output that has not arrived yet -- and then spins,
	// because the failures come back instantly.
	//
	// So the deadline is armed once, for the whole budget. A timeout at the end means
	// the marker never showed up, which is the thing worth failing on. A non-timeout
	// error means the peer is gone. Nothing in between needs a heuristic.
	readDeadline := time.Now().Add(20 * time.Second)
	var got strings.Builder
	sentExit := false
	resubscribePending := false
	_ = conn.SetReadDeadline(readDeadline)
	for {
		var m map[string]any
		if err := conn.ReadJSON(&m); err != nil {
			if isTimeout(err) {
				t.Fatalf("the marker never arrived within %v; got %q", 20*time.Second, got.String())
			}
			t.Fatalf("reading the watch channel: %v (got %q)", err, got.String())
		}
		if id, _ := m["id"].(string); id != shell.ID {
			continue
		}
		if m["type"] == "watch_ready" {
			if !resubscribePending {
				t.Fatalf("unexpected second watch_ready: %+v", m)
			}
			cursor, ok := m["cursor"].(float64)
			if !ok || cursor < 0 {
				t.Fatalf("replacement watch_ready cursor = %#v", m["cursor"])
			}
			expectedOffset = int64(cursor)
			resubscribePending = false
			if err := conn.WriteJSON(map[string]any{"type": "input", "id": shell.ID, "d": "exit\n"}); err != nil {
				t.Fatalf("input exit: %v", err)
			}
			sentExit = true
			continue
		}
		if m["type"] == "terminal_done" {
			if !sentExit {
				t.Fatalf("terminal_done arrived before the marker: %q", got.String())
			}
			cursor, ok := m["cursor"].(float64)
			if !ok {
				t.Fatalf("terminal_done lacks its final cursor: %+v", m)
			}
			if int64(cursor) != expectedOffset {
				t.Fatalf("terminal_done cursor = %.0f, want %d", cursor, expectedOffset)
			}
			return
		}
		if m["type"] != "terminal" {
			continue
		}
		d, _ := m["d"].(string)
		start, startOK := m["start"].(float64)
		end, endOK := m["end"].(float64)
		if !startOK || !endOK {
			t.Fatalf("terminal frame lacks byte range: %+v", m)
		}
		if int64(start) != expectedOffset {
			t.Fatalf("terminal start = %.0f, want %d", start, expectedOffset)
		}
		if int64(end-start) != int64(len([]byte(d))) {
			t.Fatalf("terminal range %.0f..%.0f spans %d bytes, payload has %d", start, end, int64(end-start), len([]byte(d)))
		}
		expectedOffset = int64(end)
		got.WriteString(d)
		if !sentExit && !resubscribePending && strings.Contains(got.String(), marker) {
			if err := conn.WriteJSON(map[string]any{"type": "watch_add", "id": shell.ID}); err != nil {
				t.Fatalf("replacement watch_add: %v", err)
			}
			resubscribePending = true
		}
	}
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
