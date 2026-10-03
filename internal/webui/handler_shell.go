package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// handleCreateShell creates a new shell channel on an existing session.
// POST /api/sessions/{id}/shells
func (h *Handler) handleCreateShell(w http.ResponseWriter, r *http.Request) {
	parentID := r.PathValue("id")
	parent := h.Sessions.Get(parentID)
	if parent == nil {
		http.Error(w, "parent session not found", http.StatusNotFound)
		return
	}

	var body startSessionBody
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
	}

	rows := body.Rows
	if rows < 1 {
		rows = 24
	}
	cols := body.Cols
	if cols < 1 {
		cols = 80
	}
	mode := body.Mode
	if mode == "" {
		mode = "pty"
	}
	if mode != "pty" && mode != "pipe" {
		http.Error(w, "mode must be pty or pipe", http.StatusBadRequest)
		return
	}

	cs, err := parent.CreateChildShell(body.Command, body.Args, mode == "pty", rows, cols, strings.TrimSpace(body.Name))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"shell_id":   cs.ID,
		"session_id": parentID,
		"name":       cs.Name,
		// The channel index is what termcp://#<session>:N addresses and what the
		// tab label shows. Returning it lets the UI label a new tab from the
		// server's numbering instead of guessing a number of its own.
		"index": cs.Index,
	})
}

// redirectShells handles GET /api/sessions/{id}/child-shells -> /api/sessions/{id}/shells.
func (h *Handler) redirectShells(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	http.Redirect(w, r, "/api/sessions/"+id+"/shells", http.StatusMovedPermanently)
}

// resolveOutputShell resolves a shell for output-range endpoints.
// Prefer shell_id; fall back to session primary shell when a session_id is passed.
func (h *Handler) resolveOutputShell(id string) *session.ChildShell {
	if cs := h.Sessions.GetChildShell(id); cs != nil {
		return cs
	}
	if sess := h.Sessions.Get(id); sess != nil {
		if cs := sess.PrimaryShell(); cs != nil {
			return cs
		}
	}
	return nil
}

// handleShellOutputRange is the canonical shell history endpoint: GET /api/shells/{id}/output-range.
func (h *Handler) handleShellOutputRange(w http.ResponseWriter, r *http.Request) {
	h.writeOutputRange(w, r, r.PathValue("id"))
}

// handleSessionOutputRange remains for compatibility. Path id may be shell_id
// (what the Web UI currently sends) or session_id (primary-shell fallback).
func (h *Handler) handleSessionOutputRange(w http.ResponseWriter, r *http.Request) {
	h.writeOutputRange(w, r, r.PathValue("id"))
}

func (h *Handler) writeOutputRange(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	const hardMax = 512 * 1024
	max, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("max")))
	if err != nil || max <= 0 {
		max = 256 * 1024
	}
	if max > hardMax {
		max = hardMax
	}

	tail := strings.TrimSpace(r.URL.Query().Get("tail")) == "1"
	var start int64
	if !tail {
		start, err = strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("start")), 10, 64)
		if err != nil || start < 0 {
			http.Error(w, "invalid start", http.StatusBadRequest)
			return
		}
	}

	var data []byte
	var total int64

	// One read path for every session state. A live shell reads its buffer; a DEAD
	// or restart-restored session reads the persisted byte log. Both answer in the
	// same offset space, so a client can hold an offset across a restart.
	//
	// The two differ only in where the bytes come from, so they are resolved into
	// one (len, read) pair instead of duplicating the tail/read/error handling.
	var sizeFn func() (int64, error)
	var readFn func(start int64, max int) ([]byte, int64, error)
	var baseOffset int64

	if shell := h.resolveOutputShell(id); shell != nil {
		sizeFn = func() (int64, error) { return shell.BufferLen(), nil }
		readFn = shell.OutputByteRange
		baseOffset = shell.OutputBaseOffset()
	} else {
		// DEAD/restored sessions have no live buffer; serve the read-only view from
		// the persisted log so tabs still work after a teardown or a restart.
		sid, shid, ok := h.persistedOutputFor(id)
		if !ok {
			http.NotFound(w, r)
			return
		}
		sizeFn = func() (int64, error) { return h.Sessions.OutputSize(sid, shid) }
		readFn = func(s int64, m int) ([]byte, int64, error) {
			return h.Sessions.OutputByteRange(sid, shid, s, m)
		}
	}

	if tail {
		total, err = sizeFn()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if t := total - int64(max); t > 0 {
			start = t
		} else {
			start = 0
		}
	}

	data, total, err = readFn(start, max)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// An absolute start below the earliest retained byte (dropped by buffer
	// compaction) clamps forward; report the start actually served.
	if start < baseOffset {
		start = baseOffset
	}
	if start > total {
		start = total
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"start": start,
		"end":   start + int64(len(data)),
		"total": total,
		"d":     string(data),
	})
}

// persistedOutputFor resolves a DEAD/restored session (and optional shell) for a
// read-only output range. id may be a session_id (whole merged stream) or a
// shell_id (single-shell stream). ok is false for known-live sessions (which use
// their in-memory buffer) and for unknown ids.
func (h *Handler) persistedOutputFor(id string) (sessionID, shellID string, ok bool) {
	if sess := h.Sessions.Get(id); sess != nil {
		if sess.Info().Status != api.SessionRunning {
			return sess.ID, "", true
		}
		return "", "", false
	}
	if sess := h.Sessions.GetSessionByShellID(id); sess != nil {
		if sess.Info().Status != api.SessionRunning {
			return sess.ID, id, true
		}
	}
	return "", "", false
}

func (h *Handler) handleListShells(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := h.Sessions.Get(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	// Live sessions list in-memory channel children; DEAD/restored sessions fall
	// back to the persisted shell snapshot so their tabs survive a restart.
	shells := sess.ShellsForView()
	if shells == nil {
		shells = []api.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"shells": shells})
}

// handleCloseShell closes one shell channel by shell_id (DELETE /api/shells/{id}).
// Missing shell is treated as already closed (204) so UI fire-and-forget is quiet.
func (h *Handler) handleCloseShell(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	// Internal primary shell: tab close is a no-op (process outlives the tab).
	if sess := h.Sessions.GetByShellID(id); sess != nil && sess.IsInternalPrimaryShell(id) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if cs := h.Sessions.GetChildShell(id); cs != nil {
		_, _ = h.Sessions.CloseChildShell(id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Already exited/removed from the session map.
	w.WriteHeader(http.StatusNoContent)
}

// handleClosePrimaryShell is the compatibility POST /api/sessions/{id}/close-shell path.
// Path id is a session_id: close that session's primary shell only.
func (h *Handler) handleClosePrimaryShell(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		http.NotFound(w, r)
		return
	}
	sess := h.Sessions.Get(sessionID)
	if sess == nil {
		// Idempotent if session already gone.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	shellID := sess.PrimaryShellID()
	if shellID == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if sess.SSHEndpoint == "internal" {
		// Same policy as DELETE /api/shells/{primary}: internal primary outlives the tab.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if h.Sessions.GetChildShell(shellID) != nil {
		_, _ = h.Sessions.CloseChildShell(shellID)
	}
	w.WriteHeader(http.StatusNoContent)
}
