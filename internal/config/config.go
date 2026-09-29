// Package config loads, defaults and validates the tower configuration file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"gopkg.in/yaml.v3"
)

// Source types.
const (
	SourceAlertmanager = "alertmanager"
	SourceFile         = "file"
)

// Auth types.
const (
	AuthNone   = "none"
	AuthBasic  = "basic"
	AuthBearer = "bearer"
)

// Config is the fully defaulted and normalized configuration.
type Config struct {
	// Path is the absolute path of the loaded configuration file and Dir the
	// directory containing it.
	Path string
	Dir  string

	StateDir      string
	Editor        string
	Sources       []Source
	Alerts        Alerts
	Runs          Runs
	Notifications Notifications
	Ghostty       Ghostty
	Retention     Retention
	Prompts       Prompts
}

// Source is one configured alert source.
type Source struct {
	Name                string
	Type                string
	Path                string
	GrafanaInstance     string
	GrafanaAlertmanager string
	// URL is nil when omitted, so an explicit URL can later override values
	// derived from grafana_instance.
	URL      *string
	Filter   []string
	Receiver string
	// Auth is nil when omitted.
	Auth *Auth
}

// Auth holds polling credentials. Credential variants are pointers so that the
// "exactly one of" rules can distinguish omitted from empty values. The
// *_command fields are kept exactly as decoded; they are meant to be executed
// at request time, never at load time, and are never interpolated.
type Auth struct {
	Type            string
	Username        string
	Token           *string
	TokenFile       *string
	TokenCommand    *string
	Password        *string
	PasswordFile    *string
	PasswordCommand *string
}

// String never includes credentials.
func (a Auth) String() string {
	return fmt.Sprintf("auth{type: %s, credentials: [redacted]}", a.Type)
}

// GoString never includes credentials.
func (a Auth) GoString() string { return a.String() }

// LogValue never includes credentials.
func (a Auth) LogValue() slog.Value {
	return slog.GroupValue(slog.String("type", a.Type))
}

// Alerts configures alert polling and the lifecycle timers.
type Alerts struct {
	PollInterval   time.Duration
	PrepareAfter   time.Duration
	ResolvedLinger time.Duration
	ReopenWindow   time.Duration
	SeverityOrder  []string
}

// Runs configures preparation runs.
type Runs struct {
	Concurrency int
	Timeout     time.Duration
	Command     string
	Args        []string
	Model       string
	ResumeArgs  []string
}

// Notifications configures desktop notifications.
type Notifications struct {
	Enabled bool
	Sound   string
}

// Ghostty configures the ghostty-new wrapper.
type Ghostty struct {
	Command   string
	Placement string
	Direction string
}

// Retention configures pruning of done items.
type Retention struct {
	DoneAfter time.Duration
}

// Prompts configures prompt template overrides.
type Prompts struct {
	Alert string
}

// Report collects validation findings. Messages never contain configured
// values.
type Report struct {
	Errors   []string
	Warnings []string
}

// OK reports whether the configuration has no errors.
func (r Report) OK() bool { return len(r.Errors) == 0 }

func (r *Report) errorf(format string, args ...any) {
	r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
}

func (r *Report) warnf(format string, args ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...))
}

func (r *Report) addErr(err error) {
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
	}
}

// ErrNotFound is returned (wrapped) when the configuration file is missing.
var ErrNotFound = errors.New("configuration file not found")

// ResolvePath selects the configuration file path: the explicit flag value,
// then a nonempty $TOWER_CONFIG, then $HOME/.config/tower/config.yaml.
// Explicit selectors expand a leading "~/" through HOME and are otherwise
// resolved relative to cwd without environment interpolation.
func ResolvePath(flagValue string, env LookupEnv, cwd string) (string, error) {
	selector, field := flagValue, "--config"
	if selector == "" {
		if v, ok := env("TOWER_CONFIG"); ok && v != "" {
			selector, field = v, "TOWER_CONFIG"
		}
	}
	if selector == "" {
		home, ok := env("HOME")
		if !ok || home == "" {
			return "", errors.New("cannot determine the default configuration path: HOME is not set (use --config or TOWER_CONFIG)")
		}
		return filepath.Join(home, ".config", "tower", "config.yaml"), nil
	}
	return normalizePath(field, selector, cwd, env)
}

type rawConfig struct {
	StateDir      *string          `yaml:"state_dir"`
	Editor        *string          `yaml:"editor"`
	Sources       []rawSource      `yaml:"sources"`
	Alerts        rawAlerts        `yaml:"alerts"`
	Runs          rawRuns          `yaml:"runs"`
	Notifications rawNotifications `yaml:"notifications"`
	Ghostty       rawGhostty       `yaml:"ghostty"`
	Retention     rawRetention     `yaml:"retention"`
	Prompts       rawPrompts       `yaml:"prompts"`
}

