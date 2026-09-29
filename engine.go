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
	"github.com/ricoberger/tower/internal/reconcile"
	"github.com/ricoberger/tower/internal/source"
)

const (
	defaultTick  = time.Second
	fetchTimeout = 15 * time.Second
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
}

type cachedItem struct {
	loaded item.Loaded
	alert  *source.Alert
}

type sourceStatus struct {
	snapshot reconcile.Snapshot
	lastErr  string
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
	pruned     chan []string
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
		cfg:        cfg,
		store:      store,
		log:        logger,
		now:        func() time.Time { return opts.now().UTC().Truncate(time.Second) },
		items:      map[string]*cachedItem{},
		unreadable: map[string]item.Stamp{},
		status:     map[string]*sourceStatus{},
		pruned:     make(chan []string, 1),
	}
	return e.run(ctx, opts, findings)
}

func (e *engine) run(ctx context.Context, opts engineOptions, findings config.ExecutableFindings) error {
	e.log.Info("tower engine starting (headless)", "state_dir", e.store.Dir())

	if findings.GhosttyCommand != "" {
		e.log.Warn(findings.GhosttyCommand)
	}
	for _, s := range e.cfg.Sources {
		switch s.Type {
		case config.SourceFile:
			e.sources = append(e.sources, source.NewFile(s.Name, s.Path))
			e.status[s.Name] = &sourceStatus{snapshot: reconcile.Snapshot{Source: s.Name}}
		case config.SourceAlertmanager:
			e.log.Warn("alertmanager polling is not available in this version; the source is inactive and its items are left untouched", "source", s.Name)
			if s.GrafanaInstance == "" {
				e.log.Warn("alertmanager source without grafana_instance: the skill will have no Grafana instance and may stop as blocked", "source", s.Name)
			}
		}
	}

	e.refresh()
	e.log.Info("items loaded", "items", len(e.items), "unreadable_items", len(e.unreadable))

	var wg sync.WaitGroup
	pruneCtx, cancelPrune := context.WithCancel(ctx)
	defer func() {
		cancelPrune()
		wg.Wait()
	}()
	wg.Go(func() { e.pruneOnce(pruneCtx) })

	e.pollAll(ctx)
	e.reconcile()

	pollTicker := time.NewTicker(opts.pollInterval)
	defer pollTicker.Stop()
	tick := time.NewTicker(opts.tick)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			e.log.Info("tower engine stopping")
			return nil
		case ids := <-e.pruned:
			for _, id := range ids {
				delete(e.items, id)
			}
		case <-pollTicker.C:
			e.refresh()
			e.pollAll(ctx)
			e.reconcile()
		case <-tick.C:
			e.refresh()
			e.reconcile()
		}
	}
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
}

func (e *engine) pollAll(ctx context.Context) {
	for _, src := range e.sources {
		st := e.status[src.Name()]
		fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
		alerts, err := src.Fetch(fctx)
		cancel()
		if err != nil {
			st.snapshot.OK = false
			st.lastErr = err.Error()
			e.log.Warn("source poll failed", "source", src.Name(), "error", err)
			continue
		}
		if st.lastErr != "" {
			e.log.Info("source recovered", "source", src.Name())
			st.lastErr = ""
		}
		st.snapshot = reconcile.Snapshot{Source: src.Name(), OK: true, ObservedAt: e.now(), Alerts: alerts}
		e.log.Debug("source polled", "source", src.Name(), "alerts", len(alerts))
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
		return e.store.Create(&it, ch.RawAlert)
	}
	if _, err := e.store.Update(ch.ItemID, func(disk *item.Item) error {
		merged := ch.Item.Clone()
		// Keep user actions/seen updates written concurrently by other
		// processes: append only this pass's entries to the latest disk
		// history.
		merged.History = item.AppendHistory(disk.History, ch.Appended...)
		if !ch.SeenChanged {
			merged.Seen = disk.Seen
		}
		*disk = *merged
		return nil
	}); err != nil {
		return err
	}
	if ch.RawAlert != nil {
		if err := e.store.WriteRawAlert(ch.ItemID, ch.RawAlert); err != nil {
			return fmt.Errorf("write alert.json: %w", err)
		}
	}
	for _, r := range ch.Runs {
		if err := e.store.WriteRun(ch.ItemID, r); err != nil {
			return fmt.Errorf("write run %d: %w", r.Number, err)
		}
	}
	return nil
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
