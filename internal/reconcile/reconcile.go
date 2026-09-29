// Package reconcile implements the alert item lifecycle as a pure function of
// persisted items, source snapshots, synthetic runner/user events and the
// current time.
//
// An item follows its alert (identified by source and fingerprint) through
// these rules:
//
//   - Create: a firing alert without a current item creates one in new, or
//     in snoozed if the alert is suppressed. Unprocessed alerts are ignored.
//   - Automatic preparation: a new item whose alert has been firing for at
//     least prepare_after is queued, once per alert occurrence.
//   - Run started / run finished: the runner moves a queued item to
//     preparing, and a finished run (ready, blocked, failed) moves a
//     preparing item to needs-you. A snoozed or resolved item keeps its state
//     and the result; a done item is never revived.
//   - Snooze: a suppressed alert moves new, queued, preparing and needs-you
//     items to snoozed; a queued run is cancelled, a running run may finish.
//   - Unsnooze: a snoozed item whose alert fires unsuppressed again is
//     restored to preparing (run executing), needs-you (run finished in this
//     occurrence) or new.
//   - Resolve: an alert absent from a successful poll resolves any
//     non-terminal item and cancels a queued run.
//   - Linger expiry: an item resolved for resolved_linger becomes done and a
//     running run is cancelled.
//   - Reopen resolved: a resolved item whose alert fires again is restored
//     like unsnooze and counts a new occurrence.
//   - Reopen done: a done item whose alert fires again within reopen_window
//     of its resolution returns to new as a new occurrence (not dismissed,
//     unseen); after the window a follow-up item linked through
//     previous_item is created instead.
//   - Dismiss: the user moves any non-terminal item to done, cancelling its
//     queued or running run.
//   - Dismissed while firing: a dismissed done item whose alert never
//     resolved stays done; its first absence records the resolution, after
//     which reopen done applies.
//   - Manual run: the user queues a run for a new, needs-you, snoozed or
//     resolved item regardless of prepare_after.
package reconcile

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/source"
)

// Config holds the lifecycle timers.
type Config struct {
	PrepareAfter   time.Duration
	ResolvedLinger time.Duration
	ReopenWindow   time.Duration
}

// ItemState is the persisted evidence of one item.
type ItemState struct {
	Item item.Item
	// Runs is the run metadata of the item.
	Runs []item.Run
	// Alert is the decoded stored alert.json, or nil if unknown.
	Alert *source.Alert
}

// Snapshot is the latest observation of one source.
type Snapshot struct {
	Source string
	// OK is true when the latest poll of the source succeeded. Items of
	// sources without an OK snapshot (failed, never polled or unsupported)
	// are not changed by source-driven rules.
	OK bool
	// ObservedAt is the time of the latest successful poll.
	ObservedAt time.Time
	// Alerts is the full snapshot of the latest successful poll.
	Alerts []source.Alert
}

// EventKind is the kind of a synthetic runner or user event.
type EventKind string

// Event kinds.
const (
	// EventDismiss is a user dismissal.
	EventDismiss EventKind = "dismiss"
	// EventManualRun is a user request for a manual run.
	EventManualRun EventKind = "manual-run"
	// EventRunStarted reports that the runner started a queued run.
	EventRunStarted EventKind = "run-started"
	// EventRunFinished reports that a run ended.
	EventRunFinished EventKind = "run-finished"
)

// Event is a synthetic runner or user event.
type Event struct {
	Kind   EventKind
	ItemID string
	// Run is the run metadata for runner events.
	Run item.Run
}

// Input is the complete, data-only input of a reconciliation pass.
type Input struct {
	Items []ItemState
	// Counters is the highest item number ever used per key (including
	// pruned and unreadable items).
	Counters  map[item.Key]int
	Snapshots []Snapshot
	Events    []Event
	Now       time.Time
	Config    Config
}

// EffectKind is the kind of a side-effect intent.
type EffectKind string

// Effect kinds.
const (
	EffectEnqueue       EffectKind = "enqueue"
	EffectCancelQueued  EffectKind = "cancel-queued"
	EffectCancelRunning EffectKind = "cancel-running"
	EffectNotify        EffectKind = "notify"
)

