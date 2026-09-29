package prompt

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/source"
)

var update = flag.Bool("update", false, "update golden files")

// Synthetic credentials only.
const (
	synToken    = "syn-token-5d2a"    // #nosec G101 -- synthetic test value
	synPassword = "syn-password-8e3f" // #nosec G101 -- synthetic test value
)

func ptr(s string) *string { return &s }

func sampleAlert() source.Alert {
	return source.Alert{
		Source:      "prod",
		Fingerprint: "4f1c2a9b0d3e7f11",
		Labels: map[string]string{
			"alertname": "KubePodCrashLooping",
			"severity":  "critical",
			"namespace": "core",
			"pod":       "api-x-7d9c",
			"container": "api",
		},
		Annotations: map[string]string{
			"summary":     "Pod core/api-x-7d9c is crash looping.",
			"description": "Pod core/api-x-7d9c (api) is restarting 2.1 times / 10 minutes.",
			"runbook_url": "https://runbooks.example.com/KubePodCrashLooping",
			"dashboard":   "https://grafana.example.com/d/abc",
		},
		StartsAt:     time.Date(2026, 9, 28, 9, 5, 0, 0, time.UTC),
		State:        source.StateActive,
		Receivers:    []string{"team-core", "pager"},
		GeneratorURL: "https://grafana.example.com/alerting/grafana/abc/view",
	}
}

var credentialSource = config.Source{
	Name:     "legacy",
	Type:     config.SourceAlertmanager,
	URL:      ptr("https://syn-user:" + synPassword + "@alertmanager.example.com/prefix?x=1#frag"),
	Auth:     &config.Auth{Type: config.AuthBearer, Token: ptr(synToken)},
	Instance: nil,
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) // #nosec G304 -- test fixture
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("%s mismatch:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func assertNoSecrets(t *testing.T, md []byte) {
	t.Helper()
	for _, s := range []string{synToken, synPassword, "syn-user", "tokenCommand", "echo "} {
		if strings.Contains(string(md), s) {
			t.Errorf("alert.md contains %q:\n%s", s, md)
		}
	}
}

func TestGolden(t *testing.T) {
	tests := []struct {
		file string
		src  config.Source
	}{
		{"managed.md", config.Source{
			Name: "prod", Type: config.SourceAlertmanager, GrafanaInstance: "prod", GrafanaAlertmanager: "grafana",
			Instance: &config.GrafanaInstance{URL: "https://grafana.example.com", TokenCommand: "echo " + synToken},
		}},
		{"plain-instance.md", config.Source{
			Name: "legacy", Type: config.SourceAlertmanager, URL: ptr("https://alertmanager.example.com"),
			GrafanaInstance: "prod", Instance: &config.GrafanaInstance{URL: "https://grafana.example.com"},
			Auth: &config.Auth{Type: config.AuthBasic, Username: "u", Password: ptr(synPassword)},
		}},
		{"plain-no-instance.md", config.Source{
			Name: "legacy", Type: config.SourceAlertmanager, URL: ptr("https://alertmanager.example.com"),
			Auth: &config.Auth{Type: config.AuthBearer, Token: ptr(synToken)},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			md := AlertMarkdown(sampleAlert(), MetaFor(tt.src))
			golden(t, tt.file, md)
			assertNoSecrets(t, md)
		})
	}
}

func lines(md []byte) map[string]bool {
	out := map[string]bool{}
	for l := range strings.SplitSeq(string(md), "\n") {
		out[l] = true
	}
	return out
}

