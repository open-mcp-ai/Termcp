//go:build !windows

package storage

import (
	"errors"
	"syscall"
)

// renameAttempts and renameBackoff configure the bounded retry shared by renameOver
// and removeAllRetrying. On POSIX the retry exists but can never trigger: renaming over
// an open file and unlinking one succeed, so the first attempt returns and nothing is
// ever waited out. The single attempt and the zero backoff are what guarantee that - a
// shared budget here would add latency to every write on the two platforms that never
// needed it.
const (
	renameAttempts = 1
	renameBackoff  = 0
)

// isSharingViolation is always false on POSIX: there is no "the target is busy"
// failure to recover from, so no error is retried and a real one (a missing
// directory, a full disk) reaches the caller immediately.
func isSharingViolation(error) bool { return false }

// isTemporaryLockFailure names the one removal error POSIX can report for a file that
// is still open elsewhere: NFS replaces it with a silly-rename entry (.nfsXXXX) that
// outlives the unlink, so the parent's rmdir sees ENOTEMPTY and the tree is not gone
// yet. Retrying is correct there for the same reason it is on Windows - the entry
// disappears when the last handle closes - and no other error qualifies.
func isTemporaryLockFailure(err error) bool {
	return errors.Is(err, syscall.ENOTEMPTY)
}
