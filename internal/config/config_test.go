package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// envMap returns a LookupEnv backed by a map.
func envMap(m map[string]string) LookupEnv {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

const minimalSources = `
sources:
  - name: dev
    type: file
    path: alerts.json
`

// writeConfig writes content to <dir>/conf/config.yaml and returns the path.
func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "conf")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, content string, env map[string]string) (*Config, Report) {
	t.Helper()
	cfg, report, err := Load(writeConfig(t, content), envMap(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg, report
}

func mustLoad(t *testing.T, content string, env map[string]string) *Config {
	t.Helper()
	cfg, report := load(t, content, env)
	if !report.OK() {
		t.Fatalf("unexpected errors: %v", report.Errors)
	}
	return cfg
}

func hasMessage(msgs []string, sub string) bool {
	return slices.ContainsFunc(msgs, func(m string) bool { return strings.Contains(m, sub) })
}

func TestResolvePathPrecedence(t *testing.T) {
	home := "/home/u"
	tests := []struct {
		name    string
		flag    string
		env     map[string]string
		want    string
		wantErr string
	}{
		{"flag wins", "/a/flag.yaml", map[string]string{"TOWER_CONFIG": "/b/env.yaml", "HOME": home}, "/a/flag.yaml", ""},
		{"env when no flag", "", map[string]string{"TOWER_CONFIG": "/b/env.yaml", "HOME": home}, "/b/env.yaml", ""},
		{"empty env ignored", "", map[string]string{"TOWER_CONFIG": "", "HOME": home}, "/home/u/.config/tower/config.yaml", ""},
		{"default", "", map[string]string{"HOME": home}, "/home/u/.config/tower/config.yaml", ""},
		{"default requires HOME", "", map[string]string{}, "", "HOME"},
		{"default requires nonempty HOME", "", map[string]string{"HOME": ""}, "", "HOME"},
		{"flag tilde", "~/tower/config.yaml", map[string]string{"HOME": home}, "/home/u/tower/config.yaml", ""},
		{"env tilde", "", map[string]string{"TOWER_CONFIG": "~/t.yaml", "HOME": home}, "/home/u/t.yaml", ""},
		{"flag tilde requires HOME", "~/t.yaml", map[string]string{}, "", "--config"},
		{"env tilde requires HOME", "", map[string]string{"TOWER_CONFIG": "~/t.yaml"}, "", "TOWER_CONFIG"},
		{"flag relative to cwd", "rel/c.yaml", map[string]string{}, "/work/rel/c.yaml", ""},
		{"env relative to cwd", "", map[string]string{"TOWER_CONFIG": "c.yaml"}, "/work/c.yaml", ""},
		{"flag not interpolated", "$HOME/c.yaml", map[string]string{"HOME": home}, "/work/$HOME/c.yaml", ""},
		{"env not interpolated", "", map[string]string{"TOWER_CONFIG": "${X}/c.yaml", "X": "/x"}, "/work/${X}/c.yaml", ""},
		{"absolute without HOME", "/abs/c.yaml", map[string]string{}, "/abs/c.yaml", ""},
		{"bare tilde not expanded", "~", map[string]string{"HOME": home}, "/work/~", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolvePath(tt.flag, envMap(tt.env), "/work")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v (%q)", tt.wantErr, err, got)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, _, err := Load(filepath.Join(t.TempDir(), "nope.yaml"), envMap(nil))
	if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "tower config init") {
		t.Fatalf("want ErrNotFound with hint, got %v", err)
	}
}

func TestDefaults(t *testing.T) {
	cfg := mustLoad(t, minimalSources, map[string]string{"HOME": "/home/u"})
	want := Config{
		StateDir: "/home/u/.local/state/tower",
		Editor:   "vi",
		Alerts: Alerts{
			PollInterval: time.Minute, PrepareAfter: 5 * time.Minute, ResolvedLinger: 4 * time.Hour,
			ReopenWindow: 24 * time.Hour, SeverityOrder: []string{"critical", "error", "warning", "info"},
		},
		Runs:          Runs{Concurrency: 2, Timeout: 20 * time.Minute, Command: "copilot", Args: []string{"--yolo"}, Model: "", ResumeArgs: []string{}},
		Notifications: Notifications{Enabled: true, Sound: "default"},
		Ghostty:       Ghostty{Command: "ghostty-new", Placement: "split", Direction: "right"},
		Retention:     Retention{DoneAfter: 8760 * time.Hour},
		Prompts:       Prompts{Alert: ""},
	}
	if cfg.StateDir != want.StateDir || cfg.Editor != want.Editor {
		t.Errorf("state_dir/editor = %q/%q", cfg.StateDir, cfg.Editor)
	}
	if !equalAlerts(cfg.Alerts, want.Alerts) {
		t.Errorf("alerts = %+v", cfg.Alerts)
	}
	if cfg.Runs.Concurrency != 2 || cfg.Runs.Timeout != want.Runs.Timeout || cfg.Runs.Command != "copilot" ||
		!slices.Equal(cfg.Runs.Args, want.Runs.Args) || cfg.Runs.Model != "" || len(cfg.Runs.ResumeArgs) != 0 {
		t.Errorf("runs = %+v", cfg.Runs)
	}
	if cfg.Notifications != want.Notifications || cfg.Ghostty != want.Ghostty || cfg.Retention != want.Retention || cfg.Prompts != want.Prompts {
		t.Errorf("notifications/ghostty/retention/prompts = %+v %+v %+v %+v", cfg.Notifications, cfg.Ghostty, cfg.Retention, cfg.Prompts)
	}
	if len(cfg.Sources) != 1 {
		t.Errorf("sources = %+v", cfg.Sources)
	}
}

func equalAlerts(a, b Alerts) bool {
	return a.PollInterval == b.PollInterval && a.PrepareAfter == b.PrepareAfter && a.ResolvedLinger == b.ResolvedLinger &&
		a.ReopenWindow == b.ReopenWindow && slices.Equal(a.SeverityOrder, b.SeverityOrder)
}

func TestEditorDefaultFromEnv(t *testing.T) {
	cfg := mustLoad(t, minimalSources, map[string]string{"HOME": "/h", "EDITOR": "nvim"})
	if cfg.Editor != "nvim" {
		t.Fatalf("editor = %q", cfg.Editor)
	}
	// An $EDITOR value is not interpolated again but is path-normalized.
	cfg = mustLoad(t, minimalSources, map[string]string{"HOME": "/h", "EDITOR": "~/bin/$VIM"})
	if cfg.Editor != "/h/bin/$VIM" {
		t.Fatalf("editor = %q", cfg.Editor)
	}
	cfg = mustLoad(t, minimalSources, map[string]string{"HOME": "/h", "EDITOR": ""})
	if cfg.Editor != "vi" {
		t.Fatalf("editor = %q", cfg.Editor)
	}
}

func TestExplicitZeroValuesPreserved(t *testing.T) {
	cfg := mustLoad(t, minimalSources+`
state_dir: /s
alerts:
  prepare_after: 0s
  severity_order: []
runs:
  args: []
  resume_args: []
  model: ""
notifications:
  enabled: false
  sound: ""
`, nil)
	if cfg.Alerts.PrepareAfter != 0 || cfg.Notifications.Enabled || cfg.Notifications.Sound != "" {
		t.Fatalf("zero values not preserved: %+v %+v", cfg.Alerts, cfg.Notifications)
	}
	if cfg.Runs.Args == nil || len(cfg.Runs.Args) != 0 || len(cfg.Runs.ResumeArgs) != 0 || len(cfg.Alerts.SeverityOrder) != 0 {
		t.Fatalf("empty lists not preserved: %+v %+v", cfg.Runs, cfg.Alerts)
	}
}

func TestStrictDecoding(t *testing.T) {
	for name, content := range map[string]string{
		"unknown top-level":   minimalSources + "bogus: 1\n",
		"unknown nested":      minimalSources + "alerts:\n  poll_intervall: 1m\n",
		"unknown source key":  "sources:\n  - name: a\n    type: file\n    path: x\n    interval: 1m\n",
		"unknown auth key":    "sources:\n  - name: a\n    type: alertmanager\n    url: http://x\n    auth:\n      type: none\n      secret: x\n",
		"invalid bool":        minimalSources + "notifications:\n  enabled: maybe\n",
		"invalid int":         minimalSources + "runs:\n  concurrency: two\n",
		"list instead of str": minimalSources + "editor: [a, b]\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, content), envMap(map[string]string{"HOME": "/h"}))
			if err == nil {
				t.Fatal("want decode error")
			}
		})
	}
}

func TestNativeFieldsNotInterpolated(t *testing.T) {
	env := map[string]string{"HOME": "/h", "N": "3", "B": "false"}
	if _, _, err := Load(writeConfig(t, minimalSources+"runs:\n  concurrency: $N\n"), envMap(env)); err == nil {
		t.Error("integer field must not accept an environment reference")
	}
	if _, _, err := Load(writeConfig(t, minimalSources+"notifications:\n  enabled: $B\n"), envMap(env)); err == nil {
		t.Error("boolean field must not accept an environment reference")
	}
	// Keys are not interpolated: "$K" is an unknown key.
	if _, _, err := Load(writeConfig(t, minimalSources+"$K: x\n"), envMap(map[string]string{"K": "editor"})); err == nil {
		t.Error("keys must not be interpolated")
	}
}

func TestExpand(t *testing.T) {
	env := envMap(map[string]string{"A": "alpha", "B_2": "beta", "EMPTY": "", "NESTED": "$A ${B_2} $$", "HOME": "/h"})
	tests := []struct{ in, want string }{
		{"$A", "alpha"},
		{"${A}", "alpha"},
		{"x${A}y", "xalphay"},
		{"$A$B_2", "alphabeta"},
		{"$$", "$"},
		{"$$A", "$A"},
		{"$$HOME", "$HOME"},
		{"$$$A", "$alpha"},
		{"a$EMPTY-b", "a-b"},
		{"$NESTED", "$A ${B_2} $$"},
		{"^foo.*$", "^foo.*$"},
		{"(a|b)$", "(a|b)$"},
		{"$1", "$1"},
		{"$", "$"},
		{"a $ b", "a $ b"},
		{"${A:-fallback}", "${A:-fallback}"},
		{"${UNSET:-fallback}", "${UNSET:-fallback}"},
		{"$(echo hi)", "$(echo hi)"},
		{"${", "${"},
		{"${}", "${}"},
		{"no refs", "no refs"},
	}
	for _, tt := range tests {
		got, err := expand("f", tt.in, env)
		if err != nil || got != tt.want {
			t.Errorf("expand(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{"$UNSET", "${UNSET}", "x$UNSET_2y"} {
		_, err := expand("sources[0].url", in, env)
		if err == nil || !strings.Contains(err.Error(), "sources[0].url") || !strings.Contains(err.Error(), "UNSET") {
			t.Errorf("expand(%q) error = %v", in, err)
		}
	}
}

func TestInterpolationInConfig(t *testing.T) {
	env := map[string]string{
		"HOME":    "/h",
		"SD":      "/state",
		"POLL":    "30s",
		"ARG":     "--flag",
		"FILTER":  `team="core"`,
		"INJECT":  "x\"\nbogus: 1\n",
		"EMPTY":   "",
		"SEV":     "critical",
		"TOKEN":   "tok-sentinel",
		"RECEIVE": "incident",
	}
	cfg := mustLoad(t, `
state_dir: $SD
sources:
  - name: dev
    type: file
    path: ${SD}/alerts.json
  - name: am
    type: alertmanager
    url: https://am.example.com
    grafana_instance: "$INJECT"
    filter: [$FILTER, "severity=~\"$SEV\""]
    receiver: ".*${RECEIVE}.*$"
    auth:
      type: bearer
      token: $TOKEN
alerts:
  poll_interval: $POLL
  severity_order: [$SEV, info]
runs:
  args: [$ARG, "$$literal"]
  model: $EMPTY
`, env)
	if cfg.StateDir != "/state" || cfg.Sources[0].Path != "/state/alerts.json" {
		t.Errorf("paths = %q %q", cfg.StateDir, cfg.Sources[0].Path)
	}
	if cfg.Alerts.PollInterval != 30*time.Second {
		t.Errorf("poll_interval = %v", cfg.Alerts.PollInterval)
	}
	am := cfg.Sources[1]
	if am.GrafanaInstance != env["INJECT"] {
		t.Errorf("injected value must stay a plain string, got %q", am.GrafanaInstance)
	}
	if !slices.Equal(am.Filter, []string{`team="core"`, `severity=~"critical"`}) {
		t.Errorf("filter = %q", am.Filter)
	}
	if am.Receiver != ".*incident.*$" {
		t.Errorf("receiver = %q", am.Receiver)
	}
	if *am.Auth.Token != "tok-sentinel" {
		t.Errorf("token not interpolated")
	}
	if !slices.Equal(cfg.Alerts.SeverityOrder, []string{"critical", "info"}) {
		t.Errorf("severity_order = %q", cfg.Alerts.SeverityOrder)
	}
	if !slices.Equal(cfg.Runs.Args, []string{"--flag", "$literal"}) || cfg.Runs.Model != "" {
		t.Errorf("runs = %+v", cfg.Runs)
	}
}

func TestInterpolationErrors(t *testing.T) {
	_, report := load(t, minimalSources+"state_dir: /s\nruns:\n  model: $MISSING_MODEL\n", map[string]string{})
	if !hasMessage(report.Errors, "runs.model") || !hasMessage(report.Errors, "MISSING_MODEL") {
		t.Fatalf("errors = %v", report.Errors)
	}
	// An explicitly empty variable expands to empty and is then validated.
	_, report = load(t, minimalSources+"alerts:\n  poll_interval: $EMPTY\n", map[string]string{"HOME": "/h", "EMPTY": ""})
	if !hasMessage(report.Errors, "alerts.poll_interval: invalid duration") {
		t.Fatalf("errors = %v", report.Errors)
	}
	_, report = load(t, minimalSources+"state_dir: $EMPTY\n", map[string]string{"EMPTY": ""})
	if !hasMessage(report.Errors, "state_dir: must not be empty") {
		t.Fatalf("empty state_dir must not reinstate the default: %v", report.Errors)
	}
	_, report = load(t, "sources:\n  - name: a\n    type: file\n    path: $EMPTY\n", map[string]string{"HOME": "/h", "EMPTY": ""})
	if !hasMessage(report.Errors, "sources[0].path: required") {
		t.Fatalf("errors = %v", report.Errors)
	}
}

func TestCredentialCommandsPreserved(t *testing.T) {
	const tokenCmd = `cat ~/x | jq -r "$UNSET_VAR" $(whoami) ${HOME:-/} $$` // #nosec G101 -- shell syntax fixture, not a credential
	const passCmd = `security find-generic-password -s ./rel -w $NOPE`
	cfg := mustLoad(t, minimalSources+`
  - name: bearer
    type: alertmanager
    url: https://a
    grafana_instance: g
    auth:
      type: bearer
      token_command: '`+tokenCmd+`'
  - name: basic
    type: alertmanager
    url: https://b
    grafana_instance: g
    auth:
      type: basic
      username: u
      password_command: '`+passCmd+`'
`, map[string]string{"HOME": "/h"})
	if got := *cfg.Sources[1].Auth.TokenCommand; got != tokenCmd {
		t.Errorf("token_command = %q", got)
	}
	if got := *cfg.Sources[2].Auth.PasswordCommand; got != passCmd {
		t.Errorf("password_command = %q", got)
	}
}

func TestSecretsNotDisclosed(t *testing.T) {
	const sentinel = "SENTINEL-s3cr3t-value"
	env := map[string]string{"HOME": "/h", "TOK": sentinel, "PW": sentinel}
	cfg, report := load(t, `
sources:
  - name: a
    type: alertmanager
    url: https://a
    auth:
      type: bearer
      token: $TOK
      token_file: /x
  - name: b
    type: alertmanager
    url: https://b
    auth:
      type: basic
      password: ${PW}
  - name: BAD
    type: $TOK
alerts:
  poll_interval: $TOK
runs:
  model: $MISSING
`, env)
	all := strings.Join(append(report.Errors, report.Warnings...), "\n")
	if strings.Contains(all, sentinel) {
		t.Fatalf("diagnostics disclose a credential: %s", all)
	}
	if len(report.Errors) == 0 {
		t.Fatal("expected errors")
	}
	for _, s := range cfg.Sources {
		if s.Auth == nil {
			continue
		}
		for _, repr := range []string{s.Auth.String(), s.Auth.GoString(), s.Auth.LogValue().String()} {
			if strings.Contains(repr, sentinel) {
				t.Fatalf("auth representation discloses a credential: %s", repr)
			}
		}
	}
}

func TestPathNormalization(t *testing.T) {
	// Run from a different working directory than the config directory.
	t.Chdir(t.TempDir())
	env := map[string]string{"HOME": "/home/u", "SUB": "sub", "TILDE": "~/fromenv", "HOMEREF": "$HOME"}
	path := writeConfig(t, `
state_dir: ./state
editor: ./bin/ed
sources:
  - name: rel
    type: file
    path: data/$SUB/alerts.json
  - name: abs
    type: file
    path: /abs/alerts.json
  - name: tilde
    type: file
    path: ~/alerts.json
  - name: envtilde
    type: file
    path: $TILDE/a.json
  - name: homeref
    type: file
    path: $HOMEREF/a.json
  - name: am
    type: alertmanager
    url: https://x/~/y
    grafana_instance: ~/g
    auth:
      type: bearer
      token_file: secrets/token
  - name: am2
    type: alertmanager
    url: https://x
    grafana_instance: g
    auth:
      type: basic
      username: ~/u
      password_file: ~/pw
runs:
  command: tools/copilot
  args: [./rel, ~/arg, x~/y]
ghostty:
  command: ~/bin/ghostty-new
prompts:
  alert: prompts/alert.tmpl
`)
	cfg, report, err := Load(path, envMap(env))
	if err != nil || !report.OK() {
		t.Fatalf("Load: %v %v", err, report.Errors)
	}
	dir := filepath.Dir(path)
	checks := map[string][2]string{
		"state_dir":        {cfg.StateDir, filepath.Join(dir, "state")},
		"editor":           {cfg.Editor, filepath.Join(dir, "bin/ed")},
		"rel path":         {cfg.Sources[0].Path, filepath.Join(dir, "data/sub/alerts.json")},
		"abs path":         {cfg.Sources[1].Path, "/abs/alerts.json"},
		"tilde path":       {cfg.Sources[2].Path, "/home/u/alerts.json"},
		"env tilde path":   {cfg.Sources[3].Path, "/home/u/fromenv/a.json"},
		"home ref literal": {cfg.Sources[4].Path, filepath.Join(dir, "$HOME/a.json")},
		"url untouched":    {*cfg.Sources[5].URL, "https://x/~/y"},
		"grafana instance": {cfg.Sources[5].GrafanaInstance, "~/g"},
		"token_file":       {*cfg.Sources[5].Auth.TokenFile, filepath.Join(dir, "secrets/token")},
		"username":         {cfg.Sources[6].Auth.Username, "~/u"},
		"password_file":    {*cfg.Sources[6].Auth.PasswordFile, "/home/u/pw"},
		"runs.command":     {cfg.Runs.Command, filepath.Join(dir, "tools/copilot")},
		"ghostty.command":  {cfg.Ghostty.Command, "/home/u/bin/ghostty-new"},
		"prompts.alert":    {cfg.Prompts.Alert, filepath.Join(dir, "prompts/alert.tmpl")},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	if !slices.Equal(cfg.Runs.Args, []string{"./rel", "~/arg", "x~/y"}) {
		t.Errorf("args must not be path-normalized: %q", cfg.Runs.Args)
	}
}

func TestBareExecutablesAndTildeForms(t *testing.T) {
	env := map[string]string{"HOME": "/h"}
	cfg := mustLoad(t, minimalSources+`
editor: nvim
state_dir: /s
runs:
  command: copilot
ghostty:
  command: ghostty-new
prompts:
  alert: "~"
`, env)
	if cfg.Editor != "nvim" || cfg.Runs.Command != "copilot" || cfg.Ghostty.Command != "ghostty-new" {
		t.Errorf("bare executables must be kept for PATH lookup: %q %q %q", cfg.Editor, cfg.Runs.Command, cfg.Ghostty.Command)
	}
	if cfg.Prompts.Alert != filepath.Join(cfg.Dir, "~") {
		t.Errorf("bare ~ must not expand: %q", cfg.Prompts.Alert)
	}
	cfg = mustLoad(t, minimalSources+"state_dir: ~user/x\n", env)
	if cfg.StateDir != filepath.Join(cfg.Dir, "~user/x") {
		t.Errorf("~user must not expand: %q", cfg.StateDir)
	}
	cfg = mustLoad(t, minimalSources+"state_dir: /a/~/b\n", env)
	if cfg.StateDir != "/a/~/b" {
		t.Errorf("embedded tilde must not expand: %q", cfg.StateDir)
	}
}

func TestHomeRequirements(t *testing.T) {
	// Explicit paths needing neither HOME nor interpolation work without HOME.
	cfg := mustLoad(t, minimalSources+"state_dir: /s\n", map[string]string{})
	if cfg.StateDir != "/s" {
		t.Fatalf("state_dir = %q", cfg.StateDir)
	}
	// The default state_dir requires HOME.
	for _, env := range []map[string]string{{}, {"HOME": ""}} {
		_, report := load(t, minimalSources, env)
		if !hasMessage(report.Errors, "state_dir") || !hasMessage(report.Errors, "HOME") {
			t.Errorf("errors = %v", report.Errors)
		}
	}
	// Leading ~/ requires HOME and names the field.
	for _, env := range []map[string]string{{}, {"HOME": ""}} {
		_, report := load(t, "state_dir: /s\nsources:\n  - name: a\n    type: file\n    path: ~/a.json\n", env)
		if !hasMessage(report.Errors, "sources[0].path") || !hasMessage(report.Errors, "HOME") {
			t.Errorf("errors = %v", report.Errors)
		}
	}
	// The home value is not interpolated recursively.
	cfg = mustLoad(t, minimalSources+"state_dir: ~/state\n", map[string]string{"HOME": "/x/$Y"})
	if cfg.StateDir != "/x/$Y/state" {
		t.Errorf("state_dir = %q", cfg.StateDir)
	}
}

func TestValidation(t *testing.T) {
	base := "state_dir: /s\n"
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{"no sources", base, "at least one source"},
		{"empty sources", base + "sources: []\n", "at least one source"},
		{"duplicate names", base + "sources:\n  - {name: a, type: file, path: x}\n  - {name: a, type: file, path: y}\n", "duplicates"},
		{"invalid name", base + "sources:\n  - {name: A_b, type: file, path: x}\n", "sources[0].name"},
		{"missing name", base + "sources:\n  - {type: file, path: x}\n", "sources[0].name"},
		{"unsupported type", base + "sources:\n  - {name: a, type: http, path: x}\n", "sources[0].type"},
		{"file without path", base + "sources:\n  - {name: a, type: file}\n", "sources[0].path"},
		{"plain am without url", base + "sources:\n  - {name: a, type: alertmanager, grafana_instance: g}\n", "sources[0].url"},
		{"grafana am without instance or url", base + "sources:\n  - {name: a, type: alertmanager, grafana_alertmanager: grafana}\n", "grafana_instance or url"},
		{"bearer without token", base + "sources:\n  - {name: a, type: alertmanager, url: u, auth: {type: bearer}}\n", "exactly one of token"},
		{"bearer with two tokens", base + "sources:\n  - {name: a, type: alertmanager, url: u, auth: {type: bearer, token: t, token_file: f}}\n", "only one of token"},
		{"bearer with three tokens", base + "sources:\n  - {name: a, type: alertmanager, url: u, auth: {type: bearer, token: t, token_file: f, token_command: c}}\n", "only one of token"},
		{"basic without username", base + "sources:\n  - {name: a, type: alertmanager, url: u, auth: {type: basic, password: p}}\n", "username"},
		{"basic with two passwords", base + "sources:\n  - {name: a, type: alertmanager, url: u, auth: {type: basic, username: x, password: p, password_command: c}}\n", "only one of password"},
		{"basic without password", base + "sources:\n  - {name: a, type: alertmanager, url: u, auth: {type: basic, username: x}}\n", "exactly one of password"},
		{"unknown auth", base + "sources:\n  - {name: a, type: alertmanager, url: u, auth: {type: oauth}}\n", "auth.type"},
		{"malformed duration", base + minimalSources + "alerts:\n  reopen_window: 1 day\n", "alerts.reopen_window: invalid duration"},
		{"duration without unit", base + minimalSources + "runs:\n  timeout: 60\n", "runs.timeout: invalid duration"},
		{"placement", base + minimalSources + "ghostty:\n  placement: pane\n", "ghostty.placement"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, report := load(t, tt.content, nil)
			if !hasMessage(report.Errors, tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, report.Errors)
			}
		})
	}
}

func TestValidationBoundaries(t *testing.T) {
	base := "state_dir: /s\n" + minimalSources
	tests := []struct {
		setting string
		ok      []string
		bad     []string
		field   string
	}{
		{"alerts:\n  poll_interval: %s\n", []string{"10s", "1m"}, []string{"9s", "9999ms", "0s", "-1m"}, "alerts.poll_interval"},
		{"alerts:\n  prepare_after: %s\n", []string{"0s", "1ns"}, []string{"-1ns"}, "alerts.prepare_after"},
		{"alerts:\n  resolved_linger: %s\n", []string{"1ns"}, []string{"0s", "-1s"}, "alerts.resolved_linger"},
		{"alerts:\n  reopen_window: %s\n", []string{"1ns"}, []string{"0s", "-1s"}, "alerts.reopen_window"},
		{"retention:\n  done_after: %s\n", []string{"1ns"}, []string{"0s", "-1h"}, "retention.done_after"},
		{"runs:\n  timeout: %s\n", []string{"1m", "60s"}, []string{"59s", "0s"}, "runs.timeout"},
		{"runs:\n  concurrency: %s\n", []string{"1", "10"}, []string{"0", "11", "-1"}, "runs.concurrency"},
	}
	for _, tt := range tests {
		for _, v := range tt.ok {
			_, report := load(t, base+strings.Replace(tt.setting, "%s", v, 1), nil)
			if !report.OK() {
				t.Errorf("%s=%s: unexpected errors %v", tt.field, v, report.Errors)
			}
		}
		for _, v := range tt.bad {
			_, report := load(t, base+strings.Replace(tt.setting, "%s", v, 1), nil)
			if !hasMessage(report.Errors, tt.field) {
				t.Errorf("%s=%s: want error, got %v", tt.field, v, report.Errors)
			}
		}
	}
}

func TestAlertmanagerSources(t *testing.T) {
	// Literal grafana_instance references are accepted without
	// $GRAFANA_INSTANCES (absent or invalid).
	for _, env := range []map[string]string{{}, {"GRAFANA_INSTANCES": "not json"}} {
		cfg, report := load(t, `
state_dir: /s
sources:
  - name: grafana
    type: alertmanager
    grafana_instance: prod
    grafana_alertmanager: grafana
  - name: grafana-url
    type: alertmanager
    grafana_alertmanager: grafana
    url: https://g
    auth: {type: none}
  - name: plain
    type: alertmanager
    url: https://am
    filter: ['team="core"']
    receiver: ".*"
prompts:
  alert: /does/not/exist.tmpl
`, env)
		if !report.OK() {
			t.Fatalf("errors = %v", report.Errors)
		}
		if cfg.Sources[0].URL != nil || cfg.Sources[0].Auth != nil {
			t.Error("omitted url/auth must stay nil")
		}
		if cfg.Sources[1].URL == nil || cfg.Sources[1].Auth == nil {
			t.Error("explicit url/auth must be kept")
		}
		if len(report.Warnings) != 2 || !hasMessage(report.Warnings, "sources[1]") || !hasMessage(report.Warnings, "sources[2]") ||
			!hasMessage(report.Warnings, "may stop as blocked") {
			t.Errorf("warnings = %v", report.Warnings)
		}
	}
	// An explicit reference to $GRAFANA_INSTANCES obeys the unset rule.
	_, report := load(t, "state_dir: /s\nsources:\n  - {name: a, type: file, path: x, grafana_instance: $GRAFANA_INSTANCES}\n", map[string]string{})
	if !hasMessage(report.Errors, "GRAFANA_INSTANCES") {
		t.Errorf("errors = %v", report.Errors)
	}
	// An interpolated prompt override path is accepted without being read.
	cfg := mustLoad(t, minimalSources+"state_dir: /s\nprompts:\n  alert: $P/x.tmpl\n", map[string]string{"P": "/nope"})
	if cfg.Prompts.Alert != "/nope/x.tmpl" {
		t.Errorf("prompts.alert = %q", cfg.Prompts.Alert)
	}
}

func TestCheckExecutables(t *testing.T) {
	cfg := mustLoad(t, minimalSources+"state_dir: /s\n", nil)
	missing := func(string) (string, error) { return "", errors.New("not found") }
	found := func(f string) (string, error) { return f, nil }
	f := CheckExecutables(cfg, missing)
	if f.RunsCommand == "" || f.GhosttyCommand == "" {
		t.Fatalf("findings = %+v", f)
	}
	if strings.Contains(f.RunsCommand, "copilot") {
		t.Errorf("finding must not echo the configured value: %s", f.RunsCommand)
	}
	if f := CheckExecutables(cfg, found); f.RunsCommand != "" || f.GhosttyCommand != "" {
		t.Fatalf("findings = %+v", f)
	}

	// Path-like commands resolve relative to the config directory, not CWD.
	t.Chdir(t.TempDir())
	path := writeConfig(t, minimalSources+"state_dir: /s\nruns:\n  command: bin/copilot\n")
	dir := filepath.Dir(path)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "copilot"), []byte("#!/bin/sh\n"), 0o700); err != nil { // #nosec G306 -- test executable
		t.Fatal(err)
	}
	cfg, report, err := Load(path, envMap(nil))
	if err != nil || !report.OK() {
		t.Fatal(err, report.Errors)
	}
	if f := CheckExecutables(cfg, nil); f.RunsCommand != "" {
		t.Errorf("relative runs.command not resolved against the config dir: %+v", f)
	}
}

func TestWriteExample(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "a", "b", "config.yaml")
	if err := WriteExample(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"state_dir: $HOME/.local/state/tower", "$HOME/.config/tower/legacy-am.password", "path: ./testdata/alerts.json", "directory containing this file", "#"} {
		if !strings.Contains(text, want) {
			t.Errorf("example lacks %q", want)
		}
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	// The example is valid and its paths are config-relative.
	cfg, report, err := Load(path, envMap(map[string]string{"HOME": "/home/u"}))
	if err != nil || !report.OK() {
		t.Fatalf("example invalid: %v %v", err, report.Errors)
	}
	if cfg.StateDir != "/home/u/.local/state/tower" || cfg.Sources[0].Path != filepath.Join(filepath.Dir(path), "testdata/alerts.json") {
		t.Errorf("state_dir/path = %q %q", cfg.StateDir, cfg.Sources[0].Path)
	}

	// Never overwrite.
	if err := os.WriteFile(path, []byte("custom"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteExample(path); !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}
	data, _ = os.ReadFile(path) // #nosec G304 -- test file
	if string(data) != "custom" {
		t.Fatal("existing file was modified")
	}
}
