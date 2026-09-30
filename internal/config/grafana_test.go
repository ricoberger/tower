package config

import (
	"path/filepath"
	"strings"
	"testing"
)

const instanceTokenCmd = `printf '%s' "$GRAFANA_TOKEN" | tr -d '\n' ; echo ${HOME:-/} ~/x $$` // #nosec G101 -- shell syntax fixture, not a credential

func instancesEnv(extra map[string]string) map[string]string {
	env := map[string]string{
		"HOME": "/h",
		GrafanaInstancesEnv: `{
  "prod": {"url": "https://grafana.example.com", "auth": {"tokenCommand": ` + jsonQuote(instanceTokenCmd) + `}, "extra": {"any": [1, 2]}},
  "urlonly": {"url": "https://urlonly.example.com"},
  "unusable": 42,
  "nourl": {"auth": {"tokenCommand": "echo x"}},
  "badurl": {"url": 7},
  "emptyurl": {"url": ""},
  "badauth": {"url": "https://g", "auth": "x"},
  "badcmd": {"url": "https://g", "auth": {"tokenCommand": 1}},
  "emptycmd": {"url": "https://g", "auth": {"tokenCommand": ""}},
  "nullrec": null
}`,
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func jsonQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func TestGrafanaInstancesNotRequiredWithoutReferences(t *testing.T) {
	for _, env := range []map[string]string{{}, {GrafanaInstancesEnv: "not json"}, {GrafanaInstancesEnv: "[]"}} {
		_, report := load(t, "state_dir: /s\nsources:\n  - {name: a, type: alertmanager, url: https://am}\n  - {name: b, type: file, path: x}\n", env)
		if !report.OK() {
			t.Errorf("env %v: errors = %v", env, report.Errors)
		}
	}
}

func TestGrafanaInstancesValidation(t *testing.T) {
	src := func(extra string) string {
		return "state_dir: /s\nsources:\n  - {name: a, " + extra + "}\n"
	}
	managed := func(inst string) string {
		return src("type: alertmanager, grafana_alertmanager: grafana, grafana_instance: " + inst)
	}
	tests := []struct {
		name    string
		content string
		env     map[string]string
		wantErr string // "" = valid
	}{
		{"absent variable", managed("prod"), map[string]string{}, "GRAFANA_INSTANCES: environment variable must be set"},
		{"absent variable (file)", src("type: file, path: x, grafana_instance: prod"), map[string]string{}, "GRAFANA_INSTANCES: environment variable must be set"},
		{"invalid json", managed("prod"), map[string]string{GrafanaInstancesEnv: `{"prod": `}, "must contain a JSON object"},
		{"json array", managed("prod"), map[string]string{GrafanaInstancesEnv: `[{"prod": 1}]`}, "must contain a JSON object"},
		{"json null", managed("prod"), map[string]string{GrafanaInstancesEnv: `null`}, "must contain a JSON object"},
		{"empty", managed("prod"), map[string]string{GrafanaInstancesEnv: ``}, "must contain a JSON object"},
		{"missing name", managed("missing"), instancesEnv(nil), "sources[0].grafana_instance: instance not found"},
		{"missing name (plain)", src("type: alertmanager, url: https://am, grafana_instance: missing"), instancesEnv(nil), "instance not found"},
		{"missing name (file)", src("type: file, path: x, grafana_instance: missing"), instancesEnv(nil), "instance not found"},
		{"record not an object", src("type: file, path: x, grafana_instance: unusable"), instancesEnv(nil), "record must be a JSON object"},
		{"null record", src("type: file, path: x, grafana_instance: nullrec"), instancesEnv(nil), "record must be a JSON object"},
		{"missing url", src("type: file, path: x, grafana_instance: nourl"), instancesEnv(nil), `nonempty string "url"`},
		{"non-string url", src("type: alertmanager, url: https://am, grafana_instance: badurl"), instancesEnv(nil), `nonempty string "url"`},
		{"empty url", managed("emptyurl"), instancesEnv(nil), `nonempty string "url"`},
		// Explicit URL/auth overrides do not bypass the URL requirement.
		{"explicit override still needs url", src("type: alertmanager, grafana_alertmanager: g, url: https://x, auth: {type: none}, grafana_instance: nourl"), instancesEnv(nil), `nonempty string "url"`},
		// tokenCommand is only required for derived auth.
		{"derived auth without tokenCommand", managed("urlonly"), instancesEnv(nil), "auth.tokenCommand"},
		{"derived auth with non-object auth", managed("badauth"), instancesEnv(nil), "auth.tokenCommand"},
		{"derived auth with non-string command", managed("badcmd"), instancesEnv(nil), "auth.tokenCommand"},
		{"derived auth with empty command", managed("emptycmd"), instancesEnv(nil), "auth.tokenCommand"},
		{"derived auth with explicit url", src("type: alertmanager, grafana_alertmanager: g, url: https://x, grafana_instance: urlonly"), instancesEnv(nil), "auth.tokenCommand"},
		{"explicit auth needs no command", src("type: alertmanager, grafana_alertmanager: g, grafana_instance: badcmd, auth: {type: bearer, token: t}"), instancesEnv(nil), ""},
		{"explicit none needs no command", src("type: alertmanager, grafana_alertmanager: g, grafana_instance: urlonly, auth: {type: none}"), instancesEnv(nil), ""},
		{"plain needs no command", src("type: alertmanager, url: https://am, grafana_instance: badauth"), instancesEnv(nil), ""},
		{"file needs no command", src("type: file, path: x, grafana_instance: emptycmd"), instancesEnv(nil), ""},
		{"valid managed", managed("prod"), instancesEnv(nil), ""},
		// Unknown fields and unusable unreferenced records are ignored.
		{"unknown fields", managed("prod"), instancesEnv(nil), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, report := load(t, tt.content, tt.env)
			if tt.wantErr == "" {
				if !report.OK() {
					t.Fatalf("errors = %v", report.Errors)
				}
				return
			}
			if !hasMessage(report.Errors, tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, report.Errors)
			}
			all := strings.Join(report.Errors, "\n")
			for _, secret := range []string{"missing", "prod", "grafana.example.com", "GRAFANA_TOKEN", "echo x"} {
				if strings.Contains(all, secret) {
					t.Errorf("error discloses %q: %s", secret, all)
				}
			}
		})
	}
}

