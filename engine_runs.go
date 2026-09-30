package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/notify"
	"github.com/ricoberger/tower/internal/reconcile"
	"github.com/ricoberger/tower/internal/result"
	"github.com/ricoberger/tower/internal/runner"
)

// startRetryDelay is how long a queued run whose start failed without a
// persisted run is held back before the next attempt.
const startRetryDelay = time.Minute

// Errors of the engine API. Rejections are returned before any state is
// changed.
var (
	// ErrUnknownItem is returned for an item ID that is not a readable
	// item.
	ErrUnknownItem = errors.New("unknown item")
	// ErrRunExecuting rejects a manual run while a run of the item is still
	// executing.
	ErrRunExecuting = errors.New("a run is still executing; wait for it to finish or cancel it first")
	// ErrItemDone rejects a dismissal of an item that is already done.
	ErrItemDone = errors.New("the item is already done")
	// ErrEngineStopped is returned when the engine is not running.
	ErrEngineStopped = errors.New("the engine is not running")
)

// ManualRunNotAllowedError rejects a manual run in a state that does not
// permit one (queued, preparing or done).
type ManualRunNotAllowedError struct {
	State item.State
}

func (e *ManualRunNotAllowedError) Error() string {
	return fmt.Sprintf("a manual run is not allowed while the item is %s", e.State)
}

type apiCommandKind int

const (
	apiManualRun apiCommandKind = iota
	apiDismiss
)

type apiCommand struct {
	kind  apiCommandKind
	id    string
	reply chan error
}

// engineAPI requests user actions from a running engine. Requests are
// processed by the engine loop.
type engineAPI struct {
	cmds    chan apiCommand
	stopped chan struct{}
}

// newEngineAPI returns an API for one engine instance.
func newEngineAPI() *engineAPI {
	return &engineAPI{cmds: make(chan apiCommand), stopped: make(chan struct{})}
}

// ManualRun requests a manual preparation run of an item. The request is
// queued with manual priority and ignores prepare_after. It returns
// ErrUnknownItem, *ManualRunNotAllowedError or ErrRunExecuting for
// rejected requests.
func (a *engineAPI) ManualRun(ctx context.Context, itemID string) error {
	return a.do(ctx, apiManualRun, itemID)
}

// Dismiss dismisses an item: it becomes done, queued work is removed and an
// executing run is cancelled.
func (a *engineAPI) Dismiss(ctx context.Context, itemID string) error {
	return a.do(ctx, apiDismiss, itemID)
}

