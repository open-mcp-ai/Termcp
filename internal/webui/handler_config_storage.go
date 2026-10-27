package webui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"

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
	if err := h.SSH.SetNewProfileEncryption(
		body.EncryptNew,
		configEncryptionProviders()...,
	); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, configstore.ErrProviderUnavailable) ||
			errors.Is(err, configstore.ErrAuthentication) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, err.Error(), status)
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
