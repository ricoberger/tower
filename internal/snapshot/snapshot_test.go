package snapshot

import (
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/item"
)

func TestFeed(t *testing.T) {
	f := NewFeed()
	if _, ok := f.Latest(); ok {
		t.Fatal("new feed reports a snapshot")
	}
	select {
	case <-f.Updated():
		t.Fatal("new feed signals an update")
	default:
	}

	// Publishing never blocks and several publishes coalesce into one
	// update that reads the newest snapshot.
	f.Publish(Snapshot{Running: 1})
	f.Publish(Snapshot{Running: 2})
	select {
	case <-f.Updated():
	case <-time.After(time.Second):
		t.Fatal("publish did not signal an update")
	}
	if s, ok := f.Latest(); !ok || s.Running != 2 {
		t.Fatalf("latest = %+v, %v; want running 2", s, ok)
	}
	select {
	case <-f.Updated():
		t.Fatal("coalesced publishes signalled twice")
	default:
	}
}

func TestItemViewLatest(t *testing.T) {
	v := ItemView{
		Item: &item.Item{Runs: item.RunsInfo{Current: 2}},
		Runs: []item.Run{{Number: 1, Outcome: item.OutcomeReady}, {Number: 2, Outcome: item.OutcomeRunning}},
	}
	r, ok := v.Latest()
	if !ok || r.Number != 2 || !v.Executing() {
		t.Fatalf("latest = %+v, %v; executing %v", r, ok, v.Executing())
	}
	v.Item.Runs.Current = 0
	if _, ok := v.Latest(); ok || v.Executing() {
		t.Fatal("item without a current run reports one")
	}
}
