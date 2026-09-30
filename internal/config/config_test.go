package config

import (
	"errors"
	"fmt"
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

// TestRejectWrongTypes verifies that YAML value types are enforced before
// the decoder's lenient conversions, without echoing values.
func TestRejectWrongTypes(t *testing.T) {
	const secret = "4242.4242"
	tests := []struct{ name, content, field string }{
		{"fractional integer", "runs:\n  concurrency: 1.9\n", "runs.concurrency: must be an integer"},
		{"string integer", "runs:\n  concurrency: \"2\"\n", "runs.concurrency: must be an integer"},
		{"string boolean", "notifications:\n  enabled: \"yes\"\n", "notifications.enabled: must be a boolean"},
		{"integer boolean", "notifications:\n  enabled: 1\n", "notifications.enabled: must be a boolean"},
		{"numeric path", "prompts:\n  alert: 123\n", "prompts.alert: must be a string"},
		{"boolean editor", "editor: true\n", "editor: must be a string"},
		{"boolean argument", "runs:\n  args: [true]\n", "runs.args[0]: must be a string"},
		{"numeric list element", "alerts:\n  severity_order: [critical, 1]\n", "alerts.severity_order[1]: must be a string"},
		{"null list element", "runs:\n  resume_args: [a, null]\n", "runs.resume_args[1]: must be a string"},
		{"integer duration", "runs:\n  timeout: 60\n", "runs.timeout: must be a string"},
		{"mapping instead of list", "runs:\n  args: {a: b}\n", "runs.args: must be a list"},
		{"scalar instead of mapping", "alerts: soon\n", "alerts: must be a mapping"},
		{"numeric token", "sources:\n  - {name: a, type: alertmanager, url: u, auth: {type: bearer, token: " + secret + "}}\n", "sources[0].auth.token: must be a string"},
		{"numeric source name", "sources:\n  - {name: 7, type: file, path: x}\n", "sources[0].name: must be a string"},
		{"null source", "sources:\n  - null\n", "sources[0]: must be a mapping"},
		{"list document", "- a\n", "configuration: must be a mapping"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := tt.content
			switch {
			case strings.HasPrefix(content, "- "): // whole document
			case strings.HasPrefix(content, "sources:"):
				content = "state_dir: /s\n" + content
			default:
				content = "state_dir: /s\n" + minimalSources + content
			}
			_, _, err := Load(writeConfig(t, content), envMap(map[string]string{"HOME": "/h"}))
			if err == nil {
				t.Fatal("wrong type accepted")
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("error %q does not contain %q", err, tt.field)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("error discloses the configured value")
			}
		})
	}
	t.Run("correct types and nulls are accepted", func(t *testing.T) {
		cfg, report := load(t, "state_dir: /s\n"+minimalSources+"runs:\n  concurrency: 3\n  args: [\"1\", x]\n  model: null\nnotifications:\n  enabled: false\n", nil)
		if !report.OK() || cfg.Runs.Concurrency != 3 || cfg.Notifications.Enabled || cfg.Runs.Args[0] != "1" {
			t.Fatalf("report = %+v, cfg = %+v", report, cfg.Runs)
		}
	})
}

