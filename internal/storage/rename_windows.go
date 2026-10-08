//go:build windows

package storage

import (
	"errors"
	"time"

	"golang.org/x/sys/windows"
)

// renameAttempts and renameBackoff shape the retry loop in renameOver. Five
// attempts back off 1, 2, 4, 8 and 16ms - 31ms of waiting in total, which is
// longer than a local reader holds a manifest open (a single ReadFile) and far
// below the interval any caller polls at.
const (
	renameAttempts = 5
	renameBackoff  = 1 * time.Millisecond
)

// isSharingViolation reports whether a failed rename failed because the target
// file is open elsewhere, which is the one rename error that a retry can fix.
//
// Both Windows spellings of "the target is held" are caught, and nothing else:
//
//	error 5  ERROR_ACCESS_DENIED        - MoveFileEx was refused the target.
//	error 32 ERROR_SHARING_VIOLATION   - the target is open without share-delete.
//
// A permission error on the directory, a missing file, a full disk and every
// other failure fall through unchanged: retrying those would only delay the
// error without ever succeeding, and the caller has to see it either way.
func isSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
