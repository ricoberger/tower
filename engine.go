package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/prompt"
	"github.com/ricoberger/tower/internal/reconcile"
	"github.com/ricoberger/tower/internal/source"
)

const (
	defaultTick = time.Second
	// defaultFetchTimeout is the independent end-to-end budget of each
	// source poll.
	defaultFetchTimeout = source.FetchTimeout
)

type engineOptions struct {
	cfg      *config.Config
	level    slog.Level
	stderr   io.Writer
	now      func() time.Time
	lookPath config.LookPath
	// tick and pollInterval override the defaults in tests.
	tick         time.Duration
	pollInterval time.Duration
	// logMaxBytes overrides the log rotation size in tests.
	logMaxBytes int64
	// fetchTimeout overrides the per-source poll budget in tests.
	fetchTimeout time.Duration
	// Test overrides: sources replaces the sources built from the
	// configuration; tickC and pollC replace the tickers; hooks observe the
	// engine loop.
	sources []source.Source
	tickC   <-chan time.Time
	pollC   <-chan time.Time
	hooks   engineHooks
}

// engineHooks are called by the engine loop (tests only).
type engineHooks struct {
	roundStarted func()
	roundApplied func()
	pollSkipped  func()
	ticked       func()
	pruneApplied func()
}

func call(f func()) {
	if f != nil {
		f()
	}
}

type cachedItem struct {
	loaded item.Loaded
	alert  *source.Alert
}

// sourceStatus is the health and latest applied snapshot of a source. It is
// only accessed by the engine loop.
type sourceStatus struct {
	snapshot reconcile.Snapshot
	// lastSuccess is the completion time of the latest successful poll (zero
	// if the source never succeeded) and lastErr the sanitized error of the
	// latest poll ("" after a success).
	lastSuccess time.Time
	lastErr     string
}

// pollResult is the outcome of one source poll within a round.
type pollResult struct {
	source string
	alerts []source.Alert
	err    error
	// at is the completion time of the poll.
	at time.Time
}

type engine struct {
	cfg        *config.Config
	store      *item.Store
	log        *slog.Logger
	now        func() time.Time
	items      map[string]*cachedItem
	unreadable map[string]item.Stamp
	sources    []source.Source
	status     map[string]*sourceStatus
	meta       map[string]prompt.AlertMeta
	pruned     chan []string
	// rounds delivers the combined result of a background poll round;
	// inFlight is true from the start of a round until the loop applied it.
	rounds       chan []pollResult
	inFlight     bool
	fetchTimeout time.Duration
	hooks        engineHooks
}

// runEngine runs the headless engine until ctx is cancelled.
func runEngine(ctx context.Context, opts engineOptions) error {
	cfg := opts.cfg
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.tick <= 0 {
		opts.tick = defaultTick
	}
	if opts.pollInterval <= 0 {
		opts.pollInterval = cfg.Alerts.PollInterval
	}
	if opts.logMaxBytes <= 0 {
		opts.logMaxBytes = logMaxBytes
	}
	if opts.fetchTimeout <= 0 {
		opts.fetchTimeout = defaultFetchTimeout
	}

	findings := config.CheckExecutables(cfg, opts.lookPath)
	if findings.RunsCommand != "" {
		return errors.New(findings.RunsCommand)
	}

	store, err := item.Open(cfg.StateDir)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	lock, err := store.TryLockInstance()
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	lw, err := newRotatingWriter(store, logFile, opts.logMaxBytes, logFiles)
	if err != nil {
		return err
	}
	defer func() { _ = lw.Close() }()
	logger := newLogger(lw, opts.stderr, opts.level)

	e := &engine{
		cfg:          cfg,
		store:        store,
		log:          logger,
		now:          func() time.Time { return opts.now().UTC().Truncate(time.Second) },
		items:        map[string]*cachedItem{},
		unreadable:   map[string]item.Stamp{},
		status:       map[string]*sourceStatus{},
		meta:         map[string]prompt.AlertMeta{},
		pruned:       make(chan []string, 1),
		rounds:       make(chan []pollResult, 1),
		fetchTimeout: opts.fetchTimeout,
		hooks:        opts.hooks,
	}
	return e.run(ctx, opts, findings)
}

