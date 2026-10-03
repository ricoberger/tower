package store

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Lock takes an exclusive, non-blocking lock on path so only one TUI runs per
// state directory. The lock is released when the returned file is closed or the
// process exits.
func Lock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // path comes from the user's config
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil { //nolint:gosec // fd fits in int
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another tower is already running (lock %s)", path)
		}
		return nil, err
	}
	return f, nil
}
