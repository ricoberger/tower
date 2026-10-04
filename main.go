// Command tower is a personal work board: alerts, pull requests, Jira tickets
// and tasks flow into TO DO, agents work on them in IN PROGRESS, finished
// sessions wait for the user in WAITING and resolved work ends in DONE.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/alecthomas/kong"

	"github.com/ricoberger/tower/internal/agent"
	"github.com/ricoberger/tower/internal/app"
	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/notify"
	"github.com/ricoberger/tower/internal/provider"
	"github.com/ricoberger/tower/internal/provider/alerts"
	"github.com/ricoberger/tower/internal/provider/jira"
	"github.com/ricoberger/tower/internal/provider/pullrequests"
	"github.com/ricoberger/tower/internal/provider/tasks"
	"github.com/ricoberger/tower/internal/store"
	"github.com/ricoberger/tower/internal/ui"
)

type CLI struct {
	TUI  TUICmd  `cmd:"" default:"withargs"`
	Exec ExecCmd `cmd:"" hidden:""`
}

func main() {
	ctx := kong.Parse(&CLI{})
	ctx.FatalIfErrorf(ctx.Run())
}

type TUICmd struct {
	Config string `default:"~/.config/tower/config.yaml" type:"path" help:"Configuration file."`
}

func (c *TUICmd) Run() error {
	cfg, err := config.Load(c.Config)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", cfg.StateDir, err)
	}

	lock, err := store.Lock(filepath.Join(cfg.StateDir, "tower.lock"))
	if err != nil {
		return fmt.Errorf("store lock %s: %w", filepath.Join(cfg.StateDir, "tower.lock"), err)
	}
	defer func() { _ = lock.Close() }()

	logf, err := os.OpenFile(filepath.Join(cfg.StateDir, "tower.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", filepath.Join(cfg.StateDir, "tower.log"), err)
	}
	defer func() { _ = logf.Close() }()
	log := slog.New(slog.NewTextHandler(logf, nil))

	store, err := store.New(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer func() { _ = store.Close() }()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("executable: %w", err)
	}

	alertsProvider, err := alerts.New(cfg.Providers.Alerts)
	if err != nil {
		return fmt.Errorf("alerts provider: %w", err)
	}
	tasksProvider, err := tasks.New(cfg.Providers.Tasks)
	if err != nil {
		return fmt.Errorf("tasks provider: %w", err)
	}
	pullRequestsProvider, err := pullrequests.New(cfg.Providers.PullRequests)
	if err != nil {
		return fmt.Errorf("pull requests provider: %w", err)
	}
	jiraProvider, err := jira.New(cfg.Providers.Jira)
	if err != nil {
		return fmt.Errorf("jira provider: %w", err)
	}
	providers := provider.Set{alertsProvider, pullRequestsProvider, jiraProvider, tasksProvider}

	agent, err := agent.New(cfg.Agent, cfg.StateDir, exe, store, providers)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer cancel()

	if err := store.RecoverStarts(ctx, time.Now()); err != nil {
		return fmt.Errorf("recover starts: %w", err)
	}

	app := app.New(cfg.App, store, agent, providers, tasksProvider, log)
	go app.Run(ctx)

	p := tea.NewProgram(ui.New(ctx, app, log), tea.WithContext(ctx))
	_, err = p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		err = nil
	}
	return err
}

type ExecCmd struct {
	StateDir string   `required:"" type:"path" help:"State directory."`
	Item     int64    `required:"" help:"Item id."`
	Session  string   `required:"" help:"Session id."`
	Title    string   `help:"Item title."`
	Command  []string `arg:"" help:"Command and arguments."`
}

func (c *ExecCmd) Run() error {
	store, err := store.New(c.StateDir)
	if err != nil {
		return err
	}

	code := agent.Exec(notify.New(), store, c.Item, c.Session, c.Title, c.Command)
	store.Close()

	// Keep the command's exit code; FatalIfErrorf would always exit 1.
	os.Exit(code)
	return nil
}
