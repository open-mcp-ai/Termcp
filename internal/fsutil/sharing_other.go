//go:build !windows

package fsutil

// isSharingViolation is always false off Windows: unlinking a file another
// process holds open is legal there, so there is nothing to wait for.
func isSharingViolation(error) bool { return false }