// TestTypeChecksCoverYAMLIndirection verifies that merge keys, aliases and
// explicit tags cannot bypass the type checks, while ordinary valid merges,
// aliases and matching standard tags still work.
func TestTypeChecksCoverYAMLIndirection(t *testing.T) {
	const secret = "31337.5"
	reject := []struct{ name, content, want string }{
		{"merged fractional integer", "runs:\n  <<: {concurrency: 1.9}\n", "runs.concurrency: must be an integer"},
		{"merged string boolean", "notifications:\n  <<: {enabled: \"yes\"}\n", "notifications.enabled: must be a boolean"},
		{"merged boolean argument", "runs:\n  <<: {args: [true]}\n", "runs.args[0]: must be a string"},
		{"merged list of mappings", "runs:\n  <<: [{model: m}, {concurrency: 1.5}]\n", "runs.concurrency: must be an integer"},
		{"merged through alias", "ghostty: &g {command: 12}\nruns:\n  <<: *g\n", "ghostty.command: must be a string"},
		{"merged alias with wrong type", "retention: &r {done_after: 5}\nalerts:\n  <<: *r\n", "retention.done_after: must be a string"},
		{"merge overridden by explicit key", "runs:\n  <<: {concurrency: 1.9}\n  concurrency: 2\n", "runs.concurrency: must be an integer"},
		{"explicit key before merge", "runs:\n  concurrency: 2\n  <<: {concurrency: 1.9}\n", "runs.concurrency: must be an integer"},
		{"top-level merge", "<<: {editor: true}\n", "editor: must be a string"},
		{"merge in a source", "sources:\n  - <<: {name: 5}\n    type: file\n    path: x\n", "sources[0].name: must be a string"},
		{"merge in auth", "sources:\n  - {name: a, type: alertmanager, url: u, auth: {type: bearer, <<: {token: " + secret + "}}}\n", "sources[0].auth.token: must be a string"},
		{"merge of a scalar", "runs:\n  <<: 5\n", "runs: must be merged from mappings only"},
		{"nested merge", "runs:\n  <<: {<<: {concurrency: 1.9}}\n", "runs.concurrency: must be an integer"},
		{"aliased wrong scalar", "editor: &e 1.5\nprompts:\n  alert: *e\n", "prompts.alert: must be a string"},
		{"aliased wrong list element", "runs:\n  args: &a [true]\n  resume_args: *a\n", "runs.resume_args[0]: must be a string"},
		{"aliased key", "editor: &k concurrency\nruns:\n  *k : 1.9\n", "runs.concurrency: must be an integer"},
		{"float tag for integer", "runs:\n  concurrency: !!float 1\n", "runs.concurrency: must be an integer"},
		{"str tag for integer", "runs:\n  concurrency: !!str 2\n", "runs.concurrency: must be an integer"},
		{"int tag for string", "editor: !!int 12\n", "editor: must be a string"},
		{"int tag on a non-integer", "runs:\n  concurrency: !!int '" + secret + "'\n", "runs.concurrency: must be an integer"},
		{"bool tag on yes", "notifications:\n  enabled: !!bool yes\n", "notifications.enabled: must be a boolean"},
		{"str tag for boolean", "notifications:\n  enabled: !!str true\n", "notifications.enabled: must be a boolean"},
		{"binary tag for string", "editor: !!binary aGk=\n", "editor: must be a string"},
		{"timestamp tag for string", "notifications:\n  sound: !!timestamp 2026-01-01\n", "notifications.sound: must be a string"},
		{"custom tag", "editor: !custom vim\n", "editor: must be a string"},
		{"null tag on a value", "runs:\n  model: !!null " + secret + "\n", "runs.model: must be a string"},
		{"seq tag for mapping", "runs: !!seq []\n", "runs: must be a mapping"},
		{"map tag for list", "runs:\n  args: !!map {}\n", "runs.args: must be a list"},
		{"set tag for mapping", "runs: !!set {model: m}\n", "runs: must be a mapping"},
		{"custom tag on mapping", "runs: !custom {model: m}\n", "runs: must be a mapping"},
		{"custom tag on merge value", "runs:\n  <<: !custom {concurrency: 1}\n", "runs: must be merged from mappings only"},
		{"custom tag on merged element", "runs:\n  <<: [!custom {concurrency: 1}]\n", "runs: must be merged from mappings only"},
		{"int tag on key", "runs:\n  !!int concurrency: 1\n", "runs: keys must be strings"},
		{"custom tag on key", "runs:\n  !custom concurrency: 1\n", "runs: keys must be strings"},
		{"tagged wrong value behind merge tag", "runs:\n  !!merge <<: {concurrency: !!float 1}\n", "runs.concurrency: must be an integer"},
		{"tagged wrong value in merge", "runs:\n  <<: !!map {concurrency: !!str 1}\n", "runs.concurrency: must be an integer"},
		{"unquoted timestamp string", "notifications:\n  sound: 2026-01-01\n", "notifications.sound: must be a string"},
		{"merged unknown key", "runs:\n  <<: {bogus: 1}\n", "bogus"},
	}
	for _, tt := range reject {
		t.Run(tt.name, func(t *testing.T) {
			content := tt.content
			if !strings.HasPrefix(content, "sources:") {
				content = minimalSources + content
			}
			_, _, err := Load(writeConfig(t, "state_dir: /s\n"+content), envMap(map[string]string{"HOME": "/h"}))
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatal("error discloses the configured value")
			}
		})
	}

	t.Run("standard tags matching the schema are accepted", func(t *testing.T) {
		content := "state_dir: !!str /s\n" +
			"!!str editor: !!str vi\n" +
			"sources: !!seq\n  - !!map {name: a, type: file, path: !<tag:yaml.org,2002:str> a.json}\n" +
			"runs: !!map\n  !!str concurrency: !!int 2\n  args: !!seq [!!str --x, !!str 12]\n  model: !!null\n" +
			"  !!merge <<: !!map {resume_args: [!!str r]}\n" +
			"notifications:\n  enabled: !!bool 'false'\n  sound: !!str 2026-01-01\n" +
			"prompts: {alert: ! p}\n"
		cfg, report := load(t, content, nil)
		if !report.OK() {
			t.Fatalf("report = %+v", report)
		}
		if cfg.Editor != "vi" || cfg.Sources[0].Path == "" || cfg.Runs.Concurrency != 2 ||
			!slices.Equal(cfg.Runs.Args, []string{"--x", "12"}) || !slices.Equal(cfg.Runs.ResumeArgs, []string{"r"}) ||
			cfg.Notifications.Enabled || cfg.Notifications.Sound != "2026-01-01" {
			t.Fatalf("cfg = %+v %+v %+v", cfg, cfg.Runs, cfg.Notifications)
		}
	})
	t.Run("valid merges and aliases are accepted", func(t *testing.T) {
		content := "state_dir: /s\n" +
			"sources:\n" +
			"  - &src {name: a, type: file, path: a.json}\n" +
			"  - <<: *src\n    name: b\n" +
			"runs:\n  <<: {concurrency: 3, args: [--x]}\n  model: &m gpt\n" +
			"notifications:\n  sound: *m\n"
		cfg, report := load(t, content, nil)
		if !report.OK() {
			t.Fatalf("report = %+v", report)
		}
		if len(cfg.Sources) != 2 || cfg.Sources[1].Name != "b" || cfg.Sources[1].Type != "file" ||
			cfg.Runs.Concurrency != 3 || cfg.Runs.Args[0] != "--x" || cfg.Notifications.Sound != "gpt" {
			t.Fatalf("cfg = %+v %+v %+v", cfg.Sources, cfg.Runs, cfg.Notifications)
		}
	})
}

