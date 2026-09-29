// Package item contains the item model and its on-disk store.
package item

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// Version is the current item.yaml schema version.
const Version = 1

// TypeAlert is the only item type of the MVP.
const TypeAlert = "alert"

// MaxHistory bounds the history list; the oldest entries are dropped.
const MaxHistory = 500

// State is the lifecycle state of an item.
type State string

// Item states.
const (
	StateNew       State = "new"
	StateQueued    State = "queued"
	StatePreparing State = "preparing"
	StateNeedsYou  State = "needs-you"
	StateSnoozed   State = "snoozed"
	StateResolved  State = "resolved"
	StateDone      State = "done"
)

// Valid reports whether s is a supported state.
func (s State) Valid() bool {
	switch s {
	case StateNew, StateQueued, StatePreparing, StateNeedsYou, StateSnoozed, StateResolved, StateDone:
		return true
	}
	return false
}

// AlertStatus is the persisted status of the alert behind an item.
type AlertStatus string

// Alert statuses.
const (
	AlertActive     AlertStatus = "active"
	AlertSuppressed AlertStatus = "suppressed"
	AlertResolved   AlertStatus = "resolved"
)

// Valid reports whether s is a supported alert status.
func (s AlertStatus) Valid() bool {
	return s == AlertActive || s == AlertSuppressed || s == AlertResolved
}

// RunReason is why a run was queued.
type RunReason string

// Run reasons.
const (
	ReasonAuto   RunReason = "auto"
	ReasonManual RunReason = "manual"
	ReasonRetry  RunReason = "retry"
)

// Valid reports whether r is a supported run reason.
func (r RunReason) Valid() bool {
	return r == ReasonAuto || r == ReasonManual || r == ReasonRetry
}

// Outcome is the outcome of a run.
type Outcome string

// Run outcomes.
const (
	OutcomeRunning     Outcome = "running"
	OutcomeReady       Outcome = "ready"
	OutcomeBlocked     Outcome = "blocked"
	OutcomeFailed      Outcome = "failed"
	OutcomeCancelled   Outcome = "cancelled"
	OutcomeInterrupted Outcome = "interrupted"
)

// Valid reports whether o is a supported outcome.
func (o Outcome) Valid() bool {
	switch o {
	case OutcomeRunning, OutcomeReady, OutcomeBlocked, OutcomeFailed, OutcomeCancelled, OutcomeInterrupted:
		return true
	}
	return false
}

// Finished reports whether o is a finished preparation (ready, blocked or
// failed). Cancelled and interrupted runs are not finished preparations.
func (o Outcome) Finished() bool {
	return o == OutcomeReady || o == OutcomeBlocked || o == OutcomeFailed
}

// Action is a user action recorded in the history.
type Action string

// User actions.
const (
	ActionOpenedReport   Action = "opened-report"
	ActionResumedSession Action = "resumed-session"
	ActionManualRun      Action = "manual-run"
	ActionDismissed      Action = "dismissed"
)

// Valid reports whether a is a supported action.
func (a Action) Valid() bool {
	switch a {
	case ActionOpenedReport, ActionResumedSession, ActionManualRun, ActionDismissed:
		return true
	}
	return false
}

// Item is the content of item.yaml.
type Item struct {
	Version      int            `yaml:"version"`
	ID           string         `yaml:"id"`
	Type         string         `yaml:"type"`
	State        State          `yaml:"state"`
	Title        string         `yaml:"title"`
	Severity     string         `yaml:"severity"`
	CreatedAt    time.Time      `yaml:"created_at"`
	UpdatedAt    time.Time      `yaml:"updated_at"`
	Seen         bool           `yaml:"seen"`
	Dismissed    bool           `yaml:"dismissed"`
	Source       SourceRef      `yaml:"source"`
	Alert        AlertInfo      `yaml:"alert"`
	Runs         RunsInfo       `yaml:"runs"`
	PreviousItem *string        `yaml:"previous_item"`
	History      []HistoryEntry `yaml:"history"`
}

// SourceRef identifies the source alert of an item.
type SourceRef struct {
	Name        string `yaml:"name"`
	Fingerprint string `yaml:"fingerprint"`
}

