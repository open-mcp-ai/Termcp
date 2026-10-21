package webui

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

const wsSendBuf = 1024

// terminalOutputChunkBytes caps raw PTY bytes per stream read so full-scrollback replay stays under WS/SSE message limits.
const terminalOutputChunkBytes = 256 * 1024

// terminalFlushBytes bounds one terminal frame: PTY output is accumulated into a
// single frame until this many bytes, or until the reader has caught up with the
// producer (see runWatch — that second condition is what keeps batching free of
// added latency).
//
// A busy command writes a few KiB at a time, so one frame per read meant
// thousands of tiny frames per second (measured: 11.7k frames/s at 55 bytes
// average) and the browser paid its per-message cost — a JSON parse, a DOM scan
// for the channel, a TextEncoder allocation and an xterm write — for each one.
// That is what made the page stutter while output streamed; every client-side
// fix only reduced a constant while the frame rate stayed the same.
//
// The bound is deliberately far above an interactive echo (a keystroke is a few
// bytes, so it flushes on the caught-up condition instead) and below the WS
// message limits the read side already enforces.
const terminalFlushBytes = 32 * 1024

// terminalFlushMinGap bounds how long a batch may be held once the reader has
// caught up with the producer. It is what coalesces a producer that never leaves
// a backlog — a shell printing a loop line by line, a prompt redrawing itself —
// where the caught-up test alone sees nothing to merge and would ship one frame
// per write (measured on such a shell: 100k frames/s at 13 bytes each).
//
// Two milliseconds is chosen to be invisible (the fastest screen repaints every
// 16 ms, so a batch shipped within 4 ms of its first byte cannot be seen as late)
// while still merging a burst of writes into one frame. It is a ceiling on hold
// time, not a cadence: bytes ship the moment the reader is caught up AND this
// gap has elapsed, so a quiet echo leaves after its own write with no waiting
// beyond this bound.
const terminalFlushMinGap = 4 * time.Millisecond

// terminalOutputIdleWait is how long an output pump with no backlog sleeps before
// re-checking its buffer. It is the read timeout this pump has always used: a
// quiet channel costs four wakes a second either way.
const terminalOutputIdleWait = 250 * time.Millisecond

var uiUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type wsClientMsg struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	D    string `json:"d"`
	NL   bool   `json:"nl"`
	Rows int    `json:"rows"`
	Cols int    `json:"cols"`
}

// wsWatchEntry tracks one terminal output pump per session id for this WS tab.
type wsWatchEntry struct {
	cancel context.CancelFunc
	rid    int
}

// uiWS is one browser tab: session list + terminal streams + input/resize (no per-keystroke HTTP).
type uiWS struct {
	h      *Handler
	conn   *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	send   chan []byte
	mu     sync.Mutex
	watch  map[string]*wsWatchEntry
	// viewerID is this tab's own row in the viewer registry. Each frame marks it
	// so the page can count the clients besides itself — every open page is a
	// viewer, so without the mark a lone tab always counted as one client.
	viewerID uint64
}

var errWSSessionNotFound = errors.New("session not found")

// watchCount reports how many sessions this tab is currently streaming.
func (c *uiWS) watchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.watch)
}

// getTerminalShell looks up a shell channel by shell_id.
//
// The concrete *ChildShell is returned rather than the TerminalShell interface:
// every write path needs ChildShell's full method set, and nothing in the code
// base ever treats a *Session as a terminal. The interface only made the two
// look interchangeable while one of them was unreachable.
func (c *uiWS) getTerminalShell(sid string) *session.ChildShell {
	if cs := c.h.Sessions.GetChildShell(sid); cs != nil {
		return cs
	}
	return nil
}

