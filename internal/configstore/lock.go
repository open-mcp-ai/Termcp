package configstore

import (
	"fmt"
	"os"
	"path/filepath"
)

type directoryLock struct {
	file *os.File
}

func acquireDirectoryLock(dataDir string) (*directoryLock, error) {
	path := filepath.Join(dataDir, ".config.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, privateFileMode)
	if err != nil {
		return nil, fmt.Errorf("open config store lock: %w", err)
	}
	if err := lockExclusive(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock config store: %w", err)
	}
	return &directoryLock{file: file}, nil
}

func (lock *directoryLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unlockExclusive(lock.file)
	closeErr := lock.file.Close()
	lock.file = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
