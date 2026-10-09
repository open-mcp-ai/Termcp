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
}

var errWSSessionNotFound = errors.New("session not found")

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
	// 不要对 ReadMessage 设短超时：终端输出是服务端 Write、客户端收，不会作为本循环的 Read 数据；
	// 用户长时间只看输出不按键，短 deadline 会误杀连接（SSE 时代无此问题）。
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
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
	// Review mode does NOT gate this path: this is the human's keyboard.
	//
	// The gate exists to review what the AI sends, and the AI's surface is MCP —
	// the same distinction the InputSource constants already draw (InputFromAPI is
	// "a human typing through the HTTP/WebSocket API", InputFromAI is "an AI agent
	// driving the shell through MCP"). Dropping the stream here locked the operator
	// out of their own terminal the moment they turned review on, which is not
	// what "review the AI" means: a person watching a command run must still be
	// able to interrupt it.
	//
	// A token holder can still bypass the gate by writing to the API directly,
	// but they can approve their own request just as directly, so gating this path
	// would cost the operator real usability and buy no security.
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
	if err := shell.ResizePty(msg.Rows, msg.Cols); err != nil {
		slog.Debug("ws resize", "err", err)
	}
}
