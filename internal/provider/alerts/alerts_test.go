package alerts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/config/helpers"
	"github.com/ricoberger/tower/internal/store"
)

const snapshot = `[
 {"fingerprint": "abc123", "labels": {"alertname": "KubePodCrashLooping", "namespace": "api", "pod": "api-0", "severity": "critical"},
  "annotations": {"runbook_url": "https://runbooks/crash"}, "startsAt": "2026-01-02T03:04:05Z",
  "generatorURL": "https://grafana/alert", "status": {"state": "active"}, "receivers": [{"name": "incidentio"}]},
 {"fingerprint": "def456", "labels": {"alertname": "Silenced"}, "annotations": {}, "startsAt": "2026-01-02T03:04:05Z",
  "status": {"state": "suppressed"}},
 {"fingerprint": "ghi789", "labels": {"alertname": "New"}, "status": {"state": "unprocessed"}}
]`

func TestFetch(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		_, _ = w.Write([]byte(snapshot))
	}))
	defer srv.Close()

	c := NewClient(SourceConfig{
		Name: "dev", URL: srv.URL + "/", GrafanaAlertmanager: "grafana", TokenCommand: "echo tok",
		Filter: []string{`team="infra"`, `severity!="info"`}, Receiver: "incidentio",
	})
	alerts, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/api/alertmanager/grafana/api/v2/alerts" {
		t.Errorf("path = %s", got.URL.Path)
	}
	q := got.URL.Query()
	if q.Get("active") != "true" || q.Get("silenced") != "true" || q.Get("inhibited") != "true" || q.Get("unprocessed") != "false" ||
		len(q["filter"]) != 2 || q.Get("receiver") != "incidentio" {
		t.Errorf("query = %v", q)
	}
	if h := got.Header.Get("Authorization"); h != "Bearer tok" {
		t.Errorf("auth = %q", h)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want 2 (unprocessed skipped)", len(alerts))
	}
	if alerts[0].Fingerprint != "abc123" || alerts[1].Status.State != "suppressed" {
		t.Errorf("alerts = %+v", alerts)
	}
}

func TestFetchHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("bad token"))
	}))
	defer srv.Close()
	c := NewClient(SourceConfig{Name: "dev", URL: srv.URL, GrafanaAlertmanager: "grafana", TokenCommand: "echo tok"})
	_, err := c.Fetch(context.Background())
	if err == nil || err.Error() != "HTTP 401 (token expired?)" {
		t.Fatalf("err = %v", err)
	}
}

