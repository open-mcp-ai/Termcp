//go:build windows

package fsutil

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isSharingViolation names the errors that mean "something still holds this
// file open without share-delete", which is the one class a retry can fix.
//
// error 5 and error 32 are the same condition reported at different layers: the
// delete-on-close deferral is still pending, so the rename or unlink is refused.
func isSharingViolation(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
