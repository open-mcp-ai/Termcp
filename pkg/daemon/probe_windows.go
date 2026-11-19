//go:build windows

package daemon

import "golang.org/x/sys/windows"

const connRefused = windows.WSAECONNREFUSED
