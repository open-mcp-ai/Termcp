package webui

import (
	"context"
	"encoding/json"
	"io"
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

func (c *uiWS) sendTerminalPayload(payload []byte) {
	select {
	case c.send <- payload:
	case <-c.ctx.Done():
	default:
	}
}

func (c *uiWS) sendTerminalDonePayload(payload []byte) {
	select {
	case c.send <- payload:
	case <-c.ctx.Done():
	default:
	}
}

func (c *uiWS) endWatch(sid string, shell *session.ChildShell, rid int) {
	shell.UnregisterReader(rid)
	c.mu.Lock()
	if ent, ok := c.watch[sid]; ok && ent != nil && ent.rid == rid {
		delete(c.watch, sid)
	}
	c.mu.Unlock()
}

func (c *uiWS) runWatch(ctx context.Context, shell *session.ChildShell, sid string, rid int) {
	defer c.endWatch(sid, shell, rid)
	for {
		if ctx.Err() != nil {
			return
		}
		out, err := shell.ReadTerminalStream(ctx, rid, 250*time.Millisecond, false, 0, terminalOutputChunkBytes)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			// EOF: buffer closed (shell exited but session kept alive for multiplexing).
			if err == io.EOF {
				payload, _ := json.Marshal(map[string]string{"type": "terminal_done", "id": sid})
				c.sendTerminalDonePayload(payload)
				return
			}
			continue
		}
		if out != "" {
			// Terminal bytes travel as a plain JSON string; encoding/json escapes
			// whatever it must. base64 would only add a layer the client undoes.
			payload, err := json.Marshal(map[string]string{"type": "terminal", "id": sid, "d": out})
			if err != nil {
				continue
			}
			c.sendTerminalPayload(payload)
		}
		info := shell.Info()
		if (info.Status != api.SessionRunning || shell.IsBufferClosed()) && !shell.HasMoreOutput(rid) {
			payload, err := json.Marshal(map[string]string{"type": "terminal_done", "id": sid})
			if err == nil {
				c.sendTerminalDonePayload(payload)
			}
			return
		}
	}
}

func (c *uiWS) addWatch(sid string) error {
	shell := c.getTerminalShell(sid)
	if shell == nil {
		return errWSSessionNotFound
	}
	rid, err := shell.RegisterReader()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	if old, ok := c.watch[sid]; ok && old != nil && old.cancel != nil {
		old.cancel()
	}
	c.watch[sid] = &wsWatchEntry{cancel: cancel, rid: rid}
	c.mu.Unlock()
	go c.runWatch(ctx, shell, sid, rid)
	return nil
}

func (c *uiWS) removeWatch(sid string) {
	c.mu.Lock()
	ent := c.watch[sid]
	delete(c.watch, sid)
	c.mu.Unlock()
	if ent != nil && ent.cancel != nil {
		ent.cancel()
	}
}

func (c *uiWS) removeAllWatches() {
	c.mu.Lock()
	var list []*wsWatchEntry
	for _, ent := range c.watch {
		list = append(list, ent)
	}
	c.watch = make(map[string]*wsWatchEntry)
	c.mu.Unlock()
	for _, ent := range list {
		if ent != nil && ent.cancel != nil {
			ent.cancel()
		}
	}
}
