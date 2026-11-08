// Package fsutil is the filesystem primitives several stores need, in one place
// so they cannot drift.
package fsutil

import (
	"os"
	"path/filepath"
	"time"
)

// WriteFileAtomic writes data to p by writing a sibling temporary file and
// renaming it into place, so a reader never sees a half-written file and a
// crash leaves either the old bytes or the new ones.
//
// The rename is retried, which is not belt-and-braces. On Windows,
// syscall.Open does not pass FILE_SHARE_DELETE, so renaming over a file that
// another goroutine holds open fails with a sharing violation instead of
// waiting. Both handles are transient — a reconciler re-reading a template
// while an agent saves one — so a brief retry turns it back into the atomic swap
// it is everywhere else. On POSIX the first attempt already succeeds.
//
// The file is created 0600 and chmodded before the rename, so it is never
// briefly readable by anyone else, and never arrives at its final name with the
// wrong mode.
func WriteFileAtomic(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return Retry(func() error { return os.Rename(tmpPath, p) })
}

// Retry runs op up to fsRetryAttempts times while it fails with a sharing
// violation, sleeping between attempts. Use it around the two operations that
// collide with a concurrent reader on Windows: rename and remove.
//
// A timeout is not a fix and this is not one: the retry exists because the
// window is bounded by another goroutine finishing a read, and 500ms is far
// longer than any read of a small config file. If a caller needs more than that,
// the file is not small and the caller has a different problem.
func Retry(op func() error) error {
	var err error
	for i := 0; i < fsRetryAttempts; i++ {
		if err = op(); err == nil {
			return nil
		}
		if !isSharingViolation(err) {
			return err
		}
		time.Sleep(fsRetryDelay)
	}
	return err
}

const (
	fsRetryAttempts = 20
	fsRetryDelay    = 25 * time.Millisecond
)