func (a *engineAPI) do(ctx context.Context, kind apiCommandKind, id string) error {
	c := apiCommand{kind: kind, id: id, reply: make(chan error, 1)}
	select {
	case a.cmds <- c:
	case <-a.stopped:
		return ErrEngineStopped
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-c.reply:
		return err
	case <-a.stopped:
		return ErrEngineStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

// blockedStart holds back a queued run whose start failed.
type blockedStart struct {
	run   int
	until time.Time
}

// command processes an API request on the engine loop.
func (e *engine) command(c apiCommand) error {
	e.refresh()
	cached, ok := e.items[c.id]
	if !ok {
		return ErrUnknownItem
	}
	it := cached.loaded.Item
	var ev reconcile.Event
	switch c.kind {
	case apiManualRun:
		switch it.State {
		case item.StateNew, item.StateNeedsYou, item.StateSnoozed, item.StateResolved:
		default:
			return &ManualRunNotAllowedError{State: it.State}
		}
		if e.runner.Executing(c.id) || hasRunning(cached.loaded.Runs) {
			return ErrRunExecuting
		}
		ev = reconcile.Event{Kind: reconcile.EventManualRun, ItemID: c.id}
	case apiDismiss:
		if it.State == item.StateDone {
			return ErrItemDone
		}
		ev = reconcile.Event{Kind: reconcile.EventDismiss, ItemID: c.id}
	}
	res, failed, err := e.reconcileWith([]reconcile.Event{ev})
	if err != nil {
		return fmt.Errorf("request not applied: %w", err)
	}
	if err := failed[c.id]; err != nil {
		return fmt.Errorf("persist item: %w", err)
	}
	for _, ig := range res.Ignored {
		if ig.Event.ItemID == c.id && ig.Event.Kind == ev.Kind {
			return errors.New(ig.Reason)
		}
	}
	return nil
}

func hasRunning(runs []item.Run) bool {
	return slices.ContainsFunc(runs, func(r item.Run) bool { return r.Outcome == item.OutcomeRunning })
}

// recoverRuns recovers the persisted run evidence of all readable items on
// startup, before the queue is scheduled: running runs are re-attached when
// their exit code exists or their process is verifiably theirs, and marked
// interrupted otherwise; metadata ahead of runs.current (a start that was
// not yet applied to the item) is adopted; completions not yet reflected in
// the item state are applied again.
func (e *engine) recoverRuns() {
	var events []reconcile.Event
	type cancel struct {
		id string
		n  int
	}
	var cancels []cancel
	for _, id := range slices.Sorted(maps.Keys(e.items)) {
		c := e.items[id]
		it := c.loaded.Item
		src := it.Source.Name
		cur := it.Runs.Current
		startPending := it.State == item.StateQueued
		runs := slices.Clone(c.loaded.Runs)
		slices.SortFunc(runs, func(a, b item.Run) int { return a.Number - b.Number })
		for _, run := range runs {
			queuedStart := startPending && run.Number == cur+1
			if queuedStart {
				startPending = false
			}
			started := func(r item.Run) {
				if queuedStart {
					events = append(events, reconcile.Event{Kind: reconcile.EventRunStarted, ItemID: id, Run: r})
				} else {
					events = append(events, reconcile.Event{Kind: reconcile.EventRunRecovered, ItemID: id, Run: r})
				}
			}
			if run.Outcome == item.OutcomeRunning {
				adopted, r := e.recoverRun(it, run)
				if run.Number > cur {
					e.log.Info("recovered a run that was started but not yet applied to the item",
						"item_id", id, "run", run.Number, "source", src, "adopted", adopted)
					started(r)
				}
				if !adopted {
					e.log.Info("run interrupted while tower was stopped", "item_id", id, "run", run.Number,
						"source", src, "error", r.Error)
					events = append(events, reconcile.Event{Kind: reconcile.EventRunInterrupted, ItemID: id, Run: r})
				} else if it.State == item.StateDone {
					cancels = append(cancels, cancel{id, run.Number})
				}
				continue
			}
			if run.Number > cur {
				if !run.Outcome.Finished() && run.Outcome != item.OutcomeInterrupted {
					// Only finished or interrupted evidence can complete
					// the queued start; anything else is only recorded.
					queuedStart = false
				}
				e.log.Info("recovered a run that was started but not yet applied to the item",
					"item_id", id, "run", run.Number, "source", src, "outcome", string(run.Outcome))
				started(run)
				if !queuedStart {
					continue
				}
			} else if run.Number != cur || it.State != item.StatePreparing {
				continue
			}
			// The completion of the item's current run was persisted but
			// not applied to the item.
			switch {
			case run.Outcome.Finished():
				events = append(events, reconcile.Event{Kind: reconcile.EventRunFinished, ItemID: id, Run: run})
			case run.Outcome == item.OutcomeInterrupted:
				events = append(events, reconcile.Event{Kind: reconcile.EventRunInterrupted, ItemID: id, Run: run})
			}
		}
	}
	// Finished runs whose process group cleanup was pending when tower
	// stopped keep occupying a slot until their cleanup completes.
	for _, id := range slices.Sorted(maps.Keys(e.items)) {
		c := e.items[id]
		runs := slices.Clone(c.loaded.Runs)
		slices.SortFunc(runs, func(a, b item.Run) int { return b.Number - a.Number })
		for _, run := range runs {
			if e.runner.Tracking(id) {
				break
			}
			e.runner.RecoverCleanup(id, c.loaded.Item.Source.Name, run)
		}
	}
	if len(events) > 0 {
		_, _, _ = e.reconcileWith(events)
	}
	// Runs whose exit code was written while tower was stopped complete
	// now; a done item's still executing run is cancelled.
	e.handleCompletions(e.runner.Check())
	for _, c := range cancels {
		if e.runner.Executing(c.id) {
			e.runner.Cancel(c.id, c.n)
		}
	}
}

// recoverRun adopts a persisted running run or returns it as interrupted.
func (e *engine) recoverRun(it *item.Item, run item.Run) (bool, item.Run) {
	if e.runner.Tracking(it.ID) {
		r := run.Clone()
		now := e.now()
		r.Outcome, r.FinishedAt = item.OutcomeInterrupted, &now
		r.Error = "interrupted while tower was stopped: another run of the item is already tracked"
		return false, r
	}
	adopted, r := e.runner.Recover(it.ID, it.Source.Name, run)
	if adopted {
		return true, run
	}
	return false, r
}

// handleCompletions applies finished runs to their items.
func (e *engine) handleCompletions(cs []runner.Completion) {
	if len(cs) == 0 {
		return
	}
	events := make([]reconcile.Event, 0, len(cs))
	for _, c := range cs {
		// Run metadata does not change the item.yaml stamp: reload.
		if st, err := e.store.Stamp(c.ItemID); err == nil {
			e.load(c.ItemID, st)
		}
		events = append(events, reconcile.Event{Kind: reconcile.EventRunFinished, ItemID: c.ItemID, Run: c.Run})
	}
	_, _, _ = e.reconcileWith(events)
}

// entryFor returns the queue entry of an item that is queued for its next
// run and has no executing run.
func (e *engine) entryFor(id string) (runner.Entry, bool) {
	c, ok := e.items[id]
	if !ok {
		return runner.Entry{}, false
	}
	it := c.loaded.Item
	if it.State != item.StateQueued || it.Runs.PendingReason == nil || hasRunning(c.loaded.Runs) {
		return runner.Entry{}, false
	}
	en := runner.Entry{
		ItemID:   id,
		Run:      it.Runs.Current + 1,
		Reason:   *it.Runs.PendingReason,
		Severity: it.Severity,
		StartsAt: it.Alert.StartsAt,
		QueuedAt: it.UpdatedAt,
	}
	if en.Reason == item.ReasonRetry {
		en.RetryOf = it.Runs.Current
	}
	for i := len(it.History) - 1; i >= 0; i-- {
		if h := it.History[i]; !h.IsAction() && h.To == item.StateQueued {
			en.QueuedAt = h.At
			break
		}
	}
	return en, true
}

// enqueue adds the queued run of an item to the queue.
func (e *engine) enqueue(id string) {
	if en, ok := e.entryFor(id); ok {
		e.queue.Push(en)
		e.log.Info("run queued", "item_id", id, "run", en.Run, "source", e.items[id].loaded.Item.Source.Name,
			"reason", string(en.Reason))
	}
}

// schedule starts queued runs in priority order while slots are available.
// Entries are revalidated against the latest item state before each start;
// stale entries are discarded and queued items missing from the queue are
// added.
func (e *engine) schedule() {
	if e.runner.Busy() >= e.cfg.Runs.Concurrency {
		return
	}
	e.refresh()
	for _, id := range e.queue.IDs() {
		if _, ok := e.entryFor(id); !ok {
			e.queue.Remove(id)
			delete(e.blocked, id)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(e.items)) {
		if en, ok := e.entryFor(id); ok {
			if cur, queued := e.queue.Get(id); !queued || cur.Run != en.Run || cur.Reason != en.Reason {
				e.queue.Push(en)
			}
		}
	}

	now := e.now()
	var events []reconcile.Event
	for _, en := range e.queue.Ordered() {
		if e.runner.Busy() >= e.cfg.Runs.Concurrency {
			break
		}
		if e.runner.Tracking(en.ItemID) {
			continue
		}
		if b, ok := e.blocked[en.ItemID]; ok && b.run == en.Run && now.Before(b.until) {
			continue
		}
		delete(e.blocked, en.ItemID)
		it := e.items[en.ItemID].loaded.Item
		run, err := e.runner.Start(runner.Request{
			Item: it, Number: en.Run, Reason: en.Reason, RetryOf: en.RetryOf, QueuedAt: en.QueuedAt,
		})
		if err == nil {
			e.queue.Remove(en.ItemID)
			events = append(events, reconcile.Event{Kind: reconcile.EventRunStarted, ItemID: en.ItemID, Run: run})
			continue
		}
		var se *runner.StartError
		if errors.As(err, &se) && se.Run != nil {
			e.queue.Remove(en.ItemID)
			e.log.Error("run failed to start", "item_id", en.ItemID, "run", en.Run, "source", it.Source.Name, "error", err)
			events = append(events,
				reconcile.Event{Kind: reconcile.EventRunStarted, ItemID: en.ItemID, Run: *se.Run},
				reconcile.Event{Kind: reconcile.EventRunFinished, ItemID: en.ItemID, Run: *se.Run})
			continue
		}
		e.blocked[en.ItemID] = blockedStart{run: en.Run, until: now.Add(startRetryDelay)}
		e.log.Error("run could not be started; retrying later", "item_id", en.ItemID, "run", en.Run,
			"source", it.Source.Name, "error", err, "retry_after", startRetryDelay.String())
	}
	if len(events) > 0 {
		_, _, _ = e.reconcileWith(events)
	}
}

// notifyOutcome delivers the notification of a finished run in the
// background.
func (e *engine) notifyOutcome(ef reconcile.Effect) {
	if !e.cfg.Notifications.Enabled {
		return
	}
	c, ok := e.items[ef.ItemID]
	if !ok {
		return
	}
	it := c.loaded.Item
	n := notify.Notification{ItemID: it.ID, Outcome: ef.Outcome, Subtitle: it.Title}
	switch ef.Outcome {
	case item.OutcomeReady, item.OutcomeBlocked:
		if data, err := e.store.ReadRunFile(it.ID, ef.Run, item.ResultFile); err == nil {
			if res, err := result.Parse(data); err == nil {
				n.Message = res.Summary
				if ef.Outcome == item.OutcomeBlocked {
					n.Message = res.Gate.Question
				}
			}
		}
	default:
		for _, r := range c.loaded.Runs {
			if r.Number == ef.Run {
				n.Message = r.Error
			}
		}
	}
	deliver, log, ctx := e.deliver, e.log, e.bgCtx
	attrs := []any{"item_id", it.ID, "run", ef.Run, "source", it.Source.Name, "outcome", string(ef.Outcome)}
	e.wg.Go(func() {
		if err := deliver(ctx, n); err != nil {
			log.Warn("notification delivery failed", append(attrs, "error", err.Error())...)
			return
		}
		log.Debug("notification delivered", attrs...)
	})
}
