// Package agent starts agents for items, tracks them until they finish and
// resumes their sessions.
package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"uuid"

	"github.com/ricoberger/tower/internal/provider"
	"github.com/ricoberger/tower/internal/store"
)

// Config configures the agent commands. Commands are split into arguments
// with shell-like quoting and run without a shell; environment variables are
// not expanded.
type Config struct {
	// RunCommand starts an agent for an item (TO DO → IN PROGRESS).
	RunCommand string `yaml:"run_command"`
	// ResumeCommand opens an existing session of a TO DO, WAITING or DONE item.
	ResumeCommand string `yaml:"resume_command"`
}

// Agent starts agents for items, tracks their runs and resumes their sessions.
type Agent struct {
	stateDir string
	// exe is the tower binary that runs the hidden exec wrapper.
	exe string
	// run and resume are the parsed commands.
	run       *command
	resume    *command
	store     *store.Store
	providers provider.Set
}

// New creates the agent. exe is the path of the tower binary.
func New(cfg Config, stateDir, exe string, store *store.Store, providers provider.Set) (*Agent, error) {
	run, err := parseCommand(cfg.RunCommand)
	if err != nil {
		return nil, fmt.Errorf("parse run_command: %w", err)
	}

	resume, err := parseCommand(cfg.ResumeCommand)
	if err != nil {
		return nil, fmt.Errorf("parse resume_command: %w", err)
	}

	return &Agent{
		stateDir:  stateDir,
		exe:       exe,
		run:       run,
		resume:    resume,
		store:     store,
		providers: providers,
	}, nil
}

// LogPath returns the log file of an item's runs.
func (a *Agent) LogPath(id int64) string {
	return filepath.Join(a.stateDir, "runs", strconv.FormatInt(id, 10)+".log")
}

// Prompt renders the prompt of an item with its provider.
func (a *Agent) Prompt(it store.Item) (string, error) {
	p, err := a.providers.Get(it.Kind)
	if err != nil {
		return "", err
	}
	return p.Prompt(it)
}

// Start moves a TO DO item to IN PROGRESS and runs run_command for it in a
// detached `tower exec` wrapper, which moves the item to WAITING when the
// command exits. The agent keeps running when tower quits.
func (a *Agent) Start(ctx context.Context, it store.Item) error {
	prompt, err := a.Prompt(it)
	if err != nil {
		return err
	}
	sid := uuid.NewV4().String()
	argv, err := a.run.render(commandData{ID: it.ID, Title: it.Title, SessionID: sid, Prompt: prompt})
	if err != nil {
		return fmt.Errorf("run_command: %w", err)
	}

	path := a.LogPath(it.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = logf.Close() }()
	_, _ = fmt.Fprintf(logf, "==> %s session %s: %s\n", time.Now().Format(time.RFC3339), sid, argv[0])

	if err := a.store.Start(ctx, it.ID, sid, time.Now()); err != nil {
		return err
	}
	args := append([]string{"exec", "--state-dir", a.stateDir, "--item", strconv.FormatInt(it.ID, 10), "--session", sid, "--title", it.Title, "--"}, argv...)
	// Not bound to ctx: the agent must outlive the TUI.
	cmd := exec.CommandContext(context.Background(), a.exe, args...) // #nosec G204 -- tower itself with the configured run_command
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = a.store.Abort(ctx, it.ID, sid, time.Now())
		return fmt.Errorf("start agent: %w", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	if err := a.store.SetPID(ctx, it.ID, sid, pid); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// Resume runs resume_command for an item with a session and returns once it
// exited.
func (a *Agent) Resume(ctx context.Context, it store.Item) error {
	if it.SessionID == "" {
		return errors.New("item has no session")
	}
	argv, err := a.resume.render(commandData{ID: it.ID, Title: it.Title, SessionID: it.SessionID})
	if err != nil {
		return fmt.Errorf("resume_command: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204 -- configured resume_command
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// A launcher may leave descendants holding its output pipes open.
	cmd.WaitDelay = 250 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(out.String()); msg != "" {
			if len(msg) > 300 {
				msg = msg[:300] + "…"
			}
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// Alive reports whether the wrapper process with pid exists. It never sends a
// signal.
func (a *Agent) Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
