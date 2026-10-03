// Package app wires the store, the providers and the agent together and
// implements the UI backend.
package app

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/ricoberger/tower/internal/agent"
	"github.com/ricoberger/tower/internal/config/helpers"
	"github.com/ricoberger/tower/internal/provider"
	"github.com/ricoberger/tower/internal/provider/tasks"
	"github.com/ricoberger/tower/internal/store"
)

// Config configures the app.
type Config struct {
	// Retention is how long DONE items are kept.
	Retention helpers.Duration `yaml:"retention"`
}

// App is the running tower.
type App struct {
	cfg       Config
	store     *store.Store
	agent     *agent.Agent
	providers provider.Set
	// tasks creates the tasks written in the UI.
	tasks *tasks.Provider
	log   *slog.Logger
	// triggers has one poll-now channel per provider.
	triggers []chan struct{}
}

// New creates the app.
func New(cfg Config, s *store.Store, ag *agent.Agent, providers provider.Set, tasks *tasks.Provider, log *slog.Logger) *App {
	a := &App{cfg: cfg, store: s, agent: ag, providers: providers, tasks: tasks, log: log}
	for range providers {
		a.triggers = append(a.triggers, make(chan struct{}, 1))
	}
	return a
}

// Run polls the sources, syncs their results into the store and deletes
// expired items until ctx is done.
func (a *App) Run(ctx context.Context) {
	updates := make(chan store.Update)
	for i, p := range a.providers {
		go p.Poll(ctx, a.triggers[i], updates)
	}
	a.expire(ctx)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.expire(ctx)
		case u := <-updates:
			a.sync(ctx, u)
		}
	}
}

func (a *App) sync(ctx context.Context, u store.Update) {
	err := u.Err
	if err == nil {
		var added []int64
		added, err = a.store.Sync(ctx, u)
		if len(added) > 0 {
			a.log.Info("new items", "kind", u.Kind, "source", u.Source, "count", len(added))
		}
	}
	if err == nil {
		return
	}
	a.log.Warn("poll failed", "kind", u.Kind, "source", u.Source, "err", err)
}

func (a *App) expire(ctx context.Context) {
	ids, err := a.store.DeleteDone(ctx, time.Now().Add(-a.cfg.Retention.Duration))
	if err != nil {
		a.log.Warn("retention", "err", err)
		return
	}
	for _, id := range ids {
		_ = os.Remove(a.LogPath(id))
	}
}

// Items returns all items. IN PROGRESS items whose wrapper process is gone
// without finishing are marked as failed (→ WAITING).
func (a *App) Items(ctx context.Context) ([]store.Item, error) {
	items, err := a.store.List(ctx)
	if err != nil {
		return nil, err
	}
	changed := false
	for _, it := range items {
		if it.State == store.StateInProgress && it.PID > 0 && !a.agent.Alive(it.PID) {
			if _, err := a.store.Recover(ctx, it.ID, it.SessionID, it.PID, time.Now()); err != nil {
				return nil, err
			}
			changed = true
		}
	}
	if changed {
		return a.store.List(ctx)
	}
	return items, nil
}

// Start starts an agent for a TO DO item.
func (a *App) Start(ctx context.Context, it store.Item) error {
	return a.agent.Start(ctx, it)
}

// Resume opens the agent session of an item.
func (a *App) Resume(ctx context.Context, it store.Item) error {
	return a.agent.Resume(ctx, it)
}

// Move changes the state of an item.
func (a *App) Move(ctx context.Context, id int64, to store.State) error {
	return a.store.Move(ctx, id, to, time.Now())
}

// CreateTask adds a task from the edited text. Empty text creates nothing.
func (a *App) CreateTask(ctx context.Context, text string) error {
	it, ok := a.tasks.NewItem(text, time.Now())
	if !ok {
		return nil
	}
	_, err := a.store.Create(ctx, it, it.CreatedAt)
	return err
}

// PollNow polls all sources without waiting for the interval.
func (a *App) PollNow() {
	for _, t := range a.triggers {
		select {
		case t <- struct{}{}:
		default:
		}
	}
}

// Open opens an item's URL; items without one are ignored.
func (a *App) Open(ctx context.Context, it store.Item) {
	if it.URL != "" {
		_ = exec.CommandContext(ctx, "open", it.URL).Run() // #nosec G204 -- URL passed as one argument
	}
}

// LogPath returns the run log of an item.
func (a *App) LogPath(id int64) string {
	return a.agent.LogPath(id)
}
