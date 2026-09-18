package webui

import (
	"encoding/json"
	"net/http"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// REST terminal I/O for CLI/script use: send input, press a named key, and
// resize the PTY of a running shell. These are the script-friendly mirrors of
// the WebSocket messages (and of the MCP shell_input / shell_key / shell_resize
// tools); the WS channel remains the way to do real-time bidirectional I/O.

type shellInputRequest struct {
	Text       string `json:"text"`
	PressEnter bool   `json:"press_enter"`
}

type shellKeyRequest struct {
	Key    string `json:"key"`
	Repeat int    `json:"repeat"`
}

type shellResizeRequest struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// handleShellInput writes raw text (optionally followed by enter) to a shell.
// POST /api/shells/{id}/input
func (h *Handler) handleShellInput(w http.ResponseWriter, r *http.Request) {
	var req shellInputRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	cs := h.getShellForIO(w, r)
	if cs == nil {
		return
	}
	if err := cs.SendTerminalBytes([]byte(req.Text), req.PressEnter); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleShellKey presses a named key sequence one or more times.
// POST /api/shells/{id}/key
func (h *Handler) handleShellKey(w http.ResponseWriter, r *http.Request) {
	var req shellKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Key == "" {
		http.Error(w, "key is required", http.StatusBadRequest)
		return
	}
	if req.Repeat < 1 {
		req.Repeat = 1
	}
	cs := h.getShellForIO(w, r)
	if cs == nil {
		return
	}
	if err := cs.PressKey(req.Key, req.Repeat); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleShellResize adjusts a shell's PTY window size.
// POST /api/shells/{id}/resize
func (h *Handler) handleShellResize(w http.ResponseWriter, r *http.Request) {
	var req shellResizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Rows < 1 || req.Cols < 1 {
		http.Error(w, "rows and cols must be positive", http.StatusBadRequest)
		return
	}
	cs := h.getShellForIO(w, r)
	if cs == nil {
		return
	}
	if err := cs.ResizePty(req.Rows, req.Cols); err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": req.Rows, "cols": req.Cols})
}

// getShellForIO resolves the shell named by the {id} path value and writes the
// REST error response when it is missing or not running. Returns nil after it
// has already written an error response.
func (h *Handler) getShellForIO(w http.ResponseWriter, r *http.Request) *session.ChildShell {
	id := r.PathValue("id")
	cs := h.Sessions.GetChildShell(id)
	if cs == nil {
		http.Error(w, "shell not found: "+id, http.StatusNotFound)
		return nil
	}
	if cs.Info().Status != api.SessionRunning {
		http.Error(w, "shell is not running (status "+string(cs.Info().Status)+")", http.StatusConflict)
		return nil
	}
	return cs
}
