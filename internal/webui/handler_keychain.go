package webui

import (
	"errors"
	"net/http"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/configstore"
)

/* Keychain endpoints: one route per thing a key can do.
 *
 *   GET    /api/keychain                  list the keys and the index owner
 *   POST   /api/keychain                  add a key (and its mnemonic)
 *   DELETE /api/keychain/{id}             destroy it, once the words are written down
 *   POST   /api/keychain/{id}/plug        insert it
 *   POST   /api/keychain/{id}/unplug      remove it
 *   GET    /api/keychain/{id}/mnemonic    export the 24 words
 *
 * Every one of them is a single request, because a key is independent: inserting
 * one unwraps exactly that key, exporting one writes down exactly its words.
 *
 * Inserting a key, activating a host and exporting a mnemonic are HUMAN actions
 * (§7.2). An Agent that holds the MCP token must not be able to turn these valves,
 * so a request that identifies itself as an agent is refused with 403.
 */

type keychainAddRequest struct {
	// Secret is the mnemonic to adopt. Empty means "make me one", and the generated
	// phrase comes back exactly once.
	Secret string `json:"secret"`
	// Label is the name a human will recognise this key by. It is stored in clear
	// — it has to be readable while nothing is inserted — so it is not a secret.
	Label string `json:"label"`
	// Passphrase, when present, marks this key so its hosts need it to activate.
	// It is never stored: only the fact that the key has one is.
	Passphrase string `json:"passphrase"`
}

type keychainRemoveRequest struct {
	// BackupConfirmed is the human's statement that the 24 words are written down.
	// Deleting a key destroys its only copy here.
	BackupConfirmed bool `json:"backup_confirmed"`
}