type rawSource struct {
	Name                *string  `yaml:"name"`
	Type                *string  `yaml:"type"`
	Path                *string  `yaml:"path"`
	GrafanaInstance     *string  `yaml:"grafana_instance"`
	GrafanaAlertmanager *string  `yaml:"grafana_alertmanager"`
	URL                 *string  `yaml:"url"`
	Filter              []string `yaml:"filter"`
	Receiver            *string  `yaml:"receiver"`
	Auth                *rawAuth `yaml:"auth"`
}

type rawAuth struct {
	Type            *string `yaml:"type"`
	Username        *string `yaml:"username"`
	Token           *string `yaml:"token"`
	TokenFile       *string `yaml:"token_file"`
	TokenCommand    *string `yaml:"token_command"`
	Password        *string `yaml:"password"`
	PasswordFile    *string `yaml:"password_file"`
	PasswordCommand *string `yaml:"password_command"`
}

type rawAlerts struct {
	PollInterval   *string   `yaml:"poll_interval"`
	PrepareAfter   *string   `yaml:"prepare_after"`
	ResolvedLinger *string   `yaml:"resolved_linger"`
	ReopenWindow   *string   `yaml:"reopen_window"`
	SeverityOrder  *[]string `yaml:"severity_order"`
}

type rawRuns struct {
	Concurrency *int      `yaml:"concurrency"`
	Timeout     *string   `yaml:"timeout"`
	Command     *string   `yaml:"command"`
	Args        *[]string `yaml:"args"`
	Model       *string   `yaml:"model"`
	ResumeArgs  *[]string `yaml:"resume_args"`
}

type rawNotifications struct {
	Enabled *bool   `yaml:"enabled"`
	Sound   *string `yaml:"sound"`
}

type rawGhostty struct {
	Command   *string `yaml:"command"`
	Placement *string `yaml:"placement"`
	Direction *string `yaml:"direction"`
}

type rawRetention struct {
	DoneAfter *string `yaml:"done_after"`
}

type rawPrompts struct {
	Alert *string `yaml:"alert"`
}

// Load reads, strictly decodes, interpolates, defaults, normalizes and
// validates the configuration file at path. The returned error is non-nil for
// problems that prevent evaluating the file (missing, unreadable, invalid
// YAML). Validation findings are returned in the Report; the Config is only
// safe to use when Report.OK() is true.
func Load(path string, env LookupEnv) (*Config, Report, error) {
	var report Report

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, report, fmt.Errorf("resolve configuration path: %w", err)
	}

	data, err := os.ReadFile(abs) // #nosec G304 -- the user selects the configuration file
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, report, fmt.Errorf("%w: %s (run \"tower config init\" to create one)", ErrNotFound, abs)
		}
		return nil, report, fmt.Errorf("read configuration file %s: %w", abs, err)
	}

	raw, err := decodeRaw(data)
	if err != nil {
		return nil, report, fmt.Errorf("parse configuration file %s: %w", abs, err)
	}

	cfg := build(raw, abs, env, &report)
	validate(cfg, &report)
	return cfg, report, nil
}

// decodeRaw parses exactly one YAML document. It rejects additional
// documents, values of the wrong YAML type and unknown keys.
func decodeRaw(data []byte) (*rawConfig, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("line %d: only a single YAML document is allowed", extra.Line)
	}
	if err := checkTypes(&doc, reflect.TypeFor[rawConfig]()); err != nil {
		return nil, err
	}
	var raw rawConfig
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err := strict.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return &raw, nil
}

// builder carries state while converting the raw configuration.
type builder struct {
	env    LookupEnv
	dir    string
	report *Report
}

// str interpolates an optional string setting, returning def when omitted.
func (b *builder) str(field string, v *string, def string) string {
	if v == nil {
		return def
	}
	out, err := expand(field, *v, b.env)
	if err != nil {
		b.report.addErr(err)
		return ""
	}
	return out
}

// optStr interpolates an optional string setting, keeping nil when omitted.
func (b *builder) optStr(field string, v *string) *string {
	if v == nil {
		return nil
	}
	out := b.str(field, v, "")
	return &out
}

func (b *builder) list(field string, v []string) []string {
	if v == nil {
		return nil
	}
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = b.str(fmt.Sprintf("%s[%d]", field, i), &s, "")
	}
	return out
}

func (b *builder) listDefault(field string, v *[]string, def []string) []string {
	if v == nil {
		return append([]string{}, def...)
	}
	out := b.list(field, *v)
	if out == nil {
		out = []string{}
	}
	return out
}

func (b *builder) path(field string, v string) string {
	out, err := normalizePath(field, v, b.dir, b.env)
	if err != nil {
		b.report.addErr(err)
		return ""
	}
	return out
}

func (b *builder) executable(field string, v string) string {
	out, err := normalizeExecutable(field, v, b.dir, b.env)
	if err != nil {
		b.report.addErr(err)
		return ""
	}
	return out
}

func (b *builder) duration(field string, v *string, def time.Duration) time.Duration {
	if v == nil {
		return def
	}
	s := b.str(field, v, "")
	d, err := time.ParseDuration(s)
	if err != nil {
		// Do not echo the (possibly interpolated) value.
		b.report.errorf("%s: invalid duration (use Go duration syntax, e.g. 5m or 24h)", field)
		return def
	}
	return d
}

