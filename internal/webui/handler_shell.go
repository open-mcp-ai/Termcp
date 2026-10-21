package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/transcript"
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
	// Opening a channel is a capability like any other: an extension that keeps a
	// pipe shell for one purpose must not be reachable by a second channel nobody
	// whitelisted (a plain login shell inside the same container would carry the
	// container's whole environment).
	if restRefuseTool(w, h.Sessions.Get(parentID), capability.ShellOpen) {
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

	// One read path for every session state, shared with the MCP tool through
	// the transcript package: resolve the id onto the unified byte stream, then
	// read the window. start/end/total always describe the raw window; a
	// stream= filter only shrinks the returned content.
	src, err := transcript.Resolve(h.Sessions, h.MsgMgr, id)
	if err != nil {
		writeTranscriptError(w, err)
		return
	}
	// The whitelist is checked on the resolved container rather than on the argument,
	// so every spelling transcript accepts (session id, shell id, locator) lands on
	// the same decision — the same reconciliation the MCP guard makes. This route is
	// the REST twin of shell_output, and a read is as gated as a write: the
	// whitelist is about reachability, not direction.
	if restRefuseTool(w, h.Sessions.Get(src.SessID), capability.ShellOutput) {
		return
	}

	stream := strings.TrimSpace(r.URL.Query().Get("stream"))
	if stream != "" && stream != "all" && stream != "o" && stream != "e" {
		http.Error(w, `stream must be "all", "o" or "e"`, http.StatusBadRequest)
		return
	}

	total, terr := src.Len()
	if terr != nil {
		http.Error(w, terr.Error(), http.StatusInternalServerError)
		return
	}
	if tail {
		if t := total - int64(max); t > 0 {
			start = t
		} else {
			start = 0
		}
	}
	// An absolute start below the earliest retained byte (dropped by buffer
	// compaction) clamps forward; report the start actually served.
	if start < src.BaseOffset() {
		start = src.BaseOffset()
	}
	if start > total {
		start = total
	}

	win, rerr := transcript.Read(r.Context(), src, transcript.Params{
		Offset:    &start,
		MaxBytes:  max,
		Stream:    stream,
		StripAnsi: false, // the terminal repaints raw bytes
	})
	if rerr != nil {
		writeTranscriptError(w, rerr)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"start": win.Start,
		"end":   win.End,
		"total": win.Total,
		"d":     win.Output,
	})
}

// writeTranscriptError maps the shared read path's typed errors onto HTTP
// statuses, the same kinds the MCP tool maps onto its error codes.
func writeTranscriptError(w http.ResponseWriter, err error) {
	var te *transcript.Error
	if errors.As(err, &te) {
		switch te.Kind {
		case transcript.ErrShellNotFound, transcript.ErrSessionNotFound:
			http.Error(w, te.Msg, http.StatusNotFound)
			return
		case transcript.ErrInvalid, transcript.ErrReader:
			http.Error(w, te.Msg, http.StatusBadRequest)
			return
		}
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
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
	// Closing a channel is a capability like any other, and this is the REST twin of
	// the gated shell_close tool: an extension that keeps a channel for one purpose
	// must not have it torn down by a caller it never granted shell_close to. The
	// owner is resolved first because the route names the shell, not the session,
	// and both spellings are passed so a caller cannot walk around the refusal by
	// naming the session instead. What gets closed below is unchanged: a session id
	// still finds no shell to close, as before.
	if sess := h.sessionOwning(id, id); restRefuseTool(w, sess, capability.ShellClose) {
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

// handleArchiveShell closes one shell channel and KEEPS its record
// (POST /api/shells/{id}/archive).
//
// This is what the terminal tab's X calls. DELETE /api/shells/{id} stays the erase:
// it stops the channel and drops the record, so nothing addresses the transcript
// afterwards. Both exist because both are wanted, and the UI's ordinary close needs
// the first, not the second.
//
// A missing shell answers 204 like its DELETE twin: the caller's intent — this
// channel is not running — is already true, and an error would only teach the UI to
// ignore errors.
func (h *Handler) handleArchiveShell(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	// Same gate as close: stopping a channel is one capability whichever verb carries
	// it, and an extension that never granted shell_close must not have its channel
	// stopped by a spelling it did not expect.
	if sess := h.sessionOwning(id, id); restRefuseTool(w, sess, capability.ShellClose) {
		return
	}
	// The internal primary shell outlives its tab; closing it is a no-op, and
	// archiving it would claim a record for a shell that is still running.
	if sess := h.Sessions.GetByShellID(id); sess != nil && sess.IsInternalPrimaryShell(id) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if cs := h.Sessions.GetChildShell(id); cs != nil {
		_, _ = h.Sessions.ArchiveChildShell(id)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Already gone: nothing to stop, nothing to record.
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
	// The gate runs before the not-found return, and resolves either spelling: this
	// route names a session, but the twin below it names a shell, and a refusal that
	// only held for one of them would be a matter of how the caller spelled the id.
	// The close logic stays keyed on the session id, so a shell id is still the
	// no-op it was.
	if restRefuseTool(w, h.sessionOwning(sessionID, sessionID), capability.ShellClose) {
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