// TestCyclicAliasesReturnErrors verifies that aliases referencing their own
// ancestors produce value-free configuration errors instead of unbounded
// recursion, and that repeated references are checked in bounded time.
func TestCyclicAliasesReturnErrors(t *testing.T) {
	const secret = "31337.5"
	for name, content := range map[string]string{
		"direct merge":             "runs: &r\n  model: " + secret + "x\n  <<: *r\n",
		"nested merge":             "runs: &r\n  <<: {<<: *r}\n",
		"merge list":               "runs: &r\n  <<: [{model: m}, *r]\n",
		"merge list element":       "runs: &r\n  <<: [{<<: *r}]\n",
		"merged sequence itself":   "runs:\n  <<: &l [{<<: *l}]\n",
		"through a nested mapping": "sources:\n  - &s {name: a, type: file, path: p, auth: {<<: *s}}\n",
		"list containing itself":   "runs:\n  args: &a [*a]\n",
		"top-level document":       "&d\n<<: *d\n",
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.HasPrefix(content, "sources:") && !strings.HasPrefix(content, "&d") {
				content = "state_dir: /s\n" + minimalSources + content
			}
			_, _, err := Load(writeConfig(t, content), envMap(nil))
			if err == nil {
				t.Fatal("accepted a cyclic alias")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error discloses the configured value: %v", err)
			}
		})
	}
	t.Run("direct merge reports the cycle", func(t *testing.T) {
		_, err := decodeRaw([]byte("runs: &r\n  <<: *r\n"))
		if err == nil || !strings.Contains(err.Error(), "line 2: runs: must not contain itself (cyclic YAML alias)") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("exponential alias fan-out is checked once per node", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("x0: &x0 {concurrency: 1}\n")
		for i := 1; i <= 12; i++ {
			fmt.Fprintf(&b, "x%d: &x%d {<<: [", i, i)
			for j := range 10 {
				if j > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "*x%d", i-1)
			}
			b.WriteString("]}\n")
		}
		b.WriteString("runs:\n  <<: *x12\n")
		start := time.Now()
		if _, err := decodeRaw([]byte(b.String())); err == nil {
			t.Fatal("accepted unknown keys")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("type check took %v", d)
		}
	})
}

