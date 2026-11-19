//go:build !windows

package daemon

import "syscall"

const connRefused = syscall.ECONNREFUSED
