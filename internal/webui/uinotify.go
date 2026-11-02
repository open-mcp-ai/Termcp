package webui

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/open-mcp-ai/termcp/internal/session"
)

// uiNotifyHub fans out UI notification payloads (pre-marshaled JSON) to every
// connected WebSocket tab. It is payload-carrying, unlike sessionListHub whose
// clients re-pull state on a bare signal.
//
// Subscribers are independent entries and the id is a counter, so the table is a
// sync.Map and the counter is atomic: no lock, nothing to serialise.
type uiNotifyHub struct {
	nextID  atomic.Uint64
	clients sync.Map // id uint64 -> chan<- []byte
}

func newUINotifyHub() *uiNotifyHub {
	return &uiNotifyHub{}
}

// register subscribes ch to the hub; the returned func unsubscribes.
func (h *uiNotifyHub) register(ch chan<- []byte) func() {
	id := h.nextID.Add(1)
	h.clients.Store(id, ch)
	return func() {
		h.clients.Delete(id)
	}
}

// broadcast enqueues a JSON payload to every subscribed client. A client whose
// send buffer is full is skipped (not counted as delivered) rather than blocked.
func (h *uiNotifyHub) broadcast(payload map[string]any) int {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	delivered := 0
	h.clients.Range(func(_, v any) bool {
		ch, ok := v.(chan<- []byte)
		if !ok {
			return true
		}
		select {
		case ch <- b:
			delivered++
		default:
		}
		return true
	})
	return delivered
}

func (h *Handler) uiNotifyHub() *uiNotifyHub {
	if h.notifyHub == nil {
		h.notifyHub = newUINotifyHub()
	}
	return h.notifyHub
}

// BroadcastShellActivity tells every open Web UI tab that a shell received input
// and where it came from.
//
// The source matters and cannot be inferred: a browser tab knows its own
// keystrokes, but a command an agent sends over MCP never touches the page. The
// tab's own sends are announced the same way so one path paints the status,
// instead of a local guess racing a server event for the same field.
func (h *Handler) BroadcastShellActivity(shellID string, src session.InputSource, submit bool) int {
	who := "api"
	if src == session.InputFromAI {
		who = "ai"
	}
	return h.uiNotifyHub().broadcast(map[string]any{
		"type":     "shell_activity",
		"shell_id": shellID,
		"src":      who,
		// Whether the line was submitted. Sent because the browser cannot derive
		// it: the echo of a submitted line looks exactly like a redraw, and
		// treating a redraw as a submit is what made the status flip away from
		// "an agent is typing" milliseconds after it appeared.
		"submit": submit,
	})
}

// BroadcastUINotify delivers a user-facing notification to every open Web UI tab
// and returns the number of tabs that received it. It is the delivery end of the
// MCP notify_user tool; safe to call with no clients connected.
func (h *Handler) BroadcastUINotify(level, title, message, sessionID string, durationSec int) int {
	if level == "" {
		level = "info"
	}
	return h.uiNotifyHub().broadcast(map[string]any{
		"type":             "ui_notify",
		"title":            title,
		"message":          message,
		"level":            level,
		"duration_seconds": durationSec,
		"session_id":       sessionID,
	})
}
