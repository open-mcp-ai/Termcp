package keys

import "os"

// renameOver replaces dst with src atomically on POSIX; on Windows it is a
// plain rename. A partial key file must never exist at dst.
func renameOver(src, dst string) error {
	return os.Rename(src, dst)
}
