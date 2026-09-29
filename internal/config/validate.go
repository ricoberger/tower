package config

import (
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

var sourceNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// validate checks the structural rules. It never includes configured values in
// messages.
func validate(cfg *Config, r *Report) {
	if len(cfg.Sources) == 0 {
		r.errorf("sources: at least one source is required")
	}

	seen := map[string]int{}
	for i, s := range cfg.Sources {
		validateSource(i, s, seen, r)
	}

	if cfg.StateDir == "" {
		r.errorf("state_dir: must not be empty")
	}
	if cfg.Editor == "" {
		r.errorf("editor: must not be empty")
	}

	a := cfg.Alerts
	if a.PollInterval < 10*time.Second {
		r.errorf("alerts.poll_interval: must be at least 10s")
	}
	if a.PrepareAfter < 0 {
		r.errorf("alerts.prepare_after: must not be negative")
	}
	if a.ResolvedLinger <= 0 {
		r.errorf("alerts.resolved_linger: must be greater than 0")
	}
	if a.ReopenWindow <= 0 {
		r.errorf("alerts.reopen_window: must be greater than 0")
	}
	if cfg.Retention.DoneAfter <= 0 {
		r.errorf("retention.done_after: must be greater than 0")
	}

	if cfg.Runs.Concurrency < 1 || cfg.Runs.Concurrency > 10 {
		r.errorf("runs.concurrency: must be between 1 and 10")
	}
	if cfg.Runs.Timeout < time.Minute {
		r.errorf("runs.timeout: must be at least 1m")
	}
	if cfg.Runs.Command == "" {
		r.errorf("runs.command: must not be empty")
	}
	if cfg.Ghostty.Command == "" {
		r.errorf("ghostty.command: must not be empty")
	}
	switch cfg.Ghostty.Placement {
	case "split", "tab", "window":
	default:
		r.errorf("ghostty.placement: must be one of split, tab, window")
	}
}

func validateSource(i int, s Source, seen map[string]int, r *Report) {
	f := "sources[" + strconv.Itoa(i) + "]"

	if !sourceNamePattern.MatchString(s.Name) {
		r.errorf("%s.name: must be nonempty and match ^[a-z0-9-]+$", f)
	} else if prev, ok := seen[s.Name]; ok {
		r.errorf("%s.name: duplicates the name of sources[%d]", f, prev)
	} else {
		seen[s.Name] = i
	}

	switch s.Type {
	case SourceFile:
		if s.Path == "" {
			r.errorf("%s.path: required for file sources", f)
		}
	case SourceAlertmanager:
		if s.GrafanaAlertmanager != "" {
			if s.GrafanaInstance == "" && (s.URL == nil || *s.URL == "") {
				r.errorf("%s: Grafana-managed sources require grafana_instance or url", f)
			}
		} else if s.URL == nil || *s.URL == "" {
			r.errorf("%s.url: required for plain alertmanager sources", f)
		}
		if s.Auth != nil {
			validateAuth(f+".auth", s.Auth, r)
		}
		if s.GrafanaInstance == "" {
			r.warnf("%s: alertmanager source without grafana_instance: the skill will have no Grafana instance and may stop as blocked", f)
		}
	default:
		r.errorf("%s.type: must be one of alertmanager, file", f)
	}
}

func validateAuth(f string, a *Auth, r *Report) {
	switch a.Type {
	case AuthNone:
	case AuthBearer:
		checkExactlyOne(f, []string{"token", "token_file", "token_command"}, []*string{a.Token, a.TokenFile, a.TokenCommand}, r)
	case AuthBasic:
		if a.Username == "" {
			r.errorf("%s.username: required for basic auth", f)
		}
		checkExactlyOne(f, []string{"password", "password_file", "password_command"}, []*string{a.Password, a.PasswordFile, a.PasswordCommand}, r)
	default:
		r.errorf("%s.type: must be one of none, basic, bearer", f)
	}
}

func checkExactlyOne(f string, names []string, values []*string, r *Report) {
	count := 0
	for i, v := range values {
		if v == nil {
			continue
		}
		count++
		if *v == "" {
			r.errorf("%s.%s: must not be empty", f, names[i])
		}
	}
	switch {
	case count == 0:
		r.errorf("%s: exactly one of %s, %s, %s is required", f, names[0], names[1], names[2])
	case count > 1:
		r.errorf("%s: only one of %s, %s, %s may be set", f, names[0], names[1], names[2])
	}
}

// LookPath resolves an executable, like exec.LookPath.
type LookPath func(file string) (string, error)

// ExecutableFindings reports whether runs.command and ghostty.command can be
// resolved. The commands are never executed. Callers decide whether a finding
// is a warning (config validate) or an error (engine startup, runs.command).
type ExecutableFindings struct {
	RunsCommand    string
	GhosttyCommand string
}

// CheckExecutables resolves the configured executables using lookPath (which
// defaults to exec.LookPath).
func CheckExecutables(cfg *Config, lookPath LookPath) ExecutableFindings {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	var f ExecutableFindings
	if cfg.Runs.Command != "" {
		if _, err := lookPath(cfg.Runs.Command); err != nil {
			f.RunsCommand = "runs.command: executable cannot be resolved (not found on PATH or not executable)"
		}
	}
	if cfg.Ghostty.Command != "" {
		if _, err := lookPath(cfg.Ghostty.Command); err != nil {
			f.GhosttyCommand = "ghostty.command: executable cannot be resolved (not found on PATH or not executable); resume will not work"
		}
	}
	return f
}
