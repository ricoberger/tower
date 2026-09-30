// Package ghostty hands a prepared Copilot session over to a new Ghostty
// surface through the external ghostty-new helper. The helper types the
// resume command into the surface's interactive shell, so every word of that
// command is single-quoted and control characters are rejected before
// anything is launched.
package ghostty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ricoberger/tower/internal/bounded"
)

const (
	// Timeout bounds one helper invocation. The helper returns once the
	// surface was created; it does not wait for the interactive session.
	Timeout = 30 * time.Second
	// WaitDelay bounds waiting for the helper's process and output pipes
	// after it exited or was killed.
	WaitDelay = 500 * time.Millisecond
)

// Placements accepted by the helper.
const (
	PlacementSplit  = "split"
	PlacementTab    = "tab"
	PlacementWindow = "window"
)

// ValidPlacement reports whether p is split, tab or window.
func ValidPlacement(p string) bool {
	switch p {
	case PlacementSplit, PlacementTab, PlacementWindow:
		return true
	}
	return false
}

// Request describes one resume handoff.
type Request struct {
	// Helper is the resolved ghostty.command executable.
	Helper string
	// Placement is split, tab or window; Direction is only passed for
	// split.
	Placement string
	Direction string
	// WorkingDir is the absolute item directory.
	WorkingDir string
	// Title is the item title (shown as "tower: <title>").
	Title string
	// Command is the resolved runs.command executable, SessionID the
	// session of the resumed run and ResumeArgs runs.resume_args.
	Command    string
	SessionID  string
	ResumeArgs []string
}

// quote encodes s as one POSIX single-quoted shell word. Inside single
// quotes no expansion (including zsh/bash history expansion) happens.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// checkWord rejects values that cannot be typed safely into a shell. The
// error names the component, never its value.
func checkWord(component, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", component)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character (such as a newline), which cannot be typed into the resume shell", component)
		}
	}
	return nil
}

// TypedCommand returns the command typed into the new surface's shell:
// "<command> --resume <session> <resume args...>", every word single-quoted.
func TypedCommand(command, sessionID string, resumeArgs []string) (string, error) {
	if command == "" {
		return "", errors.New("runs.command is empty")
	}
	if sessionID == "" {
		return "", errors.New("the run has no session ID")
	}
	if err := checkWord("runs.command", command); err != nil {
		return "", err
	}
	if err := checkWord("session ID", sessionID); err != nil {
		return "", err
	}
	words := []string{quote(command), quote("--resume"), quote(sessionID)}
	for i, a := range resumeArgs {
		if err := checkWord("runs.resume_args["+strconv.Itoa(i)+"]", a); err != nil {
			return "", err
		}
		words = append(words, quote(a))
	}
	return strings.Join(words, " "), nil
}

// cleanTitle replaces control characters of a title with spaces; the title
// is display text passed as a separate argument.
func cleanTitle(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// Args returns the helper arguments of a request.
func Args(r Request) ([]string, error) {
	if !ValidPlacement(r.Placement) {
		return nil, errors.New("placement must be one of split, tab, window")
	}
	if r.WorkingDir == "" {
		return nil, errors.New("the item directory is unknown")
	}
	if err := checkWord("the item directory", r.WorkingDir); err != nil {
		return nil, err
	}
	typed, err := TypedCommand(r.Command, r.SessionID, r.ResumeArgs)
	if err != nil {
		return nil, err
	}
	args := []string{"--placement", r.Placement}
	if r.Placement == PlacementSplit && r.Direction != "" {
		if err := checkWord("ghostty.direction", r.Direction); err != nil {
			return nil, err
		}
		args = append(args, "--direction", r.Direction)
	}
	args = append(args,
		"--working-dir", r.WorkingDir,
		"--title", "tower: "+cleanTitle(r.Title),
		"--command", typed,
	)
	return args, nil
}

// Launcher runs the helper.
type Launcher struct {
	// Env is the helper's environment (nil inherits the process
	// environment).
	Env []string
	// Timeout and WaitDelay override the defaults (tests).
	Timeout   time.Duration
	WaitDelay time.Duration
}

// Launch validates the request and runs the helper once. Nothing is started
// when validation fails. Errors never contain the typed command.
func (l *Launcher) Launch(ctx context.Context, r Request) error {
	if r.Helper == "" {
		return errors.New("ghostty.command is empty")
	}
	args, err := Args(r)
	if err != nil {
		return err
	}
	timeout, waitDelay := l.Timeout, l.WaitDelay
	if timeout <= 0 {
		timeout = Timeout
	}
	if waitDelay <= 0 {
		waitDelay = WaitDelay
	}
	cmd, cctx, cancel := bounded.Command(ctx, bounded.Options{Timeout: timeout, WaitDelay: waitDelay}, r.Helper, args...)
	defer cancel()
	cmd.Env = l.Env
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := bounded.Run(cctx, cmd); err != nil {
		return fmt.Errorf("ghostty.command: %w", err)
	}
	return nil
}
