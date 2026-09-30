package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ricoberger/tower/internal/bounded"
	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/ui"
)

const (
	// openCommand opens URLs in the default browser (macOS).
	openCommand = "/usr/bin/open"
	// openTimeout bounds one browser launch; open returns once the URL was
	// handed to the browser.
	openTimeout = 10 * time.Second
)

// runProgram runs the Bubble Tea program. Quitting through a signal or a
// cancelled context is a normal exit.
func runProgram(ctx context.Context, m tea.Model) error {
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	if errors.Is(err, tea.ErrInterrupted) || (errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil) {
		return nil
	}
	return err
}

// openBrowser opens an http(s) URL with macOS open. The URL is a separate
// argument, never shell source.
func openBrowser(ctx context.Context, rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("only http(s) URLs are opened")
	}
	cmd, cctx, cancel := bounded.Command(ctx, bounded.Options{Timeout: openTimeout}, openCommand, rawURL)
	defer cancel()
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := bounded.Run(cctx, cmd); err != nil {
		return fmt.Errorf("open the browser: %w", err)
	}
	return nil
}

// runTUI runs the engine and the terminal UI. The engine keeps running while
// the UI waits for input or an editor; quitting the UI stops the engine
// (detached preparation runs continue).
func runTUI(ctx context.Context, cfg *config.Config, level slog.Level, stderr io.Writer, e env) int {
	engineCtx, stopEngine := context.WithCancel(ctx)
	defer stopEngine()
	feed := ui.NewFeed()
	api := newEngineAPI()
	started := make(chan struct{})
	engineErr := make(chan error, 1)
	opts := engineOptions{
		cfg:      cfg,
		level:    level,
		stderr:   io.Discard, // the screen belongs to the UI; tower.log has everything
		now:      e.now,
		lookPath: e.lookPath,
		deliver:  e.deliver,
		api:      api,
		feed:     feed,
		started:  func() { close(started) },
		mode:     "tui",
	}
	if e.engineOptions != nil {
		e.engineOptions(&opts)
	}
	go func() { engineErr <- runEngine(engineCtx, opts) }()
	select {
	case <-started:
	case err := <-engineErr:
		if err == nil {
			return 0
		}
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	// The UI's own store handle reads artifacts and records actions under
	// the same per-item locks as every other writer.
	store, err := item.Open(cfg.StateDir)
	if err != nil {
		stopEngine()
		<-engineErr
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer func() { _ = store.Close() }()

	uiCtx, stopUI := context.WithCancel(ctx)
	defer stopUI()
	var (
		stopped    error
		engineDone bool
	)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case stopped = <-engineErr:
			// The engine stopped (an error or a signal): the UI cannot
			// continue.
			engineDone = true
			stopUI()
		case <-uiCtx.Done():
		}
	}()

	res := &resumer{
		cfg:         cfg,
		store:       store,
		now:         e.now,
		lookPath:    e.lookPath,
		processOwns: e.processOwns,
	}
	m := ui.New(uiCtx, ui.Options{
		Feed:          feed,
		Engine:        api,
		Store:         store,
		Now:           e.now,
		SeverityOrder: cfg.Alerts.SeverityOrder,
		PrepareAfter:  cfg.Alerts.PrepareAfter,
		Editor:        cfg.Editor,
		Placement:     cfg.Ghostty.Placement,
		Resume: func(ctx context.Context, r ui.ResumeRequest) error {
			_, err := res.resume(ctx, resumeTarget{id: r.ItemID, run: r.Run, session: r.SessionID, placement: r.Placement})
			return err
		},
		Open: e.openURL,
		Hint: apiHint,
	})
	uiErr := e.tui(uiCtx, m)
	stopUI()
	<-watchDone
	stopEngine()
	if !engineDone {
		// Wait for the engine to release the instance lock.
		stopped = <-engineErr
	}
	switch {
	case uiErr != nil && !errors.Is(uiErr, context.Canceled):
		_, _ = fmt.Fprintln(stderr, "error: terminal UI:", uiErr)
		return 1
	case stopped != nil:
		_, _ = fmt.Fprintln(stderr, "error: engine stopped:", stopped)
		return 1
	}
	return 0
}
