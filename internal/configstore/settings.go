package configstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

const maxSettingsFile = 4 << 10

type storageSettings struct {
	Version    uint16 `json:"version"`
	EncryptNew bool   `json:"encrypt_new"`
}

// LoadEncryptNew reports the persisted new-profile encryption preference. A
// missing settings file means the default: plaintext.
func LoadEncryptNew(dataDir string) (bool, error) {
	root, err := filepath.Abs(filepath.Clean(dataDir))
	if err != nil {
		return false, err
	}
	root, err = filepath.EvalSymlinks(root)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	path := filepath.Join(root, "keys", "config.settings")
	data, err := readBoundedFile(path, maxSettingsFile)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer clear(data)
	var settings storageSettings
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&settings); err != nil {
		return false, ErrFormat
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return false, ErrFormat
	}
	if settings.Version != repositoryVersion {
		return false, ErrVersion
	}
	return settings.EncryptNew, nil
}

// SaveEncryptNew persists whether newly created persistent profiles should be
// encrypted. Existing profiles keep their current storage mode.
func SaveEncryptNew(dataDir string, enabled bool) error {
	root, err := prepareDataDir(dataDir)
	if err != nil {
		return err
	}
	data, err := json.Marshal(storageSettings{
		Version:    repositoryVersion,
		EncryptNew: enabled,
	})
	if err != nil {
		return err
	}
	defer clear(data)
	return writeFileAtomic(filepath.Join(root, "keys", "config.settings"), data)
}
