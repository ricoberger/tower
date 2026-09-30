package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/notify"
	"github.com/ricoberger/tower/internal/prompt"
	"github.com/ricoberger/tower/internal/prompt/prep"
	"github.com/ricoberger/tower/internal/reconcile"
	"github.com/ricoberger/tower/internal/runner"
	"github.com/ricoberger/tower/internal/source"
	"github.com/ricoberger/tower/internal/ui"
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

	// api receives manual-run and dismiss requests (optional).
	api *engineAPI
	// deliver overrides the notification delivery (tests).
	deliver func(context.Context, notify.Notification) error
	// monitorC overrides the periodic run monitoring ticker and runner
	// overrides the runner's grace, signalling and ownership checks
	// (tests).
	monitorC <-chan time.Time
	runner   runner.Options

	// feed receives a snapshot of the applied state after every loop
	// iteration (TUI mode; optional).
	feed *ui.Feed
	// started is called once the instance lock is held and the engine is
	// about to enter its loop (optional).
	started func()
	// mode names the frontend in the startup log line.
	mode string
}

// engineHooks are called by the engine loop (tests only).
type engineHooks struct {
	roundStarted func()
	roundApplied func()
	pollSkipped  func()
	ticked       func()
	pruneApplied func()
	monitored    func()
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
	// pruning holds the IDs the background pruner is deleting; a failed
	// load of such an item is a concurrent deletion, not corruption.
	pruning sync.Map
	// rounds delivers the combined result of a background poll round;
	// inFlight is true from the start of a round until the loop applied it.
	rounds       chan []pollResult
	inFlight     bool
	fetchTimeout time.Duration
	hooks        engineHooks

	runner  *runner.Runner
	queue   *runner.Queue
	blocked map[string]blockedStart
	// pending are runner events whose item change could not be persisted;
	// they are applied again by the next reconciliation.
	pending []reconcile.Event
	deliver func(context.Context, notify.Notification) error
	// bgCtx and wg bound background work (polls, pruning, notifications).
	bgCtx context.Context
	wg    *sync.WaitGroup
}

// runEngine runs the engine until ctx is cancelled.
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

	tmpl := prep.Default()
	if cfg.Prompts.Alert != "" {
		if tmpl, err = prep.Parse(cfg.Prompts.AlertText); err != nil {
			return fmt.Errorf("prompts.alert: %w", err)
		}
	}
	ro := opts.runner
	ro.Store, ro.Log, ro.Template = store, logger, tmpl
	ro.Command, ro.Model, ro.Args, ro.Timeout = cfg.Runs.Command, cfg.Runs.Model, cfg.Runs.Args, cfg.Runs.Timeout
	if ro.Now == nil {
		ro.Now = opts.now
	}
	deliver := opts.deliver
	if deliver == nil {
		n := &notify.Notifier{Sound: cfg.Notifications.Sound, ConfigPath: cfg.Path, Executable: os.Executable}
		deliver = n.Deliver
	}

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
		runner:       runner.New(ro),
		queue:        runner.NewQueue(cfg.Alerts.SeverityOrder),
		blocked:      map[string]blockedStart{},
		deliver:      deliver,
	}
	return e.run(ctx, opts, findings)
}

func (e *engine) run(ctx context.Context, opts engineOptions, findings config.ExecutableFindings) error {
	mode := opts.mode
	if mode == "" {
		mode = "headless"
	}
	e.log.Info("tower engine starting ("+mode+")", "state_dir", e.store.Dir())

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
	e.bgCtx, e.wg = bgCtx, &wg
	defer func() {
		// Cancel outstanding polls, credential commands and notification
		// deliveries; their cleanup is bounded (CredentialWaitDelay,
		// notify.WaitDelay), not their remaining budget.
		cancelBg()
		wg.Wait()
	}()
	// Detached runs are neither signalled nor waited for on shutdown.
	defer e.runner.Close()
	api := opts.api
	if api == nil {
		api = newEngineAPI()
	}
	defer close(api.stopped)

	// Recover run evidence before the rebuilt queue is scheduled and
	// before pruning starts; items with tracked runs are never pruned.
	e.recoverRuns()
	e.schedule()
	skip := map[string]bool{}
	for _, id := range e.runner.TrackedIDs() {
		skip[id] = true
	}
	wg.Go(func() { e.pruneOnce(bgCtx, skip) })

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
	monitorC := opts.monitorC
	if monitorC == nil {
		monitor := time.NewTicker(runner.MonitorInterval)
		defer monitor.Stop()
		monitorC = monitor.C
	}

	e.publish(opts.feed)
	call(opts.started)
	for {
		e.publish(opts.feed)
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
			e.schedule()
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
			e.schedule()
			call(e.hooks.ticked)
		case w := <-e.runner.WaitC():
			e.handleCompletions(e.runner.HandleWait(w))
			e.schedule()
		case o := <-e.runner.OwnershipC():
			// Ownership checks of re-attached runs run off the loop.
			e.handleCompletions(e.runner.HandleOwnership(o))
			e.schedule()
		case <-monitorC:
			e.handleCompletions(e.runner.Check())
			e.schedule()
			call(e.hooks.monitored)
		case c := <-api.cmds:
			err := e.command(c)
			e.schedule()
			c.reply <- err
		}
	}
}