// Effect is a side-effect intent for the runner or notifier.
type Effect struct {
	Kind   EffectKind
	ItemID string
	// Run is the run to create (enqueue), cancel or notify about.
	Run     int
	Reason  item.RunReason
	Outcome item.Outcome
}

// Change describes the mutation of one item.
type Change struct {
	ItemID string
	// Create is true for a new item.
	Create bool
	// Item is the full item after this pass.
	Item item.Item
	// Appended are the history entries added in this pass.
	Appended []item.HistoryEntry
	// SeenChanged is true when this pass changed Item.Seen (reopening a done
	// item marks it unseen).
	SeenChanged bool
	// RawAlert is the raw alert to write to alert.json, if it changed.
	RawAlert json.RawMessage
	// Runs is run metadata to persist (runner events).
	Runs []item.Run
}

// Transitions returns the state transitions of the change.
func (c Change) Transitions() []item.HistoryEntry {
	var out []item.HistoryEntry
	for _, h := range c.Appended {
		if !h.IsAction() {
			out = append(out, h)
		}
	}
	return out
}

// Ignored is an event that could not be applied.
type Ignored struct {
	Event  Event
	Reason string
}

// Result is the outcome of a reconciliation pass.
type Result struct {
	Changes []Change
	Effects []Effect
	Ignored []Ignored
}

type work struct {
	it          *item.Item
	runs        []item.Run
	stored      *source.Alert
	create      bool
	dirty       bool
	appended    []item.HistoryEntry
	seenChanged bool
	raw         json.RawMessage
	runWrites   []item.Run
}

type pass struct {
	in       Input
	now      time.Time
	works    map[string]*work
	current  map[item.Key]*work
	counters map[item.Key]int
	effects  []Effect
	ignored  []Ignored
}

// Reconcile applies the lifecycle rules. It performs no I/O, does not read the
// clock and never mutates its input.
func Reconcile(in Input) Result {
	p := &pass{
		in:       in,
		now:      in.Now,
		works:    map[string]*work{},
		current:  map[item.Key]*work{},
		counters: map[item.Key]int{},
	}
	maps.Copy(p.counters, in.Counters)

	for _, is := range in.Items {
		w := &work{it: is.Item.Clone()}
		for _, r := range is.Runs {
			w.runs = append(w.runs, r.Clone())
		}
		if is.Alert != nil {
			a := cloneAlert(*is.Alert)
			w.stored = &a
		}
		if w.it.Runs.OccurrenceBase == nil {
			// Migrate a document written before runs.occurrence_base
			// existed: derive the boundary from history/run evidence and
			// persist it, without touching updated_at.
			base := item.DeriveOccurrenceBase(w.it, w.runs)
			w.it.Runs.OccurrenceBase = &base
			w.dirty = true
		}
		p.works[w.it.ID] = w
		if _, n, err := item.ParseID(w.it.ID); err == nil {
			k := w.it.Key()
			p.counters[k] = max(p.counters[k], n)
		}
	}
	// The current item of a key is its highest-numbered extant (loaded)
	// item, independent of the durable counter: the counter only allocates
	// never-used IDs, so pruning or losing a newer item makes the next older
	// present item current again. Unreadable items are not part of the input
	// and only reserve their numbers through the counter.
	currentN := map[item.Key]int{}
	for _, w := range p.works {
		k := w.it.Key()
		if _, n, err := item.ParseID(w.it.ID); err == nil && n > currentN[k] {
			currentN[k] = n
			p.current[k] = w
		}
	}

	for _, ev := range in.Events {
		p.event(ev)
	}

	snapshots := slices.Clone(in.Snapshots)
	slices.SortFunc(snapshots, func(a, b Snapshot) int { return strings.Compare(a.Source, b.Source) })
	for _, s := range snapshots {
		if s.OK {
			p.snapshot(s)
		}
	}

	return p.result()
}

