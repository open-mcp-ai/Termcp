package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

const maxConfigStorageJSON = 64 << 10

// configStorageSelection is the batch body: which hosts, which key seals them, and —
// for a key that carries one — that key's passphrase. It is the same shape the
// conversion and the activation routes use, because both act on a ticked set.
type configStorageSelection struct {
	Names []string `json:"names"`
	Key   string   `json:"key"`
	// Passphrase is the key's passphrase. It is required when that key carries one:
	// converting with it writes each host's `.adk`, and activating with it derives
	// each host's activation key. It never reaches disk or a log line.
	Passphrase string `json:"passphrase"`
	// Passphrases is the per-key form the activation route takes: one entry per key
	// that needs one, keyed by the key id the panel already displays. A request that
	// does not name a key's passphrase refuses the whole batch (§8.5 step 2).
	Passphrases map[string]string `json:"passphrases"`
}

type configStorageMaskRequest struct {
	Masked bool `json:"masked"`
}

func (h *Handler) handleConfigStorageStatus(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	status, err := h.SSH.EncryptionStatus()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

/* broadcastConfigStorage pushes the new state to every open tab after a key
 * operation. The badge and the panel are two views of one truth, and a second
 * tab — or the same tab after an MCP client acted — must not keep showing the
 * old one. Failures are ignored on purpose: a notification is not part of the
 * operation's contract, and its absence only costs a stale glyph.
 */
func (h *Handler) broadcastConfigStorage() {
	if h.SSH == nil {
		return
	}
	status, err := h.SSH.EncryptionStatus()
	if err != nil {
		return
	}
	h.uiNotifyHub().broadcast(map[string]any{"type": "config-storage", "config_storage": status})
}

/* writeConfigStorageError is the one place a storage error becomes an HTTP answer.
 * The codes are §7.3's table verbatim, because the page maps codes to sentences and
 * a second vocabulary is how one condition ends up described two ways.
 */
func writeConfigStorageError(w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	status, code, message := http.StatusInternalServerError, "config_storage_failed", "Configuration encryption operation failed."
	switch {
	case errors.Is(err, configstore.ErrKeyInUse):
		status, code, message = http.StatusConflict, "key_in_use", "Hosts are still encrypted with this key."
	case errors.Is(err, configstore.ErrBackupUnconfirmed):
		status, code, message = http.StatusConflict, "key_backup_required", "Confirm the recovery phrase is written down first."
	case errors.Is(err, configstore.ErrKeyNotLoaded):
		status, code, message = http.StatusConflict, "key_locked", "Load the key that seals this host first."
	case errors.Is(err, configstore.ErrNotActivated):
		status, code, message = http.StatusConflict, "host_not_activated", "This host is not activated. Activate it, with its key's passphrase if it has one."
	case errors.Is(err, configstore.ErrPassphraseRequired):
		status, code, message = http.StatusConflict, "passphrase_required", err.Error()
	case errors.Is(err, configstore.ErrPassphraseWrong):
		status, code, message = http.StatusBadRequest, "passphrase_incorrect", err.Error()
	case errors.Is(err, configstore.ErrKeyPassphraseMissing):
		status, code, message = http.StatusConflict, "host_no_passphrase", "This key has no passphrase, so there is nothing to type."
	case errors.Is(err, configstore.ErrOrphaned):
		status, code, message = http.StatusConflict, "config_encryption_orphaned", "This host was encrypted with a key that has been deleted."
	case errors.Is(err, configstore.ErrKeyNotFound):
		status, code, message = http.StatusNotFound, "key_unknown", "No such key in the keychain."
	/* "Choose a key" is not "there is no such key": the second one tells the caller
	   to stop looking, and this one tells it to ask the question it skipped. They
	   were the same code, so a form that forgot to send a key read as a broken
	   keychain. */
	case errors.Is(err, configstore.ErrKeyRequired):
		status, code, message = http.StatusBadRequest, "key_required", "Choose the key that should seal this host."
	case errors.Is(err, configstore.ErrNotFound):
		status, code, message = http.StatusNotFound, "connection_not_found", "No such host."
	case errors.Is(err, configstore.ErrNotInitialized):
		status, code, message = http.StatusConflict, "config_initialization_required", "Add a key to the keychain first."
	case errors.Is(err, configstore.ErrProviderUnavailable):
		status, code, message = http.StatusServiceUnavailable, "key_provider_unavailable", "The key provider is unavailable. In containers, inject TERMCP_CONFIG_KEK."
	case errors.Is(err, configstore.ErrAuthentication):
		status, code, message = http.StatusServiceUnavailable, "keyring_authentication_failed", "The key provider cannot authenticate the key."
	case errors.Is(err, configstore.ErrDurability):
		code, message = "storage_durability_unconfirmed", "Storage publication durability is unconfirmed; refresh the keychain before retrying."
	case errors.Is(err, configstore.ErrKeyringMissing):
		code, message = "keyring_missing", "The keychain file cannot be read."
	case errors.Is(err, configstore.ErrMnemonicInvalid):
		status, code, message = http.StatusBadRequest, "key_mnemonic_invalid", "That is not a valid recovery phrase."
	case errors.Is(err, sshconfig.ErrInvalid):
		status, code, message = http.StatusBadRequest, "invalid_input", err.Error()
	}
	writeJSON(w, status, map[string]string{"code": code, "error": message})
}

/*
The same chain the server starts with. Registering it here too is what lets a
new key get a slot this machine can present by itself: with no provider the only
way in is a word list nobody can read back.
*/
func configEncryptionProviders() []configstore.Provider {
	return configstore.DefaultProviders()
}

func (h *Handler) handleEncryptConfigs(w http.ResponseWriter, r *http.Request) {
	h.handleConfigConversion(w, r, true)
}

func (h *Handler) handleRestoreConfigs(w http.ResponseWriter, r *http.Request) {
	h.handleConfigConversion(w, r, false)
}

func (h *Handler) handleConfigConversion(w http.ResponseWriter, r *http.Request, encrypt bool) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	var body configStorageSelection
	if err := decodeConfigStorageJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	keyID, err := configStorageKeyID(body.Key, r.URL.Query().Get("key"))
	if err != nil {
		writeConfigStorageError(w, err)
		return
	}
	var results []sshconfig.EncryptionResult
	if encrypt {
		results, err = h.SSH.EncryptProfiles(body.Names, keyID, body.Passphrase)
	} else {
		results, err = h.SSH.RestoreProfiles(body.Names)
	}
	if err != nil {
		writeConfigStorageError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
	h.broadcastConfigStorage()
}

// handleActivateConfigs turns on the second layer for a selection. One request, one
// passphrase per key that needs one, and the whole batch or nothing (§8.5).
func (h *Handler) handleActivateConfigs(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	var body configStorageSelection
	if err := decodeConfigStorageJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	passphrases := make(map[configstore.ID]string, len(body.Passphrases))
	for text, passphrase := range body.Passphrases {
		id, err := configStorageKeyID(text)
		if err != nil {
			writeConfigStorageError(w, err)
			return
		}
		passphrases[id] = passphrase
	}
	results, err := h.SSH.Activate(body.Names, passphrases)
	if err != nil {
		writeConfigStorageError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"results": hostResults(results)})
	h.broadcastConfigStorage()
}