func build(raw *rawConfig, path string, env LookupEnv, report *Report) *Config {
	b := &builder{env: env, dir: filepath.Dir(path), report: report}
	cfg := &Config{Path: path, Dir: b.dir}

	if raw.StateDir == nil {
		home, ok := env("HOME")
		if !ok || home == "" {
			report.errorf("state_dir: the default location requires HOME to be set")
		} else {
			cfg.StateDir = filepath.Join(home, ".local", "state", "tower")
		}
	} else {
		cfg.StateDir = b.path("state_dir", b.str("state_dir", raw.StateDir, ""))
	}

	if raw.Editor == nil {
		editor := "vi"
		if v, ok := env("EDITOR"); ok && v != "" {
			editor = v
		}
		cfg.Editor = b.executable("editor", editor)
	} else {
		cfg.Editor = b.executable("editor", b.str("editor", raw.Editor, ""))
	}

	for i, rs := range raw.Sources {
		f := fmt.Sprintf("sources[%d]", i)
		s := Source{
			Name:                b.str(f+".name", rs.Name, ""),
			Type:                b.str(f+".type", rs.Type, ""),
			GrafanaInstance:     b.str(f+".grafana_instance", rs.GrafanaInstance, ""),
			GrafanaAlertmanager: b.str(f+".grafana_alertmanager", rs.GrafanaAlertmanager, ""),
			URL:                 b.optStr(f+".url", rs.URL),
			Filter:              b.list(f+".filter", rs.Filter),
			Receiver:            b.str(f+".receiver", rs.Receiver, ""),
		}
		s.Path = b.path(f+".path", b.str(f+".path", rs.Path, ""))
		if rs.Auth != nil {
			af := f + ".auth"
			a := &Auth{
				Type:     b.str(af+".type", rs.Auth.Type, ""),
				Username: b.str(af+".username", rs.Auth.Username, ""),
				Token:    b.optStr(af+".token", rs.Auth.Token),
				Password: b.optStr(af+".password", rs.Auth.Password),
				// Credential commands are preserved exactly as decoded.
				TokenCommand:    rs.Auth.TokenCommand,
				PasswordCommand: rs.Auth.PasswordCommand,
			}
			if v := b.optStr(af+".token_file", rs.Auth.TokenFile); v != nil {
				p := b.path(af+".token_file", *v)
				a.TokenFile = &p
			}
			if v := b.optStr(af+".password_file", rs.Auth.PasswordFile); v != nil {
				p := b.path(af+".password_file", *v)
				a.PasswordFile = &p
			}
			s.Auth = a
		}
		cfg.Sources = append(cfg.Sources, s)
	}

	cfg.Alerts = Alerts{
		PollInterval:   b.duration("alerts.poll_interval", raw.Alerts.PollInterval, time.Minute),
		PrepareAfter:   b.duration("alerts.prepare_after", raw.Alerts.PrepareAfter, 5*time.Minute),
		ResolvedLinger: b.duration("alerts.resolved_linger", raw.Alerts.ResolvedLinger, 4*time.Hour),
		ReopenWindow:   b.duration("alerts.reopen_window", raw.Alerts.ReopenWindow, 24*time.Hour),
		SeverityOrder:  b.listDefault("alerts.severity_order", raw.Alerts.SeverityOrder, []string{"critical", "error", "warning", "info"}),
	}

	cfg.Runs = Runs{
		Concurrency: 2,
		Timeout:     b.duration("runs.timeout", raw.Runs.Timeout, 20*time.Minute),
		Command:     b.executable("runs.command", b.str("runs.command", raw.Runs.Command, "copilot")),
		Args:        b.listDefault("runs.args", raw.Runs.Args, []string{"--yolo"}),
		Model:       b.str("runs.model", raw.Runs.Model, ""),
		ResumeArgs:  b.listDefault("runs.resume_args", raw.Runs.ResumeArgs, nil),
	}
	if raw.Runs.Concurrency != nil {
		cfg.Runs.Concurrency = *raw.Runs.Concurrency
	}

	cfg.Notifications = Notifications{Enabled: true, Sound: b.str("notifications.sound", raw.Notifications.Sound, "default")}
	if raw.Notifications.Enabled != nil {
		cfg.Notifications.Enabled = *raw.Notifications.Enabled
	}

	cfg.Ghostty = Ghostty{
		Command:   b.executable("ghostty.command", b.str("ghostty.command", raw.Ghostty.Command, "ghostty-new")),
		Placement: b.str("ghostty.placement", raw.Ghostty.Placement, "split"),
		Direction: b.str("ghostty.direction", raw.Ghostty.Direction, "right"),
	}

	cfg.Retention = Retention{DoneAfter: b.duration("retention.done_after", raw.Retention.DoneAfter, 8760*time.Hour)}

	cfg.Prompts = Prompts{Alert: b.path("prompts.alert", b.str("prompts.alert", raw.Prompts.Alert, ""))}

	return cfg
}
