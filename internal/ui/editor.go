package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// editorPollInterval is how often a running editor is checked for having
// exited or stopped, and how often tower checks whether it is back in the
// terminal's foreground after a suspension.
const editorPollInterval = 20 * time.Millisecond

// EditorCommand is one editor invocation that gets the terminal (a
// tea.ExecCommand). It runs in its own process group, which tower hands the
// terminal's foreground to, so the editor and everything it starts can be
// stopped as a whole on shutdown while terminal job control keeps working:
//
//   - Normal use is not time limited, and nothing is signalled when the
//     editor exits by itself.
//   - Suspending the editor (ctrl+z) suspends tower like any job; once
//     tower is continued in the foreground, it hands the terminal back to
//     the editor and continues it.
//   - When the context ends, the group gets SIGTERM, and SIGKILL once the
//     editor exited or the stop delay passed. The editor is reaped only
//     after that, so its PID, and with it the process group ID, cannot be
//     reused by an unrelated process while the group is signalled.
//
// A process that deliberately leaves the group (a new session or process
// group, like a daemon) is not the editor invocation's anymore and is not
// stopped.
type EditorCommand struct {
	ctx       context.Context
	stopDelay time.Duration
	args      []string
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	// signalGroup sends sig to the process group pgid (tests observe it).
	signalGroup func(pgid int, sig syscall.Signal) error
}

func newEditorCommand(ctx context.Context, stopDelay time.Duration, name string, args ...string) *EditorCommand {
	return &EditorCommand{
		ctx:       ctx,
		stopDelay: stopDelay,
		args:      append([]string{name}, args...),
		signalGroup: func(pgid int, sig syscall.Signal) error {
			return syscall.Kill(-pgid, sig)
		},
	}
}

// Args returns the editor executable followed by its arguments.
func (c *EditorCommand) Args() []string { return slices.Clone(c.args) }

// SetStdin sets the editor's standard input.
func (c *EditorCommand) SetStdin(r io.Reader) { c.stdin = r }

// SetStdout sets the editor's standard output.
func (c *EditorCommand) SetStdout(w io.Writer) { c.stdout = w }

// SetStderr sets the editor's standard error.
func (c *EditorCommand) SetStderr(w io.Writer) { c.stderr = w }

// Run starts the editor and returns once it exited. Only terminal files are
// passed on; other streams are replaced by the null device, so no copying
// outlives the editor.
func (c *EditorCommand) Run() error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	// The context is not attached: on shutdown the whole process group is
	// stopped (see editorGroup.stop), not only the editor process.
	cmd := exec.Command(c.args[0], c.args[1:]...) //nolint:gosec,noctx // configured editor executable; paths are arguments
	if f, ok := c.stdin.(*os.File); ok {
		cmd.Stdin = f
	}
	if f, ok := c.stdout.(*os.File); ok {
		cmd.Stdout = f
	}
	if f, ok := c.stderr.(*os.File); ok {
		cmd.Stderr = f
	}
	tty := foregroundTerminal(cmd.Stdin)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if tty >= 0 {
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = tty
	}
	if err := cmd.Start(); err != nil {
		if tty >= 0 && !isForeground(tty) {
			// The child made its group the foreground job before its exec
			// failed (for example a script with a missing interpreter);
			// tower, the foreground job before the start, takes the
			// terminal back from that ended group.
			_ = setForeground(tty, syscall.Getpgrp())
		}
		return err
	}
	defer func() { _ = cmd.Process.Release() }()
	g := &editorGroup{pid: cmd.Process.Pid, tty: tty, signal: c.signalGroup}
	defer g.reclaimTerminal()
	return g.wait(c.ctx, c.stopDelay)
}

// foregroundTerminal returns the descriptor of stdin if it is tower's
// controlling terminal and tower is its foreground job, else -1.
func foregroundTerminal(stdin io.Reader) int {
	f, ok := stdin.(*os.File)
	if !ok {
		return -1
	}
	fd := int(f.Fd()) // #nosec G115 -- file descriptors fit into int
	if !isForeground(fd) {
		return -1
	}
	return fd
}

// isForeground reports whether tower's process group is the foreground job
// of the terminal fd.
func isForeground(fd int) bool {
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	return err == nil && pgrp == syscall.Getpgrp()
}

