package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/session"
	"github.com/open-mcp-ai/termcp/internal/sshclient"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

// handleVersion reports the build the instance is serving — the same string
// `termcp -version` prints first. The Web UI header labels itself with it so a
// screenshot or a page always says which build it came from.
func (h *Handler) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"version": h.Version})
}

func (h *Handler) handleListSessions(w http.ResponseWriter, r *http.Request) {
	list := h.Sessions.ListAll()
	writeJSON(w, http.StatusOK, map[string]any{"sessions": list})
}

type startSessionBody struct {
	Command         string   `json:"command"`
	Args            []string `json:"args"`
	Mode            string   `json:"mode"`
	Name            string   `json:"name"`
	Rows            int      `json:"rows"`
	Cols            int      `json:"cols"`
	SSHConfig       string   `json:"ssh_config"`
	ParentSessionID string   `json:"parent_session_id"`
}

func (h *Handler) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body startSessionBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Command) == "" && len(body.Args) > 0 {
		http.Error(w, "command is required when args are provided", http.StatusBadRequest)
		return
	}

	cfgName, ent, remote, err := h.resolveSSH(body.SSHConfig)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	cmd, args := sshconfig.EffectiveCommand(ent, body.Command, body.Args)
	if strings.TrimSpace(cmd) == "" && len(args) > 0 {
		http.Error(w, "command is required when args are provided", http.StatusBadRequest)
		return
	}

	mode := sshconfig.EffectiveMode(ent, body.Mode)
	if mode != "pty" && mode != "pipe" {
		http.Error(w, "mode must be pty or pipe", http.StatusBadRequest)
		return
	}
	sessName := strings.TrimSpace(body.Name)
	if sessName == "" {
		sessName = cfgName
	}
	rows := body.Rows
	if rows < 1 {
		rows = 24
	}
	if rows > 1000 {
		http.Error(w, "rows out of range", http.StatusBadRequest)
		return
	}
	cols := body.Cols
	if cols < 1 {
		cols = 80
	}
	if cols > 1000 {
		http.Error(w, "cols out of range", http.StatusBadRequest)
		return
	}

	sess, err := h.Sessions.Create(session.Config{
		Command:      cmd,
		Args:         args,
		Mode:         api.SessionMode(mode),
		Name:         sessName,
		Rows:         rows,
		Cols:         cols,
		Remote:       remote,
		DefaultShell: sshconfig.EffectiveDefaultShell(ent),
		Approval:     sshconfig.EffectiveApproval(ent),
	})
	if err != nil {
		http.Error(w, sshclient.DescribeDialError(err), http.StatusBadRequest)
		return
	}
	time.Sleep(100 * time.Millisecond)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sess.ID,
		"shell_id":   sess.PrimaryShellID(),
		"pid":        sess.PID,
		"ssh_config": cfgName,
	})
}

// handleGetSession returns a single session's full info.
// GET /api/sessions/{id}
func (h *Handler) handleGetSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := h.Sessions.Get(id)
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, sess.Info())
}

// batchSessionResults applies op to every id in a comma-separated batch (the
// session lifecycle routes accept one). Entries run independently: a missing id
// or a failed op is reported as {"id":...,"ok":false,"code":...,"error":...}
// and never stops the remaining entries; successes are {"id":...,"ok":true}.
// The codes mirror the MCP error codes (session_not_found / operation_failed)
// so clients can branch without parsing error text — the Web UI treats
// session_not_found as already cleared.
func (h *Handler) batchSessionResults(rawIDs string, op func(id string) error) []map[string]any {
	results := make([]map[string]any, 0)
	for _, id := range strings.Split(rawIDs, ",") {
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		if h.Sessions.Get(id) == nil {
			results = append(results, map[string]any{"id": id, "ok": false, "code": "session_not_found", "error": "session '" + id + "' not found"})
			continue
		}
		if err := op(id); err != nil {
			results = append(results, map[string]any{"id": id, "ok": false, "code": "operation_failed", "error": err.Error()})
			continue
		}
		results = append(results, map[string]any{"id": id, "ok": true})
	}
	return results
}

// handleDeleteSession closes a session in place (terminate + DEAD), keeping it
// in the registry so the Web UI shows it as a read-only tile and its output
// stays readable. POST /api/sessions/{id}/terminate and /disconnect both route
// here. Use DELETE /api/sessions/{id} to erase it for good.
//
// {id} may be a comma-separated batch (a,b,c): every entry is closed
// independently, the response is 200 with a per-id results array instead of
// 204, and one bad entry does not stop the rest. A single id is unchanged.
func (h *Handler) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.Contains(id, ",") {
		results := h.batchSessionResults(id, func(sid string) error {
			h.Sessions.Terminate(sid, true, 0)
			return nil
		})
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
		return
	}
	if h.Sessions.Get(id) == nil {
		http.NotFound(w, r)
		return
	}
	// Close only: disconnect ≠ delete. The DEAD entry keeps its buffers and
	// byte log, so the tile stays visible and clickable for replay.
	h.Sessions.Terminate(id, true, 0)
	w.WriteHeader(http.StatusNoContent)
}

// handlePurgeSession terminates a live session and permanently removes its
// on-disk directory (DELETE /api/sessions/{id}).
//
// {id} may be a comma-separated batch (a,b,c): 200 with per-id results; a
// failing entry (locked log file, session already gone) does not stop the
// others. A single id is unchanged (204 or 404).
func (h *Handler) handlePurgeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if strings.Contains(id, ",") {
		results := h.batchSessionResults(id, h.Sessions.Delete)
		writeJSON(w, http.StatusOK, map[string]any{"results": results})
		return
	}
	if h.Sessions.Get(id) != nil {
		if err := h.Sessions.Delete(id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.NotFound(w, r)
}

type sessionRenameBody struct {
	Name *string `json:"name"`
}

// handleRenameSession renames a live or closed (DEAD) session (PATCH /api/sessions/{id}).
func (h *Handler) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body sessionRenameBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON body"})
		return
	}
	if body.Name == nil || strings.TrimSpace(*body.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	name := strings.TrimSpace(*body.Name)

	if sess := h.Sessions.Get(id); sess != nil {
		if err := h.Sessions.Rename(id, name); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, h.Sessions.Get(id).Info())
		return
	}
	http.NotFound(w, r)
}
