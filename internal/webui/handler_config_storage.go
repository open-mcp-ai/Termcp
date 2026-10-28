package webui

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/open-mcp-ai/termcp/internal/configstore"
	"github.com/open-mcp-ai/termcp/internal/sshconfig"
)

const maxConfigStorageJSON = 64 << 10

type configStorageSetting struct {
	EncryptNew bool `json:"encrypt_new"`
}

type configStorageSelection struct {
	Names []string `json:"names"`
}

type configStoragePassword struct {
	Password        string `json:"password"`
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	Confirmation    string `json:"confirmation"`
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

func (h *Handler) handleConfigStorageSetting(w http.ResponseWriter, r *http.Request) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	var body configStorageSetting
	if err := decodeConfigStorageJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.EncryptNew {
		status, err := h.SSH.EncryptionStatus()
		if err != nil {
			writeConfigStorageError(w, err)
			return
		}
		if !status.Initialized {
			w.Header().Set("Cache-Control", "no-store")
			writeJSON(w, http.StatusConflict, map[string]string{
				"code": "config_initialization_required", "error": "Set an encryption password before enabling encryption.",
			})
			return
		}
	}
	if err := h.SSH.SetNewProfileEncryption(
		body.EncryptNew,
		configEncryptionProviders()...,
	); err != nil {
		writeConfigStorageError(w, err)
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

func (h *Handler) handleInitializeConfigKey(w http.ResponseWriter, r *http.Request) {
	h.handleConfigPassword(w, r, "initialize")
}

func (h *Handler) handleRecoverConfigKey(w http.ResponseWriter, r *http.Request) {
	h.handleConfigPassword(w, r, "recover")
}

func (h *Handler) handleUnlockConfigKey(w http.ResponseWriter, r *http.Request) {
	h.handleConfigPassword(w, r, "unlock")
}
func (h *Handler) handleChangeConfigPassword(w http.ResponseWriter, r *http.Request) {
	h.handleConfigPassword(w, r, "password")
}

func (h *Handler) handleConfigPassword(w http.ResponseWriter, r *http.Request, action string) {
	if !allowConfigKeyRequest(w, r) {
		return
	}
	if h.SSH == nil {
		writeConfigStorageError(w, configstore.ErrNotInitialized)
		return
	}
	var body configStoragePassword
	if err := decodeConfigStorageJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": "Expected one password operation JSON object."})
		return
	}
	if !h.configKeyMu.TryLock() {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"code": "key_operation_busy", "error": "Another password operation is in progress."})
		return
	}
	defer h.configKeyMu.Unlock()
	var err error
	switch action {
	case "initialize":
		err = h.SSH.InitializeEncryption(body.Password, body.Confirmation, configEncryptionProviders()...)
	case "password":
		err = h.SSH.ChangeEncryptionPassword(body.CurrentPassword, body.NewPassword, body.Confirmation, configEncryptionProviders()...)
	case "unlock", "recover":
		err = h.SSH.UnlockEncryption(body.Password, action == "recover", configEncryptionProviders()...)
	}
	body = configStoragePassword{}
	if err != nil {
		if errors.Is(err, configstore.ErrAuthentication) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": "password_incorrect", "error": "The password is incorrect or encrypted data cannot be authenticated."})
			return
		}
		writeConfigStorageError(w, err)
		return
	}
	status, err := h.SSH.EncryptionStatus()
	if err != nil {
		writeConfigStorageError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func allowConfigKeyRequest(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	badOrigin := r.Header.Get("Sec-Fetch-Site") == "cross-site"
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		badOrigin = badOrigin || err != nil || parsed == nil ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") || !strings.EqualFold(parsed.Host, r.Host)
	}
	if badOrigin {
		writeJSON(w, http.StatusForbidden, map[string]string{"code": "invalid_origin", "error": "Main Key operations must originate from this instance."})
		return false
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSON(w, http.StatusUnsupportedMediaType, map[string]string{"code": "invalid_request", "error": "Content-Type must be application/json."})
		return false
	}
	return true
}