// publish hands an independent copy of the applied state to the UI. It runs
// on the engine loop only.
func (e *engine) publish(feed *ui.Feed) {
	if feed == nil {
		return
	}
	snap := ui.Snapshot{
		Unreadable:  len(e.unreadable),
		Running:     e.runner.Busy(),
		Concurrency: e.cfg.Runs.Concurrency,
		Queued:      e.queue.Len(),
	}
	for _, c := range e.items {
		runs := make([]item.Run, len(c.loaded.Runs))
		for i, r := range c.loaded.Runs {
			runs[i] = r.Clone()
		}
		snap.Items = append(snap.Items, ui.ItemView{Item: c.loaded.Item.Clone(), Runs: runs})
	}
	for _, src := range e.sources {
		h := ui.SourceHealth{Name: src.Name()}
		if st := e.status[src.Name()]; st != nil {
			h.LastSuccess, h.LastErr = st.lastSuccess, st.lastErr
		}
		snap.Sources = append(snap.Sources, h)
	}
	feed.Publish(snap)
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

// removed reports whether an item is being deleted by the pruner or its
// directory no longer exists.
func (e *engine) removed(id string) bool {
	if _, ok := e.pruning.Load(id); ok {
		return true
	}
	_, err := os.Lstat(e.store.ItemDir(id))
	return errors.Is(err, fs.ErrNotExist)
}

func (e *engine) load(id string, st item.Stamp) {
	l, err := e.store.Load(id)
	if err != nil && e.removed(id) {
		// Deleted concurrently (retention pruning): forget it without
		// reporting it as unreadable. A still existing item is loaded
		// again by the next refresh.
		delete(e.items, id)
		delete(e.unreadable, id)
		e.log.Debug("item removed while loading", "item_id", id, "error", err)
		return
	}
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

func (e *engine) reconcile() { _, _, _ = e.reconcileWith(nil) }

// reconcileWith runs a reconciliation pass with the given runner or user
// events (plus runner events pending from failed writes) and applies its
// result. It returns the result and the write errors by item ID, or an
// error if the pass could not run; runner events are then retained for the
// next pass, user requests are not.
func (e *engine) reconcileWith(events []reconcile.Event) (reconcile.Result, map[string]error, error) {
	counters, err := e.store.Counters()
	if err != nil {
		e.log.Error("read item counters; skipping reconciliation", "error", err)
		e.pending = append(e.pending, runnerEvents(events)...)
		return reconcile.Result{}, nil, fmt.Errorf("read item counters: %w", err)
	}
	events = append(e.pending, events...)
	e.pending = nil
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
	in.Events = events
	res := reconcile.Reconcile(in)
	failed := e.apply(res)
	for _, ev := range runnerEvents(events) {
		if failed[ev.ItemID] != nil {
			e.pending = append(e.pending, ev)
		}
	}
	return res, failed, nil
}

// runnerEvents returns the runner events of events. Unlike user requests,
// they report facts that must eventually be applied.
func runnerEvents(events []reconcile.Event) []reconcile.Event {
	var out []reconcile.Event
	for _, ev := range events {
		switch ev.Kind {
		case reconcile.EventRunStarted, reconcile.EventRunFinished, reconcile.EventRunInterrupted, reconcile.EventRunRecovered:
			out = append(out, ev)
		}
	}
	return out
}

// apply persists the changes of a reconciliation result and executes the
// effects of the successfully persisted items. It returns the write errors
// by item ID.
func (e *engine) apply(res reconcile.Result) map[string]error {
	failed := map[string]error{}
	for _, ch := range res.Changes {
		if err := e.applyChange(ch); err != nil {
			failed[ch.ItemID] = err
			e.log.Error("persist item change", "item_id", ch.ItemID, "source", ch.Item.Source.Name, "error", err)
			// Reload: the item may be partially written.
			if st, err := e.store.Stamp(ch.ItemID); err == nil {
				e.load(ch.ItemID, st)
			}
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
		// Effects of items whose change was not persisted are stale.
		if failed[ef.ItemID] != nil {
			continue
		}
		switch ef.Kind {
		case reconcile.EffectEnqueue:
			e.enqueue(ef.ItemID)
		case reconcile.EffectCancelQueued:
			if e.queue.Remove(ef.ItemID) {
				e.log.Info("queued run cancelled", "item_id", ef.ItemID, "run", ef.Run)
			}
		case reconcile.EffectCancelRunning:
			e.runner.Cancel(ef.ItemID, ef.Run)
		case reconcile.EffectNotify:
			e.notifyOutcome(ef)
		}
	}
	for _, ig := range res.Ignored {
		e.log.Warn("event ignored", "item_id", ig.Event.ItemID, "event", string(ig.Event.Kind), "reason", ig.Reason)
	}
	return failed
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
	// Run metadata is written first so that the item never references
	// runs whose evidence was not persisted.
	for _, r := range ch.Runs {
		if err := e.store.WriteRun(ch.ItemID, r); err != nil {
			return fmt.Errorf("write run %d: %w", r.Number, err)
		}
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
// Items in skip (with runs tracked on startup) are never pruned.
func (e *engine) pruneOnce(ctx context.Context, skip map[string]bool) {
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
		if skip[c.ID] {
			continue
		}
		e.pruning.Store(c.ID, struct{}{})
		ok, err := e.store.PruneItem(c.ID, cutoff)
		e.pruning.Delete(c.ID)
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