func TestGrafanaInstanceOverrides(t *testing.T) {
	cfg := mustLoad(t, `
state_dir: /s
sources:
  - name: derived
    type: alertmanager
    grafana_instance: prod
    grafana_alertmanager: grafana
  - name: url-override
    type: alertmanager
    grafana_instance: prod
    grafana_alertmanager: grafana
    url: https://override.example.com/prefix
  - name: auth-override
    type: alertmanager
    grafana_instance: prod
    grafana_alertmanager: grafana
    auth: {type: basic, username: u, password: p}
  - name: none
    type: alertmanager
    grafana_instance: prod
    grafana_alertmanager: grafana
    auth: {type: none}
  - name: plain
    type: alertmanager
    grafana_instance: prod
    url: https://am.example.com
  - name: file
    type: file
    path: x
    grafana_instance: prod
  - name: no-instance
    type: alertmanager
    grafana_alertmanager: grafana
    url: https://managed.example.com
`, instancesEnv(nil))
	s := cfg.Sources

	// Instance-derived URL and bearer auth, with the command verbatim.
	if got := s[0].PollURL(); got != "https://grafana.example.com" {
		t.Errorf("derived url = %q", got)
	}
	if a := s[0].PollAuth(); a == nil || a.Type != AuthBearer || a.TokenCommand == nil || *a.TokenCommand != instanceTokenCmd {
		t.Errorf("derived auth = %#v", a)
	}
	if s[0].URL != nil || s[0].Auth != nil {
		t.Error("derivation must not change the configured url/auth")
	}
	// Explicit URL keeps derived auth.
	if got := s[1].PollURL(); got != "https://override.example.com/prefix" {
		t.Errorf("url override = %q", got)
	}
	if a := s[1].PollAuth(); a == nil || a.TokenCommand == nil || *a.TokenCommand != instanceTokenCmd {
		t.Errorf("url override auth = %#v", a)
	}
	if s[1].Instance.URL != "https://grafana.example.com" {
		t.Errorf("instance metadata = %q", s[1].Instance.URL)
	}
	// Explicit auth keeps the derived URL.
	if got := s[2].PollURL(); got != "https://grafana.example.com" {
		t.Errorf("auth override url = %q", got)
	}
	if a := s[2].PollAuth(); a == nil || a.Type != AuthBasic || a.TokenCommand != nil {
		t.Errorf("auth override = %#v", a)
	}
	// Explicit none disables derived auth.
	if a := s[3].PollAuth(); a == nil || a.Type != AuthNone {
		t.Errorf("explicit none = %#v", a)
	}
	// Plain and file sources use the instance as metadata only.
	if got := s[4].PollURL(); got != "https://am.example.com" {
		t.Errorf("plain url = %q", got)
	}
	for _, i := range []int{4, 5} {
		if s[i].PollAuth() != nil {
			t.Errorf("%s must not borrow instance auth", s[i].Name)
		}
		if s[i].Instance == nil || s[i].Instance.URL != "https://grafana.example.com" || s[i].Instance.TokenCommand != "" {
			t.Errorf("%s instance = %#v", s[i].Name, s[i].Instance)
		}
	}
	// A managed source without an instance uses its own URL and no auth.
	if s[6].PollURL() != "https://managed.example.com" || s[6].PollAuth() != nil || s[6].Instance != nil {
		t.Errorf("managed without instance = %q %#v", s[6].PollURL(), s[6].PollAuth())
	}
	// Instance representations never disclose the record.
	for _, repr := range []string{s[0].Instance.String(), s[0].Instance.GoString(), s[0].Instance.LogValue().String()} {
		if strings.Contains(repr, "GRAFANA_TOKEN") || strings.Contains(repr, "grafana.example.com") {
			t.Errorf("instance representation discloses the record: %s", repr)
		}
	}
}