// TestRejectExtraDocuments verifies that exactly one YAML document is read.
func TestRejectExtraDocuments(t *testing.T) {
	for name, trailer := range map[string]string{
		"second document":        "---\nbogus: true\n",
		"empty second document":  "---\n",
		"malformed second":       "---\n: : [\n",
		"repeated configuration": "---\n" + minimalSources,
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Load(writeConfig(t, "state_dir: /s\n"+minimalSources+trailer), envMap(nil))
			if err == nil {
				t.Fatal("accepted content after the configuration document")
			}
		})
	}
	t.Run("leading document marker is fine", func(t *testing.T) {
		if _, report := load(t, "---\nstate_dir: /s\n"+minimalSources, nil); !report.OK() {
			t.Fatalf("report = %+v", report)
		}
	})
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
		// The instance name is the injected value, JSON-escaped.
		"GRAFANA_INSTANCES": `{"x\"\nbogus: 1\n": {"url": "https://g"}}`,
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
`, map[string]string{"HOME": "/h", "GRAFANA_INSTANCES": `{"g": {"url": "https://grafana.example.com"}}`})
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
	env := map[string]string{"HOME": "/home/u", "SUB": "sub", "TILDE": "~/fromenv", "HOMEREF": "$HOME",
		"GRAFANA_INSTANCES": `{"g": {"url": "https://g"}, "~/g": {"url": "https://tilde"}}`}
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
		{"duration without unit", base + minimalSources + "runs:\n  timeout: \"60\"\n", "runs.timeout: invalid duration"},
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
`, map[string]string{"GRAFANA_INSTANCES": `{"prod": {"url": "https://grafana.example.com", "auth": {"tokenCommand": "echo t"}}}`})
	if !report.OK() {
		t.Fatalf("errors = %v", report.Errors)
	}
	if cfg.Sources[0].URL != nil || cfg.Sources[0].Auth != nil {
		t.Error("omitted url/auth must stay nil")
	}
	if cfg.Sources[1].URL == nil || cfg.Sources[1].Auth == nil {
		t.Error("explicit url/auth must be kept")
	}
	// The missing-instance warning applies to both endpoint variants.
	if len(report.Warnings) != 2 || !hasMessage(report.Warnings, "sources[1]") || !hasMessage(report.Warnings, "sources[2]") ||
		!hasMessage(report.Warnings, "may stop as blocked") {
		t.Errorf("warnings = %v", report.Warnings)
	}
	// An explicit reference to $GRAFANA_INSTANCES obeys the unset rule.
	_, report = load(t, "state_dir: /s\nsources:\n  - {name: a, type: file, path: x, grafana_instance: $GRAFANA_INSTANCES}\n", map[string]string{})
	if !hasMessage(report.Errors, "GRAFANA_INSTANCES") {
		t.Errorf("errors = %v", report.Errors)
	}
	// An interpolated prompt override path is accepted without being read.
	cfg = mustLoad(t, minimalSources+"state_dir: /s\nprompts:\n  alert: $P/x.tmpl\n", map[string]string{"P": "/nope"})
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
