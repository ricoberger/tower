package ui

import (
	"errors"
	"syscall"
	"time"
)

// waitExited waits up to timeout for the child pid to exit without reaping
// it and reports whether it exited.
func waitExited(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	kq, err := syscall.Kqueue()
	if err != nil {
		time.Sleep(timeout)
		return false
	}
	defer func() { _ = syscall.Close(kq) }()
	var ev syscall.Kevent_t
	syscall.SetKevent(&ev, pid, syscall.EVFILT_PROC, syscall.EV_ADD|syscall.EV_ONESHOT)
	ev.Fflags = syscall.NOTE_EXIT
	changes := []syscall.Kevent_t{ev}
	out := make([]syscall.Kevent_t, 1)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		ts := syscall.NsecToTimespec(remaining.Nanoseconds())
		n, err := syscall.Kevent(kq, changes, out, &ts)
		changes = nil
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.ESRCH):
			// The unreaped child can no longer be watched: it exited.
			return true
		case err != nil:
			time.Sleep(time.Until(deadline))
			return false
		case n == 0:
			return false
		case out[0].Flags&syscall.EV_ERROR != 0:
			if syscall.Errno(out[0].Data) == syscall.ESRCH { // #nosec G115 -- Data holds an errno here
				return true
			}
			time.Sleep(time.Until(deadline))
			return false
		}
		return true
	}
}