// AlertInfo is the alert section of item.yaml.
type AlertInfo struct {
	StartsAt     time.Time         `yaml:"starts_at"`
	LastSeenAt   time.Time         `yaml:"last_seen_at"`
	ResolvedAt   *time.Time        `yaml:"resolved_at"`
	Status       AlertStatus       `yaml:"status"`
	Occurrences  int               `yaml:"occurrences"`
	Labels       map[string]string `yaml:"labels"`
	GeneratorURL string            `yaml:"generator_url"`
	RunbookURL   string            `yaml:"runbook_url"`
}

// RunsInfo is the runs section of item.yaml.
type RunsInfo struct {
	// Current is the latest run number; 0 means no run.
	Current int `yaml:"current"`
	// PendingReason is set while the item is queued.
	PendingReason *RunReason `yaml:"pending_reason"`
	// OccurrenceBase is the value of Current when the current alert
	// occurrence started (creation, or a reopen into new by T9/T10). Runs
	// numbered above it belong to the current occurrence. It is kept
	// separately from the bounded history so that evicting old history
	// entries never changes occurrence identity. Internal bookkeeping; 0
	// means every run belongs to the current occurrence.
	OccurrenceBase int `yaml:"occurrence_base"`
}

// HistoryEntry is either a state transition (From, To, Reason) or a user
// action (Action, optional Run).
type HistoryEntry struct {
	At     time.Time
	From   State
	To     State
	Reason string
	Action Action
	Run    int
}

// IsAction reports whether the entry is a user action.
func (h HistoryEntry) IsAction() bool { return h.Action != "" }

type transitionYAML struct {
	At     time.Time `yaml:"at"`
	From   State     `yaml:"from"`
	To     State     `yaml:"to"`
	Reason string    `yaml:"reason"`
}

type actionYAML struct {
	At     time.Time `yaml:"at"`
	Action Action    `yaml:"action"`
	Run    int       `yaml:"run,omitempty"`
}

type entryYAML struct {
	At     time.Time `yaml:"at"`
	From   *State    `yaml:"from"`
	To     *State    `yaml:"to"`
	Reason *string   `yaml:"reason"`
	Action *Action   `yaml:"action"`
	Run    *int      `yaml:"run"`
}

// MarshalYAML writes one of the two entry shapes.
func (h HistoryEntry) MarshalYAML() (any, error) {
	if h.IsAction() {
		return actionYAML{At: h.At, Action: h.Action, Run: h.Run}, nil
	}
	return transitionYAML{At: h.At, From: h.From, To: h.To, Reason: h.Reason}, nil
}

// UnmarshalYAML reads one of the two entry shapes and rejects mixtures.
func (h *HistoryEntry) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return errors.New("history entry must be a mapping")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch node.Content[i].Value {
		case "at", "from", "to", "reason", "action", "run":
		default:
			return fmt.Errorf("history entry: unknown field %q", node.Content[i].Value)
		}
	}
	var e entryYAML
	if err := node.Decode(&e); err != nil {
		return err
	}
	*h = HistoryEntry{At: e.At}
	if e.Action != nil {
		if e.From != nil || e.To != nil || e.Reason != nil {
			return errors.New("history entry mixes action and transition fields")
		}
		if !e.Action.Valid() {
			return errors.New("history entry: unsupported action")
		}
		h.Action = *e.Action
		if e.Run != nil {
			h.Run = *e.Run
		}
		return nil
	}
	if e.Run != nil || e.To == nil {
		return errors.New("history entry: transition requires to and must not have run")
	}
	h.To = *e.To
	if e.From != nil {
		h.From = *e.From
	}
	if e.Reason != nil {
		h.Reason = *e.Reason
	}
	if !h.To.Valid() || (h.From != "" && !h.From.Valid()) {
		return errors.New("history entry: unsupported state")
	}
	return nil
}

// Key is the identity key of an alert: (source, fingerprint).
type Key struct {
	Source      string
	Fingerprint string
}

// Key returns the identity key of the item.
func (it *Item) Key() Key {
	return Key{Source: it.Source.Name, Fingerprint: it.Source.Fingerprint}
}

