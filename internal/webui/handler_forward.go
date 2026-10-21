package webui

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/capability"
	"github.com/open-mcp-ai/termcp/internal/forward"
	"github.com/open-mcp-ai/termcp/internal/notify"
	"github.com/open-mcp-ai/termcp/pkg/api"
)

func (h *Handler) handleListForwards(w http.ResponseWriter, r *http.Request) {
	if h.ForwardMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"forwards": []any{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"forwards": h.ForwardMgr.List()})
}

// handleListNotifications returns active shell notification rules, optionally
// filtered by shell_id and/or session_id query parameters.
func (h *Handler) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	if h.NotifyMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"notifications": []any{}})
		return
	}
	q := r.URL.Query()
	rules := h.NotifyMgr.List(q.Get("shell_id"))
	if sessionID := q.Get("session_id"); sessionID != "" {
		filtered := make([]notify.RuleView, 0, len(rules))
		for _, rule := range rules {
			if rule.SessionID == sessionID {
				filtered = append(filtered, rule)
			}
		}
		rules = filtered
	}
	if rules == nil {
		rules = []notify.RuleView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"notifications": rules})
}

// handleDeleteNotification unregisters one notification rule by id.
func (h *Handler) handleDeleteNotification(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "notification rule id required"})
		return
	}
	if h.NotifyMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "notification manager not available"})
		return
	}
	// The route names the rule, not the session, so the owner is looked up the way
	// the forward-close route does it: a rule is a capability of the session that
	// registered it, and an extension granted no shell_notify must not have one
	// removed — or, more to the point, a caller with a restricted handle must not
	// be able to reach a rule belonging to a session it may not touch.
	for _, rule := range h.NotifyMgr.List("") {
		if rule.ID != id {
			continue
		}
		if restRefuseTool(w, h.sessionOwning(rule.SessionID, rule.ShellID), capability.ShellNotify) {
			return
		}
		break
	}
	if !h.NotifyMgr.Unregister(id) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "notification rule not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "rule_id": id})
}

func (h *Handler) handleDeleteForward(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	slog.Info("handleDeleteForward called", "forward_id", id)
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "forward id required"})
		return
	}
	if h.ForwardMgr == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "forward manager not available"})
		return
	}
	// Closing is a capability too: a tunnel a template declared is torn down by the
	// reconciler when the extension is parked or removed, so letting REST close one
	// out from under a running extension would be a way to break a session that
	// granted no forward capability at all. The forward's owner is looked up here
	// because the route names the forward, not the session.
	for _, fw := range h.ForwardMgr.List() {
		if fw.ForwardID == id {
			if restRefuseTool(w, h.sessionOwning(fw.SessionID, ""), capability.ForwardClose) {
				return
			}
			break
		}
	}
	if err := h.ForwardMgr.Close(id); err != nil {
		slog.Error("handleDeleteForward: close failed", "forward_id", id, "err", err)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	slog.Info("handleDeleteForward: success", "forward_id", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleCreateForward(w http.ResponseWriter, r *http.Request) {
	if h.ForwardMgr == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "forward manager not available"})
		return
	}

	var req struct {
		SessionID  string `json:"session_id"`
		Direction  string `json:"direction"`
		RemoteHost string `json:"remote_host"`
		RemotePort int    `json:"remote_port"`
		BindHost   string `json:"bind_host"`  // -L/-D: what this host listens on
		LocalHost  string `json:"local_host"` // -R: what the target listens on
		LocalPort  int    `json:"local_port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}

	// Prefer path id (canonical POST /api/sessions/{id}/forwards); body session_id is for compat POST /api/forwards.
	sessionID := r.PathValue("id")
	if sessionID == "" {
		sessionID = strings.TrimSpace(req.SessionID)
	}
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "session_id required (use POST /api/sessions/{id}/forwards)"})
		return
	}
	sess := h.Sessions.Get(sessionID)
	if sess == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "session not found"})
		return
	}
	// A tunnel is a capability of the session it rides: an extension whose template
	// granted no forwards must not have one opened on its connection by REST, and
	// the ports it names are subject to the same whitelist rule as everything else.
	if restRefuseTool(w, sess, capability.ForwardOpen) {
		return
	}
	// A forward needs a live transport; a closed (DEAD) session must not mint a
	// listener that can never carry traffic.
	if sess.Info().Status != api.SessionRunning {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "session is closed; forwards need a live connection"})
		return
	}
	sshClient := sess.SSHClient()
	if sshClient == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "session has no SSH client — may not be ready"})
		return
	}

	if req.Direction == "" {
		req.Direction = "local"
	}

	// Validate required parameters per direction.
	switch req.Direction {
	case "local":
		if req.RemoteHost == "" || req.RemotePort <= 0 || req.RemotePort > 65535 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "local forward requires remote_host and remote_port (1-65535)"})
			return
		}
	case "remote":
		if req.LocalHost == "" || req.LocalPort <= 0 || req.LocalPort > 65535 ||
			req.RemoteHost == "" || req.RemotePort <= 0 || req.RemotePort > 65535 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "remote forward requires local_host, local_port, remote_host, remote_port (all ports 1-65535)"})
			return
		}
	case "dynamic":
		// dynamic only needs local_port (optional, auto-assigned if 0).
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "direction must be local, remote, or dynamic"})
		return
	}

	info := sess.Info()
	sshConfig := info.Name
	if sshConfig == "" {
		sshConfig = info.SSHEndpoint
	}
	isInternal := info.SSHEndpoint == "internal"

	var fw *forward.ForwardInfo
	var fwErr error
	switch req.Direction {
	case "dynamic":
		fw, fwErr = h.ForwardMgr.CreateDynamic(sessionID, sshConfig, req.BindHost, req.LocalPort, sshClient, isInternal)
	case "local":
		fw, fwErr = h.ForwardMgr.CreateLocal(sessionID, sshConfig, req.BindHost, req.RemoteHost, req.RemotePort, req.LocalPort, sshClient)
	case "remote":
		fw, fwErr = h.ForwardMgr.CreateRemote(sessionID, sshConfig, req.LocalHost, req.LocalPort, req.RemoteHost, req.RemotePort, sshClient)
	}
	if fwErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": fwErr.Error()})
		return
	}
	// Attach at creation: the session's ResourceScope closes this forward when
	// the session is deleted.
	forwardID := fw.ForwardID
	sess.AttachCleanup(func() { _ = h.ForwardMgr.Close(forwardID) })
	writeJSON(w, http.StatusCreated, fw)
}

// handleListSessionForwards returns forwards scoped to a single session.
// GET /api/sessions/{id}/forwards
func (h *Handler) handleListSessionForwards(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if h.ForwardMgr == nil {
		writeJSON(w, http.StatusOK, map[string]any{"forwards": []forward.ForwardInfo{}})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"forwards": h.ForwardMgr.ListBySession(sessionID)})
}
