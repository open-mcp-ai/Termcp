package configstore

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The directory lock is what keeps a CLI invocation and a running daemon from
// writing the same catalog at once. Its whole contract is "one holder, and a
// released lock is acquirable again" — those two sentences are the tests.

func TestDirectoryLockExcludesASecondHolder(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireDirectoryLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer func() { _ = first.Close() }()

	second, err := acquireDirectoryLock(dir)
	if err == nil {
		_ = second.Close()
		t.Fatal("a second holder acquired the same directory lock")
	}
}

func TestReleasedDirectoryLockIsAcquirableAgain(t *testing.T) {
	dir := t.TempDir()
	first, err := acquireDirectoryLock(dir)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := acquireDirectoryLock(dir)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func TestDirectoryLockCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireDirectoryLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	var nilLock *directoryLock
	if err := nilLock.Close(); err != nil {
		t.Fatalf("closing a nil lock: %v", err)
	}
}

func TestDirectoryLockFileIsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not report POSIX permission bits")
	}
	dir := t.TempDir()
	lock, err := acquireDirectoryLock(dir)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer func() { _ = lock.Close() }()

	info, err := os.Stat(filepath.Join(dir, ".config.lock"))
	if err != nil {
		t.Fatalf("stat the lock file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("lock file mode = %04o, want 0600", perm)
	}
}
