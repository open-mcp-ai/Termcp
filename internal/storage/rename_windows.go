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
//
// removeAttempts and removeBackoff shape removeAllRetrying. Removing a tree waits for
// a different thing than publishing a manifest: Windows keeps a deleted file in the
// directory until its LAST handle closes (delete-on-close), so a log file that an
// in-flight /output-range read, an antivirus scan or the indexer still has open makes
// the unlink fail with "in use" and the parent rmdir fail with "directory is not
// empty". That hold outlasts a read - it lasts until the other party is done - so the
// removal budget is larger, and still bounded: 5, 10, 20, 40, 80, 160ms is ~315ms,
// well under the second an operator would wait on a delete request and far under any
// poll interval.
const (
	renameAttempts = 5
	renameBackoff  = 1 * time.Millisecond

	removeAttempts = 6
	removeBackoff  = 5 * time.Millisecond
)

// isSharingViolation reports whether a failed rename failed because the target
// file is open elsewhere, which is the one rename error that a retry can fix.
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

// isTemporaryLockFailure is the removal twin of isSharingViolation: the error that
// says "something still holds a piece of this tree", which is the one removal error a
// retry can fix.
//
// Three Windows spellings mean that, all of them produced by the same delete-on-close
// deferral:
//
//	error 5   ERROR_ACCESS_DENIED    - the file is open without share-delete.
//	error 32  ERROR_SHARING_VIOLATION - the file is held by another handle.
//	error 145 ERROR_DIR_NOT_EMPTY    - the directory still lists a file whose last
//	                                      handle has not closed yet, so the rmdir is
//	                                      refused even though the unlink succeeded.
//
// The third is what the tests and users see: the leaf file is gone from the caller's
// point of view, the parent directory refuses to empty, and the error names a
// directory rather than the file that caused it. A permission error, a read-only
// volume, a missing path and everything else fall through unchanged: retrying those
// only delays an error the caller has to see.
func isTemporaryLockFailure(err error) bool {
	return isSharingViolation(err) || errors.Is(err, windows.ERROR_DIR_NOT_EMPTY)
}
