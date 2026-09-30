package ui

import (
	"context"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ricoberger/tower/internal/item"
)

// Snapshot is an independent copy of the engine's applied state. The engine
// builds it on its loop; the UI never reads mutable engine state.
type Snapshot struct {
	Items   []ItemView
	Sources []SourceHealth
	// Unreadable is the number of item directories that cannot be loaded.
	Unreadable int
	// Running is the number of occupied run slots, Concurrency the
	// configured slots and Queued the number of queued runs.
	Running     int
	Concurrency int
	Queued      int
}

// ItemView is one applied item with its runs.
type ItemView struct {
	Item *item.Item
	Runs []item.Run
}

// Latest returns the item's current run (runs.current), if it has one.
func (v ItemView) Latest() (item.Run, bool) {
	if v.Item == nil || v.Item.Runs.Current == 0 {
		return item.Run{}, false
	}
	for _, r := range v.Runs {
		if r.Number == v.Item.Runs.Current {
			return r, true
		}
	}
	return item.Run{}, false
}

// Executing reports whether the item's current run is still executing.
func (v ItemView) Executing() bool {
	r, ok := v.Latest()
	return ok && r.Outcome == item.OutcomeRunning
}

// SourceHealth is the poll health of one configured source.
type SourceHealth struct {
	Name string
	// LastSuccess is the time of the latest successful poll (zero if the
	// source never succeeded); LastErr is the sanitized error of the latest
	// poll ("" after a success).
	LastSuccess time.Time
	LastErr     string
}

// Feed hands the latest snapshot from the engine to the UI. Publishing never
// blocks: a slow consumer only skips intermediate snapshots.
type Feed struct {
	mu     sync.Mutex
	snap   Snapshot
	has    bool
	signal chan struct{}
}

// NewFeed returns an empty feed.
func NewFeed() *Feed {
	return &Feed{signal: make(chan struct{}, 1)}
}

// Publish replaces the latest snapshot and wakes the consumer.
func (f *Feed) Publish(s Snapshot) {
	f.mu.Lock()
	f.snap, f.has = s, true
	f.mu.Unlock()
	select {
	case f.signal <- struct{}{}:
	default:
	}
}

// Latest returns the latest snapshot and whether one was published.
func (f *Feed) Latest() (Snapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, f.has
}

// SnapshotMsg delivers a snapshot to the model.
type SnapshotMsg Snapshot

// wait returns a command that delivers the next published snapshot.
func (f *Feed) wait(ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-f.signal:
			s, _ := f.Latest()
			return SnapshotMsg(s)
		case <-ctx.Done():
			return nil
		}
	}
}
