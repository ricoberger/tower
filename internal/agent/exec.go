package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/ricoberger/tower/internal/store"
)

// Exec is the hidden `tower exec` wrapper: it claims the session, runs argv,
// records failure status, and moves the item to WAITING if it is still IN
// PROGRESS in this session. The TUI notifies the user about the transition,
// because only it writes to a terminal. Output and the exit code go to the
// run log. Exec returns the command's exit code, or 127 if the session could
// not be claimed.
func Exec(store *store.Store, id int64, sessionID string, argv []string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err := store.SetPID(ctx, id, sessionID, os.Getpid())
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "tower exec: claim session: %v\n", err)
		return 127
	}

	code := runExec(argv)
	fmt.Fprintf(os.Stderr, "==> %s exit code %d\n", time.Now().Format(time.RFC3339), code)

	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := store.Finish(ctx, id, sessionID, code != 0, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "tower exec: %v\n", err)
	}
	return code
}

// runExec runs argv with stdin from /dev/null, forwards SIGINT/SIGTERM/SIGHUP
// and returns its exit code (127 if it could not be started, 128+n if it was
// killed by signal n).
func runExec(argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "tower exec: no command")
		return 127
	}
	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...) // #nosec G204 -- configured run_command
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "tower exec: %v\n", err)
		return 127
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case sig := <-sigs:
			_ = cmd.Process.Signal(sig)
		case err := <-done:
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					return 128 + int(ws.Signal())
				}
				return ee.ExitCode()
			}
			if err != nil {
				return 1
			}
			return 0
		}
	}
}
