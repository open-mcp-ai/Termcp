package daemon

import (
	"errors"

	"github.com/open-mcp-ai/termcp/pkg/detach"
	"golang.org/x/sys/unix"
)

// PID 1 in a container need not reap detached children. A pidfd becomes
// readable on exit even for those zombies. On older kernels use the more
// conservative kill(pid, 0) test; never classify an uncertain PID as exited.
func processExited(pid int) bool {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return errors.Is(err, unix.ESRCH) || !detach.ProcessAlive(pid)
	}
	defer unix.Close(fd)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	_, err = unix.Poll(fds, 0)
	return err == nil && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0
}
