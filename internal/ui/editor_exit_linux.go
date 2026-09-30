package ui

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// waitExited waits up to timeout for the child pid to exit without reaping
// it and reports whether it exited.
func waitExited(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil:
			time.Sleep(time.Until(deadline))
			return false
		case info.Signo != 0:
			return true
		case !time.Now().Before(deadline):
			return false
		}
		time.Sleep(min(editorPollInterval, time.Until(deadline)))
	}
}