func (p *pass) result() Result {
	var res Result
	ids := slices.Sorted(maps.Keys(p.works))
	for _, id := range ids {
		w := p.works[id]
		if !w.dirty {
			continue
		}
		res.Changes = append(res.Changes, Change{
			ItemID:      id,
			Create:      w.create,
			Item:        *w.it.Clone(),
			Appended:    slices.Clone(w.appended),
			SeenChanged: w.seenChanged,
			RawAlert:    slices.Clone(w.raw),
			Runs:        slices.Clone(w.runWrites),
		})
	}
	res.Effects = p.effects
	res.Ignored = p.ignored
	return res
}

func (p *pass) touch(w *work) {
	w.dirty = true
	w.it.UpdatedAt = p.now
}

func (p *pass) transition(w *work, to item.State, reason string) {
	e := item.HistoryEntry{At: p.now, From: w.it.State, To: to, Reason: reason}
	if item.OccurrenceStarts(w.it.State, to) {
		// A new occurrence starts (creation, or a reopen into new): only runs
		// created from now on count as its preparation.
		base := w.it.Runs.Current
		w.it.Runs.OccurrenceBase = &base
	}
	w.it.State = to
	w.it.History = item.AppendHistory(w.it.History, e)
	w.appended = append(w.appended, e)
	p.touch(w)
}

func (p *pass) action(w *work, a item.Action, run int) {
	e := item.HistoryEntry{At: p.now, Action: a, Run: run}
	w.it.History = item.AppendHistory(w.it.History, e)
	w.appended = append(w.appended, e)
	p.touch(w)
}

func (p *pass) effect(e Effect) { p.effects = append(p.effects, e) }

func (p *pass) ignore(ev Event, reason string) {
	p.ignored = append(p.ignored, Ignored{Event: ev, Reason: reason})
}

// inOccurrence reports whether run r belongs to the current occurrence. The
// boundary is the durable runs.occurrence_base, not the bounded history, so
// history eviction cannot change it.
func inOccurrence(it *item.Item, r item.Run) bool {
	return it.Runs.OccurrenceBase == nil || r.Number > *it.Runs.OccurrenceBase
}

func (w *work) executing() (int, bool) {
	for i := len(w.runs) - 1; i >= 0; i-- {
		if w.runs[i].Outcome == item.OutcomeRunning {
			return w.runs[i].Number, true
		}
	}
	return 0, false
}

// finishedInOccurrence reports whether a finished preparation (ready, blocked
// or failed) exists for the current occurrence.
func (w *work) finishedInOccurrence() bool {
	for _, r := range w.runs {
		if r.Outcome.Finished() && inOccurrence(w.it, r) {
			return true
		}
	}
	return false
}

// startedInOccurrence reports whether any run was started for the current
// occurrence. Queued work cancelled before starting has no run.
func (w *work) startedInOccurrence() bool {
	for _, r := range w.runs {
		if inOccurrence(w.it, r) {
			return true
		}
	}
	return false
}

// restoreTarget is the state restored by unsnoozing or reopening a resolved
// item.
func (w *work) restoreTarget() item.State {
	if _, ok := w.executing(); ok {
		return item.StatePreparing
	}
	if w.finishedInOccurrence() {
		return item.StateNeedsYou
	}
	return item.StateNew
}

func (w *work) setRun(r item.Run) {
	for i := range w.runs {
		if w.runs[i].Number == r.Number {
			w.runs[i] = r.Clone()
			return
		}
	}
	w.runs = append(w.runs, r.Clone())
	slices.SortFunc(w.runs, func(a, b item.Run) int { return a.Number - b.Number })
}

func nonterminal(s item.State) bool { return s != item.StateDone }