// Clone returns a deep copy of the item.
func (it *Item) Clone() *Item {
	c := *it
	c.Alert.Labels = maps.Clone(it.Alert.Labels)
	if it.Alert.ResolvedAt != nil {
		t := *it.Alert.ResolvedAt
		c.Alert.ResolvedAt = &t
	}
	if it.Runs.PendingReason != nil {
		r := *it.Runs.PendingReason
		c.Runs.PendingReason = &r
	}
	if it.PreviousItem != nil {
		p := *it.PreviousItem
		c.PreviousItem = &p
	}
	c.History = slices.Clone(it.History)
	return &c
}

// AppendHistory appends entries, keeping at most MaxHistory entries by
// dropping the oldest ones.
func AppendHistory(history []HistoryEntry, entries ...HistoryEntry) []HistoryEntry {
	out := append(slices.Clone(history), entries...)
	if len(out) > MaxHistory {
		out = slices.Clone(out[len(out)-MaxHistory:])
	}
	return out
}

// ApplyAction appends a user action to the item. Opening a report or resuming
// a session also marks the item as seen. The state is never changed.
func (it *Item) ApplyAction(action Action, run int, now time.Time) error {
	if !action.Valid() {
		return errors.New("unsupported action")
	}
	if action == ActionDismissed {
		run = 0
	}
	it.History = AppendHistory(it.History, HistoryEntry{At: now, Action: action, Run: run})
	if action == ActionOpenedReport || action == ActionResumedSession {
		it.Seen = true
	}
	it.UpdatedAt = now
	return nil
}

// Validate checks that the item is a well-formed version 1 alert item stored
// under the given ID.
func (it *Item) Validate(id string) error {
	if it.Version != Version {
		return fmt.Errorf("unsupported version %d", it.Version)
	}
	if it.ID != id {
		return errors.New("id does not match the item directory")
	}
	key, _, err := ParseID(id)
	if err != nil {
		return err
	}
	if it.Key() != key {
		return errors.New("source does not match the item id")
	}
	if it.Type != TypeAlert {
		return errors.New("unsupported type")
	}
	if !it.State.Valid() {
		return errors.New("unsupported state")
	}
	if !it.Alert.Status.Valid() {
		return errors.New("unsupported alert status")
	}
	if it.Runs.Current < 0 {
		return errors.New("negative runs.current")
	}
	if it.Runs.OccurrenceBase < 0 || it.Runs.OccurrenceBase > it.Runs.Current {
		return errors.New("runs.occurrence_base out of range")
	}
	if it.Runs.PendingReason != nil && !it.Runs.PendingReason.Valid() {
		return errors.New("unsupported runs.pending_reason")
	}
	if it.PreviousItem != nil {
		if _, _, err := ParseID(*it.PreviousItem); err != nil {
			return fmt.Errorf("previous_item: %w", err)
		}
	}
	return nil
}

// Run is the content of runs/<n>/meta.yaml.
type Run struct {
	Number     int        `yaml:"number"`
	SessionID  string     `yaml:"session_id"`
	Reason     RunReason  `yaml:"reason"`
	Skill      string     `yaml:"skill"`
	QueuedAt   time.Time  `yaml:"queued_at"`
	StartedAt  *time.Time `yaml:"started_at"`
	FinishedAt *time.Time `yaml:"finished_at"`
	PID        int        `yaml:"pid"`
	Outcome    Outcome    `yaml:"outcome"`
	Error      string     `yaml:"error"`
	ExitCode   *int       `yaml:"exit_code"`
	RetryOf    int        `yaml:"retry_of"`
}

// Clone returns a deep copy of the run.
func (r Run) Clone() Run {
	c := r
	if r.StartedAt != nil {
		t := *r.StartedAt
		c.StartedAt = &t
	}
	if r.FinishedAt != nil {
		t := *r.FinishedAt
		c.FinishedAt = &t
	}
	if r.ExitCode != nil {
		e := *r.ExitCode
		c.ExitCode = &e
	}
	return c
}

// Validate checks the run metadata.
func (r Run) Validate() error {
	if r.Number < 1 {
		return errors.New("run number must be at least 1")
	}
	if !r.Reason.Valid() {
		return errors.New("unsupported run reason")
	}
	if !r.Outcome.Valid() {
		return errors.New("unsupported run outcome")
	}
	return nil
}
