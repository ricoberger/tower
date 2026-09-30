package result

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// productExample is the §9.3 example of the product specification.
const productExample = `{
  "version": 1,
  "status": "ready",
  "summary": "api-x pods crash-loop since the 09:02 rollout of v1.42.0: missing env var PAYMENTS_URL.",
  "confidence": "high",
  "root_cause": "Deployment core/api-x v1.42.0 references PAYMENTS_URL which is not set in the HelmRelease values.",
  "assumptions": [
    "The rollout at 09:02 is the only change in the time window."
  ],
  "gate": {
    "question": "Do you want me to prepare a rollback PR to v1.41.3 in Staffbase/mops?",
    "options": ["Prepare rollback PR", "Prepare fix PR adding PAYMENTS_URL", "Stop here"]
  },
  "proposed_actions": [
    {
      "id": 1,
      "type": "create-pr",
      "title": "Roll back api-x to v1.41.3",
      "description": "Revert the image tag in kubernetes/prod-de1/core/api-x/helmrelease.yaml.",
      "preview": "` + "```diff\\n- tag: v1.42.0\\n+ tag: v1.41.3\\n```" + `"
    }
  ]
}`

func TestParseProductExample(t *testing.T) {
	r, err := Parse([]byte(productExample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Version != 1 || r.Status != StatusReady || r.Confidence != ConfidenceHigh ||
		!strings.HasPrefix(r.Summary, "api-x pods") || !strings.HasPrefix(r.RootCause, "Deployment") {
		t.Errorf("result = %+v", r)
	}
	if len(r.Assumptions) != 1 || len(r.Gate.Options) != 3 || !strings.HasPrefix(r.Gate.Question, "Do you want") {
		t.Errorf("assumptions/gate = %q %+v", r.Assumptions, r.Gate)
	}
	if len(r.ProposedActions) != 1 {
		t.Fatalf("actions = %+v", r.ProposedActions)
	}
	a := r.ProposedActions[0]
	if a.ID != 1 || a.Type != "create-pr" || a.Title == "" || a.Description == "" || !strings.Contains(a.Preview, "+ tag: v1.41.3") {
		t.Errorf("action = %+v", a)
	}
}

func TestParseValidVariants(t *testing.T) {
	cases := map[string]string{
		"blocked without confidence": `{"version":1,"status":"blocked","summary":"s","assumptions":[],"gate":{"question":"which cluster?"},"proposed_actions":[]}`,
		"empty arrays":               `{"version":1,"status":"ready","summary":"s","confidence":"low","assumptions":[],"gate":{"question":"q","options":[]},"proposed_actions":[]}`,
		"unknown fields ignored":     `{"version":1,"status":"ready","summary":"s","confidence":"high","assumptions":["a"],"gate":{"question":"q","extra":1},"proposed_actions":[{"id":1,"type":"silence","title":"t","description":"d","x":[1]}],"future":{"a":true}}`,
		"many sentences":             `{"version":1,"status":"ready","summary":"One. Two. Three. Four. Five! Six?","confidence":"high","assumptions":[],"gate":{"question":"q"},"proposed_actions":[]}`,
		"no punctuation":             `{"version":1,"status":"ready","summary":"no sentence terminator at all","confidence":"high","assumptions":[],"gate":{"question":"q"},"proposed_actions":[]}`,
		"surrounding whitespace":     " \n" + `{"version":1,"status":"blocked","summary":"s","assumptions":[],"gate":{"question":"q"},"proposed_actions":[]}` + "\n\t",
		"all action types": `{"version":1,"status":"ready","summary":"s","confidence":"high","assumptions":[],"gate":{"question":"q"},"proposed_actions":[
			{"id":1,"type":"create-pr","title":"t","description":"d"},{"id":2,"type":"rollback","title":"t","description":"d"},
			{"id":3,"type":"scale","title":"t","description":"d"},{"id":4,"type":"silence","title":"t","description":"d"},
			{"id":5,"type":"suggest-ticket","title":"t","description":"d"},{"id":6,"type":"other","title":"t","description":"d"}]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(doc)); err != nil {
				t.Errorf("Parse: %v", err)
			}
		})
	}
}

// mutate returns the product example with a top-level or nested change
// applied through a generic map.
func mutate(t *testing.T, fn func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(productExample), &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func action0(m map[string]any) map[string]any {
	return m["proposed_actions"].([]any)[0].(map[string]any)
}

func TestParseRejects(t *testing.T) {
	secret := "SECRET-VALUE-42"
	cases := []struct {
		name string
		fn   func(m map[string]any)
		want string
	}{
		{"missing version", func(m map[string]any) { delete(m, "version") }, "version: required"},
		{"version 2", func(m map[string]any) { m["version"] = 2 }, "version: must be 1"},
		{"version string", func(m map[string]any) { m["version"] = "1" }, "version: must be an integer"},
		{"version float", func(m map[string]any) { m["version"] = 1.5 }, "version: must be an integer"},
		{"missing status", func(m map[string]any) { delete(m, "status") }, "status: required"},
		{"bad status", func(m map[string]any) { m["status"] = secret }, "status: must be ready or blocked"},
		{"status number", func(m map[string]any) { m["status"] = 1 }, "status: must be a string"},
		{"missing summary", func(m map[string]any) { delete(m, "summary") }, "summary: required"},
		{"null summary", func(m map[string]any) { m["summary"] = nil }, "summary: required"},
		{"blank summary", func(m map[string]any) { m["summary"] = " \n\t " }, "summary: must not be empty"},
		{"summary array", func(m map[string]any) { m["summary"] = []any{secret} }, "summary: must be a string"},
		{"ready without confidence", func(m map[string]any) { delete(m, "confidence") }, "confidence: required when status is ready"},
		{"ready null confidence", func(m map[string]any) { m["confidence"] = nil }, "confidence: must be a string"},
		{"blocked null confidence", func(m map[string]any) { m["status"] = "blocked"; m["confidence"] = nil }, "confidence: must be a string"},
		{"null root_cause", func(m map[string]any) { m["root_cause"] = nil }, "root_cause: must be a string"},
		{"null options", func(m map[string]any) { m["gate"] = map[string]any{"question": "q", "options": nil} }, "gate.options: must be an array"},
		{"null preview", func(m map[string]any) { action0(m)["preview"] = nil }, "proposed_actions[0].preview: must be a string"},
		{"bad confidence", func(m map[string]any) { m["confidence"] = secret }, "confidence: must be high, medium or low"},
		{"blocked bad confidence", func(m map[string]any) { m["status"] = "blocked"; m["confidence"] = "certain" }, "confidence: must be"},
		{"root_cause number", func(m map[string]any) { m["root_cause"] = 3 }, "root_cause: must be a string"},
		{"missing assumptions", func(m map[string]any) { delete(m, "assumptions") }, "assumptions: required"},
		{"null assumptions", func(m map[string]any) { m["assumptions"] = nil }, "assumptions: required"},
		{"assumptions string", func(m map[string]any) { m["assumptions"] = secret }, "assumptions: must be an array"},
		{"assumptions numbers", func(m map[string]any) { m["assumptions"] = []any{1} }, "assumptions[0]: must be a string"},
		{"missing gate", func(m map[string]any) { delete(m, "gate") }, "gate: required"},
		{"gate string", func(m map[string]any) { m["gate"] = secret }, "gate: must be an object"},
		{"gate array", func(m map[string]any) { m["gate"] = []any{} }, "gate: must be an object"},
		{"missing question", func(m map[string]any) { m["gate"] = map[string]any{} }, "gate.question: required"},
		{"blank question", func(m map[string]any) { m["gate"] = map[string]any{"question": "  "} }, "gate.question: must not be empty"},
		{"options string", func(m map[string]any) { m["gate"] = map[string]any{"question": "q", "options": secret} }, "gate.options: must be an array"},
		{"options objects", func(m map[string]any) {
			m["gate"] = map[string]any{"question": "q", "options": []any{"a", map[string]any{}}}
		}, "gate.options[1]: must be a string"},
		{"missing actions", func(m map[string]any) { delete(m, "proposed_actions") }, "proposed_actions: required"},
		{"null actions", func(m map[string]any) { m["proposed_actions"] = nil }, "proposed_actions: required"},
		{"actions object", func(m map[string]any) { m["proposed_actions"] = map[string]any{} }, "proposed_actions: must be an array"},
		{"action string", func(m map[string]any) { m["proposed_actions"] = []any{secret} }, "proposed_actions[0]: must be an object"},
		{"action id zero", func(m map[string]any) { action0(m)["id"] = 0 }, "proposed_actions[0].id: must be 1"},
		{"action id missing", func(m map[string]any) { delete(action0(m), "id") }, "proposed_actions[0].id: required"},
		{"action id string", func(m map[string]any) { action0(m)["id"] = "1" }, "proposed_actions[0].id: must be an integer"},
		{"action gap", func(m map[string]any) {
			second := map[string]any{"id": 3, "type": "other", "title": "t", "description": "d"}
			m["proposed_actions"] = append(m["proposed_actions"].([]any), second)
		}, "proposed_actions[1].id: must be 2"},
		{"action duplicate", func(m map[string]any) {
			second := map[string]any{"id": 1, "type": "other", "title": "t", "description": "d"}
			m["proposed_actions"] = append(m["proposed_actions"].([]any), second)
		}, "proposed_actions[1].id: must be 2"},
		{"action reordered", func(m map[string]any) {
			action0(m)["id"] = 2
			second := map[string]any{"id": 1, "type": "other", "title": "t", "description": "d"}
			m["proposed_actions"] = append(m["proposed_actions"].([]any), second)
		}, "proposed_actions[0].id: must be 1"},
		{"action bad type", func(m map[string]any) { action0(m)["type"] = secret }, "proposed_actions[0].type: must be one of"},
		{"action missing type", func(m map[string]any) { delete(action0(m), "type") }, "proposed_actions[0].type: required"},
		{"action blank title", func(m map[string]any) { action0(m)["title"] = "\t" }, "proposed_actions[0].title: must not be empty"},
		{"action missing description", func(m map[string]any) { delete(action0(m), "description") }, "proposed_actions[0].description: required"},
		{"action blank description", func(m map[string]any) { action0(m)["description"] = " " }, "proposed_actions[0].description: must not be empty"},
		{"action preview number", func(m map[string]any) { action0(m)["preview"] = 1 }, "proposed_actions[0].preview: must be a string"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(mutate(t, c.fn))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error echoes a value: %v", err)
			}
		})
	}
}

func TestParseRejectsDocuments(t *testing.T) {
	valid := `{"version":1,"status":"blocked","summary":"s","assumptions":[],"gate":{"question":"q"},"proposed_actions":[]}`
	cases := map[string]struct{ doc, want string }{
		"empty":         {"", "empty document"},
		"whitespace":    {" \n ", "empty document"},
		"malformed":     {`{"version":1,`, "malformed JSON"},
		"syntax":        {`{"version" 1}`, "malformed JSON"},
		"trailing json": {valid + valid, "unexpected data after"},
		"trailing text": {valid + " x", "unexpected data after"},
		"array":         {"[" + valid + "]", "result: must be an object"},
		"null":          {"null", "result: must be an object"},
		"string":        {`"SECRET"`, "result: must be an object"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.doc))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
			if strings.Contains(err.Error(), "SECRET") {
				t.Errorf("error echoes content: %v", err)
			}
		})
	}
}

func TestSchema(t *testing.T) {
	var out bytes.Buffer
	if err := json.Indent(&out, []byte(Schema), "", "  "); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	var s struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(Schema), &s); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"version", "status", "summary", "assumptions", "gate", "proposed_actions"} {
		if !slices.Contains(s.Required, f) {
			t.Errorf("schema does not require %s", f)
		}
	}
	for _, f := range []string{"version", "status", "summary", "confidence", "root_cause", "assumptions", "gate", "proposed_actions"} {
		if _, ok := s.Properties[f]; !ok {
			t.Errorf("schema lacks %s", f)
		}
	}
	for _, typ := range ActionTypes {
		if !strings.Contains(Schema, `"`+typ+`"`) {
			t.Errorf("schema lacks action type %s", typ)
		}
	}
}