func (p *pass) event(ev Event) {
	w, ok := p.works[ev.ItemID]
	if !ok {
		p.ignore(ev, "unknown item")
		return
	}
	it := w.it
	switch ev.Kind {
	case EventDismiss:
		if !nonterminal(it.State) {
			p.ignore(ev, "item is already done")
			return
		}
		wasQueued := it.State == item.StateQueued
		p.action(w, item.ActionDismissed, 0)
		it.Dismissed = true
		p.transition(w, item.StateDone, "dismissed by user")
		if wasQueued {
			it.Runs.PendingReason = nil
			p.effect(Effect{Kind: EffectCancelQueued, ItemID: it.ID, Run: it.Runs.Current + 1})
		}
		if n, ok := w.executing(); ok {
			p.effect(Effect{Kind: EffectCancelRunning, ItemID: it.ID, Run: n})
		}

	case EventManualRun:
		switch it.State {
		case item.StateNew, item.StateNeedsYou, item.StateSnoozed, item.StateResolved:
		default:
			p.ignore(ev, fmt.Sprintf("manual run not allowed in state %s", it.State))
			return
		}
		if _, ok := w.executing(); ok {
			p.ignore(ev, "a run is still executing")
			return
		}
		next := it.Runs.Current + 1
		p.action(w, item.ActionManualRun, next)
		reason := item.ReasonManual
		it.Runs.PendingReason = &reason
		p.transition(w, item.StateQueued, "manual run requested")
		p.effect(Effect{Kind: EffectEnqueue, ItemID: it.ID, Run: next, Reason: item.ReasonManual})

	case EventRunStarted:
		r := ev.Run
		if it.State != item.StateQueued || r.Number != it.Runs.Current+1 {
			p.ignore(ev, "item is not queued for this run")
			return
		}
		if r.Outcome == "" {
			r.Outcome = item.OutcomeRunning
		}
		if r.Reason == "" && it.Runs.PendingReason != nil {
			r.Reason = *it.Runs.PendingReason
		}
		it.Runs.Current = r.Number
		it.Runs.PendingReason = nil
		w.setRun(r)
		w.runWrites = append(w.runWrites, r.Clone())
		p.transition(w, item.StatePreparing, fmt.Sprintf("run %d started", r.Number))

	case EventRunFinished:
		r := ev.Run
		if r.Number < 1 || r.Number > it.Runs.Current {
			p.ignore(ev, "unknown run")
			return
		}
		w.setRun(r)
		w.runWrites = append(w.runWrites, r.Clone())
		p.touch(w)
		if it.State == item.StatePreparing && r.Number == it.Runs.Current && r.Outcome.Finished() {
			p.transition(w, item.StateNeedsYou, fmt.Sprintf("run %d %s", r.Number, r.Outcome))
			p.effect(Effect{Kind: EffectNotify, ItemID: it.ID, Run: r.Number, Outcome: r.Outcome})
		}

	default:
		p.ignore(ev, "unsupported event")
	}
}

func (p *pass) snapshot(s Snapshot) {
	observed := map[string]source.Alert{}
	for _, a := range s.Alerts {
		observed[a.Fingerprint] = a
	}

	keys := map[item.Key]bool{}
	for fp := range observed {
		keys[item.Key{Source: s.Source, Fingerprint: fp}] = true
	}
	for k := range p.current {
		if k.Source == s.Source {
			keys[k] = true
		}
	}
	sorted := slices.SortedFunc(maps.Keys(keys), func(a, b item.Key) int {
		return strings.Compare(a.Fingerprint, b.Fingerprint)
	})

	for _, k := range sorted {
		a, present := observed[k.Fingerprint]
		if present && a.Unprocessed() {
			continue // neutral: neither creates nor resolves
		}
		w := p.current[k]
		switch {
		case w == nil && present:
			p.create(k, a, s.ObservedAt, nil, "alert firing")
		case w == nil:
		case present:
			p.present(w, a, s.ObservedAt)
		default:
			p.absent(w)
		}
	}
}

