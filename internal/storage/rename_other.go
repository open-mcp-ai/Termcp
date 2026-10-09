//go:build !windows

package storage

// renameAttempts and renameBackoff configure renameOver's retry loop. On POSIX the
// retry exists but can never trigger: renaming over an open file succeeds, so the
// first attempt returns and nothing is ever waited out. The single attempt and the
// zero backoff are what guarantee that - a shared budget here would add latency to
// every manifest write on the two platforms that never needed it.
const (
	renameAttempts = 1
	renameBackoff  = 0
)

// isSharingViolation is always false on POSIX: there is no "the target is busy"
// rename failure to recover from, so no error is retried and a real one (a missing
// directory, a full disk) reaches the caller immediately.
func isSharingViolation(error) bool { return false }
