//go:build unix && !darwin && !linux

package ui

import (
	"os"
	"testing"
)

// openPTY skips the test: pseudo terminals are only set up on macOS and
// Linux.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	t.Skip("no pseudo terminal support")
	return nil, ""
}
