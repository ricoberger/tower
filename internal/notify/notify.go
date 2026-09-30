// Package notify delivers macOS desktop notifications for finished
// preparation runs through terminal-notifier or, when it is not installed,
// osascript. Content is always passed as process arguments, never as code.
package notify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ricoberger/tower/internal/item"
)

const (
	// Timeout is the execution limit of one delivery.
	Timeout = 10 * time.Second
	// WaitDelay bounds waiting for the delivery process and its output
	// pipes after it exited or was killed.
	WaitDelay = 500 * time.Millisecond
	// MaxMessage is the maximum message length in characters.
	MaxMessage = 200
)

// ErrTimeout is returned (wrapped) when a delivery exceeded its limit.
var ErrTimeout = errors.New("notification delivery timed out")

// Titles by outcome.
const (
	TitleReady   = "tower — report ready"
	TitleBlocked = "tower — blocked"
	TitleFailed  = "tower — run failed"
)

// Notification is the content of one notification.
type Notification struct {
	// ItemID is the validated item ID (used for -group and the resume
	// target).
	ItemID string
	// Outcome is ready, blocked or failed.
	Outcome item.Outcome
	// Subtitle is the item title.
	Subtitle string
	// Message is result.summary, gate.question or the run error.
	Message string
}

// Title returns the title of the notification.
func (n Notification) Title() string {
	switch n.Outcome {
	case item.OutcomeReady:
		return TitleReady
	case item.OutcomeBlocked:
		return TitleBlocked
	default:
		return TitleFailed
	}
}

// Notifier delivers notifications.
type Notifier struct {
	// Sound is the notification sound; empty disables sound.
	Sound string
	// ConfigPath is the resolved absolute path of the active configuration.
	ConfigPath string
	// Executable returns the path of the running tower binary
	// (os.Executable).
	Executable func() (string, error)
	// LookPath finds delivery executables (exec.LookPath).
	LookPath func(string) (string, error)
	// Timeout is the execution limit of one delivery (Timeout).
	Timeout time.Duration
	// WaitDelay is exec.Cmd.WaitDelay of a delivery (WaitDelay).
	WaitDelay time.Duration
}

// Truncate limits s to max characters without splitting UTF-8 sequences,
// marking a cut with an ellipsis. Invalid UTF-8 is replaced.
func Truncate(s string, max int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}

// ShellQuote quotes s as a single POSIX shell word.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ResumeCommand returns the -execute command of terminal-notifier. The
// binary and configuration paths must be absolute.
func ResumeCommand(executable, config, itemID string) (string, error) {
	if !filepath.IsAbs(executable) {
		return "", errors.New("cannot resolve the absolute tower executable")
	}
	if !filepath.IsAbs(config) {
		return "", errors.New("cannot resolve the absolute configuration path")
	}
	if _, _, err := item.ParseID(itemID); err != nil {
		return "", fmt.Errorf("invalid item ID: %w", err)
	}
	return ShellQuote(executable) + " --config " + ShellQuote(config) + " resume " + itemID, nil
}

// Command returns the delivery executable and its arguments. terminal-notifier
// is preferred when found; otherwise osascript is used.
func (n *Notifier) Command(x Notification) (string, []string, error) {
	lookPath := n.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	message := Truncate(x.Message, MaxMessage)
	if message == "" {
		message = " " // terminal-notifier reads stdin without a message
	}
	subtitle := strings.ToValidUTF8(x.Subtitle, "\uFFFD")

	if path, err := lookPath("terminal-notifier"); err == nil {
		exe := n.Executable
		if exe == nil {
			return "", nil, errors.New("cannot resolve the tower executable")
		}
		bin, err := exe()
		if err != nil {
			return "", nil, fmt.Errorf("cannot resolve the tower executable: %w", err)
		}
		execute, err := ResumeCommand(bin, n.ConfigPath, x.ItemID)
		if err != nil {
			return "", nil, err
		}
		args := []string{"-title", x.Title(), "-subtitle", subtitle, "-message", message}
		if n.Sound != "" {
			args = append(args, "-sound", n.Sound)
		}
		args = append(args, "-group", x.ItemID, "-execute", execute)
		return path, args, nil
	}

	path, err := lookPath("osascript")
	if err != nil {
		return "", nil, errors.New("neither terminal-notifier nor osascript found")
	}
	// The content is passed as run handler arguments, so it is AppleScript
	// data and needs no escaping.
	display := "display notification (item 1 of argv) with title (item 2 of argv) subtitle (item 3 of argv)"
	rest := []string{message, x.Title(), subtitle}
	if n.Sound != "" {
		display += " sound name (item 4 of argv)"
		rest = append(rest, n.Sound)
	}
	args := append([]string{"-e", "on run argv", "-e", display, "-e", "end run", "--"}, rest...)
	return path, args, nil
}

// Deliver shows one notification. It blocks for at most the timeout plus the
// bounded WaitDelay cleanup; callers run it off the engine loop.
func (n *Notifier) Deliver(ctx context.Context, x Notification) error {
	path, args, err := n.Command(x)
	if err != nil {
		return err
	}
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = Timeout
	}
	waitDelay := n.WaitDelay
	if waitDelay <= 0 {
		waitDelay = WaitDelay
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, args...) // #nosec G204 -- fixed executables, content passed as arguments
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = waitDelay
	// Output is discarded through pipes; WaitDelay bounds waiting for
	// descendants that keep them open.
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", filepath.Base(path), err)
	}
	err = cmd.Wait()
	// Descendants may outlive the delivery process (for example holding its
	// output pipe); the group is always removed once the command ended.
	_ = killGroup(cmd)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w after %s (%s)", ErrTimeout, timeout, filepath.Base(path))
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
			return fmt.Errorf("%s failed: %s", filepath.Base(path), ee.ProcessState)
		}
		return fmt.Errorf("%s failed: %w", filepath.Base(path), err)
	}
	return nil
}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