// create creates an item (for a new alert, or as the follow-up of a done item
// reopened after the reopen window) and queues automatic preparation when the
// alert is active and already eligible.
func (p *pass) create(k item.Key, a source.Alert, observedAt time.Time, previous *string, reason string) {
	n := p.counters[k] + 1
	id, err := item.NewID(k, n)
	if err != nil {
		return // invalid keys are rejected by the source; never create unsafe IDs
	}
	p.counters[k] = n
	it := &item.Item{
		Version:      item.Version,
		ID:           id,
		Type:         item.TypeAlert,
		CreatedAt:    p.now,
		Source:       item.SourceRef{Name: k.Source, Fingerprint: k.Fingerprint},
		Alert:        item.AlertInfo{Occurrences: 1},
		Runs:         item.RunsInfo{OccurrenceBase: new(int)},
		PreviousItem: previous,
	}
	w := &work{it: it, create: true}
	p.works[id] = w
	p.current[k] = w
	p.refresh(w, a, observedAt)
	to := item.StateNew
	if a.Suppressed() {
		to = item.StateSnoozed
		reason = "alert suppressed"
	}
	p.transition(w, to, reason)
	p.maybePrepare(w, a)
}

// refresh updates the alert metadata of an item from a present observation.
// It never changes the lifecycle state.
func (p *pass) refresh(w *work, a source.Alert, observedAt time.Time) {
	it := w.it
	before := it.Clone()
	status := item.AlertActive
	if a.Suppressed() {
		status = item.AlertSuppressed
	}
	it.Alert.Status = status
	it.Alert.Labels = maps.Clone(a.Labels)
	it.Alert.StartsAt = a.StartsAt
	it.Alert.LastSeenAt = observedAt
	it.Alert.GeneratorURL = a.GeneratorURL
	it.Alert.RunbookURL = a.Annotations["runbook_url"]
	it.Title = item.Title(a.Labels)
	it.Severity = a.Labels["severity"]
	if status != item.AlertResolved {
		it.Alert.ResolvedAt = nil
	}
	if !sameMeta(before, it) {
		p.touch(w)
	}
	if w.stored == nil || alertChanged(*w.stored, a) {
		w.raw = slices.Clone(a.Raw)
		c := cloneAlert(a)
		w.stored = &c
		p.touch(w)
	}
}

func sameMeta(a, b *item.Item) bool {
	return a.Alert.Status == b.Alert.Status &&
		maps.Equal(a.Alert.Labels, b.Alert.Labels) &&
		a.Alert.StartsAt.Equal(b.Alert.StartsAt) &&
		a.Alert.LastSeenAt.Equal(b.Alert.LastSeenAt) &&
		a.Alert.GeneratorURL == b.Alert.GeneratorURL &&
		a.Alert.RunbookURL == b.Alert.RunbookURL &&
		a.Title == b.Title &&
		a.Severity == b.Severity &&
		(a.Alert.ResolvedAt == nil) == (b.Alert.ResolvedAt == nil)
}

// alertChanged reports whether labels, annotations or status differ.
func alertChanged(old, cur source.Alert) bool {
	return !maps.Equal(old.Labels, cur.Labels) ||
		!maps.Equal(old.Annotations, cur.Annotations) ||
		old.State != cur.State ||
		!slices.Equal(old.SilencedBy, cur.SilencedBy) ||
		!slices.Equal(old.InhibitedBy, cur.InhibitedBy)
}

func cloneAlert(a source.Alert) source.Alert {
	c := a
	c.Labels = maps.Clone(a.Labels)
	c.Annotations = maps.Clone(a.Annotations)
	c.SilencedBy = slices.Clone(a.SilencedBy)
	c.InhibitedBy = slices.Clone(a.InhibitedBy)
	c.Receivers = slices.Clone(a.Receivers)
	c.Raw = slices.Clone(a.Raw)
	return c
}

// maybePrepare queues automatic preparation when the item is due for it.
func (p *pass) maybePrepare(w *work, a source.Alert) {
	it := w.it
	if it.State != item.StateNew || !a.Active() || it.Runs.PendingReason != nil {
		return
	}
	if w.startedInOccurrence() {
		return
	}
	age := p.now.Sub(it.Alert.StartsAt)
	if age < p.in.Config.PrepareAfter {
		return
	}
	reason := item.ReasonAuto
	it.Runs.PendingReason = &reason
	p.transition(w, item.StateQueued, fmt.Sprintf("firing for %s >= %s", age.Round(time.Second), p.in.Config.PrepareAfter))
	p.effect(Effect{Kind: EffectEnqueue, ItemID: it.ID, Run: it.Runs.Current + 1, Reason: item.ReasonAuto})
}