// setForeground makes pgid the foreground job of the terminal fd. SIGTTOU,
// which a background process changing the foreground job gets, is ignored
// for the call only.
func setForeground(fd, pgid int) error {
	signal.Ignore(syscall.SIGTTOU)
	defer signal.Reset(syscall.SIGTTOU)
	return unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, pgid)
}

// editorGroup is a started editor that leads its own process group (the
// group ID is the editor's PID). All signals go to the group and are sent
// only while the editor is not reaped yet.
type editorGroup struct {
	pid    int
	tty    int
	signal func(pgid int, sig syscall.Signal) error
}

// reclaimTerminal makes tower the terminal's foreground job again if the
// editor's group still is.
func (g *editorGroup) reclaimTerminal() {
	if g.tty < 0 {
		return
	}
	if pgrp, err := unix.IoctlGetInt(g.tty, unix.TIOCGPGRP); err == nil && pgrp == g.pid {
		_ = setForeground(g.tty, syscall.Getpgrp())
	}
}

// wait reaps the editor once it exited, handles its suspension and stops
// the group when ctx ends.
func (g *editorGroup) wait(ctx context.Context, stopDelay time.Duration) error {
	tick := time.NewTicker(editorPollInterval)
	defer tick.Stop()
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(g.pid, &ws, syscall.WNOHANG|syscall.WUNTRACED, nil)
		switch {
		case errors.Is(err, syscall.EINTR):
			continue
		case err != nil:
			return fmt.Errorf("wait for the editor: %w", err)
		case pid == g.pid && ws.Stopped():
			g.suspended(ctx)
			continue
		case pid == g.pid:
			return exitError(ws)
		}
		select {
		case <-ctx.Done():
			return g.stop(ctx, stopDelay)
		case <-tick.C:
		}
	}
}

// suspended follows the editor into the background: tower takes the
// terminal back and stops itself like the editor did (the stop is
// discarded when tower's process group has no job control, as with the
// editor before). Once tower is continued in the foreground, the editor
// gets the terminal back and is continued.
func (g *editorGroup) suspended(ctx context.Context) {
	if g.tty < 0 || ctx.Err() != nil {
		// Without a terminal there is nothing to hand over (whoever
		// stopped the editor continues it); on shutdown the group is
		// stopped instead.
		return
	}
	g.reclaimTerminal()
	_ = syscall.Kill(0, syscall.SIGTSTP)
	for !isForeground(g.tty) {
		if ctx.Err() != nil {
			return
		}
		// Continued in the background: wait until brought to the
		// foreground, as a job reading the terminal would.
		_ = syscall.Kill(0, syscall.SIGTTIN)
		time.Sleep(editorPollInterval)
	}
	_ = setForeground(g.tty, g.pid)
	_ = g.signal(g.pid, syscall.SIGCONT)
}

// stop ends the editor's process group: SIGTERM (and SIGCONT, so a
// suspended editor can act on it) first, SIGKILL once the editor exited or
// the delay passed, which also ends what the editor left behind or started
// while exiting. The editor is reaped last.
func (g *editorGroup) stop(ctx context.Context, delay time.Duration) error {
	err := g.signal(g.pid, syscall.SIGTERM)
	_ = g.signal(g.pid, syscall.SIGCONT)
	waitExited(g.pid, delay)
	if kerr := g.signal(g.pid, syscall.SIGKILL); err == nil && !errors.Is(kerr, syscall.ESRCH) {
		err = kerr
	}
	deadline := time.Now().Add(delay)
	for {
		var ws syscall.WaitStatus
		pid, werr := syscall.Wait4(g.pid, &ws, syscall.WNOHANG, nil)
		if errors.Is(werr, syscall.EINTR) {
			continue
		}
		if werr != nil || pid == g.pid || time.Now().After(deadline) {
			break
		}
		time.Sleep(editorPollInterval)
	}
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stop the editor: %w", err)
	}
	return ctx.Err()
}

// exitError describes an unsuccessful editor exit like exec does.
func exitError(ws syscall.WaitStatus) error {
	switch {
	case ws.Signaled():
		return fmt.Errorf("signal: %s", ws.Signal())
	case ws.ExitStatus() != 0:
		return fmt.Errorf("exit status %d", ws.ExitStatus())
	}
	return nil
}
