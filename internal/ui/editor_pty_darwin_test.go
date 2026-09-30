package ui

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY opens a new pseudo terminal and returns its controlling side and
// the path of its terminal side.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no pseudo terminal: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	fd := int(master.Fd()) // #nosec G115 -- file descriptors fit into int
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		t.Fatal(err)
	}
	var name [128]byte
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); errno != 0 {
		t.Fatal(errno)
	}
	return master, string(name[:bytes.IndexByte(name[:], 0)])
}