func hasPrefixLine(md []byte, prefix string) bool {
	for l := range lines(md) {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func TestMetadataLines(t *testing.T) {
	instance := &config.GrafanaInstance{URL: "https://grafana.example.com/sub", TokenCommand: "echo " + synToken}
	tests := []struct {
		name    string
		src     config.Source
		present []string
		absent  []string
	}{
		{
			name:    "file source",
			src:     config.Source{Name: "dev", Type: config.SourceFile, Path: "/x.json"},
			absent:  []string{"- **Alertmanager URL:**", "- **Grafana Instance:**", "- **Grafana URL:**", "- **Grafana Credentials:**", "- **Grafana Alertmanager Datasource:**"},
			present: []string{"- **Severity:** critical"},
		},
		{
			name: "file source with instance",
			src:  config.Source{Name: "dev", Type: config.SourceFile, Path: "/x.json", GrafanaInstance: "prod", Instance: instance},
			present: []string{
				"- **Grafana Instance:** prod",
				"- **Grafana URL:** https://grafana.example.com/sub",
				"- **Grafana Credentials:** resolve via `$GRAFANA_INSTANCES` instance `prod` (sre-grafana Option A)",
			},
			absent: []string{"- **Alertmanager URL:**", "- **Grafana Alertmanager Datasource:**"},
		},
		{
			name: "managed with explicit url override",
			src: config.Source{
				Name: "g", Type: config.SourceAlertmanager, GrafanaAlertmanager: "ds", GrafanaInstance: "prod",
				URL: ptr("https://override.example.com"), Instance: instance,
			},
			present: []string{"- **Grafana URL:** https://grafana.example.com/sub", "- **Grafana Alertmanager Datasource:** ds"},
			absent:  []string{"- **Alertmanager URL:**", "override.example.com"},
		},
		{
			name:    "managed without instance",
			src:     config.Source{Name: "g", Type: config.SourceAlertmanager, GrafanaAlertmanager: "ds", URL: ptr("https://grafana.example.com")},
			present: []string{"- **Grafana Alertmanager Datasource:** ds"},
			absent:  []string{"- **Alertmanager URL:**", "- **Grafana Instance:**", "- **Grafana URL:**", "- **Grafana Credentials:**"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := AlertMarkdown(sampleAlert(), MetaFor(tt.src))
			for _, p := range tt.present {
				if !lines(md)[p] {
					t.Errorf("missing line %q:\n%s", p, md)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(string(md), a) {
					t.Errorf("unexpected %q:\n%s", a, md)
				}
			}
			assertNoSecrets(t, md)
		})
	}
}

func TestEmptyOptionalData(t *testing.T) {
	md := string(AlertMarkdown(source.Alert{Fingerprint: "abc", State: source.StateActive}, AlertMeta{}))
	want := `#

- **Severity:**
- **State:** Active
- **Started:**
- **Receivers:**
- **Source:**

## Summary

## Description

## Labels

## Annotations
`
	if md != want {
		t.Errorf("got:\n%q\nwant:\n%q", md, want)
	}
}

func TestSuppressedState(t *testing.T) {
	for name, mutate := range map[string]func(*source.Alert){
		"state":     func(a *source.Alert) { a.State = source.StateSuppressed },
		"silenced":  func(a *source.Alert) { a.SilencedBy = []string{"s1"} },
		"inhibited": func(a *source.Alert) { a.InhibitedBy = []string{"i1"} },
	} {
		t.Run(name, func(t *testing.T) {
			a := sampleAlert()
			mutate(&a)
			if md := AlertMarkdown(a, AlertMeta{}); !lines(md)["- **State:** Suppressed"] {
				t.Errorf("not suppressed:\n%s", md)
			}
		})
	}
}

func TestAnnotationsNotDuplicated(t *testing.T) {
	md := string(AlertMarkdown(sampleAlert(), AlertMeta{}))
	annotations := md[strings.Index(md, "## Annotations"):]
	if strings.Contains(annotations, "summary") || strings.Contains(annotations, "description") {
		t.Errorf("summary/description listed as annotations:\n%s", annotations)
	}
	if !strings.Contains(annotations, "- `dashboard`: `https://grafana.example.com/d/abc`\n- `runbook_url`:") {
		t.Errorf("annotations not sorted:\n%s", annotations)
	}
}

func TestAlertmanagerURLUserinfoRemoved(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"user and password", "https://syn-user:" + synPassword + "@am.example.com:9093/prefix/?q=1#f", "https://am.example.com:9093/prefix/?q=1#f"},
		{"percent-encoded", "https://syn%2Duser:syn%2Dpassword%2D8e3f@am.example.com/p%20x", "https://am.example.com/p%20x"},
		{"username only", "http://syn-user@am.example.com", "http://am.example.com"},
		{"no userinfo", "https://am.example.com/a/b?c=d", "https://am.example.com/a/b?c=d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := credentialSource
			src.URL = ptr(tt.url)
			md := AlertMarkdown(sampleAlert(), MetaFor(src))
			if !lines(md)["- **Alertmanager URL:** "+tt.want] {
				t.Errorf("want URL %q:\n%s", tt.want, md)
			}
			for _, s := range []string{"syn-user", "syn%2Duser", "syn%2Dpassword", "@"} {
				if strings.Contains(string(md), s) {
					t.Errorf("rendered login details %q", s)
				}
			}
			assertNoSecrets(t, md)
			// The polling configuration is not altered.
			if *src.URL != tt.url {
				t.Errorf("source URL changed to %q", *src.URL)
			}
		})
	}
	// URLs that cannot be sanitized are omitted, never rendered verbatim.
	src := credentialSource
	src.URL = ptr("syn-user:" + synPassword + "@am.example.com")
	if md := AlertMarkdown(sampleAlert(), MetaFor(src)); hasPrefixLine(md, "- **Alertmanager URL:**") {
		t.Errorf("unsanitizable URL rendered:\n%s", md)
	}
}
