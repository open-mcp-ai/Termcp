package webui

import (
	"encoding/json"
	"sync"
)

// uiNotifyHub fans out UI notification payloads (pre-marshaled JSON) to every
// connected WebSocket tab. It is payload-carrying, unlike sessionListHub whose
// clients re-pull state on a bare signal.
type uiNotifyHub struct {
	mu      sync.Mutex
	nextID  uint64
	clients map[uint64]chan<- []byte
}

func newUINotifyHub() *uiNotifyHub {
	return &uiNotifyHub{clients: make(map[uint64]chan<- []byte)}
}

// register subscribes ch to the hub; the returned func unsubscribes.
func (h *uiNotifyHub) register(ch chan<- []byte) func() {
	h.mu.Lock()
	h.nextID++
	id := h.nextID
	h.clients[id] = ch
	h.mu.Unlock()
	return func() {
		h.mu.Lock()
		delete(h.clients, id)
		h.mu.Unlock()
	}
}

// broadcast enqueues a JSON payload to every subscribed client. A client whose
// send buffer is full is skipped (not counted as delivered) rather than blocked.
func (h *uiNotifyHub) broadcast(payload map[string]any) int {
	b, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delivered := 0
	for _, ch := range h.clients {
		select {
		case ch <- b:
			delivered++
		default:
		}
	}
	return delivered
}

func (h *Handler) uiNotifyHub() *uiNotifyHub {
	if h.notifyHub == nil {
		h.notifyHub = newUINotifyHub()
	}
	return h.notifyHub
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
