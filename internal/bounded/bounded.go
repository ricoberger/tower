// Package bounded runs short-lived external commands in their own process
// group under a time limit, with bounded cleanup of the group and its output
// pipes.
package bounded

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// DefaultTimeout is the execution limit when Options.Timeout is zero.
	DefaultTimeout = 10 * time.Second
	// DefaultWaitDelay bounds waiting for the process and its pipes after
	// it exited or was killed, when Options.WaitDelay is zero.
	DefaultWaitDelay = 500 * time.Millisecond
)

// ErrTimeout is returned (wrapped) when a command exceeded its limit.
var ErrTimeout = errors.New("timed out")

// Options configures one bounded command.
type Options struct {
	Timeout   time.Duration
	WaitDelay time.Duration
}

// Command prepares cmd to run in its own process group that is killed as a
// whole when ctx is done. The returned context bounds the command; callers
// must call cancel after Wait.
func Command(ctx context.Context, opts Options, name string, args ...string) (*exec.Cmd, context.Context, context.CancelFunc) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	waitDelay := opts.WaitDelay
	if waitDelay <= 0 {
		waitDelay = DefaultWaitDelay
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- callers pass fixed or validated executables; data is passed as arguments
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return KillGroup(cmd) }
	cmd.WaitDelay = waitDelay
	return cmd, ctx, cancel
}

// Run starts cmd (prepared by Command), waits for it and always removes its
// process group afterwards. The error names only the executable's base name,
// never its arguments.
func Run(ctx context.Context, cmd *exec.Cmd) error {
	name := filepath.Base(cmd.Path)
	if err := cmd.Start(); err != nil {
		// Path errors repeat the full path; only their cause is kept.
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return fmt.Errorf("start %s: %w", name, err)
	}
	err := cmd.Wait()
	// Descendants may outlive the process (for example holding an output
	// pipe); the group is always removed once the command ended.
	_ = KillGroup(cmd)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%s %w", name, ErrTimeout)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command succeeded but descendants kept its output open;
		// they were killed with the group above.
		return nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("%s failed: %s", name, ee.ProcessState)
		}
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

// KillGroup kills the process group of a started command.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
