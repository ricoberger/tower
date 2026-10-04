package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `
state_dir: $HOME/.local/state/tower
app:
  retention: 48h
agent:
  run_command: copilot --yolo --session-id={{.SessionID}} -p {{ .Prompt }} --dir=$HOME
  resume_command: ghostty-new --title={{.Title}} --command 'copilot --resume={{.SessionID}} --name={{shquote .Title}}'
providers:
  alerts:
    poll_interval: 1m
    reopen_window: 24h
    sources:
      - name: dev-de1
        grafana_instance: dev
        grafana_alertmanager: grafana
        filter:
          - team="infra"
        receiver: ""
    prompt: "Investigate {{.Title}} from {{.Source}}"
  pullrequests:
    poll_interval: 5m
    max_age: 336h
    sources:
      - name: review-requested
        query: is:open review-requested:@me -author:app/dependabot
    prompt: "Review {{.URL}}"
  tasks:
    prompt: "{{.Title}}: {{.Description}}"
`

func load(t *testing.T, data string) (*Config, error) {
	t.Helper()
	t.Setenv("HOME", "/home/rico")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoad(t *testing.T) {
	cfg, err := load(t, validConfig)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StateDir != "/home/rico/.local/state/tower" {
		t.Errorf("state_dir = %q", cfg.StateDir)
	}
	if cfg.Agent.RunCommand != "copilot --yolo --session-id={{.SessionID}} -p {{ .Prompt }} --dir=$HOME" {
		t.Errorf("run_command = %q", cfg.Agent.RunCommand)
	}
	if cfg.App.Retention.Duration != 48*time.Hour || cfg.Providers.Alerts.PollInterval.Duration != time.Minute {
		t.Errorf("durations = %v %v", cfg.App.Retention, cfg.Providers.Alerts.PollInterval)
	}
	if s := cfg.Providers.Alerts.Sources[0]; s.Name != "dev-de1" || s.GrafanaInstance != "dev" || s.Filter[0] != `team="infra"` {
		t.Errorf("source = %+v", s)
	}
	if pr := cfg.Providers.PullRequests; pr.MaxAge.Duration != 336*time.Hour || pr.Sources[0].Query != "is:open review-requested:@me -author:app/dependabot" {
		t.Errorf("pullrequests = %+v", pr)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct{ name, from, to, want string }{
		{name: "unknown key", from: "  retention: 48h\n", to: "  retention: 48h\n  foo: bar\n", want: `unknown field "foo"`},
		{name: "unknown list key", from: "        grafana_instance: dev\n", to: "        type: alertmanager\n        grafana_instance: dev\n", want: `unknown field "type"`},
		{name: "bad duration", from: "retention: 48h", to: "retention: 2d", want: "invalid duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !strings.Contains(validConfig, tt.from) {
				t.Fatalf("test config does not contain %q", tt.from)
			}
			_, err := load(t, strings.Replace(validConfig, tt.from, tt.to, 1))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}