func (e *engine) run(ctx context.Context, opts engineOptions, findings config.ExecutableFindings) error {
	e.log.Info("tower engine starting (headless)", "state_dir", e.store.Dir())

	if findings.GhosttyCommand != "" {
		e.log.Warn(findings.GhosttyCommand)
	}
	for _, s := range e.cfg.Sources {
		e.meta[s.Name] = prompt.MetaFor(s)
		switch s.Type {
		case config.SourceFile:
			e.sources = append(e.sources, source.NewFile(s.Name, s.Path))
		case config.SourceAlertmanager:
			e.sources = append(e.sources, source.NewAlertmanager(s))
			if s.GrafanaInstance == "" {
				e.log.Warn("alertmanager source without grafana_instance: the skill will have no Grafana instance and may stop as blocked", "source", s.Name)
			}
		}
	}
	if opts.sources != nil {
		e.sources = opts.sources
	}
	for _, src := range e.sources {
		e.status[src.Name()] = &sourceStatus{snapshot: reconcile.Snapshot{Source: src.Name()}}
	}

	e.refresh()
	e.log.Info("items loaded", "items", len(e.items), "unreadable_items", len(e.unreadable))

	var wg sync.WaitGroup
	bgCtx, cancelBg := context.WithCancel(ctx)
	defer func() {
		// Cancel outstanding polls and credential commands; their cleanup
		// is bounded (CredentialWaitDelay), not their remaining budget.
		cancelBg()
		wg.Wait()
	}()
	wg.Go(func() { e.pruneOnce(bgCtx) })

	// The startup round runs in the background like every later round.
	e.startRound(bgCtx, &wg)

	tickC, pollC := opts.tickC, opts.pollC
	if pollC == nil {
		pollTicker := time.NewTicker(opts.pollInterval)
		defer pollTicker.Stop()
		pollC = pollTicker.C
	}
	if tickC == nil {
		tick := time.NewTicker(opts.tick)
		defer tick.Stop()
		tickC = tick.C
	}

	for {
		select {
		case <-ctx.Done():
			e.log.Info("tower engine stopping")
			return nil
		case ids := <-e.pruned:
			for _, id := range ids {
				delete(e.items, id)
			}
			call(e.hooks.pruneApplied)
		case results := <-e.rounds:
			e.applyRound(results)
			e.inFlight = false
			e.refresh()
			e.reconcile()
			call(e.hooks.roundApplied)
		case <-pollC:
			if e.inFlight {
				e.log.Debug("poll skipped: the previous poll round is still running")
				call(e.hooks.pollSkipped)
				continue
			}
			e.startRound(bgCtx, &wg)
		case <-tickC:
			// Timer ticks only use the snapshots of already applied rounds.
			e.refresh()
			e.reconcile()
			call(e.hooks.ticked)
		}
	}
}

// startRound polls all sources concurrently in the background, one goroutine
// per source with its own budget, and delivers their combined result over
// e.rounds once every poll completed or timed out. Poll workers never touch
// engine state.
func (e *engine) startRound(ctx context.Context, wg *sync.WaitGroup) {
	e.inFlight = true
	sources := slices.Clone(e.sources)
	now, timeout := e.now, e.fetchTimeout
	wg.Go(func() {
		results := make([]pollResult, len(sources))
		var polls sync.WaitGroup
		for i, src := range sources {
			polls.Go(func() {
				fctx, cancel := context.WithTimeout(ctx, timeout)
				alerts, err := src.Fetch(fctx)
				cancel()
				results[i] = pollResult{source: src.Name(), alerts: alerts, err: err, at: now()}
			})
		}
		polls.Wait()
		// e.rounds has room for the only round in flight; the select
		// never blocks a stopped engine.
		select {
		case e.rounds <- results:
		case <-ctx.Done():
		}
	})
	call(e.hooks.roundStarted)
}

// applyRound updates the cached snapshots and health of all sources from a
// completed round. It runs on the engine loop only.
func (e *engine) applyRound(results []pollResult) {
	for _, r := range results {
		st := e.status[r.source]
		if st == nil {
			continue
		}
		if r.err != nil {
			st.snapshot.OK = false
			st.lastErr = r.err.Error()
			e.log.Warn("source poll failed", "source", r.source, "error", st.lastErr,
				"last_success", formatSuccess(st.lastSuccess))
			continue
		}
		prevErr := st.lastErr
		st.lastErr = ""
		st.lastSuccess = r.at
		st.snapshot = reconcile.Snapshot{Source: r.source, OK: true, ObservedAt: r.at, Alerts: r.alerts}
		if prevErr != "" {
			e.log.Info("source recovered", "source", r.source, "last_success", formatSuccess(st.lastSuccess), "previous_error", prevErr)
		}
		e.log.Debug("source polled", "source", r.source, "alerts", len(r.alerts), "last_success", formatSuccess(st.lastSuccess))
	}
}

