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

// requireRealDirectory refuses a directory that is itself a symbolic link: a
// write through one would land where this store never authenticated the bytes.
//
// Lstat is the whole test. A link's own mode has no directory bit, so it fails
// the IsDir check above. Resolving the path and comparing it as a string — what
// this used to do — rejects ordinary directories: Windows spells one directory
// several ways (8.3 short names, letter case), and %TEMP% is handed out in
// short form, so every write under it "traversed a symbolic link".
//
// ponytail: only the last component is checked. Link or junction a directory
// above it and the write follows; whoever can write inside a 0700 store can
// swap the keyring instead, so that attacker is already past this gate.
func requireRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a real directory", ErrFormat, path)
	}
	return nil
}
