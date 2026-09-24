package webui

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/approval"
	"github.com/open-mcp-ai/termcp/internal/session"
)

// Approval mode over HTTP.
//
// Two groups of endpoints, and the split matters:
//
//   - mode control (GET/PATCH /api/sessions/{id}/approval) is for whoever is
//     authorized to relax the gate. Authorization is the existing HTTP auth:
//     a caller holding the deployment token can turn the gate on or off.
//   - decisions (GET/POST /api/approvals...) are for the approvers. The approver
//     name is part of the request body and is recorded verbatim — until the
//     deployment issues a token per approver, that name is asserted rather than
//     proven, which is why the threshold, not the name, is the real control.

type approvalModeBody struct {
	Enabled bool `json:"enabled"`
	// Need is accepted for compatibility and must be 1: review has a single
	// reviewer, so the field exists only so an older client's body still parses.
	Need int `json:"need"`
	// TimeoutSeconds expires a pending request. 0 disables the timeout, which is
	// accepted but not recommended: a request that never expires keeps holding a
	// waiter and a queue entry.
	TimeoutSeconds int `json:"timeout_seconds"`
}

type approvalDecisionBody struct {
	Reason string `json:"reason"`
}

// handleSessionApproval reads or flips a session's approval mode.
//
//	GET   /api/sessions/{id}/approval
//	PATCH /api/sessions/{id}/approval
func (h *Handler) handleSessionApproval(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess := h.Sessions.Get(id)
	if sess == nil {
		http.Error(w, "session not found: "+id, http.StatusNotFound)
		return
	}

	switch r.Method {
	case http.MethodGet:
		q := sess.ApprovalQueue()
		payload := map[string]any{
			"session_id":    sess.ID,
			"approval_mode": sess.ApprovalEnabled(),
			"pending_count": 0,
			"requests":      []any{},
			// Always true: review gates the AI's MCP surface, never the operator's
			// WebSocket terminal. The field is kept because it answers the question
			// a client actually has — "can I type?" — and that answer is yes.
			"websocket_input": true,
		}
		if q != nil {
			reqs := q.List()
			payload["need"] = q.Need()
			payload["pending_count"] = countPendingRequests(reqs)
			payload["requests"] = reqs
		}
		writeJSON(w, http.StatusOK, payload)

	case http.MethodPatch:
		var body approvalModeBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if !body.Enabled {
			// Through the Manager, not the session: it also broadcasts the new
			// state so open cards stop showing the old one.
			if err := h.Sessions.DisableApproval(id); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"session_id":    sess.ID,
				"approval_mode": false,
			})
			return
		}
		if body.Need < 1 {
			// One reviewer is the model, so an omitted threshold means "one"
			// rather than an error: the client should not have to send a number
			// that has only one valid value. A caller that asks for more than one
			// is refused instead of silently downgraded, so it cannot believe it
			// configured a threshold the server ignores.
			body.Need = 1
		}
		if body.Need > 1 {
			http.Error(w, "need must be 1: review mode has a single reviewer", http.StatusBadRequest)
			return
		}
		if body.TimeoutSeconds < 0 {
			http.Error(w, "timeout_seconds cannot be negative", http.StatusBadRequest)
			return
		}
		if err := h.Sessions.EnableApproval(id, body.Need, time.Duration(body.TimeoutSeconds)*time.Second); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"session_id":      sess.ID,
			"approval_mode":   true,
			"need":            body.Need,
			"timeout_seconds": body.TimeoutSeconds,
			"websocket_input": true,
		})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleApprovalList returns every pending request across sessions, so an