func (h *Handler) handleWebUIWS(w http.ResponseWriter, r *http.Request) {
	if h.Sessions == nil {
		http.Error(w, "sessions unavailable", http.StatusServiceUnavailable)
		return
	}
	wsConn, err := uiUpgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Debug("webui ws upgrade", "err", err)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	c := &uiWS{
		h:      h,
		conn:   wsConn,
		ctx:    ctx,
		cancel: cancel,
		send:   make(chan []byte, wsSendBuf),
		watch:  make(map[string]*wsWatchEntry),
	}
	// Who is watching: the tab is registered for the life of its socket, and its
	// watched-session count is read at list time rather than captured here.
	viewerID := c.h.viewerHub().add(r.RemoteAddr, r.UserAgent(), c.watchCount)
	c.viewerID = viewerID
	defer c.h.viewerHub().remove(viewerID)
	// The panel is pushed, not polled: an arrival or departure is a change to a
	// list the page is already showing, exactly like the session list.
	c.h.sessionHub().broadcast()
	defer c.h.sessionHub().broadcast()

	go c.writePump()
	go c.sessionLoop()
	// Subscribe to UI notification broadcasts (notify_user tool); deliveries land
	// on the same send channel writePump drains.
	unregNotify := c.h.uiNotifyHub().register(c.send)
	defer unregNotify()
	c.readPump()
}

func (c *uiWS) sessionLoop() {
	_, sig, unreg := c.h.sessionHub().register()
	defer unreg()
	c.enqueueSessions()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-sig:
			c.enqueueSessions()
		}
	}
}

func (c *uiWS) enqueueSessions() {
	list := c.h.Sessions.ListAll()
	payload := map[string]any{"type": "sessions", "sessions": list}
	if c.h.SSH != nil {
		names, err := c.h.SSH.List()
		if err == nil {
			var conns []connectionSummary
			for _, n := range names {
				ent, err := c.h.SSH.Load(n)
				if err != nil {
					continue
				}
				if c.h.NoInternal && ent.Kind == "internal" {
					continue
				}
				conns = append(conns, summarizeConnection(n, ent))
			}
			payload["connections"] = conns
		}
	}
	/* Who is attached rides the same frame as the session list. It changes when a
	   tab opens or closes, or when an MCP client connects or drops — all of which
	   already wake this loop through the session hub, so there is no second timer
	   and no polling. */
	payload["clients"] = c.h.viewerHub().list(c.viewerID)
	if c.h.MCPConnections != nil {
		payload["mcp_clients"] = c.h.MCPConnections()
	}
	if c.h.Extensions != nil {
		// The right panel renders from the same frame, so a status transition is
		// one round trip: the signal arrives, the tab re-reads this payload.
		if rows := summarizeExtensions(c.h.Extensions); rows != nil {
			payload["extensions"] = rows
		}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	select {
	case c.send <- b:
	case <-c.ctx.Done():
	}
}

func (c *uiWS) writePump() {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case msg := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if err := c.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *uiWS) readPump() {
	defer func() {
		c.cancel()
		c.removeAllWatches()
		_ = c.conn.Close()
	}()

	c.conn.SetReadLimit(1 << 20)
	// The read deadline and the pong handler are a PAIR, and the deadline is
	// meaningless without the handler. writePump pings every 45s, and the standard
	// library's PongHandler is what refreshes the deadline — so with a deadline and
	// no handler, a quiet but perfectly healthy tab would be killed; with a handler
	// and no deadline (what this was), a tab that vanished without a FIN is never
	// noticed at all.
	//
	// That second case is the bug behind "why does it say two web clients when I
	// have one page open?": a browser that is closed, crashes, sleeps or changes
	// networks leaves a socket the kernel still believes in. ReadMessage blocks
	// forever, readPump never returns, `defer remove(viewerID)` never runs, and the
	// registry keeps a row for a tab that no longer exists. One reconnection leaves
	// one ghost, and the count climbs with every crash.
	//
	// 90s is two ping intervals: one lost pong is a hiccup, two in a row is a peer
	// that is not answering. Terminal output is server→client only and never arrives
	// through this loop, so nothing a user does — including watching output for an
	// hour without touching the keyboard — refreshes the deadline. Only a pong or a
	// real client message does, which is exactly the liveness question being asked.
	const wsReadIdle = 90 * time.Second
	_ = c.conn.SetReadDeadline(time.Now().Add(wsReadIdle))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(wsReadIdle))
	})
	// Any inbound frame is proof of life too: the page sends input, resizes and
	// watch changes, and a client that is talking is not one to time out.
	c.conn.SetPingHandler(func(data string) error {
		_ = c.conn.SetReadDeadline(time.Now().Add(wsReadIdle))
		return c.conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(wsReadIdle))
		var msg wsClientMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch strings.TrimSpace(msg.Type) {
		case "watch_add":
			if id := strings.TrimSpace(msg.ID); id != "" {
				_ = c.addWatch(id)
			}
		case "watch_remove":
			if id := strings.TrimSpace(msg.ID); id != "" {
				c.removeWatch(id)
			}
		case "input":
			c.handleWSInput(&msg)
		case "resize":
			c.handleWSResize(&msg)
		}
	}
}

