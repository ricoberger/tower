package runner

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ricoberger/tower/internal/item"
)

// Entry is a queued preparation run of an item.
type Entry struct {
	ItemID string
	// Run is the number the run gets when it starts (runs.current + 1).
	Run    int
	Reason item.RunReason
	// RetryOf is the interrupted run a retry repeats (0 otherwise).
	RetryOf  int
	Severity string
	StartsAt time.Time
	QueuedAt time.Time
}

// Queue holds at most one queued run per item and orders them by priority:
// manual requests first, then by the index of the severity in the configured
// severity order (unknown or missing severities last), then by the oldest
// alert start, the oldest queue time and the item ID.
type Queue struct {
	rank    map[string]int
	entries map[string]Entry
}

// NewQueue returns an empty queue ordering severities by severityOrder.
func NewQueue(severityOrder []string) *Queue {
	q := &Queue{rank: map[string]int{}, entries: map[string]Entry{}}
	for i, s := range severityOrder {
		if _, ok := q.rank[s]; !ok {
			q.rank[s] = i
		}
	}
	return q
}

// Push queues e, replacing an earlier entry of the same item.
func (q *Queue) Push(e Entry) { q.entries[e.ItemID] = e }

// Remove removes the entry of an item and reports whether one existed.
func (q *Queue) Remove(itemID string) bool {
	_, ok := q.entries[itemID]
	delete(q.entries, itemID)
	return ok
}

// Get returns the entry of an item.
func (q *Queue) Get(itemID string) (Entry, bool) {
	e, ok := q.entries[itemID]
	return e, ok
}

// Len returns the number of queued entries.
func (q *Queue) Len() int { return len(q.entries) }

// IDs returns the queued item IDs in ID order.
func (q *Queue) IDs() []string { return slices.Sorted(maps.Keys(q.entries)) }

// Ordered returns the entries in start order.
func (q *Queue) Ordered() []Entry {
	out := slices.Collect(maps.Values(q.entries))
	slices.SortFunc(out, q.compare)
	return out
}

func (q *Queue) severityRank(s string) int {
	if r, ok := q.rank[s]; ok {
		return r
	}
	return len(q.rank)
}

func (q *Queue) compare(a, b Entry) int {
	am, bm := a.Reason == item.ReasonManual, b.Reason == item.ReasonManual
	if am != bm {
		if am {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(q.severityRank(a.Severity), q.severityRank(b.Severity)); c != 0 {
		return c
	}
	if c := a.StartsAt.Compare(b.StartsAt); c != 0 {
		return c
	}
	if c := a.QueuedAt.Compare(b.QueuedAt); c != 0 {
		return c
	}
	return strings.Compare(a.ItemID, b.ItemID)
}
