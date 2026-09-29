package reconcile

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/source"
)

var (
	t0  = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cfg = Config{PrepareAfter: 5 * time.Minute, ResolvedLinger: 4 * time.Hour, ReopenWindow: 24 * time.Hour}
)

// alert builds a source alert through the real decoder so it carries a raw
// payload. state is active, suppressed (silenced) or unprocessed.
func alert(t *testing.T, src, fp, state string, startsAt time.Time, labels map[string]string) source.Alert {
	t.Helper()
	if labels == nil {
		labels = map[string]string{"alertname": "HighLatency", "namespace": "core", "pod": "api-1", "severity": "critical"}
	}
	status := map[string]any{"state": state, "silencedBy": []string{}, "inhibitedBy": []string{}}
	if state == "suppressed" {
		status["silencedBy"] = []string{"silence-1"}
	}
	raw, err := json.Marshal(map[string]any{
		"fingerprint":  fp,
		"labels":       labels,
		"annotations":  map[string]string{"runbook_url": "https://runbooks/x"},
		"startsAt":     startsAt,
		"updatedAt":    startsAt,
		"endsAt":       startsAt.Add(time.Hour),
		"generatorURL": "https://gen/x",
		"status":       status,
		"receivers":    []map[string]string{{"name": "team"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := source.DecodeAlert(src, raw)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func snap(src string, at time.Time, alerts ...source.Alert) Snapshot {
	return Snapshot{Source: src, OK: true, ObservedAt: at, Alerts: alerts}
}

// sim applies reconciliation results the way the engine persists them.
type sim struct {
	t        *testing.T
	items    map[string]*ItemState
	counters map[item.Key]int
	cfg      Config
}

func newSim(t *testing.T) *sim {
	return &sim{t: t, items: map[string]*ItemState{}, counters: map[item.Key]int{}, cfg: cfg}
}

func (s *sim) input(now time.Time, snaps []Snapshot, events []Event) Input {
	in := Input{Counters: map[item.Key]int{}, Now: now, Config: s.cfg}
	for k, v := range s.counters {
		in.Counters[k] = v
	}
	ids := make([]string, 0, len(s.items))
	for id := range s.items {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		st := s.items[id]
		c := ItemState{Item: *st.Item.Clone()}
		for _, r := range st.Runs {
			c.Runs = append(c.Runs, r.Clone())
		}
		if st.Alert != nil {
			a := cloneAlert(*st.Alert)
			c.Alert = &a
		}
		in.Items = append(in.Items, c)
	}
	for _, sn := range snaps {
		c := sn
		c.Alerts = nil
		for _, a := range sn.Alerts {
			c.Alerts = append(c.Alerts, cloneAlert(a))
		}
		in.Snapshots = append(in.Snapshots, c)
	}
	for _, e := range events {
		e.Run = e.Run.Clone()
		in.Events = append(in.Events, e)
	}
	return in
}

// step reconciles, asserts purity and applies the result.
func (s *sim) step(now time.Time, snaps []Snapshot, events ...Event) Result {
	s.t.Helper()
	in := s.input(now, snaps, events)
	pristine := s.input(now, snaps, events)
	res := Reconcile(in)
	if !reflect.DeepEqual(in, pristine) {
		s.t.Fatal("Reconcile mutated its input")
	}
	again := Reconcile(s.input(now, snaps, events))
	if !reflect.DeepEqual(res, again) {
		s.t.Fatal("Reconcile is not deterministic")
	}
	for _, c := range res.Changes {
		st, ok := s.items[c.ItemID]
		if c.Create {
			if ok {
				s.t.Fatalf("created existing item %s", c.ItemID)
			}
			st = &ItemState{}
			s.items[c.ItemID] = st
		} else if !ok {
			s.t.Fatalf("change for unknown item %s", c.ItemID)
		}
		st.Item = *c.Item.Clone()
		for _, r := range c.Runs {
			replaced := false
			for i := range st.Runs {
				if st.Runs[i].Number == r.Number {
					st.Runs[i] = r.Clone()
					replaced = true
				}
			}
			if !replaced {
				st.Runs = append(st.Runs, r.Clone())
			}
		}
		if c.RawAlert != nil {
			a, err := source.DecodeAlert(c.Item.Source.Name, c.RawAlert)
			if err != nil {
				s.t.Fatal(err)
			}
			st.Alert = &a
		}
		k, n, err := item.ParseID(c.ItemID)
		if err != nil {
			s.t.Fatal(err)
		}
		s.counters[k] = max(s.counters[k], n)
	}
	return res
}

func (s *sim) get(id string) *item.Item {
	s.t.Helper()
	st, ok := s.items[id]
	if !ok {
		s.t.Fatalf("no item %s", id)
	}
	return &st.Item
}

// assertStable reconciles the same evidence again and asserts that nothing
// changes (idempotence).
func (s *sim) assertStable(now time.Time, snaps ...Snapshot) {
	s.t.Helper()
	res := s.step(now, snaps)
	if len(res.Changes) != 0 || len(res.Effects) != 0 {
		s.t.Fatalf("not idempotent: changes=%+v effects=%+v", res.Changes, res.Effects)
	}
}

func effects(res Result, kind EffectKind) []Effect {
	var out []Effect
	for _, e := range res.Effects {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func transitions(res Result, id string) []item.HistoryEntry {
	for _, c := range res.Changes {
		if c.ItemID == id {
			return c.Transitions()
		}
	}
	return nil
}

func ev(kind EventKind, id string) Event { return Event{Kind: kind, ItemID: id} }

func started(id string, n int, at time.Time) Event {
	return Event{Kind: EventRunStarted, ItemID: id, Run: item.Run{Number: n, QueuedAt: at, StartedAt: &at, Outcome: item.OutcomeRunning}}
}

func finished(id string, n int, queued, at time.Time, o item.Outcome, reason item.RunReason) Event {
	return Event{Kind: EventRunFinished, ItemID: id, Run: item.Run{Number: n, Reason: reason, QueuedAt: queued, StartedAt: &queued, FinishedAt: &at, Outcome: o}}
}

const id1 = "alert-dev-fp1-1"

func wantState(t *testing.T, it *item.Item, want item.State) {
	t.Helper()
	if it.State != want {
		t.Fatalf("%s: state %s, want %s", it.ID, it.State, want)
	}
}

// newYoung creates an active item that is not yet eligible for preparation.
func newYoung(t *testing.T) (*sim, source.Alert) {
	s := newSim(t)
	a := alert(t, "dev", "fp1", "active", t0, nil)
	s.step(t0, []Snapshot{snap("dev", t0, a)})
	wantState(t, s.get(id1), item.StateNew)
	return s, a
}

// queuedItem creates an item queued automatically at t0.
func queuedItem(t *testing.T) (*sim, source.Alert) {
	s := newSim(t)
	a := alert(t, "dev", "fp1", "active", t0.Add(-time.Hour), nil)
	s.step(t0, []Snapshot{snap("dev", t0, a)})
	wantState(t, s.get(id1), item.StateQueued)
	return s, a
}

// preparingItem creates an item with run 1 executing.
func preparingItem(t *testing.T) (*sim, source.Alert) {
	s, a := queuedItem(t)
	s.step(t0.Add(time.Second), nil, started(id1, 1, t0.Add(time.Second)))
	wantState(t, s.get(id1), item.StatePreparing)
	return s, a
}

// needsYouItem creates an item whose run 1 finished ready.
func needsYouItem(t *testing.T) (*sim, source.Alert) {
	s, a := preparingItem(t)
	s.step(t0.Add(time.Minute), nil, finished(id1, 1, t0.Add(time.Second), t0.Add(time.Minute), item.OutcomeReady, item.ReasonAuto))
	wantState(t, s.get(id1), item.StateNeedsYou)
	return s, a
}

func snoozedItem(t *testing.T) (*sim, source.Alert) {
	s, a := newYoung(t)
	sup := alert(t, "dev", "fp1", "suppressed", t0, nil)
	s.step(t0.Add(time.Second), []Snapshot{snap("dev", t0.Add(time.Second), sup)})
	wantState(t, s.get(id1), item.StateSnoozed)
	return s, a
}

func resolvedItem(t *testing.T) (*sim, source.Alert) {
	s, a := newYoung(t)
	s.step(t0.Add(time.Second), []Snapshot{snap("dev", t0.Add(time.Second))})
	wantState(t, s.get(id1), item.StateResolved)
	return s, a
}

func TestT1Create(t *testing.T) {
	t.Run("T1 active creates new", func(t *testing.T) {
		s := newSim(t)
		a := alert(t, "dev", "fp1", "active", t0, nil)
		res := s.step(t0, []Snapshot{snap("dev", t0, a)})
		if len(res.Changes) != 1 || !res.Changes[0].Create || len(res.Effects) != 0 {
			t.Fatalf("result = %+v", res)
		}
		if string(res.Changes[0].RawAlert) != string(a.Raw) {
			t.Fatal("raw alert not persisted on creation")
		}
		it := s.get(id1)
		wantState(t, it, item.StateNew)
		if it.Version != 1 || it.Type != "alert" || it.Title != "HighLatency: core/api-1" || it.Severity != "critical" ||
			it.Alert.RunbookURL != "https://runbooks/x" || it.Alert.GeneratorURL != "https://gen/x" ||
			it.Alert.Occurrences != 1 || it.Alert.Status != item.AlertActive || !it.Alert.StartsAt.Equal(t0) ||
			!it.Alert.LastSeenAt.Equal(t0) || it.Alert.ResolvedAt != nil || it.PreviousItem != nil || it.Seen ||
			!it.CreatedAt.Equal(t0) || !it.UpdatedAt.Equal(t0) {
			t.Fatalf("item = %+v", it)
		}
		if len(it.History) != 1 || it.History[0].From != "" || it.History[0].To != item.StateNew {
			t.Fatalf("history = %+v", it.History)
		}
		s.assertStable(t0, snap("dev", t0, a))
	})
	t.Run("T1 suppressed creates snoozed without preparation", func(t *testing.T) {
		s := newSim(t)
		res := s.step(t0, []Snapshot{snap("dev", t0, alert(t, "dev", "fp1", "suppressed", t0.Add(-time.Hour), nil))})
		it := s.get(id1)
		wantState(t, it, item.StateSnoozed)
		if it.Alert.Status != item.AlertSuppressed || len(res.Effects) != 0 || len(it.History) != 1 || it.History[0].From != "" {
			t.Fatalf("item = %+v effects = %+v", it, res.Effects)
		}
	})
	t.Run("T1 inhibited despite active state creates snoozed", func(t *testing.T) {
		s := newSim(t)
		a := alert(t, "dev", "fp1", "active", t0.Add(-time.Hour), nil)
		a.InhibitedBy = []string{"x"}
		res := s.step(t0, []Snapshot{snap("dev", t0, a)})
		wantState(t, s.get(id1), item.StateSnoozed)
		if len(res.Effects) != 0 {
			t.Fatal("suppressed alert must not be prepared")
		}
	})
	t.Run("T1 unprocessed creates nothing", func(t *testing.T) {
		s := newSim(t)
		res := s.step(t0, []Snapshot{snap("dev", t0, alert(t, "dev", "fp1", "unprocessed", t0.Add(-time.Hour), nil))})
		if len(res.Changes) != 0 || len(s.items) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("T1 uses next never-used id", func(t *testing.T) {
		s := newSim(t)
		s.counters[item.Key{Source: "dev", Fingerprint: "fp1"}] = 7 // pruned or unreadable items 1..7
		s.step(t0, []Snapshot{snap("dev", t0, alert(t, "dev", "fp1", "active", t0, nil))})
		it := s.get("alert-dev-fp1-8")
		if it.PreviousItem != nil {
			t.Fatal("no previous_item when the prior item is not present")
		}
	})
	t.Run("T1 isolates keys by source", func(t *testing.T) {
		s := newSim(t)
		s.step(t0, []Snapshot{
			snap("dev", t0, alert(t, "dev", "fp1", "active", t0, nil)),
			snap("prod", t0, alert(t, "prod", "fp1", "active", t0, nil)),
		})
		s.get("alert-dev-fp1-1")
		s.get("alert-prod-fp1-1")
		// Absence in prod resolves only prod's item.
		s.step(t0.Add(time.Second), []Snapshot{snap("prod", t0.Add(time.Second))})
		wantState(t, s.get("alert-dev-fp1-1"), item.StateNew)
		wantState(t, s.get("alert-prod-fp1-1"), item.StateResolved)
	})
}

func TestT2AutomaticPreparation(t *testing.T) {
	t.Run("T1+T2 in one pass for an already-old alert", func(t *testing.T) {
		s := newSim(t)
		a := alert(t, "dev", "fp1", "active", t0.Add(-time.Hour), nil)
		res := s.step(t0, []Snapshot{snap("dev", t0, a)})
		it := s.get(id1)
		wantState(t, it, item.StateQueued)
		tr := transitions(res, id1)
		if len(tr) != 2 || tr[0].To != item.StateNew || tr[1].From != item.StateNew || tr[1].To != item.StateQueued {
			t.Fatalf("transitions = %+v", tr)
		}
		enq := effects(res, EffectEnqueue)
		if len(enq) != 1 || enq[0].Run != 1 || enq[0].Reason != item.ReasonAuto || *it.Runs.PendingReason != item.ReasonAuto || it.Runs.Current != 0 {
			t.Fatalf("enqueue = %+v item runs = %+v", enq, it.Runs)
		}
		s.assertStable(t0, snap("dev", t0, a))
		// Further polls and ticks never enqueue again.
		for i := 1; i <= 5; i++ {
			res := s.step(t0.Add(time.Duration(i)*time.Minute), []Snapshot{snap("dev", t0.Add(time.Duration(i)*time.Minute), a)})
			if len(res.Effects) != 0 || len(transitions(res, id1)) != 0 {
				t.Fatalf("repeated preparation: %+v", res)
			}
		}
	})
	t.Run("T2 threshold boundary on timer ticks", func(t *testing.T) {
		s, a := newYoung(t)
		poll := snap("dev", t0, a)
		// Tick just before the threshold: nothing.
		res := s.step(t0.Add(5*time.Minute-time.Second), []Snapshot{poll})
		if len(res.Changes) != 0 {
			t.Fatalf("changes before threshold: %+v", res.Changes)
		}
		// Tick at exactly the threshold (equality belongs to reached).
		res = s.step(t0.Add(5*time.Minute), []Snapshot{poll})
		it := s.get(id1)
		wantState(t, it, item.StateQueued)
		if len(effects(res, EffectEnqueue)) != 1 {
			t.Fatal("want one enqueue")
		}
		// A timer tick does not advance last_seen_at.
		if !it.Alert.LastSeenAt.Equal(t0) {
			t.Fatalf("last_seen_at = %v", it.Alert.LastSeenAt)
		}
	})
	t.Run("T2 measured from current starts_at", func(t *testing.T) {
		s, _ := newYoung(t)
		later := alert(t, "dev", "fp1", "active", t0.Add(3*time.Minute), nil)
		s.step(t0.Add(6*time.Minute), []Snapshot{snap("dev", t0.Add(6*time.Minute), later)})
		wantState(t, s.get(id1), item.StateNew)
		s.step(t0.Add(8*time.Minute), []Snapshot{snap("dev", t0.Add(8*time.Minute), later)})
		wantState(t, s.get(id1), item.StateQueued)
	})
	t.Run("T2 at most once after a completed run", func(t *testing.T) {
		s, a := needsYouItem(t)
		// Suppress and reactivate: restores needs-you, no new enqueue.
		sup := alert(t, "dev", "fp1", "suppressed", a.StartsAt, nil)
		s.step(t0.Add(2*time.Minute), []Snapshot{snap("dev", t0.Add(2*time.Minute), sup)})
		res := s.step(t0.Add(3*time.Minute), []Snapshot{snap("dev", t0.Add(3*time.Minute), a)})
		wantState(t, s.get(id1), item.StateNeedsYou)
		if len(res.Effects) != 0 {
			t.Fatalf("effects = %+v", res.Effects)
		}
	})
	t.Run("T2 blocked while a run of the occurrence exists even if interrupted", func(t *testing.T) {
		s, a := preparingItem(t)
		sup := alert(t, "dev", "fp1", "suppressed", a.StartsAt, nil)
		s.step(t0.Add(2*time.Second), []Snapshot{snap("dev", t0.Add(2*time.Second), sup)})
		s.step(t0.Add(3*time.Second), nil, finished(id1, 1, t0.Add(time.Second), t0.Add(3*time.Second), item.OutcomeInterrupted, item.ReasonAuto))
		res := s.step(t0.Add(4*time.Second), []Snapshot{snap("dev", t0.Add(4*time.Second), a)})
		wantState(t, s.get(id1), item.StateNew)
		if len(res.Effects) != 0 {
			t.Fatalf("automatic preparation repeated: %+v", res.Effects)
		}
	})
}

func TestT3RunStarted(t *testing.T) {
	s, _ := queuedItem(t)
	res := s.step(t0.Add(time.Second), nil, started(id1, 2, t0))
	if len(res.Ignored) != 1 || len(res.Changes) != 0 {
		t.Fatalf("wrong run number must be ignored: %+v", res)
	}
	res = s.step(t0.Add(time.Second), nil, started(id1, 1, t0))
	it := s.get(id1)
	wantState(t, it, item.StatePreparing)
	if it.Runs.Current != 1 || it.Runs.PendingReason != nil {
		t.Fatalf("runs = %+v", it.Runs)
	}
	c := res.Changes[0]
	if len(c.Runs) != 1 || c.Runs[0].Number != 1 || c.Runs[0].Outcome != item.OutcomeRunning || c.Runs[0].Reason != item.ReasonAuto {
		t.Fatalf("run evidence = %+v", c.Runs)
	}
	res = s.step(t0.Add(2*time.Second), nil, started(id1, 2, t0))
	if len(res.Ignored) != 1 {
		t.Fatal("start for a non-queued item must be ignored")
	}
}

func TestT4RunFinished(t *testing.T) {
	for _, o := range []item.Outcome{item.OutcomeReady, item.OutcomeBlocked, item.OutcomeFailed} {
		t.Run("T4 preparing "+string(o), func(t *testing.T) {
			s, _ := preparingItem(t)
			res := s.step(t0.Add(time.Minute), nil, finished(id1, 1, t0, t0.Add(time.Minute), o, item.ReasonAuto))
			wantState(t, s.get(id1), item.StateNeedsYou)
			n := effects(res, EffectNotify)
			if len(n) != 1 || n[0].Run != 1 || n[0].Outcome != o {
				t.Fatalf("notify = %+v", n)
			}
			if s.items[id1].Runs[0].Outcome != o {
				t.Fatal("outcome not saved")
			}
		})
		t.Run("T4 snoozed retains "+string(o), func(t *testing.T) {
			s, a := preparingItem(t)
			sup := alert(t, "dev", "fp1", "suppressed", a.StartsAt, nil)
			res := s.step(t0.Add(2*time.Second), []Snapshot{snap("dev", t0.Add(2*time.Second), sup)})
			wantState(t, s.get(id1), item.StateSnoozed)
			if len(res.Effects) != 0 {
				t.Fatalf("suppression must not cancel executing work: %+v", res.Effects)
			}
			res = s.step(t0.Add(time.Minute), nil, finished(id1, 1, t0, t0.Add(time.Minute), o, item.ReasonAuto))
			wantState(t, s.get(id1), item.StateSnoozed)
			if len(res.Effects) != 0 || len(transitions(res, id1)) != 0 || s.items[id1].Runs[0].Outcome != o {
				t.Fatalf("result = %+v", res)
			}
		})
		t.Run("T4 resolved retains "+string(o), func(t *testing.T) {
			s, _ := preparingItem(t)
			res := s.step(t0.Add(2*time.Second), []Snapshot{snap("dev", t0.Add(2*time.Second))})
			wantState(t, s.get(id1), item.StateResolved)
			if len(res.Effects) != 0 {
				t.Fatalf("resolution must not cancel executing work: %+v", res.Effects)
			}
			res = s.step(t0.Add(time.Minute), nil, finished(id1, 1, t0, t0.Add(time.Minute), o, item.ReasonAuto))
			wantState(t, s.get(id1), item.StateResolved)
			if len(res.Effects) != 0 || s.items[id1].Runs[0].Outcome != o {
				t.Fatalf("result = %+v", res)
			}
		})
	}
	t.Run("T4 does not revive done", func(t *testing.T) {
		s, _ := preparingItem(t)
		s.step(t0.Add(2*time.Second), nil, ev(EventDismiss, id1))
		res := s.step(t0.Add(time.Minute), nil, finished(id1, 1, t0, t0.Add(time.Minute), item.OutcomeCancelled, item.ReasonAuto))
		wantState(t, s.get(id1), item.StateDone)
		if len(res.Effects) != 0 || s.items[id1].Runs[0].Outcome != item.OutcomeCancelled {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("T4 not for cancelled or interrupted", func(t *testing.T) {
		for _, o := range []item.Outcome{item.OutcomeCancelled, item.OutcomeInterrupted} {
			s, _ := preparingItem(t)
			res := s.step(t0.Add(time.Minute), nil, finished(id1, 1, t0, t0.Add(time.Minute), o, item.ReasonAuto))
			wantState(t, s.get(id1), item.StatePreparing)
			if len(res.Effects) != 0 {
				t.Fatalf("%s: effects = %+v", o, res.Effects)
			}
		}
	})
	t.Run("T4 unknown run ignored", func(t *testing.T) {
		s, _ := preparingItem(t)
		res := s.step(t0.Add(time.Minute), nil, finished(id1, 5, t0, t0.Add(time.Minute), item.OutcomeReady, item.ReasonAuto))
		if len(res.Ignored) != 1 || len(res.Changes) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestT5Suppression(t *testing.T) {
	cases := []struct {
		name         string
		setup        func(t *testing.T) (*sim, source.Alert)
		cancelQueued bool
	}{
		{"new", newYoung, false},
		{"queued", queuedItem, true},
		{"preparing", preparingItem, false},
		{"needs-you", needsYouItem, false},
	}
	for _, c := range cases {
		t.Run("T5 from "+c.name, func(t *testing.T) {
			s, a := c.setup(t)
			now := t0.Add(2 * time.Minute)
			sup := alert(t, "dev", "fp1", "suppressed", a.StartsAt, nil)
			res := s.step(now, []Snapshot{snap("dev", now, sup)})
			it := s.get(id1)
			wantState(t, it, item.StateSnoozed)
			if it.Alert.Status != item.AlertSuppressed || res.Changes[0].RawAlert == nil {
				t.Fatal("suppressed status/raw alert not updated")
			}
			cq := effects(res, EffectCancelQueued)
			if c.cancelQueued != (len(cq) == 1) || len(effects(res, EffectCancelRunning)) != 0 {
				t.Fatalf("effects = %+v", res.Effects)
			}
			if c.cancelQueued && (cq[0].Run != 1 || it.Runs.PendingReason != nil) {
				t.Fatalf("cancel = %+v pending = %v", cq, it.Runs.PendingReason)
			}
			s.assertStable(now, snap("dev", now, sup))
		})
	}
	t.Run("T5 then T6 re-enqueues cancelled queued work", func(t *testing.T) {
		s, a := queuedItem(t)
		sup := alert(t, "dev", "fp1", "suppressed", a.StartsAt, nil)
		s.step(t0.Add(time.Second), []Snapshot{snap("dev", t0.Add(time.Second), sup)})
		res := s.step(t0.Add(2*time.Second), []Snapshot{snap("dev", t0.Add(2*time.Second), a)})
		wantState(t, s.get(id1), item.StateQueued)
		if enq := effects(res, EffectEnqueue); len(enq) != 1 || enq[0].Run != 1 {
			t.Fatalf("effects = %+v", res.Effects)
		}
	})
}

func TestT6Unsnooze(t *testing.T) {
	t.Run("T6 restores preparing for executing run", func(t *testing.T) {
		s, a := preparingItem(t)
		sup := alert(t, "dev", "fp1", "suppressed", a.StartsAt, nil)
		s.step(t0.Add(2*time.Second), []Snapshot{snap("dev", t0.Add(2*time.Second), sup)})
		res := s.step(t0.Add(3*time.Second), []Snapshot{snap("dev", t0.Add(3*time.Second), a)})
		wantState(t, s.get(id1), item.StatePreparing)
		if len(res.Effects) != 0 {
			t.Fatalf("duplicate run: %+v", res.Effects)
		}
		// The retained run completes normally.
		res = s.step(t0.Add(time.Minute), nil, finished(id1, 1, t0, t0.Add(time.Minute), item.OutcomeBlocked, item.ReasonAuto))
		wantState(t, s.get(id1), item.StateNeedsYou)
		if len(effects(res, EffectNotify)) != 1 {
			t.Fatal("want notify")
		}
	})
	for _, o := range []item.Outcome{item.OutcomeReady, item.OutcomeBlocked, item.OutcomeFailed} {
		t.Run("T6 restores needs-you for finished "+string(o), func(t *testing.T) {
			s, a := preparingItem(t)
			sup := alert(t, "dev", "fp1", "suppressed", a.StartsAt, nil)
			s.step(t0.Add(2*time.Second), []Snapshot{snap("dev", t0.Add(2*time.Second), sup)})
			s.step(t0.Add(time.Minute), nil, finished(id1, 1, t0, t0.Add(time.Minute), o, item.ReasonAuto))
			res := s.step(t0.Add(2*time.Minute), []Snapshot{snap("dev", t0.Add(2*time.Minute), a)})
			wantState(t, s.get(id1), item.StateNeedsYou)
			if len(res.Effects) != 0 {
				t.Fatalf("effects = %+v", res.Effects)
			}
		})
	}
	t.Run("T6 restores new without preparation", func(t *testing.T) {
		s, a := snoozedItem(t)
		now := t0.Add(2 * time.Second)
		res := s.step(now, []Snapshot{snap("dev", now, a)})
		wantState(t, s.get(id1), item.StateNew)
		if len(res.Effects) != 0 {
			t.Fatal("young alert must not be prepared")
		}
		// It becomes eligible once the threshold is reached.
		res = s.step(t0.Add(5*time.Minute), []Snapshot{snap("dev", now, a)})
		wantState(t, s.get(id1), item.StateQueued)
		if len(effects(res, EffectEnqueue)) != 1 {
			t.Fatal("want enqueue")
		}
	})
	t.Run("T6 snoozed stays snoozed while still suppressed", func(t *testing.T) {
		s, _ := snoozedItem(t)
		sup := alert(t, "dev", "fp1", "suppressed", t0, nil)
		res := s.step(t0.Add(time.Hour), []Snapshot{snap("dev", t0.Add(time.Hour), sup)})
		wantState(t, s.get(id1), item.StateSnoozed)
		if len(transitions(res, id1)) != 0 || len(res.Effects) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestT7Resolution(t *testing.T) {
	cases := []struct {
		name         string
		setup        func(t *testing.T) (*sim, source.Alert)
		cancelQueued bool
	}{
		{"new", newYoung, false},
		{"queued", queuedItem, true},
		{"preparing", preparingItem, false},
		{"needs-you", needsYouItem, false},
		{"snoozed", snoozedItem, false},
	}
	for _, c := range cases {
		t.Run("T7 from "+c.name, func(t *testing.T) {
			s, _ := c.setup(t)
			now := t0.Add(2 * time.Minute)
			res := s.step(now, []Snapshot{snap("dev", now)}) // successful empty snapshot
			it := s.get(id1)
			wantState(t, it, item.StateResolved)
			if it.Alert.Status != item.AlertResolved || it.Alert.ResolvedAt == nil || !it.Alert.ResolvedAt.Equal(now) {
				t.Fatalf("alert = %+v", it.Alert)
			}
			if res.Changes[0].RawAlert != nil {
				t.Fatal("absence must not supply a raw alert")
			}
			if c.cancelQueued != (len(effects(res, EffectCancelQueued)) == 1) || len(effects(res, EffectCancelRunning)) != 0 {
				t.Fatalf("effects = %+v", res.Effects)
			}
			if c.cancelQueued && it.Runs.PendingReason != nil {
				t.Fatal("pending reason not cleared")
			}
			// Repeated absence preserves the resolution timestamp.
			later := now.Add(time.Hour)
			res = s.step(later, []Snapshot{snap("dev", later)})
			if len(res.Changes) != 0 || !s.get(id1).Alert.ResolvedAt.Equal(now) {
				t.Fatalf("repeated absence changed the item: %+v", res.Changes)
			}
		})
	}
	t.Run("T7 unprocessed is not absence", func(t *testing.T) {
		s, _ := newYoung(t)
		now := t0.Add(time.Minute)
		res := s.step(now, []Snapshot{snap("dev", now, alert(t, "dev", "fp1", "unprocessed", t0, nil))})
		wantState(t, s.get(id1), item.StateNew)
		if len(res.Changes) != 0 {
			t.Fatalf("changes = %+v", res.Changes)
		}
	})
}

// TestUnprocessedNeutral verifies that an explicitly unprocessed alert is
// neutral even when silencedBy/inhibitedBy are set (review round 1, B7).
func TestUnprocessedNeutral(t *testing.T) {
	unprocessed := func(t *testing.T, at time.Time) source.Alert {
		a := alert(t, "dev", "fp1", "unprocessed", at, nil)
		a.SilencedBy = []string{"silence-1"}
		a.InhibitedBy = []string{"rule-1"}
		return a
	}
	t.Run("new key creates nothing", func(t *testing.T) {
		s := newSim(t)
		res := s.step(t0, []Snapshot{snap("dev", t0, unprocessed(t, t0.Add(-time.Hour)))})
		if len(res.Changes) != 0 || len(res.Effects) != 0 || len(s.items) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("queued item is neither suppressed nor cancelled", func(t *testing.T) {
		s, _ := queuedItem(t)
		now := t0.Add(time.Minute)
		res := s.step(now, []Snapshot{snap("dev", now, unprocessed(t, t0.Add(-time.Hour)))})
		wantState(t, s.get(id1), item.StateQueued)
		if len(res.Changes) != 0 || len(res.Effects) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("resolved item is not reopened or advanced", func(t *testing.T) {
		s, _ := resolvedItem(t)
		now := t0.Add(cfg.ResolvedLinger + time.Hour)
		res := s.step(now, []Snapshot{snap("dev", now, unprocessed(t, t0))})
		wantState(t, s.get(id1), item.StateResolved)
		if len(res.Changes) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestT8Linger(t *testing.T) {
	t.Run("T8 boundary", func(t *testing.T) {
		s, _ := resolvedItem(t)
		resolvedAt := *s.get(id1).Alert.ResolvedAt
		empty := snap("dev", resolvedAt)
		res := s.step(resolvedAt.Add(cfg.ResolvedLinger-time.Second), []Snapshot{empty})
		if len(res.Changes) != 0 {
			t.Fatal("done before linger elapsed")
		}
		res = s.step(resolvedAt.Add(cfg.ResolvedLinger), []Snapshot{empty})
		it := s.get(id1)
		wantState(t, it, item.StateDone)
		if len(res.Effects) != 0 || it.Dismissed || !it.Alert.ResolvedAt.Equal(resolvedAt) {
			t.Fatalf("result = %+v", res)
		}
		s.assertStable(resolvedAt.Add(cfg.ResolvedLinger), empty)
	})
	t.Run("T8 cancels executing run", func(t *testing.T) {
		s, _ := preparingItem(t)
		now := t0.Add(time.Minute)
		s.step(now, []Snapshot{snap("dev", now)})
		res := s.step(now.Add(cfg.ResolvedLinger), []Snapshot{snap("dev", now.Add(cfg.ResolvedLinger))})
		wantState(t, s.get(id1), item.StateDone)
		if cr := effects(res, EffectCancelRunning); len(cr) != 1 || cr[0].Run != 1 {
			t.Fatalf("effects = %+v", res.Effects)
		}
	})
	t.Run("T8 fresh active evidence is not absence", func(t *testing.T) {
		s, a := resolvedItem(t)
		now := t0.Add(cfg.ResolvedLinger + time.Hour)
		s.step(now, []Snapshot{snap("dev", now, a)})
		if st := s.get(id1).State; st == item.StateDone || st == item.StateResolved {
			t.Fatalf("state = %s", st)
		}
	})
}

func TestT9Reopen(t *testing.T) {
	t.Run("T9 unprepared reopens new and is eligible again", func(t *testing.T) {
		s, _ := resolvedItem(t)
		now := t0.Add(time.Hour)
		old := alert(t, "dev", "fp1", "active", t0, nil)
		res := s.step(now, []Snapshot{snap("dev", now, old)})
		it := s.get(id1)
		tr := transitions(res, id1)
		if len(tr) != 2 || tr[0].From != item.StateResolved || tr[0].To != item.StateNew || tr[1].To != item.StateQueued {
			t.Fatalf("transitions = %+v", tr)
		}
		if it.Alert.Occurrences != 2 || it.Alert.ResolvedAt != nil || it.Alert.Status != item.AlertActive {
			t.Fatalf("alert = %+v", it.Alert)
		}
		if enq := effects(res, EffectEnqueue); len(enq) != 1 || enq[0].Run != 1 {
			t.Fatalf("effects = %+v", res.Effects)
		}
	})
	t.Run("T9 restores preparing without duplicate", func(t *testing.T) {
		s, a := preparingItem(t)
		s.step(t0.Add(time.Minute), []Snapshot{snap("dev", t0.Add(time.Minute))})
		now := t0.Add(time.Hour)
		res := s.step(now, []Snapshot{snap("dev", now, a)})
		it := s.get(id1)
		wantState(t, it, item.StatePreparing)
		if len(res.Effects) != 0 || it.Alert.Occurrences != 2 || it.Alert.ResolvedAt != nil {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("T9 restores needs-you after finished preparation", func(t *testing.T) {
		s, a := needsYouItem(t)
		s.step(t0.Add(2*time.Minute), []Snapshot{snap("dev", t0.Add(2*time.Minute))})
		now := t0.Add(time.Hour)
		res := s.step(now, []Snapshot{snap("dev", now, a)})
		wantState(t, s.get(id1), item.StateNeedsYou)
		if len(res.Effects) != 0 {
			t.Fatalf("effects = %+v", res.Effects)
		}
	})
	t.Run("T9 not applied for a suppressed observation", func(t *testing.T) {
		s, _ := resolvedItem(t)
		now := t0.Add(time.Hour)
		res := s.step(now, []Snapshot{snap("dev", now, alert(t, "dev", "fp1", "suppressed", t0, nil))})
		wantState(t, s.get(id1), item.StateResolved)
		if len(res.Changes) != 0 {
			t.Fatalf("changes = %+v", res.Changes)
		}
	})
}

// doneAfterResolution returns an item that was prepared (run 1 ready), then
// resolved at t0+2m and moved to done by T8.
func doneAfterResolution(t *testing.T) (*sim, source.Alert, time.Time) {
	s, a := needsYouItem(t)
	resolvedAt := t0.Add(2 * time.Minute)
	s.step(resolvedAt, []Snapshot{snap("dev", resolvedAt)})
	s.step(resolvedAt.Add(cfg.ResolvedLinger), []Snapshot{snap("dev", resolvedAt.Add(cfg.ResolvedLinger))})
	wantState(t, s.get(id1), item.StateDone)
	s.items[id1].Item.Seen = true
	return s, a, resolvedAt
}

func TestT10T11DoneReopen(t *testing.T) {
	t.Run("T10 at window boundary reopens and is eligible again", func(t *testing.T) {
		s, a, resolvedAt := doneAfterResolution(t)
		now := resolvedAt.Add(cfg.ReopenWindow)
		res := s.step(now, []Snapshot{snap("dev", now, a)})
		it := s.get(id1)
		tr := transitions(res, id1)
		if len(tr) != 2 || tr[0].From != item.StateDone || tr[0].To != item.StateNew || tr[1].To != item.StateQueued {
			t.Fatalf("transitions = %+v", tr)
		}
		if it.Alert.Occurrences != 2 || it.Alert.ResolvedAt != nil || it.Dismissed || it.Seen || !res.Changes[0].SeenChanged {
			t.Fatalf("item = %+v", it)
		}
		if enq := effects(res, EffectEnqueue); len(enq) != 1 || enq[0].Run != 2 {
			t.Fatalf("effects = %+v", res.Effects)
		}
		if len(s.items[id1].Runs) != 1 {
			t.Fatal("previous runs must be kept")
		}
		s.assertStable(now, snap("dev", now, a))
	})
	t.Run("T10 clears dismissed", func(t *testing.T) {
		s, a := newYoung(t)
		s.step(t0.Add(time.Second), nil, ev(EventDismiss, id1))
		s.step(t0.Add(time.Minute), []Snapshot{snap("dev", t0.Add(time.Minute))})
		now := t0.Add(2 * time.Minute)
		s.step(now, []Snapshot{snap("dev", now, a)})
		it := s.get(id1)
		wantState(t, it, item.StateNew)
		if it.Dismissed {
			t.Fatal("dismissed not cleared")
		}
	})
	t.Run("T11 after window creates linked item", func(t *testing.T) {
		s, a, resolvedAt := doneAfterResolution(t)
		before := *s.get(id1).Clone()
		now := resolvedAt.Add(cfg.ReopenWindow + time.Second)
		res := s.step(now, []Snapshot{snap("dev", now, a)})
		if !reflect.DeepEqual(*s.get(id1), before) {
			t.Fatal("old item must be left unchanged")
		}
		id2 := "alert-dev-fp1-2"
		it := s.get(id2)
		if it.PreviousItem == nil || *it.PreviousItem != id1 || it.Alert.Occurrences != 1 || len(res.Changes) != 1 || !res.Changes[0].Create {
			t.Fatalf("item = %+v", it)
		}
		// The new item follows T1+T2 for an old alert.
		wantState(t, it, item.StateQueued)
		if enq := effects(res, EffectEnqueue); len(enq) != 1 || enq[0].ItemID != id2 || enq[0].Run != 1 {
			t.Fatalf("effects = %+v", res.Effects)
		}
		// Later absence resolves the new current item only.
		res = s.step(now.Add(time.Minute), []Snapshot{snap("dev", now.Add(time.Minute))})
		wantState(t, s.get(id2), item.StateResolved)
		if len(res.Changes) != 1 {
			t.Fatalf("changes = %+v", res.Changes)
		}
	})
	t.Run("T11 suppressed re-fire leaves done item alone", func(t *testing.T) {
		s, _, resolvedAt := doneAfterResolution(t)
		now := resolvedAt.Add(time.Hour)
		res := s.step(now, []Snapshot{snap("dev", now, alert(t, "dev", "fp1", "suppressed", t0, nil))})
		if len(res.Changes) != 0 || len(s.items) != 1 {
			t.Fatalf("result = %+v", res)
		}
	})
}

func TestT12Dismiss(t *testing.T) {
	cases := []struct {
		name          string
		setup         func(t *testing.T) (*sim, source.Alert)
		cancelQueued  bool
		cancelRunning bool
	}{
		{"new", newYoung, false, false},
		{"queued", queuedItem, true, false},
		{"preparing", preparingItem, false, true},
		{"needs-you", needsYouItem, false, false},
		{"snoozed", snoozedItem, false, false},
		{"resolved", resolvedItem, false, false},
	}
	for _, c := range cases {
		t.Run("T12 from "+c.name, func(t *testing.T) {
			s, _ := c.setup(t)
			from := s.get(id1).State
			now := t0.Add(3 * time.Minute)
			res := s.step(now, nil, ev(EventDismiss, id1))
			it := s.get(id1)
			wantState(t, it, item.StateDone)
			if !it.Dismissed {
				t.Fatal("dismissed not set")
			}
			app := res.Changes[0].Appended
			if len(app) != 2 || app[0].Action != item.ActionDismissed || app[0].Run != 0 || app[1].From != from || app[1].To != item.StateDone {
				t.Fatalf("appended = %+v", app)
			}
			if c.cancelQueued != (len(effects(res, EffectCancelQueued)) == 1) || c.cancelRunning != (len(effects(res, EffectCancelRunning)) == 1) {
				t.Fatalf("effects = %+v", res.Effects)
			}
			if it.Runs.PendingReason != nil {
				t.Fatal("pending reason not cleared")
			}
			res = s.step(now, nil, ev(EventDismiss, id1))
			if len(res.Ignored) != 1 || len(res.Changes) != 0 {
				t.Fatal("dismissing a done item must be ignored")
			}
		})
	}
}

func TestT13DismissedFiring(t *testing.T) {
	s, a := queuedItem(t)
	s.step(t0.Add(time.Second), nil, ev(EventDismiss, id1))
	historyLen := len(s.get(id1).History)
	for i := 1; i <= 3; i++ {
		now := t0.Add(time.Duration(i) * time.Minute)
		res := s.step(now, []Snapshot{snap("dev", now, a)})
		wantState(t, s.get(id1), item.StateDone)
		if len(res.Effects) != 0 || len(transitions(res, id1)) != 0 {
			t.Fatalf("result = %+v", res)
		}
	}
	// A far later poll still does not reopen without a recorded resolution.
	far := t0.Add(100 * 24 * time.Hour)
	s.step(far, []Snapshot{snap("dev", far, a)})
	if len(s.get(id1).History) != historyLen || s.get(id1).State != item.StateDone {
		t.Fatal("no-op transitions appended")
	}
	// First absence records the resolution while staying done.
	absent := far.Add(time.Minute)
	res := s.step(absent, []Snapshot{snap("dev", absent)})
	it := s.get(id1)
	wantState(t, it, item.StateDone)
	if it.Alert.Status != item.AlertResolved || !it.Alert.ResolvedAt.Equal(absent) || len(transitions(res, id1)) != 0 || len(it.History) != historyLen {
		t.Fatalf("item = %+v", it)
	}
	// Later absence preserves the timestamp.
	s.step(absent.Add(time.Hour), []Snapshot{snap("dev", absent.Add(time.Hour))})
	if !s.get(id1).Alert.ResolvedAt.Equal(absent) {
		t.Fatal("resolution time reset")
	}
	// Only now does a later active observation reopen (T10).
	again := absent.Add(2 * time.Hour)
	s.step(again, []Snapshot{snap("dev", again, a)})
	if st := s.get(id1).State; st != item.StateQueued {
		t.Fatalf("state = %s", st)
	}
}

func TestT14ManualRun(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T) (*sim, source.Alert)
		nextRun int
	}{
		{"new", newYoung, 1},
		{"needs-you", needsYouItem, 2},
		{"snoozed", snoozedItem, 1},
		{"resolved", resolvedItem, 1},
	}
	for _, c := range cases {
		t.Run("T14 from "+c.name, func(t *testing.T) {
			s, _ := c.setup(t)
			from := s.get(id1).State
			now := t0.Add(3 * time.Minute)
			res := s.step(now, nil, ev(EventManualRun, id1))
			it := s.get(id1)
			wantState(t, it, item.StateQueued)
			if it.Runs.PendingReason == nil || *it.Runs.PendingReason != item.ReasonManual {
				t.Fatal("pending reason not manual")
			}
			app := res.Changes[0].Appended
			if len(app) != 2 || app[0].Action != item.ActionManualRun || app[0].Run != c.nextRun || app[1].From != from || app[1].To != item.StateQueued {
				t.Fatalf("appended = %+v", app)
			}
			if enq := effects(res, EffectEnqueue); len(enq) != 1 || enq[0].Run != c.nextRun || enq[0].Reason != item.ReasonManual {
				t.Fatalf("effects = %+v", res.Effects)
			}
		})
	}
	for _, c := range []struct {
		name  string
		setup func(t *testing.T) (*sim, source.Alert)
	}{{"queued", queuedItem}, {"preparing", preparingItem}} {
		t.Run("T14 ignored in "+c.name, func(t *testing.T) {
			s, _ := c.setup(t)
			res := s.step(t0.Add(3*time.Minute), nil, ev(EventManualRun, id1))
			if len(res.Ignored) != 1 || len(res.Changes) != 0 || len(res.Effects) != 0 {
				t.Fatalf("result = %+v", res)
			}
		})
	}
	t.Run("T14 ignored in done", func(t *testing.T) {
		s, _ := newYoung(t)
		s.step(t0.Add(time.Second), nil, ev(EventDismiss, id1))
		res := s.step(t0.Add(2*time.Second), nil, ev(EventManualRun, id1))
		if len(res.Ignored) != 1 || len(res.Changes) != 0 {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("unknown item events are ignored", func(t *testing.T) {
		s := newSim(t)
		res := s.step(t0, nil, ev(EventManualRun, "alert-dev-nope-1"), ev("bogus", "alert-dev-nope-1"))
		if len(res.Ignored) != 2 {
			t.Fatalf("ignored = %+v", res.Ignored)
		}
	})
}

func TestManualOverride(t *testing.T) {
	t.Run("from snoozed survives unchanged suppression", func(t *testing.T) {
		s, _ := snoozedItem(t)
		sup := alert(t, "dev", "fp1", "suppressed", t0, nil)
		s.step(t0.Add(time.Minute), nil, ev(EventManualRun, id1))
		for i := 2; i < 5; i++ {
			now := t0.Add(time.Duration(i) * time.Minute)
			res := s.step(now, []Snapshot{snap("dev", now, sup)})
			wantState(t, s.get(id1), item.StateQueued)
			if len(res.Effects) != 0 {
				t.Fatalf("effects = %+v", res.Effects)
			}
		}
		s.step(t0.Add(5*time.Minute), []Snapshot{snap("dev", t0.Add(5*time.Minute), sup)}, started(id1, 1, t0.Add(time.Minute)))
		wantState(t, s.get(id1), item.StatePreparing)
		res := s.step(t0.Add(6*time.Minute), []Snapshot{snap("dev", t0.Add(6*time.Minute), sup)},
			finished(id1, 1, t0.Add(time.Minute), t0.Add(6*time.Minute), item.OutcomeReady, item.ReasonManual))
		wantState(t, s.get(id1), item.StateNeedsYou)
		if len(effects(res, EffectNotify)) != 1 {
			t.Fatal("want notify")
		}
		if s.get(id1).Alert.Status != item.AlertSuppressed {
			t.Fatal("actual alert status must stay separate from the manual override")
		}
	})
	t.Run("from snoozed is subject to a new suppression", func(t *testing.T) {
		s, a := snoozedItem(t)
		sup := alert(t, "dev", "fp1", "suppressed", t0, nil)
		s.step(t0.Add(time.Minute), nil, ev(EventManualRun, id1))
		s.step(t0.Add(2*time.Minute), []Snapshot{snap("dev", t0.Add(2*time.Minute), a)})
		wantState(t, s.get(id1), item.StateQueued)
		res := s.step(t0.Add(3*time.Minute), []Snapshot{snap("dev", t0.Add(3*time.Minute), sup)})
		wantState(t, s.get(id1), item.StateSnoozed)
		if len(effects(res, EffectCancelQueued)) != 1 {
			t.Fatalf("effects = %+v", res.Effects)
		}
	})
	t.Run("from resolved survives unchanged absence", func(t *testing.T) {
		s, _ := resolvedItem(t)
		s.step(t0.Add(time.Minute), nil, ev(EventManualRun, id1))
		for _, d := range []time.Duration{2 * time.Minute, cfg.ResolvedLinger + time.Hour} {
			now := t0.Add(d)
			res := s.step(now, []Snapshot{snap("dev", now)})
			wantState(t, s.get(id1), item.StateQueued)
			if len(res.Effects) != 0 || len(res.Changes) != 0 {
				t.Fatalf("result = %+v", res)
			}
		}
		s.step(t0.Add(5*time.Hour), nil, started(id1, 1, t0.Add(time.Minute)))
		s.step(t0.Add(6*time.Hour), []Snapshot{snap("dev", t0.Add(6*time.Hour))},
			finished(id1, 1, t0.Add(time.Minute), t0.Add(6*time.Hour), item.OutcomeFailed, item.ReasonManual))
		wantState(t, s.get(id1), item.StateNeedsYou)
	})
	t.Run("from resolved is subject to a new resolution", func(t *testing.T) {
		s, a := resolvedItem(t)
		s.step(t0.Add(time.Minute), nil, ev(EventManualRun, id1))
		s.step(t0.Add(2*time.Minute), []Snapshot{snap("dev", t0.Add(2*time.Minute), a)})
		wantState(t, s.get(id1), item.StateQueued)
		res := s.step(t0.Add(3*time.Minute), []Snapshot{snap("dev", t0.Add(3*time.Minute))})
		wantState(t, s.get(id1), item.StateResolved)
		if len(effects(res, EffectCancelQueued)) != 1 {
			t.Fatalf("effects = %+v", res.Effects)
		}
	})
}

func TestSourceFailure(t *testing.T) {
	failed := func(src string) Snapshot { return Snapshot{Source: src, OK: false} }
	t.Run("freezes T2", func(t *testing.T) {
		s, _ := newYoung(t)
		res := s.step(t0.Add(time.Hour), []Snapshot{failed("dev")})
		wantState(t, s.get(id1), item.StateNew)
		if len(res.Changes) != 0 {
			t.Fatal("frozen source changed items")
		}
	})
	t.Run("freezes T7 and T8", func(t *testing.T) {
		s, _ := resolvedItem(t)
		res := s.step(t0.Add(cfg.ResolvedLinger+time.Hour), []Snapshot{failed("dev")})
		wantState(t, s.get(id1), item.StateResolved)
		if len(res.Changes) != 0 {
			t.Fatal("T8 applied for a failed source")
		}
		s2, _ := newYoung(t)
		s2.step(t0.Add(time.Minute), []Snapshot{failed("dev")})
		wantState(t, s2.get(id1), item.StateNew)
	})
	t.Run("user and runner events still apply", func(t *testing.T) {
		s, _ := queuedItem(t)
		s.step(t0.Add(time.Second), []Snapshot{failed("dev")}, started(id1, 1, t0))
		wantState(t, s.get(id1), item.StatePreparing)
		s.step(t0.Add(time.Minute), []Snapshot{failed("dev")}, finished(id1, 1, t0, t0.Add(time.Minute), item.OutcomeReady, item.ReasonAuto))
		wantState(t, s.get(id1), item.StateNeedsYou)
		s.step(t0.Add(2*time.Minute), []Snapshot{failed("dev")}, ev(EventDismiss, id1))
		wantState(t, s.get(id1), item.StateDone)
	})
	t.Run("recovers on success", func(t *testing.T) {
		s, a := newYoung(t)
		s.step(t0.Add(time.Hour), []Snapshot{failed("dev")})
		s.step(t0.Add(time.Hour+time.Minute), []Snapshot{snap("dev", t0.Add(time.Hour+time.Minute), a)})
		wantState(t, s.get(id1), item.StateQueued)
	})
	t.Run("unpolled or other sources never resolve items", func(t *testing.T) {
		s, _ := newYoung(t)
		res := s.step(t0.Add(time.Hour), nil)
		if len(res.Changes) != 0 {
			t.Fatal("unpolled source resolved an item")
		}
		res = s.step(t0.Add(time.Hour), []Snapshot{snap("am", t0.Add(time.Hour))})
		wantState(t, s.get(id1), item.StateNew)
		if len(res.Changes) != 0 {
			t.Fatal("another source's snapshot changed the item")
		}
	})
}

func TestMetadataAndRawAlert(t *testing.T) {
	s, a := newYoung(t)
	now := t0.Add(time.Minute)
	res := s.step(now, []Snapshot{snap("dev", now, a)})
	if len(res.Changes) != 1 || res.Changes[0].RawAlert != nil || len(res.Changes[0].Appended) != 0 {
		t.Fatalf("unchanged poll must only refresh last_seen_at: %+v", res.Changes)
	}
	it := s.get(id1)
	if !it.Alert.LastSeenAt.Equal(now) || !it.UpdatedAt.Equal(now) {
		t.Fatalf("last_seen_at/updated_at = %v %v", it.Alert.LastSeenAt, it.UpdatedAt)
	}
	// Labels change: title, severity and raw alert are updated, no transition.
	labels := map[string]string{"alertname": "HighLatency", "namespace": "core", "deployment": "api", "severity": "warning"}
	changed := alert(t, "dev", "fp1", "active", t0, labels)
	now = now.Add(time.Second)
	res = s.step(now, []Snapshot{snap("dev", now, changed)})
	it = s.get(id1)
	if res.Changes[0].RawAlert == nil || it.Title != "HighLatency: core/api" || it.Severity != "warning" || len(res.Changes[0].Appended) != 0 {
		t.Fatalf("item = %+v", it)
	}
	// Annotation change also rewrites the raw alert.
	var doc map[string]any
	if err := json.Unmarshal(changed.Raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["annotations"] = map[string]string{"runbook_url": "https://runbooks/y"}
	rawAnn, _ := json.Marshal(doc)
	ann, err := source.DecodeAlert("dev", rawAnn)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	res = s.step(now, []Snapshot{snap("dev", now, ann)})
	if res.Changes[0].RawAlert == nil || s.get(id1).Alert.RunbookURL != "https://runbooks/y" {
		t.Fatal("annotation change not applied")
	}
	// Only non-identity fields differing in raw (e.g. updatedAt) do not rewrite.
	same := ann
	same.UpdatedAt = now
	same.Raw = json.RawMessage(`{"different":"bytes"}`)
	res = s.step(now, []Snapshot{snap("dev", now, same)})
	if len(res.Changes) != 0 {
		t.Fatalf("unchanged alert rewrote state: %+v", res.Changes)
	}
}

func TestIdempotentFullLifecycle(t *testing.T) {
	s := newSim(t)
	a := alert(t, "dev", "fp1", "active", t0.Add(-time.Hour), nil)
	s.step(t0, []Snapshot{snap("dev", t0, a)})
	s.assertStable(t0, snap("dev", t0, a))
	s.step(t0.Add(time.Second), nil, started(id1, 1, t0))
	s.assertStable(t0.Add(time.Second), snap("dev", t0, a))
	s.step(t0.Add(time.Minute), nil, finished(id1, 1, t0, t0.Add(time.Minute), item.OutcomeReady, item.ReasonAuto))
	s.assertStable(t0.Add(time.Minute), snap("dev", t0, a))
	end := t0.Add(2 * time.Minute)
	s.step(end, []Snapshot{snap("dev", end)})
	s.assertStable(end, snap("dev", end))
	if n := len(s.get(id1).History); n != 5 {
		t.Fatalf("history = %+v", s.get(id1).History)
	}
}