func (c *uiWS) handleWSInput(msg *wsClientMsg) {
	sid := strings.TrimSpace(msg.ID)
	if sid == "" {
		return
	}
	shell := c.getTerminalShell(sid)
	if shell == nil {
		return
	}
	if shell.Info().Status != api.SessionRunning {
		return
	}
	// The capability whitelist gates this path; review mode does not.
	//
	// Review mode is about reviewing what the AI sends, and the AI's surface is
	// MCP — dropping the stream here locked the operator out of their own terminal
	// the moment they turned review on. A template's whitelist is a different
	// statement: "this session has no terminal", said by whoever pinned it. That
	// one a token holder cannot approve their way past, so the human's own
	// keyboard is no exception to it — and this socket is reachable by any client
	// holding the token, not only by the browser. The REST mirror of this call
	// (/api/shells/{id}/input) gates the same way; leaving one open would make the
	// whitelist a matter of which transport you picked.
	//
	// A refusal is silent here because a stream has no response to carry an error:
	// the frame is dropped and the reason is logged. The REST endpoint is where a
	// caller gets told why.
	if c.refuseWSCapability(shell, capability.ShellInput) {
		return
	}
	// msg.D is the JSON string xterm produced, not base64: the frame is standard
	// JSON, so encoding/json already recovered the text and no decode step is
	// needed. See the "传输编码" section of docs/design/session-storage.md.
	if err := shell.SendTerminalBytes([]byte(msg.D), msg.NL); err != nil {
		slog.Debug("ws input", "err", err)
	}
}

func (c *uiWS) handleWSResize(msg *wsClientMsg) {
	sid := strings.TrimSpace(msg.ID)
	if sid == "" || msg.Rows < 1 || msg.Cols < 1 {
		return
	}
	shell := c.getTerminalShell(sid)
	if shell == nil {
		return
	}
	if shell.Info().Status != api.SessionRunning {
		return
	}
	if c.refuseWSCapability(shell, capability.ShellResize) {
		return
	}
	if err := shell.ResizePty(msg.Rows, msg.Cols); err != nil {
		slog.Debug("ws resize", "err", err)
	}
}

// refuseWSCapability reports whether the session owning shell does not carry tool
// in its whitelist, and logs the drop. It resolves the parent the way the shell
// itself does — a child shell's restriction is the container's — so the check
// cannot disagree with what the REST and MCP surfaces answer at the same moment.
func (c *uiWS) refuseWSCapability(shell *session.ChildShell, tool string) bool {
	sess := c.h.Sessions.Get(shell.ParentSessionID())
	if sess == nil || sess.Restriction().AllowsTool(tool) {
		return false
	}
	slog.Debug("ws frame dropped by session restriction", "shell_id", shell.ID, "tool", tool)
	return true
}