func TestCredentialFilePaths(t *testing.T) {
	t.Chdir(t.TempDir())
	path := writeConfig(t, `
state_dir: /s
sources:
  - name: a
    type: alertmanager
    url: https://a
    auth: {type: bearer, token_file: secrets/$NAME.token}
  - name: b
    type: alertmanager
    url: https://b
    auth: {type: basic, username: u, password_file: "~/pw/${NAME}"}
  - name: c
    type: alertmanager
    url: https://c
    auth: {type: bearer, token_command: 'cat ./rel ~/x $NAME'}
  - name: d
    type: alertmanager
    url: https://d
    auth: {type: basic, username: u, password_command: 'cat secrets/$NAME'}
`)
	cfg, report, err := Load(path, envMap(map[string]string{"HOME": "/home/u", "NAME": "prod"}))
	if err != nil || !report.OK() {
		t.Fatalf("Load: %v %v", err, report.Errors)
	}
	dir := filepath.Dir(path)
	if got := *cfg.Sources[0].Auth.TokenFile; got != filepath.Join(dir, "secrets/prod.token") {
		t.Errorf("token_file = %q", got)
	}
	if got := *cfg.Sources[1].Auth.PasswordFile; got != "/home/u/pw/prod" {
		t.Errorf("password_file = %q", got)
	}
	if got := *cfg.Sources[2].Auth.TokenCommand; got != "cat ./rel ~/x $NAME" {
		t.Errorf("token_command = %q", got)
	}
	if got := *cfg.Sources[3].Auth.PasswordCommand; got != "cat secrets/$NAME" {
		t.Errorf("password_command = %q", got)
	}
}
