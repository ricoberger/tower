// Package source defines the alert source interface and its implementations.
package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"
)

// Alert states as reported by the Alertmanager API.
const (
	StateActive      = "active"
	StateSuppressed  = "suppressed"
	StateUnprocessed = "unprocessed"
)

// Alert is one normalized alert from a source snapshot.
type Alert struct {
	Source       string
	Fingerprint  string
	Labels       map[string]string
	Annotations  map[string]string
	StartsAt     time.Time
	UpdatedAt    time.Time
	EndsAt       time.Time
	State        string // active | suppressed | unprocessed
	SilencedBy   []string
	InhibitedBy  []string
	Receivers    []string
	GeneratorURL string
	Raw          json.RawMessage
}

// Suppressed reports whether the alert is silenced or inhibited. An
// explicitly unprocessed alert is never suppressed, even when silencedBy or
// inhibitedBy are set: the unprocessed state takes precedence.
func (a Alert) Suppressed() bool {
	if a.Unprocessed() {
		return false
	}
	return a.State == StateSuppressed || len(a.SilencedBy) > 0 || len(a.InhibitedBy) > 0
}

// Unprocessed reports whether the alert is unprocessed. Unprocessed alerts are
// neutral: they neither create items, change existing items nor count as
// absence.
func (a Alert) Unprocessed() bool {
	return a.State == StateUnprocessed
}

// Active reports whether the alert is firing and not suppressed.
func (a Alert) Active() bool {
	return !a.Suppressed() && !a.Unprocessed()
}

// Source produces full snapshots of the alerts of one configured source.
type Source interface {
	Name() string
	Fetch(ctx context.Context) ([]Alert, error)
}

var fingerprintPattern = regexp.MustCompile(`^[A-Za-z0-9]+$`)

// ValidFingerprint reports whether fp is safe to use in item IDs and paths.
func ValidFingerprint(fp string) bool {
	return len(fp) <= 128 && fingerprintPattern.MatchString(fp)
}

type apiAlert struct {
	Fingerprint  string            `json:"fingerprint"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Status       struct {
		State       string   `json:"state"`
		SilencedBy  []string `json:"silencedBy"`
		InhibitedBy []string `json:"inhibitedBy"`
	} `json:"status"`
	Receivers []struct {
		Name string `json:"name"`
	} `json:"receivers"`
}

// DecodeAlert decodes a single alert in the GET /api/v2/alerts format.
func DecodeAlert(sourceName string, raw json.RawMessage) (Alert, error) {
	var a apiAlert
	if err := json.Unmarshal(raw, &a); err != nil {
		return Alert{}, err
	}
	if !ValidFingerprint(a.Fingerprint) {
		return Alert{}, errInvalidFingerprint
	}
	switch a.Status.State {
	case StateActive, StateSuppressed, StateUnprocessed:
	default:
		return Alert{}, fmt.Errorf("alert %s: %w", a.Fingerprint, errUnsupportedState)
	}
	out := Alert{
		Source:       sourceName,
		Fingerprint:  a.Fingerprint,
		Labels:       a.Labels,
		Annotations:  a.Annotations,
		StartsAt:     a.StartsAt,
		UpdatedAt:    a.UpdatedAt,
		EndsAt:       a.EndsAt,
		State:        a.Status.State,
		SilencedBy:   a.Status.SilencedBy,
		InhibitedBy:  a.Status.InhibitedBy,
		GeneratorURL: a.GeneratorURL,
		Raw:          append(json.RawMessage(nil), bytes.TrimSpace(raw)...),
	}
	if out.Labels == nil {
		out.Labels = map[string]string{}
	}
	if out.Annotations == nil {
		out.Annotations = map[string]string{}
	}
	for _, r := range a.Receivers {
		out.Receivers = append(out.Receivers, r.Name)
	}
	return out, nil
}

// Decoding failure categories. Their messages never contain response data.
var (
	errInvalidFingerprint   = errors.New("missing or invalid fingerprint")
	errUnsupportedState     = errors.New("unsupported status.state")
	errDuplicateFingerprint = errors.New("duplicate fingerprint")
	errNotArray             = errors.New("expected a JSON array")
)

// alertDecodeError is the failure to decode the alert at index of a snapshot.
type alertDecodeError struct {
	index int
	err   error
}

func (e *alertDecodeError) Error() string { return fmt.Sprintf("decode alert %d: %v", e.index, e.err) }

func (e *alertDecodeError) Unwrap() error { return e.err }

// DecodeSnapshot decodes a JSON array in the GET /api/v2/alerts format.
func DecodeSnapshot(sourceName string, data []byte) ([]Alert, error) {
	var raws []json.RawMessage
	if err := json.Unmarshal(data, &raws); err != nil {
		return nil, fmt.Errorf("decode alerts: %w", err)
	}
	if raws == nil {
		return nil, fmt.Errorf("decode alerts: %w", errNotArray)
	}
	alerts := make([]Alert, 0, len(raws))
	seen := map[string]bool{}
	for i, raw := range raws {
		a, err := DecodeAlert(sourceName, raw)
		if err != nil {
			return nil, &alertDecodeError{index: i, err: err}
		}
		if seen[a.Fingerprint] {
			return nil, &alertDecodeError{index: i, err: fmt.Errorf("%w %s", errDuplicateFingerprint, a.Fingerprint)}
		}
		seen[a.Fingerprint] = true
		alerts = append(alerts, a)
	}
	return alerts, nil
}

// File is a source that reads a JSON file in the GET /api/v2/alerts format on
// every fetch.
type File struct {
	name string
	path string
}

// NewFile creates a file source.
func NewFile(name, path string) *File {
	return &File{name: name, path: path}
}

// Name returns the configured source name.
func (f *File) Name() string { return f.name }

// Fetch reads and decodes the file. Read and decode failures are errors, never
// empty snapshots.
func (f *File) Fetch(ctx context.Context) ([]Alert, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(f.path) // #nosec G304 -- path comes from the user's configuration
	if err != nil {
		return nil, fmt.Errorf("read alerts file: %w", err)
	}
	return DecodeSnapshot(f.name, data)
}