func TestFetchTokenCommandFailure(t *testing.T) {
	c := NewClient(SourceConfig{Name: "dev", URL: "http://127.0.0.1:1", GrafanaAlertmanager: "grafana", TokenCommand: "echo leaked; exit 3"}) //nolint:gosec // test fixture
	_, err := c.Fetch(context.Background())
	if err == nil || err.Error() != "token command: exit status 3" {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeErrors(t *testing.T) {
	for _, in := range []string{`{}`, `null`, `[{"fingerprint": "../x"}]`, `[{"fingerprint": 1}]`} {
		if _, err := Decode([]byte(in)); err == nil {
			t.Errorf("%s: no error", in)
		}
	}
}

const wantDetails = `# KubePodCrashLooping

- **Grafana Instance:** dev-de1
- **Grafana URL:** https://grafana.dev
- **Grafana Credentials:** resolve via ` + "`$GRAFANA_INSTANCES`" + ` instance ` + "`dev-de1`" + `
- **Grafana Alertmanager Datasource:** grafana
- **Severity:** critical
- **State:** active
- **Started:** 2026-01-02T03:04:05Z
- **Receivers:** incidentio
- **Source:** https://grafana/alert

## Summary

Pod is crash looping

## Description

api-0 restarted 5 times

## Labels

- ` + "`alertname`: `KubePodCrashLooping`" + `
- ` + "`namespace`: `api`" + `
- ` + "`pod`: `api-0`" + `
- ` + "`severity`: `critical`" + `

## Annotations

- ` + "`runbook_url`: `https://runbooks/crash`" + `
`

func TestItem(t *testing.T) {
	alerts, err := Decode([]byte(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	a := alerts[0]
	a.Annotations["summary"] = "Pod is crash looping"
	a.Annotations["description"] = "api-0 restarted 5 times"
	src := SourceConfig{Name: "dev", GrafanaInstance: "dev-de1", GrafanaAlertmanager: "grafana", URL: "https://grafana.dev"}
	it, err := Item(src, a)
	if err != nil {
		t.Fatal(err)
	}
	want := store.Incoming{
		Key:         "alert:dev:abc123",
		CreatedAt:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Title:       "KubePodCrashLooping · critical · dev",
		Description: "Pod is crash looping\napi-0 restarted 5 times",
		Details:     wantDetails,
		URL:         "https://grafana/alert",
	}
	if it != want {
		t.Errorf("item = %+v", it)
	}
	if it.Details != wantDetails {
		t.Errorf("details:\n%s", it.Details)
	}
}

func TestItemNormalizesTitleWhitespace(t *testing.T) {
	a := Alert{
		Fingerprint: "abc123",
		Labels: map[string]string{
			"alertname": "  Pod\n\r\nCrash\u2028Loop  ",
			"severity":  "critical\t alert",
		},
	}
	it, err := Item(SourceConfig{Name: "dev\r west"}, a)
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "Pod Crash Loop · critical alert · dev west" {
		t.Fatalf("title = %q", it.Title)
	}
	if !strings.Contains(it.Details, a.Labels["alertname"]) {
		t.Fatal("normalizing the title changed the raw alert details")
	}
}

func TestItemWithoutAnnotations(t *testing.T) {
	alerts, err := Decode([]byte(snapshot))
	if err != nil {
		t.Fatal(err)
	}
	it, err := Item(SourceConfig{Name: "dev"}, alerts[1])
	if err != nil {
		t.Fatal(err)
	}
	if it.Title != "Silenced · · dev" || it.Description != "" || it.URL != "" || !strings.Contains(it.Details, "- **State:** suppressed") {
		t.Errorf("item = %+v", it)
	}
}

func TestPrompt(t *testing.T) {
	t.Setenv("GRAFANA_INSTANCES", `{}`)
	p, err := New(Config{Prompt: "{{.ID}} {{.Title}} {{.URL}}\n{{.Details}}"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Prompt(store.Item{ID: 7, Title: "T", URL: "https://u", Details: "# D"})
	if err != nil || got != "7 T https://u\n# D" {
		t.Fatalf("prompt = %q, err = %v", got, err)
	}
}

func TestNew(t *testing.T) {
	t.Setenv("GRAFANA_INSTANCES", `{"dev": {"url": "https://grafana.example", "auth": {"tokenCommand": "echo secret"}}}`)
	p, err := New(Config{Prompt: "Investigate {{.Title}}", Sources: []SourceConfig{{Name: "dev-de1", GrafanaInstance: "dev"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.clients) != 1 {
		t.Fatalf("clients = %d", len(p.clients))
	}
	if s := p.clients[0].source; s.Name != "dev-de1" || s.URL != "https://grafana.example" || s.TokenCommand != "echo secret" {
		t.Errorf("source = %+v", s)
	}
}

func TestNewErrors(t *testing.T) {
	tests := []struct{ name, prompt, instance, env, want string }{
		{name: "bad template", prompt: "{{.Title", want: "parse prompt"},
		{name: "unknown instance", instance: "prod", want: "prod not found in $GRAFANA_INSTANCES"},
		{name: "bad instances json", env: "{", want: "parse $GRAFANA_INSTANCES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := `{"dev": {"url": "https://grafana.example", "auth": {"tokenCommand": "echo secret"}}}`
			if tt.env != "" {
				env = tt.env
			}
			t.Setenv("GRAFANA_INSTANCES", env)
			c := Config{Prompt: "Investigate {{.Title}}", Sources: []SourceConfig{{Name: "dev-de1", GrafanaInstance: "dev"}}}
			if tt.prompt != "" {
				c.Prompt = tt.prompt
			}
			if tt.instance != "" {
				c.Sources[0].GrafanaInstance = tt.instance
			}
			if _, err := New(c); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPollPollNow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trigger := make(chan struct{})
	out := make(chan store.Update)
	t.Setenv("GRAFANA_INSTANCES", `{"dev": {"url": "`+srv.URL+`", "auth": {"tokenCommand": "echo t"}}}`)
	src := SourceConfig{Name: "dev", GrafanaInstance: "dev", GrafanaAlertmanager: "grafana"}
	p, err := New(Config{Sources: []SourceConfig{src}, PollInterval: helpers.Duration{Duration: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	go p.Poll(ctx, trigger, out)
	for range 2 {
		select {
		case u := <-out:
			if u.Err != nil || u.Kind != Kind || u.Source != "dev" || u.Items == nil {
				t.Fatalf("update = %+v", u)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("no update")
		}
		trigger <- struct{}{}
	}
}
