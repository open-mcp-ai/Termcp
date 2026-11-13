//go:build !windows

package auth

import "os"

func replaceLocalDaemonCredential(tempName, target string) error {
	return os.Rename(tempName, target)
}
