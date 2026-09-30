// Package result defines the result.json contract of preparation runs: the
// Go types, the JSON schema handed to the agent and the validation applied
// when a run finishes.
package result

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Version is the only supported result.json version.
const Version = 1

// Statuses of a result.
const (
	StatusReady   = "ready"
	StatusBlocked = "blocked"
)

// Confidence values.
const (
	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"
)

// ActionTypes are the supported proposed action types.
var ActionTypes = []string{"create-pr", "rollback", "scale", "silence", "suggest-ticket", "other"}

// Result is a validated result.json document.
type Result struct {
	Version         int      `json:"version"`
	Status          string   `json:"status"`
	Summary         string   `json:"summary"`
	Confidence      string   `json:"confidence,omitempty"`
	RootCause       string   `json:"root_cause,omitempty"`
	Assumptions     []string `json:"assumptions"`
	Gate            Gate     `json:"gate"`
	ProposedActions []Action `json:"proposed_actions"`
}

// Gate is the stop point of the preparation run.
type Gate struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// Action is a proposed (write) action the user can continue with.
type Action struct {
	ID          int    `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Preview     string `json:"preview,omitempty"`
}

// Parse decodes and validates a result.json document. It accepts exactly one
// JSON object whose fields have the declared JSON types; unknown fields are
// ignored for forward compatibility. Errors name the offending field and rule
// but never include field values.
func Parse(data []byte) (*Result, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty document")
		}
		return nil, fmt.Errorf("malformed JSON: %s", syntaxError(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON object")
	}
	obj, err := object("result", raw)
	if err != nil {
		return nil, err
	}

	var r Result
	if r.Version, err = requiredInt(obj, "version"); err != nil {
		return nil, err
	}
	if r.Version != Version {
		return nil, fmt.Errorf("version: must be %d", Version)
	}
	if r.Status, err = requiredString(obj, "status"); err != nil {
		return nil, err
	}
	if r.Status != StatusReady && r.Status != StatusBlocked {
		return nil, errors.New("status: must be ready or blocked")
	}
	if r.Summary, err = requiredText(obj, "summary"); err != nil {
		return nil, err
	}
	var ok bool
	if r.Confidence, ok, err = optionalString(obj, "confidence"); err != nil {
		return nil, err
	}
	switch {
	case !ok && r.Status == StatusReady:
		return nil, errors.New("confidence: required when status is ready")
	case ok && r.Confidence != ConfidenceHigh && r.Confidence != ConfidenceMedium && r.Confidence != ConfidenceLow:
		return nil, errors.New("confidence: must be high, medium or low")
	}
	if r.RootCause, _, err = optionalString(obj, "root_cause"); err != nil {
		return nil, err
	}
	if r.Assumptions, err = requiredStrings(obj, "assumptions"); err != nil {
		return nil, err
	}

	gateRaw, err := required(obj, "gate")
	if err != nil {
		return nil, err
	}
	gate, err := object("gate", gateRaw)
	if err != nil {
		return nil, err
	}
	gate = prefixed(gate, "gate.")
	if r.Gate.Question, err = requiredText(gate, "gate.question"); err != nil {
		return nil, err
	}
	if v, ok := gate["gate.options"]; ok && !isNull(v) {
		if r.Gate.Options, err = stringList("gate.options", v); err != nil {
			return nil, err
		}
	}

	actionsRaw, err := required(obj, "proposed_actions")
	if err != nil {
		return nil, err
	}
	items, err := array("proposed_actions", actionsRaw)
	if err != nil {
		return nil, err
	}
	r.ProposedActions = make([]Action, 0, len(items))
	for i, it := range items {
		a, err := action(i, it)
		if err != nil {
			return nil, err
		}
		r.ProposedActions = append(r.ProposedActions, a)
	}
	return &r, nil
}

func action(i int, raw json.RawMessage) (Action, error) {
	f := fmt.Sprintf("proposed_actions[%d]", i)
	obj, err := object(f, raw)
	if err != nil {
		return Action{}, err
	}
	obj = prefixed(obj, f+".")
	var a Action
	if a.ID, err = requiredInt(obj, f+".id"); err != nil {
		return a, err
	}
	if a.ID != i+1 {
		return a, fmt.Errorf("%s.id: must be %d (ids are 1..N in list order)", f, i+1)
	}
	if a.Type, err = requiredString(obj, f+".type"); err != nil {
		return a, err
	}
	if !slices.Contains(ActionTypes, a.Type) {
		return a, fmt.Errorf("%s.type: must be one of %s", f, strings.Join(ActionTypes, ", "))
	}
	if a.Title, err = requiredText(obj, f+".title"); err != nil {
		return a, err
	}
	if a.Description, err = requiredText(obj, f+".description"); err != nil {
		return a, err
	}
	if a.Preview, _, err = optionalString(obj, f+".preview"); err != nil {
		return a, err
	}
	return a, nil
}

// syntaxError describes a JSON decoding error without echoing content.
func syntaxError(err error) string {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Sprintf("invalid syntax at offset %d", se.Offset)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected end of input"
	}
	return "invalid document"
}

// prefixed returns obj with its keys prefixed, so nested fields are looked up
// and reported by their full name.
func prefixed(obj map[string]json.RawMessage, prefix string) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(obj))
	for k, v := range obj {
		out[prefix+k] = v
	}
	return out
}

func isNull(raw json.RawMessage) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// kind returns the first byte of a JSON value, which identifies its type.
func kind(raw json.RawMessage) byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return 0
	}
	return raw[0]
}

func object(field string, raw json.RawMessage) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if kind(raw) != '{' || json.Unmarshal(raw, &obj) != nil {
		return nil, fmt.Errorf("%s: must be an object", field)
	}
	return obj, nil
}

func array(field string, raw json.RawMessage) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if kind(raw) != '[' || json.Unmarshal(raw, &items) != nil {
		return nil, fmt.Errorf("%s: must be an array", field)
	}
	return items, nil
}

// required returns a present, non-null value.
func required(obj map[string]json.RawMessage, field string) (json.RawMessage, error) {
	v, ok := obj[field]
	if !ok || isNull(v) {
		return nil, fmt.Errorf("%s: required", field)
	}
	return v, nil
}

func str(field string, raw json.RawMessage) (string, error) {
	var s string
	if kind(raw) != '"' || json.Unmarshal(raw, &s) != nil {
		return "", fmt.Errorf("%s: must be a string", field)
	}
	return s, nil
}

func requiredString(obj map[string]json.RawMessage, field string) (string, error) {
	v, err := required(obj, field)
	if err != nil {
		return "", err
	}
	return str(field, v)
}

// requiredText is a required string that is neither empty nor whitespace
// only.
func requiredText(obj map[string]json.RawMessage, field string) (string, error) {
	s, err := requiredString(obj, field)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s) == "" {
		return "", fmt.Errorf("%s: must not be empty", field)
	}
	return s, nil
}

// optionalString returns the string value and whether it was provided. A
// null value counts as omitted.
func optionalString(obj map[string]json.RawMessage, field string) (string, bool, error) {
	v, ok := obj[field]
	if !ok || isNull(v) {
		return "", false, nil
	}
	s, err := str(field, v)
	if err != nil {
		return "", false, err
	}
	return s, true, nil
}

func requiredInt(obj map[string]json.RawMessage, field string) (int, error) {
	v, err := required(obj, field)
	if err != nil {
		return 0, err
	}
	var n int
	if k := kind(v); (k != '-' && (k < '0' || k > '9')) || json.Unmarshal(v, &n) != nil {
		return 0, fmt.Errorf("%s: must be an integer", field)
	}
	return n, nil
}

// stringList decodes an array of strings.
func stringList(field string, raw json.RawMessage) ([]string, error) {
	items, err := array(field, raw)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		s, err := str(fmt.Sprintf("%s[%d]", field, i), it)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func requiredStrings(obj map[string]json.RawMessage, field string) ([]string, error) {
	v, err := required(obj, field)
	if err != nil {
		return nil, err
	}
	return stringList(field, v)
}
