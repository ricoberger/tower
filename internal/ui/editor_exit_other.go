//go:build unix && !darwin && !linux

package ui

import "time"

// waitExited waits for the stop delay: without a way to observe the exit
// of the child pid without reaping it, it reports that it did not exit.
func waitExited(_ int, timeout time.Duration) bool {
	time.Sleep(timeout)
	return false
}
