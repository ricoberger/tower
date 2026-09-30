package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
)

// GrafanaInstancesEnv is the environment variable describing the Grafana
// instances shared with the SRE skills.
const GrafanaInstancesEnv = "GRAFANA_INSTANCES"

// GrafanaInstance is the resolved $GRAFANA_INSTANCES record referenced by a
// source's grafana_instance.
type GrafanaInstance struct {
	// URL is the Grafana URL. It is investigation metadata for alert.md and,
	// for Grafana-managed sources without an explicit url, the polling URL.
	URL string
	// TokenCommand is the instance's auth.tokenCommand. It is only set for
	// Grafana-managed sources that derive their polling auth from the
	// instance; it is kept verbatim (never interpolated) and executed with
	// sh -c at request time only.
	TokenCommand string
}

// String never includes the record's contents.
func (g GrafanaInstance) String() string {
	return "grafana_instance{[redacted]}"
}

// GoString never includes the record's contents.
func (g GrafanaInstance) GoString() string { return g.String() }

// LogValue never includes the record's contents.
func (g GrafanaInstance) LogValue() slog.Value {
	return slog.StringValue("[redacted]")
}

// derivesAuth reports whether a source polls with bearer auth derived from
// its Grafana instance: Grafana-managed sources without explicit auth.
func (s Source) derivesAuth() bool {
	return s.Type == SourceAlertmanager && s.GrafanaAlertmanager != "" && s.GrafanaInstance != "" && s.Auth == nil
}

// Managed reports whether the source is a Grafana-managed Alertmanager.
func (s Source) Managed() bool {
	return s.Type == SourceAlertmanager && s.GrafanaAlertmanager != ""
}

// PollURL returns the effective polling base URL of an alertmanager source:
// the explicit url, or for Grafana-managed sources without one the URL of
// the referenced Grafana instance. Plain sources never borrow the instance
// URL.
func (s Source) PollURL() string {
	if s.URL != nil && *s.URL != "" {
		return *s.URL
	}
	if s.Managed() && s.Instance != nil {
		return s.Instance.URL
	}
	return ""
}

// PollAuth returns the effective polling auth of an alertmanager source: the
// explicit auth (including type none), or for Grafana-managed sources without
// explicit auth bearer auth through the instance's auth.tokenCommand. It
// returns nil when no Authorization header is sent.
func (s Source) PollAuth() *Auth {
	if s.Auth != nil {
		return s.Auth
	}
	if s.derivesAuth() && s.Instance != nil && s.Instance.TokenCommand != "" {
		cmd := s.Instance.TokenCommand
		return &Auth{Type: AuthBearer, TokenCommand: &cmd}
	}
	return nil
}

// resolveInstances resolves the $GRAFANA_INSTANCES records referenced by
// sources. The variable is only required (and must only be a valid JSON
// object) when a source references grafana_instance. Only referenced records
// are validated, and only the fields tower consumes: a nonempty string url,
// and a nonempty string auth.tokenCommand when a Grafana-managed source
// derives its polling auth. Unknown fields are allowed. Messages never
// include the variable's contents, instance names or commands.
func resolveInstances(cfg *Config, env LookupEnv, r *Report) {
	var refs []int
	for i, s := range cfg.Sources {
		if s.GrafanaInstance != "" && (s.Type == SourceAlertmanager || s.Type == SourceFile) {
			refs = append(refs, i)
		}
	}
	if len(refs) == 0 {
		return
	}
	value, ok := env(GrafanaInstancesEnv)
	if !ok {
		r.errorf("%s: environment variable must be set because a source references grafana_instance", GrafanaInstancesEnv)
		return
	}
	instances, ok := jsonObject([]byte(value))
	if !ok {
		r.errorf("%s: must contain a JSON object keyed by instance name", GrafanaInstancesEnv)
		return
	}
	for _, i := range refs {
		s := &cfg.Sources[i]
		f := fmt.Sprintf("sources[%d].grafana_instance", i)
		rec, found := instances[s.GrafanaInstance]
		if !found {
			r.errorf("%s: instance not found in $%s", f, GrafanaInstancesEnv)
			continue
		}
		fields, ok := jsonObject(rec)
		if !ok {
			r.errorf("%s: the $%s record must be a JSON object", f, GrafanaInstancesEnv)
			continue
		}
		inst := &GrafanaInstance{}
		valid := true
		if inst.URL, ok = jsonString(fields["url"]); !ok || inst.URL == "" {
			r.errorf("%s: the $%s record needs a nonempty string \"url\"", f, GrafanaInstancesEnv)
			valid = false
		}
		if s.derivesAuth() {
			var cmd string
			if auth, ok := jsonObject(fields["auth"]); ok {
				cmd, _ = jsonString(auth["tokenCommand"])
			}
			if cmd == "" {
				r.errorf("%s: the $%s record needs a nonempty string \"auth.tokenCommand\" to derive polling auth (or configure auth explicitly)", f, GrafanaInstancesEnv)
				valid = false
			}
			inst.TokenCommand = cmd
		}
		if valid {
			s.Instance = inst
		}
	}
}

// jsonObject decodes a JSON object into its raw members. It reports false
// for any other JSON value, including null.
func jsonObject(data []byte) (map[string]json.RawMessage, bool) {
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return nil, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return nil, false
	}
	return m, true
}

// jsonString decodes a JSON string. It reports false for a missing value or
// any other JSON type.
func jsonString(data json.RawMessage) (string, bool) {
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte(`"`)) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return "", false
	}
	return s, true
}