func writeConfigStorageError(w http.ResponseWriter, err error) {
	w.Header().Set("Cache-Control", "no-store")
	status, code, message := http.StatusInternalServerError, "config_storage_failed", "Configuration encryption operation failed."
	switch {
	case errors.Is(err, configstore.ErrPasswordConfirmation):
		status, code, message = http.StatusBadRequest, "password_confirmation_mismatch", "The new password and confirmation do not match."
	case errors.Is(err, configstore.ErrPasswordInvalid):
		status, code, message = http.StatusBadRequest, "password_invalid", "Use at least 8 characters and at most 1024 UTF-8 bytes."
	case errors.Is(err, configstore.ErrPasswordNotConfigured):
		status, code, message = http.StatusConflict, "password_not_configured", "Restore the original key provider, then set an encryption password."
	case errors.Is(err, configstore.ErrPasswordRequired):
		status, code, message = http.StatusConflict, "password_required", "Enter the encryption password in the Web UI."
	case errors.Is(err, configstore.ErrRecoveryKey):
		status, code, message = http.StatusBadRequest, "main_key_invalid", "The supplied key material cannot decrypt all stored configurations."
	case errors.Is(err, configstore.ErrLocked):
		status, code, message = http.StatusServiceUnavailable, "config_encryption_locked", "Recover the Main Key in configuration encryption settings."
	case errors.Is(err, configstore.ErrNotLocked):
		status, code, message = http.StatusConflict, "config_already_unlocked", "The Main Key is already unlocked."
	case errors.Is(err, configstore.ErrAlreadyInitialized):
		status, code, message = http.StatusConflict, "config_already_initialized", "Configuration encryption has already been initialized."
	case errors.Is(err, configstore.ErrNotInitialized):
		status, code, message = http.StatusConflict, "config_initialization_required", "Enable and initialize configuration encryption first."
	case errors.Is(err, configstore.ErrRotationPending):
		status, code, message = http.StatusConflict, "key_rotation_pending", "Resume Main Key replacement before changing encrypted configurations."
	case errors.Is(err, configstore.ErrProviderUnavailable):
		status, code, message = http.StatusServiceUnavailable, "key_provider_unavailable", "The key provider is unavailable. In containers, inject TERMCP_CONFIG_KEK."
	case errors.Is(err, configstore.ErrAuthentication):
		status, code, message = http.StatusServiceUnavailable, "keyring_authentication_failed", "The key provider cannot authenticate the Main Key."
	case errors.Is(err, configstore.ErrDurability):
		code, message = "storage_durability_unconfirmed", "Storage publication durability is unconfirmed; refresh the encryption status before retrying."
	}
	writeJSON(w, status, map[string]string{"code": code, "error": message})
}

func configEncryptionProviders() []configstore.Provider {
	if os.Getenv(configstore.EnvKEKVariable) != "" {
		return []configstore.Provider{configstore.NewEnvProvider("")}
	}
	return []configstore.Provider{configstore.SystemProvider()}
}

func (h *Handler) handleEncryptConfigs(w http.ResponseWriter, r *http.Request) {
	h.handleConfigConversion(w, r, h.SSH.EncryptProfiles)
}

func (h *Handler) handleRestoreConfigs(w http.ResponseWriter, r *http.Request) {
	h.handleConfigConversion(w, r, h.SSH.RestoreProfiles)
}

func (h *Handler) handleConfigConversion(
	w http.ResponseWriter,
	r *http.Request,
	convert func([]string) ([]sshconfig.EncryptionResult, error),
) {
	if h.SSH == nil {
		http.Error(w, "ssh store not configured", http.StatusServiceUnavailable)
		return
	}
	var body configStorageSelection
	if err := decodeConfigStorageJSON(r, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	results, err := convert(body.Names)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, configstore.ErrRotationPending) {
			status = http.StatusConflict
		}
		if errors.Is(err, configstore.ErrProviderUnavailable) ||
			errors.Is(err, configstore.ErrAuthentication) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), status)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
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