// handleDeactivateConfigs drops the second layer for a selection. It touches no
// ciphertext, so there is nothing to confirm.
func (h *Handler) handleDeactivateConfigs(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	if !h.humanOnly(w, r) {
		return
	}
	var body configStorageSelection
	if err := decodeConfigStorageJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.SSH.Deactivate(body.Names); err != nil {
		writeConfigStorageError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"deactivated": body.Names})
	h.broadcastConfigStorage()
}

// handleMaskConfigs flips the global masking switch. It changes serialization only:
// storage, connectivity and validation are untouched (§10.2).
func (h *Handler) handleMaskConfigs(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	var body configStorageMaskRequest
	if err := decodeConfigStorageJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.SSH.SetMasked(body.Masked); err != nil {
		writeConfigStorageError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]bool{"masked": body.Masked})
	h.broadcastConfigStorage()
}

// hostResults renders the per-host map an activation returns in the same shape the
// conversion routes use, so the page has one result vocabulary.
func hostResults(results map[string]error) []sshconfig.EncryptionResult {
	out := make([]sshconfig.EncryptionResult, 0, len(results))
	for name, err := range results {
		entry := sshconfig.EncryptionResult{Name: name, OK: err == nil}
		if err != nil {
			entry.Code = "activation_failed"
			entry.Error = err.Error()
		}
		out = append(out, entry)
	}
	sortResults(out)
	return out
}

func sortResults(results []sshconfig.EncryptionResult) {
	for i := 1; i < len(results); i++ {
		for j := i; j > 0 && results[j].Name < results[j-1].Name; j-- {
			results[j], results[j-1] = results[j-1], results[j]
		}
	}
}

// configStorageKeyID parses a key id from the body or the query. Empty means "no
// key named", which the store refuses for a multi-key keychain: there is no
// implicit choice.
func configStorageKeyID(values ...string) (configstore.ID, error) {
	for _, value := range values {
		if value == "" {
			continue
		}
		var id configstore.ID
		if err := id.UnmarshalText([]byte(value)); err != nil {
			return id, configstore.ErrKeyNotFound
		}
		return id, nil
	}
	return configstore.ID{}, nil
}

func decodeConfigStorageJSON(r *http.Request, dst any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, maxConfigStorageJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
}