// present applies the rules for an item whose alert is in the snapshot as
// active or suppressed.
func (p *pass) present(w *work, a source.Alert, observedAt time.Time) {
	it := w.it
	prevStatus := it.Alert.Status

	switch it.State {
	case item.StateDone:
		if prevStatus != item.AlertResolved || it.Alert.ResolvedAt == nil {
			// Dismissed while firing: no resolution since dismissal, so only
			// refresh metadata.
			p.refresh(w, a, observedAt)
			return
		}
		if !a.Active() {
			return // a suppressed re-fire of a resolved done item is left alone
		}
		if p.now.Sub(*it.Alert.ResolvedAt) <= p.in.Config.ReopenWindow {
			// Reopen within the reopen window.
			p.refresh(w, a, observedAt)
			it.Alert.Occurrences++
			it.Alert.ResolvedAt = nil
			it.Dismissed = false
			it.Seen = false
			w.seenChanged = true
			p.transition(w, item.StateNew, "alert firing again within the reopen window")
			p.maybePrepare(w, a)
			return
		}
		// Reopened after the reopen window: create a follow-up item.
		prev := it.ID
		p.create(it.Key(), a, observedAt, &prev, fmt.Sprintf("alert firing again after the reopen window (previous item %s)", prev))

	case item.StateResolved:
		if !a.Active() {
			return // suppressed while resolved: no rule applies, linger paused
		}
		// Reopen a resolved item.
		p.refresh(w, a, observedAt)
		it.Alert.Occurrences++
		it.Alert.ResolvedAt = nil
		p.transition(w, w.restoreTarget(), "alert firing again")
		p.maybePrepare(w, a)

	case item.StateSnoozed:
		p.refresh(w, a, observedAt)
		if a.Active() {
			// Unsnooze.
			p.transition(w, w.restoreTarget(), "alert no longer suppressed")
			p.maybePrepare(w, a)
		}

	default: // new, queued, preparing, needs-you
		p.refresh(w, a, observedAt)
		if a.Suppressed() && prevStatus != item.AlertSuppressed {
			// Snooze on a new suppression only; an unchanged suppression
			// keeps a manual run requested from snoozed.
			wasQueued := it.State == item.StateQueued
			p.transition(w, item.StateSnoozed, "alert suppressed")
			if wasQueued {
				it.Runs.PendingReason = nil
				p.effect(Effect{Kind: EffectCancelQueued, ItemID: it.ID, Run: it.Runs.Current + 1})
			}
			return
		}
		p.maybePrepare(w, a)
	}
}

// absent applies the rules for an item whose alert is not in a successful
// snapshot of its source.
func (p *pass) absent(w *work) {
	it := w.it
	if it.Alert.Status != item.AlertResolved {
		now := p.now
		it.Alert.Status = item.AlertResolved
		it.Alert.ResolvedAt = &now
		if it.State == item.StateDone {
			// Dismissed while firing: record the resolution, stay done.
			p.touch(w)
			return
		}
		// Resolve.
		wasQueued := it.State == item.StateQueued
		p.transition(w, item.StateResolved, "alert no longer firing")
		if wasQueued {
			it.Runs.PendingReason = nil
			p.effect(Effect{Kind: EffectCancelQueued, ItemID: it.ID, Run: it.Runs.Current + 1})
		}
		return
	}
	// Linger expiry.
	if it.State != item.StateResolved || it.Alert.ResolvedAt == nil {
		return
	}
	since := p.now.Sub(*it.Alert.ResolvedAt)
	if since < p.in.Config.ResolvedLinger {
		return
	}
	p.transition(w, item.StateDone, fmt.Sprintf("resolved for %s >= %s", since.Round(time.Second), p.in.Config.ResolvedLinger))
	if n, ok := w.executing(); ok {
		p.effect(Effect{Kind: EffectCancelRunning, ItemID: it.ID, Run: n})
	}
}