// handleKeychainList answers with the list even when nothing is plugged in: the
// panel has to render the rows before anyone can insert a key.
func (h *Handler) handleKeychainList(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	status, err := h.SSH.KeychainStatus()
	if err != nil {
		writeKeychainError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

// handleKeychainAdd creates a key. It also creates the keychain itself when
// encryption has never been enabled, because the first key and the first keychain
// are the same object.
func (h *Handler) handleKeychainAdd(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	var body keychainAddRequest
	if err := decodeConfigStorageJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_, generated, err := h.SSH.CreateKeyWithLabel(body.Secret, body.Passphrase, body.Label, configEncryptionProviders()...)
	if err != nil {
		writeKeychainError(w, err)
		return
	}
	payload := h.keychainPayload()
	if generated != "" {
		/* The phrase is key material and is returned exactly once, to be written
		   down. Everything the store keeps is derived from it. */
		payload["mnemonic"] = generated
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
	h.broadcastConfigStorage()
}

/*
handleKeychainNewMnemonic hands out a fresh phrase without creating anything.

The panel asks for one when the person presses Generate in the create form: the
words are a field they can read, paste elsewhere, or replace with ones they already
have, and pressing Save is what makes a key. Generating them server-side on demand
keeps the wordlist in one place and keeps the form honest — before this, "generate"
meant "create a key with whatever name was in the box", which is why a key could be
born unnamed.
*/
func (h *Handler) handleKeychainNewMnemonic(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	phrase, err := h.SSH.NewMnemonic()
	if err != nil {
		writeKeychainError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"mnemonic": phrase})
}

type keychainPlugRequest struct {
	Passphrase string `json:"passphrase"`
}

// handleKeychainPlug inserts one key and, when a passphrase is given (or the key
// needs none), activates the hosts it seals in the same step. An empty body still
// inserts the key without activating — callers that do both in two turns keep
// working, and a caller that passes {"passphrase":"…"} gets the one-step flow.
func (h *Handler) handleKeychainPlug(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	id, ok := keychainID(w, r)
	if !ok {
		return
	}
	var body keychainPlugRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeConfigStorageJSON(r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	var err error
	if body.Passphrase != "" {
		err = h.SSH.PlugKeyWithPassphrase(id, body.Passphrase, configEncryptionProviders()...)
	} else {
		err = h.SSH.PlugKey(id, configEncryptionProviders()...)
	}
	if err != nil {
		writeKeychainError(w, err)
		return
	}
	h.respondKeychain(w)
}

// handleKeychainUnplug drops one key from memory and from the next start. Nothing
// is deleted: its hosts simply stop being readable until it is inserted again.
func (h *Handler) handleKeychainUnplug(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	id, ok := keychainID(w, r)
	if !ok {
		return
	}
	if err := h.SSH.UnplugKey(id); err != nil {
		writeKeychainError(w, err)
		return
	}
	h.respondKeychain(w)
}

// handleKeychainMnemonic exports one key's 24 words. This machine already holds the
// slot, so it asks for nothing (§9.4) — and the answer is never cached.
func (h *Handler) handleKeychainMnemonic(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	id, ok := keychainID(w, r)
	if !ok {
		return
	}
	phrase, err := h.SSH.ExportMnemonic(id)
	if err != nil {
		writeKeychainError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"mnemonic": phrase})
}

/*
handleDiscardKeychain is the human exit for a keychain file this build cannot
read (§7.2). It is not the same action as DELETE /api/keychain/{id}: that one
destroys a READABLE key and has its own guards (backup confirmed, seals nothing,
not the index owner). This one only clears a file that is already unreadable, and
still refuses when any host's ciphertext depends on it — deleting the only thing
that could ever open a host is a different decision.
*/
func (h *Handler) handleDiscardKeychain(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	if err := h.SSH.DiscardDamagedKeychain(); err != nil {
		writeKeychainError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, h.keychainPayload())
}

// handleKeychainRemove destroys one key, refusing until the human confirms the words
// are backed up and the key seals no host and does not own the index.
func (h *Handler) handleKeychainRemove(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	id, ok := keychainID(w, r)
	if !ok {
		return
	}
	var body keychainRemoveRequest
	if r.ContentLength > 0 {
		if err := decodeConfigStorageJSON(r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := h.SSH.RemoveKey(id, body.BackupConfirmed); err != nil {
		writeKeychainError(w, err)
		return
	}
	h.respondKeychain(w)
}

/* humanOnly refuses key management to anything that says it is an agent.
 *
 * It is the valve's definition, not a configurable policy (§7.2, §10.3): an Agent
 * holding the MCP token must not be able to insert a key, activate a host or export
 * the words. The identity it reads is the one the request carries; the full
 * human/agent authentication this needs is §7.2's stated precondition and is not
 * implemented here.
 */
func (h *Handler) humanOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-Termcp-Role") != "agent" {
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusForbidden, map[string]string{
		"code":  "forbidden",
		"error": "This action is human-only: it turns a security valve.",
	})
	return false
}

// respondKeychain writes the list back so one request updates the whole panel.
func (h *Handler) respondKeychain(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, h.keychainPayload())
	h.broadcastConfigStorage()
}

func (h *Handler) keychainPayload() map[string]any {
	status, err := h.SSH.KeychainStatus()
	if err != nil {
		return map[string]any{"keys": []configstore.KeyInfo{}, "catalog_key_id": ""}
	}
	return map[string]any{"keys": status.Keys}
}

// keychainID parses the path id, answering 404 for something that cannot be a key.
type keychainRenameRequest struct {
	Label string `json:"label"`
}

/*
keychainPassphraseRequest carries both layers of the same rotation: the phrase

	that opens the hosts now, and the one that should open them afterwards. An empty
	`new` removes the second layer, which is a decision rather than an omission, so
	the two fields cannot be confused for "leave it alone".
*/
type keychainPassphraseRequest struct {
	Old string `json:"old"`
	New string `json:"new"`
}

type keychainRotateRequest struct {
	// Passphrase is the second layer the NEW key should carry. Rotation used to
	// drop it silently; a person rotating a passphrase-bearing key now says what
	// they want instead of inheriting a security property by accident.
	Passphrase string `json:"passphrase"`
}

// handleKeychainRename renames one key. A name is public and re-seals nothing, so
// it needs no confirmation and destroys no ciphertext: it is the one action on a
// key that is safe to take back.
func (h *Handler) handleKeychainRename(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	id, ok := keychainID(w, r)
	if !ok {
		return
	}
	var body keychainRenameRequest
	if err := decodeConfigStorageJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.SSH.SetKeyLabel(id, body.Label); err != nil {
		writeKeychainError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, h.keychainPayload())
	h.broadcastConfigStorage()
}

/* handleKeychainPassphrase changes one key's passphrase and nothing else. The words
 * stay, the id stays and the address stays, so no host has to be handed over; each
 * host is re-sealed in place, which is the difference between this route and
 * /rotate. */
func (h *Handler) handleKeychainPassphrase(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	id, ok := keychainID(w, r)
	if !ok {
		return
	}
	var body keychainPassphraseRequest
	if err := decodeConfigStorageJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.SSH.ChangeKeyPassphrase(id, body.Old, body.New); err != nil {
		writeKeychainError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, h.keychainPayload())
	h.broadcastConfigStorage()
}

// handleKeychainRotate gives one key new words, handing its hosts over to the new
// key first. The phrase comes back exactly once, like a fresh key's, because the
// old words no longer open anything.
func (h *Handler) handleKeychainRotate(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	id, ok := keychainID(w, r)
	if !ok {
		return
	}
	var body keychainRotateRequest
	if r.ContentLength > 0 {
		if err := decodeConfigStorageJSON(r, &body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	_, generated, err := h.SSH.RotateKey(id, body.Passphrase, configEncryptionProviders()...)
	if err != nil {
		writeKeychainError(w, err)
		return
	}
	payload := h.keychainPayload()
	payload["mnemonic"] = generated
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, payload)
	h.broadcastConfigStorage()
}

func keychainID(w http.ResponseWriter, r *http.Request) (configstore.ID, bool) {
	var id configstore.ID
	if err := id.UnmarshalText([]byte(r.PathValue("id"))); err != nil {
		writeKeychainError(w, configstore.ErrKeyNotFound)
		return id, false
	}
	return id, true
}

// writeKeychainError keeps the panel's vocabulary in codes (§7.3), so the page
// decides the sentence and the language.
func writeKeychainError(w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	status, code, message := statusAndCodeForKeychainError(err)
	if code == "" {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	body := map[string]string{"code": code, "error": message}
	/* A refusal that names what is in the way is actionable; the same refusal with
	   only a count sends the person hunting for the panel they are already in. The
	   detail rides in `error` so the page can print it without a second request —
	   and the page still owns the sentence, because `code` is what it switches on. */
	if detail := keychainErrorDetail(err); detail != "" {
		body["error"] = message + " " + detail
		body["detail"] = detail
	}
	writeJSON(w, status, body)
}

// keychainErrorDetail pulls the part of an error that names things (host names,
// counts) out of the wrapped message, without inventing a second error type.
func keychainErrorDetail(err error) string {
	text := err.Error()
	if index := strings.Index(text, ": "); index >= 0 {
		return strings.TrimSpace(text[index+2:])
	}
	return ""
}

func statusAndCodeForKeychainError(err error) (int, string, string) {
	switch {
	case err == nil:
		return http.StatusOK, "", ""
	case errors.Is(err, configstore.ErrKeyInUse):
		return http.StatusConflict, "key_in_use", "Hosts are still encrypted with this key."
	case errors.Is(err, configstore.ErrBackupUnconfirmed):
		return http.StatusConflict, "key_backup_required", "Confirm the recovery phrase is written down first."
	case errors.Is(err, configstore.ErrKeyNotFound):
		return http.StatusNotFound, "key_unknown", "No such key in the keychain."
	case errors.Is(err, configstore.ErrKeyNotLoaded):
		return http.StatusConflict, "key_locked", "Load the key that seals this host first."
	case errors.Is(err, configstore.ErrMnemonicInvalid):
		return http.StatusBadRequest, "key_mnemonic_invalid", "That is not a valid recovery phrase."
	case errors.Is(err, configstore.ErrInvalidLabel):
		/* A name the caller has to fix, so 400 and not 500: an empty one used to be
		   accepted and is now the store's rule, and the person sees the sentence
		   rather than a status code. */
		return http.StatusBadRequest, "key_label_invalid", "A key needs a name (at most 64 characters, no control characters)."
	case errors.Is(err, configstore.ErrPassphraseWrong):
		return http.StatusBadRequest, "passphrase_incorrect", "That passphrase is not the one this key's hosts were sealed with."
	case errors.Is(err, configstore.ErrPassphraseRequired):
		return http.StatusConflict, "passphrase_required", "This key's hosts need its passphrase."
	case errors.Is(err, configstore.ErrLastWrapper):
		return http.StatusConflict, "key_last_wrapper", "This is the key's last way in."
	case errors.Is(err, configstore.ErrWrapperNotFound):
		return http.StatusNotFound, "key_not_found", "This key has no such wrapper."
	case errors.Is(err, configstore.ErrNotInitialized):
		return http.StatusConflict, "config_initialization_required", "Add a key to the keychain first."
	case errors.Is(err, configstore.ErrProviderUnavailable):
		return http.StatusServiceUnavailable, "key_provider_unavailable", "The key provider is unavailable. In containers, inject TERMCP_CONFIG_KEK."
	case errors.Is(err, configstore.ErrAuthentication):
		return http.StatusServiceUnavailable, "keyring_authentication_failed", "The key provider cannot authenticate the key."
	case errors.Is(err, configstore.ErrDurability):
		return http.StatusInternalServerError, "storage_durability_unconfirmed", "Storage publication durability is unconfirmed; refresh the keychain before retrying."
	case errors.Is(err, configstore.ErrKeyringMissing):
		return http.StatusConflict, "keyring_missing", "The keychain file cannot be read."
	case errors.Is(err, configstore.ErrNotDamaged):
		return http.StatusConflict, "keychain_readable", "The keychain is readable."
	case errors.Is(err, configstore.ErrSealedProfiles):
		return http.StatusConflict, "keychain_seals_hosts", "Hosts are still encrypted with this keychain."
	default:
		return http.StatusInternalServerError, "", ""
	}
}
