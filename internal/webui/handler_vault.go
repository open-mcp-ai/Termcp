package webui

import (
	"encoding/json"
	"io"
	"net/http"
)

// Vault status and unlock endpoints. They sit on the operator's REST surface
// like every other management route; the passphrase travels in the request
// body, never the URL (query strings land in shell history and proxy logs).
//
// Locking semantics live in internal/vault: these handlers only translate
// between HTTP and the vault's own error vocabulary.

// vaultStatus is GET /api/vault's answer.
type vaultStatus struct {
	Initialized   bool     `json:"initialized"`
	Unlocked      bool     `json:"unlocked"`
	CanAutoUnlock bool     `json:"can_auto_unlock"`
	Profiles      []string `json:"profiles"`
}

func (h *Handler) handleVaultStatus(w http.ResponseWriter, r *http.Request) {
	// Vault state is instance management, not a capability: operator only,
	// same rule as the token routes.
	if !h.requireOperator(w, r) {
		return
	}
	if h.Vault == nil || !h.Vault.Exists() {
		writeJSON(w, http.StatusOK, vaultStatus{})
		return
	}
	names, err := h.Vault.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, vaultStatus{
		Initialized:   true,
		Unlocked:      h.Vault.IsUnlocked(),
		CanAutoUnlock: h.Vault.CanAutoUnlock(),
		Profiles:      names,
	})
}

func (h *Handler) handleVaultUnlock(w http.ResponseWriter, r *http.Request) {
	if !h.requireOperator(w, r) {
		return
	}
	if h.Vault == nil || !h.Vault.Exists() {
		http.Error(w, "no vault: run `termcp vault init` first", http.StatusNotFound)
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.Passphrase == "" {
		http.Error(w, "passphrase must not be empty", http.StatusBadRequest)
		return
	}
	if err := h.Vault.Unlock(req.Passphrase); err != nil {
		// Wrong passphrase and corrupt identity file share one answer: 401.
		// The vault's own message does not distinguish them either.
		http.Error(w, "unlock failed: wrong passphrase or corrupt identity file", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleVaultLock(w http.ResponseWriter, r *http.Request) {
	if !h.requireOperator(w, r) {
		return
	}
	if h.Vault == nil || !h.Vault.Exists() {
		http.Error(w, "no vault: run `termcp vault init` first", http.StatusNotFound)
		return
	}
	h.Vault.Lock()
	w.WriteHeader(http.StatusNoContent)
}
