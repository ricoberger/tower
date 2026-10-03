package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/agent"
	"github.com/ricoberger/tower/internal/provider"
	"github.com/ricoberger/tower/internal/provider/alerts"
	"github.com/ricoberger/tower/internal/provider/tasks"
	"github.com/ricoberger/tower/internal/store"
)

func setup(t *testing.T) (*App, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var cfg Config
	cfg.Retention.Duration = time.Hour
	var ac alerts.Config
	ac.ReopenWindow.Duration = time.Hour
	t.Setenv("GRAFANA_INSTANCES", `{}`)
	ap, err := alerts.New(ac)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := tasks.New(tasks.Config{})
	if err != nil {
		t.Fatal(err)
	}
	providers := provider.Set{ap, mp}
	r, err := agent.New(agent.Config{RunCommand: "true {{.Prompt}} {{.SessionID}}", ResumeCommand: "true {{.SessionID}}"}, dir, "tower", s, providers)
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, s, r, providers, mp, slog.New(slog.NewTextHandler(io.Discard, nil))), s
}

func TestItemsMarksDeadAgentsFailed(t *testing.T) {
	ctx := context.Background()
	a, s := setup(t)
	if err := a.CreateTask(ctx, "\n\nFix it\nsoon\n"); err != nil {
		t.Fatal(err)
	}
	if err := a.CreateTask(ctx, " \n\n"); err != nil {
		t.Fatalf("empty task: %v", err)
	}
	items, _ := s.List(ctx)
	id := items[0].ID
	if len(items) != 1 || items[0].Source != tasks.Kind || items[0].Kind != tasks.Kind || items[0].Title != "Fix it" || items[0].Description != "soon" {
		t.Fatalf("items = %+v", items)
	}
	_ = s.Start(ctx, id, "sess", time.Now())
	_ = s.SetPID(ctx, id, "sess", 999_999_999)

	items, err := a.Items(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if it := items[0]; it.State != store.StateWaiting || !it.Failed {
		t.Fatalf("item = %+v", it)
	}

	// A live PID stays in progress.
	_ = s.Move(ctx, id, store.StateTodo, time.Now())
	_ = s.Start(ctx, id, "sess2", time.Now())
	_ = s.SetPID(ctx, id, "sess2", os.Getpid())
	items, _ = a.Items(ctx)
	if items[0].State != store.StateInProgress {
		t.Fatalf("item = %+v", items[0])
	}
}

func TestPendingStartsRecoverOnlyAtStartup(t *testing.T) {
	ctx := context.Background()
	a, s := setup(t)
	if err := a.CreateTask(ctx, "Task"); err != nil {
		t.Fatal(err)
	}
	items, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(ctx, items[0].ID, "pending", time.Now()); err != nil {
		t.Fatal(err)
	}
	items, err = a.Items(ctx)
	if err != nil || items[0].State != store.StateInProgress || items[0].Failed {
		t.Fatalf("pending start = %+v, err = %v", items, err)
	}
	if err := s.RecoverStarts(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	items, err = a.Items(ctx)
	if err != nil || items[0].State != store.StateWaiting || !items[0].Failed {
		t.Fatalf("recovered start = %+v, err = %v", items, err)
	}
}

func TestSync(t *testing.T) {
	ctx := context.Background()
	a, s := setup(t)
	a.sync(ctx, store.Update{Kind: alerts.Kind, Source: "dev", At: time.Now(), Items: []store.Incoming{{Key: "alert:dev:a"}}})
	// A failed poll changes nothing.
	a.sync(ctx, store.Update{Kind: alerts.Kind, Source: "dev", At: time.Now(), Err: errors.New("HTTP 500")})
	items, _ := s.List(ctx)
	if len(items) != 1 || items[0].State != store.StateTodo {
		t.Fatalf("items = %+v", items)
	}
}

func TestExpire(t *testing.T) {
	ctx := context.Background()
	a, s := setup(t)
	_ = a.CreateTask(ctx, "Task")
	items, _ := s.List(ctx)
	id := items[0].ID
	_ = s.Move(ctx, id, store.StateDone, time.Now().Add(-2*time.Hour))
	log := a.LogPath(id)
	_ = os.MkdirAll(filepath.Dir(log), 0o700)
	_ = os.WriteFile(log, []byte("x"), 0o600)
	a.expire(ctx)
	if items, _ := s.List(ctx); len(items) != 0 {
		t.Fatalf("items = %+v", items)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("log not removed: %v", err)
	}
}

func TestPollNow(t *testing.T) {
	a, _ := setup(t)
	a.PollNow()
	a.PollNow()
	for i, tr := range a.triggers {
		if len(tr) != 1 {
			t.Errorf("trigger %d has %d signals", i, len(tr))
		}
	}
}