// approver's page can show one worklist instead of one tab per session.
//
//	GET /api/approvals
func (h *Handler) handleApprovalList(w http.ResponseWriter, r *http.Request) {
	filterSession := strings.TrimSpace(r.URL.Query().Get("session_id"))
	out := []map[string]any{}
	for _, info := range h.Sessions.ListAll() {
		sess := h.Sessions.Get(info.ID)
		if sess == nil {
			continue
		}
		if filterSession != "" && sess.ID != filterSession {
			continue
		}
		q := sess.ApprovalQueue()
		if q == nil {
			continue
		}
		for _, req := range q.List() {
			out = append(out, map[string]any{
				"request": req,
				"need":    q.Need(),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": out, "count": len(out)})
}

// handleApprovalDecision approves or rejects one request.
//
//	POST /api/approvals/{id}/approve
//	POST /api/approvals/{id}/reject
func (h *Handler) handleApprovalDecision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	approve := strings.HasSuffix(strings.TrimSuffix(r.URL.Path, "/"), "approve")

	var body approvalDecisionBody
	// An empty body is fine: a decision is the click itself, and a rejection
	// without a reason is still a decision.
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	q, sess := h.queueForApproval(id)
	if q == nil {
		http.Error(w, "no approval request "+id+" found", http.StatusNotFound)
		return
	}

	var (
		req approval.Request
		err error
	)
	if approve {
		req, err = q.Approve(id)
	} else {
		req, err = q.Reject(id, body.Reason)
	}
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, approval.ErrNotFound) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	// An approved request is executed here, by the approver's action. The write
	// happens after the state flip so a failed write cannot leave the queue
	// believing an unexecuted command ran.
	if approve && req.State == approval.Approved {
		if err := h.executeApproved(sess, req); err != nil {
			http.Error(w, "approved but execution failed: "+err.Error(), http.StatusConflict)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"request": req})
}

// queueForApproval finds the queue holding a request id.
func (h *Handler) queueForApproval(pendingID string) (*approval.Queue, *session.Session) {
	for _, info := range h.Sessions.ListAll() {
		sess := h.Sessions.Get(info.ID)
		if sess == nil {
			continue
		}
		q := sess.ApprovalQueue()
		if q == nil {
			continue
		}
		if _, err := q.Get(pendingID); err == nil {
			return q, sess
		}
	}
	return nil, nil
}

// executeApproved runs a decided request, by the kind of thing it is.
//
// Two kinds of request exist and they execute on different machinery:
//
//   - A command line (and its keys) is written to a shell, here.
//   - A file transfer or a port forward is replayed through the MCP handler that
//     submitted it (ExecuteOperation), because those operations live with the MCP
//     server, not with the Web UI.
//
// Dispatching on the kind is load-bearing, not tidiness. The shell lookup below
// needs a shell id, and an operation's payload carries only a session id — so
// routing an operation down the command-line path failed with
// "shell  is no longer attached" (note the empty id) before the operation was
// ever attempted. Review accepted the decision, reported a failure, and did
// nothing: the worst possible outcome, because the reviewer believes it ran.
func (h *Handler) executeApproved(sess *session.Session, req approval.Request) error {
	if sess == nil {
		return errors.New("session is gone")
	}
	// An operation replays through the MCP server. req.Kind is empty for the
	// command lines written before kinds existed, and those are shell input.
	if req.Kind != "" && req.Kind != approval.KindShellInput {
		if h.ExecuteOperation == nil {
			return errors.New("this deployment cannot execute " + string(req.Kind) + " operations")
		}
		return h.ExecuteOperation(req)
	}
	cs := sess.GetChildShell(req.ShellID)
	if cs == nil {
		return errors.New("shell " + req.ShellID + " is no longer attached")
	}
	src := session.InputFromAI
	if req.Source == "rest" {
		src = session.InputFromAPI
	}
	if req.Text != "" {
		if err := cs.ExecuteApprovedInput(req.Text, false, src); err != nil {
			return err
		}
	}
	for _, key := range req.Keys {
		if err := cs.ExecuteApprovedKey(key, 1, src); err != nil {
			return err
		}
	}
	return nil
}

// countPendingRequests counts the requests still awaiting a decision.
func countPendingRequests(reqs []approval.Request) int {
	n := 0
	for _, r := range reqs {
		if r.State == approval.Pending {
			n++
		}
	}
	return n
}

// BroadcastApproval pushes one approval transition to every open Web UI tab.
//
// It reuses the existing ui_notify hub rather than adding a second push channel:
// the payload already carries a type field, the browser already routes on it,
// and an approval event needs exactly the same delivery guarantees as a toast
// (fan out to whoever is watching, drop for a tab whose buffer is full).
func (h *Handler) BroadcastApproval(sessionID string, req approval.Request) {
	if h == nil || h.notifyHub == nil {
		return
	}
	level := "info"
	title := "Approval requested"
	switch req.State {
	case approval.Approved:
		level, title = "success", "Approval granted"
	case approval.Rejected:
		level, title = "warn", "Approval rejected"
	case approval.Expired:
		level, title = "warn", "Approval expired"
	case approval.Cancelled:
		level, title = "warn", "Approval cancelled"
	}
	// A pending request is the one case that needs the human's eyes, so it is
	// sticky; the outcome is informational and dismisses itself.
	duration := 8
	if req.State == approval.Pending {
		duration = 0
	}
	h.uiNotifyHub().broadcast(map[string]any{
		"type":             "approval",
		"title":            title,
		"message":          approvalSummary(req),
		"level":            level,
		"duration_seconds": duration,
		"session_id":       sessionID,
		"request":          req,
	})
}

// approvalSummary is the line a toast shows for a request.
//
// It is just the request's Summary, because the queue guarantees one: Submit
// refuses a request without a reviewer-readable summary, and it is the single
// constructor. The other fields describe the request to the EXECUTOR (Text and
// Keys, or a Payload); Summary is the one field that describes it to a person.
//
// This function used to rebuild that line from Text and Keys, with a fallback to
// the literal "(empty)". Two things were wrong with that:
//
//   - Operations have neither Text nor Keys — they carry Summary — so every file
//     transfer and port forward toasted "(empty)": something is waiting and the
//     reviewer is not told what.
//   - The rebuild duplicated approval.ShellInputSummary, so the two could drift
//     while both looked correct. A request now has one description, produced
//     where the request is created.
//
// There is no fallback. A blank line is worse than useless here: it looks like a
// rendering glitch rather than a missing description, and it would hide the bug
// instead of surfacing it. Submit's error is the place that case is caught.
func approvalSummary(req approval.Request) string {
	return req.Summary
}