// formatSuccess renders a last successful poll time, or "never".
func formatSuccess(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

// refresh synchronizes the in-memory items with the disk, reloading items
// whose item.yaml changed (e.g. through another process).
func (e *engine) refresh() {
	ids, err := e.store.List()
	if err != nil {
		e.log.Error("list items", "error", err)
		return
	}
	present := map[string]bool{}
	for _, id := range ids {
		present[id] = true
		st, err := e.store.Stamp(id)
		if c, ok := e.items[id]; ok && err == nil && c.loaded.Stamp == st {
			continue
		}
		if bad, ok := e.unreadable[id]; ok && err == nil && bad == st {
			continue
		}
		e.load(id, st)
	}
	for id := range e.items {
		if !present[id] {
			delete(e.items, id)
		}
	}
	for id := range e.unreadable {
		if !present[id] {
			delete(e.unreadable, id)
		}
	}
}

func (e *engine) load(id string, st item.Stamp) {
	l, err := e.store.Load(id)
	if err != nil {
		delete(e.items, id)
		_, known := e.unreadable[id]
		e.unreadable[id] = st
		if !known {
			e.log.Error("skipping unreadable item", "item_id", id, "path", e.store.ItemDir(id), "error", err)
			e.log.Warn("unreadable items", "count", len(e.unreadable))
		}
		return
	}
	if _, was := e.unreadable[id]; was {
		delete(e.unreadable, id)
		e.log.Info("item readable again", "item_id", id, "unreadable_items", len(e.unreadable))
	}
	c := &cachedItem{loaded: l}
	if l.RawAlert != nil {
		if a, err := source.DecodeAlert(l.Item.Source.Name, l.RawAlert); err == nil {
			c.alert = &a
		}
	}
	e.items[id] = c
	e.initMarkdown(id, c)
}

// initMarkdown creates a missing alert.md of a readable item from its stored
// alert.json and its currently configured source. It changes no other file,
// never replaces an existing alert.md and never resolves credentials. Items
// without a decodable stored alert or configured source are skipped.
func (e *engine) initMarkdown(id string, c *cachedItem) {
	meta, ok := e.meta[c.loaded.Item.Source.Name]
	if c.alert == nil || !ok {
		return
	}
	written, err := e.store.InitAlertMarkdown(id, prompt.AlertMarkdown(*c.alert, meta))
	if err != nil {
		e.log.Error("initialize alert.md", "item_id", id, "source", c.loaded.Item.Source.Name, "error", err)
		return
	}
	if written {
		e.log.Info("initialized missing alert.md", "item_id", id, "source", c.loaded.Item.Source.Name)
	}
}

func (e *engine) reconcile() {
	counters, err := e.store.Counters()
	if err != nil {
		e.log.Error("read item counters; skipping reconciliation", "error", err)
		return
	}
	in := reconcile.Input{
		Counters: counters,
		Now:      e.now(),
		Config: reconcile.Config{
			PrepareAfter:   e.cfg.Alerts.PrepareAfter,
			ResolvedLinger: e.cfg.Alerts.ResolvedLinger,
			ReopenWindow:   e.cfg.Alerts.ReopenWindow,
		},
	}
	for _, id := range slices.Sorted(maps.Keys(e.items)) {
		c := e.items[id]
		in.Items = append(in.Items, reconcile.ItemState{Item: *c.loaded.Item, Runs: c.loaded.Runs, Alert: c.alert})
	}
	for _, name := range slices.Sorted(maps.Keys(e.status)) {
		in.Snapshots = append(in.Snapshots, e.status[name].snapshot)
	}
	e.apply(reconcile.Reconcile(in))
}

func (e *engine) apply(res reconcile.Result) {
	for _, ch := range res.Changes {
		if err := e.applyChange(ch); err != nil {
			e.log.Error("persist item change", "item_id", ch.ItemID, "source", ch.Item.Source.Name, "error", err)
			continue
		}
		for _, t := range ch.Transitions() {
			e.log.Info("transition", "item_id", ch.ItemID, "source", ch.Item.Source.Name,
				"from", string(t.From), "to", string(t.To), "reason", t.Reason, "title", ch.Item.Title)
		}
		if st, err := e.store.Stamp(ch.ItemID); err == nil {
			e.load(ch.ItemID, st)
		}
	}
	for _, ef := range res.Effects {
		e.log.Info("intent recorded; the runner is not available in this version", "intent", string(ef.Kind),
			"item_id", ef.ItemID, "run", ef.Run, "reason", string(ef.Reason))
	}
	for _, ig := range res.Ignored {
		e.log.Warn("event ignored", "item_id", ig.Event.ItemID, "event", string(ig.Event.Kind), "reason", ig.Reason)
	}
}

func (e *engine) applyChange(ch reconcile.Change) error {
	if ch.Create {
		it := ch.Item
		if err := e.store.Create(&it, ch.RawAlert); err != nil {
			return err
		}
		e.writeMarkdown(ch)
		return nil
	}
	if _, err := e.store.Update(ch.ItemID, func(disk *item.Item) error {
		*disk = *mergeChange(disk, ch)
		return nil
	}); err != nil {
		return err
	}
	if ch.RawAlert != nil {
		if err := e.store.WriteRawAlert(ch.ItemID, ch.RawAlert); err != nil {
			return fmt.Errorf("write alert.json: %w", err)
		}
		e.writeMarkdown(ch)
	}
	for _, r := range ch.Runs {
		if err := e.store.WriteRun(ch.ItemID, r); err != nil {
			return fmt.Errorf("write run %d: %w", r.Number, err)
		}
	}
	return nil
}

// writeMarkdown renders alert.md from the raw alert of a change, i.e. when
// alert.json is written. Failures are logged and never abort the change; a
// stale alert.md is removed so that it is initialized again from alert.json.
func (e *engine) writeMarkdown(ch reconcile.Change) {
	if ch.RawAlert == nil {
		return
	}
	name := ch.Item.Source.Name
	meta, ok := e.meta[name]
	if !ok {
		return
	}
	a, err := source.DecodeAlert(name, ch.RawAlert)
	if err == nil {
		err = e.store.WriteAlertMarkdown(ch.ItemID, prompt.AlertMarkdown(a, meta))
	}
	if err != nil {
		e.log.Error("write alert.md", "item_id", ch.ItemID, "source", name, "error", err)
		if !ch.Create {
			_ = e.store.RemoveAlertMarkdown(ch.ItemID)
		}
	}
}

// mergeChange merges a reconciliation change into the latest on-disk item
// read under the item lock. User actions and seen updates written
// concurrently by other processes are kept: only this pass's history entries
// are appended to the disk history. Timestamps never move backwards: an entry
// computed before a concurrently persisted newer entry is recorded at that
// newer time so the history stays chronological, and updated_at is never
// older than the disk value or the last history entry.
func mergeChange(disk *item.Item, ch reconcile.Change) *item.Item {
	merged := ch.Item.Clone()
	var last time.Time
	if n := len(disk.History); n > 0 {
		last = disk.History[n-1].At
	}
	appended := make([]item.HistoryEntry, 0, len(ch.Appended))
	for _, h := range ch.Appended {
		if h.At.Before(last) {
			h.At = last
		}
		last = h.At
		appended = append(appended, h)
	}
	merged.History = item.AppendHistory(disk.History, appended...)
	if !ch.SeenChanged {
		merged.Seen = disk.Seen
	}
	for _, t := range []time.Time{disk.UpdatedAt, last} {
		if merged.UpdatedAt.Before(t) {
			merged.UpdatedAt = t
		}
	}
	return merged
}

// pruneOnce runs retention pruning once, using the already held engine lock.
func (e *engine) pruneOnce(ctx context.Context) {
	cutoff := e.now().Add(-e.cfg.Retention.DoneAfter)
	cands, err := e.store.PruneCandidates(cutoff)
	if err != nil {
		e.log.Error("retention pruning", "error", err)
		return
	}
	var deleted []string
	for _, c := range cands {
		if ctx.Err() != nil {
			break
		}
		ok, err := e.store.PruneItem(c.ID, cutoff)
		if err != nil {
			e.log.Error("prune item", "item_id", c.ID, "error", err)
			continue
		}
		if ok {
			e.log.Info("pruned done item", "item_id", c.ID)
			deleted = append(deleted, c.ID)
		}
	}
	if len(deleted) > 0 {
		select {
		case e.pruned <- deleted:
		case <-ctx.Done():
		}
	}
}
