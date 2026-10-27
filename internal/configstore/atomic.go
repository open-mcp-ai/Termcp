package configstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	privateDirMode  = 0o700
	privateFileMode = 0o600
)

// writeFileAtomic publishes data only after the complete replacement has been
// written and synced in the destination directory.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, privateDirMode); err != nil {
		return fmt.Errorf("create config store directory: %w", err)
	}
	if err := requireRealDirectory(dir); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".termcp-config-*")
	if err != nil {
		return fmt.Errorf("create temporary config file: %w", err)
	}
	tmpPath := tmp.Name()
	published := false
	defer func() {
		_ = tmp.Close()
		if !published {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(privateFileMode); err != nil {
		return fmt.Errorf("set temporary config permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary config file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary config file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("publish config file: %w", err)
	}
	published = true
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("%w: the new file is visible but directory sync failed: %v", ErrDurability, err)
	}
	return nil
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func requireRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrFormat
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if filepath.Clean(resolved) != filepath.Clean(path) {
		return fmt.Errorf("%w: directory path traverses a symbolic link", ErrFormat)
	}
	return nil
}
