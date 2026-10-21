package webui

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/open-mcp-ai/termcp/internal/auth"
)

// Token management routes. Issuing and revoking credentials is the operator's
// job, so every route here requires a human principal — a scoped agent token
// must not mint or kill tokens. On a no-auth loopback deployment (no
// middleware ran) there is no principal at all, which is the operator by
// definition of the loopback trust model.
func (h *Handler) requireOperator(w http.ResponseWriter, r *http.Request) bool {
	p, ok := auth.PrincipalFromContext(r.Context())
	if ok && p.Kind != auth.KindOperator {
		http.Error(w, "forbidden: token management is an operator operation", http.StatusForbidden)
		return false
	}
	return true
}

func (h *Handler) handleListTokens(w http.ResponseWriter, r *http.Request) {
	if h.Tokens == nil {
		http.Error(w, "token store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.requireOperator(w, r) {
		return
	}
	recs, err := h.Tokens.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": recs})
}

func (h *Handler) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	if h.Tokens == nil {
		http.Error(w, "token store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.requireOperator(w, r) {
		return
	}
	var req struct {
		Kind             string   `json:"kind"`
		Tools            []string `json:"tools"`
		Sessions         []string `json:"sessions"`
		ApprovalRequired bool     `json:"approval_required"`
		TTLSeconds       int64    `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.TTLSeconds < 0 {
		http.Error(w, "ttl_seconds must be >= 0 (0 = no expiry)", http.StatusBadRequest)
		return
	}
	createdBy := "operator"
	if p, ok := auth.PrincipalFromContext(r.Context()); ok {
		createdBy = p.ID
	}
	token, rec, err := h.Tokens.Create(req.Kind, req.Tools, req.Sessions, req.ApprovalRequired,
		time.Duration(req.TTLSeconds)*time.Second, createdBy)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// The plaintext token is in THIS response and nowhere else — the table
	// holds only its verifier.
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "record": rec})
}

func (h *Handler) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if h.Tokens == nil {
		http.Error(w, "token store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.requireOperator(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := h.Tokens.Revoke(id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
