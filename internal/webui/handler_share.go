package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/hostshare"
)

/* Sharing a host between people: the sender seals one profile to the recipient's
 * public key and pastes the result into whatever chat they already use. The
 * server's only jobs are to hand out this store's public key, to seal on demand,
 * and to import what somebody else sealed for us.
 *
 * Import is treated as untrusted input: a blob proves only that its author had
 * this store's public key, which anyone can obtain. The name it carries is a
 * suggestion; the profile is validated like any other paste before it is saved.
 */

/* handleShareIdentity returns ONE key's public half, named by ?key=. A keychain has
 * an address per key, so an address without a key is not a thing the page can show:
 * it would silently pick one, and the sender would have no way to say which of your
 * keys should hold what they send.
 */
func (h *Handler) handleShareIdentity(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	var keyID configstore.ID
	if err := keyID.UnmarshalText([]byte(r.URL.Query().Get("key"))); err != nil {
		http.Error(w, "key= is required", http.StatusBadRequest)
		return
	}
	publicKey, err := h.SSH.SharePublicKey(keyID)
	if err != nil {
		writeShareError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"public_key": publicKey})
}

// handleShareExport seals one profile to the recipient named in the body.
func (h *Handler) handleShareExport(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Name      string `json:"name"`
		Recipient string `json:"recipient"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": "Expected a JSON object with name and recipient."})
		return
	}
	if strings.TrimSpace(body.Recipient) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "share_recipient_required", "error": "Paste the other person's public key first."})
		return
	}
	blob, err := h.SSH.ExportSharedProfile(strings.TrimSpace(body.Name), body.Recipient)
	if err != nil {
		writeShareError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"blob": blob})
}

// handleShareImport saves a profile somebody sealed for this store.
func (h *Handler) handleShareImport(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Blob string `json:"blob"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": "Expected a JSON object with a blob."})
		return
	}
	name, err := h.SSH.ImportSharedProfile(body.Blob)
	if err != nil {
		writeShareError(w, err)
		return
	}
	/* Every open tab lists connections from the session frame, so wake them
	   instead of leaving a stale list behind a successful import. */
	h.sessionHub().broadcast()
	writeJSON(w, http.StatusOK, map[string]string{"name": name})
}

func writeShareError(w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	switch {
	case errors.Is(err, hostshare.ErrRecipient):
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "share_recipient_invalid", "error": "That public key is not valid."})
	case errors.Is(err, hostshare.ErrFormat):
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "share_blob_invalid", "error": "That text is not a Termcp host share."})
	case errors.Is(err, hostshare.ErrTooLarge):
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"code": "share_blob_too_large", "error": "That share is too large."})
	case errors.Is(err, hostshare.ErrDecrypt):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "share_wrong_key", "error": "This share was sealed for a different key."})
	case errors.Is(err, configstore.ErrKeyNotLoaded):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "key_locked", "error": "Load the key that seals this store first."})
	case errors.Is(err, configstore.ErrNotInitialized):
		writeJSON(w, http.StatusConflict, map[string]string{"code": "config_initialization_required", "error": "Add a key to the keychain first."})
	default:
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
