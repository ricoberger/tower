// Package prompt renders the documents handed to preparation runs. It
// currently renders alert.md.
package prompt

import (
	"maps"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/source"
)

// AlertMeta is the non-secret source metadata rendered into alert.md.
type AlertMeta struct {
	// AlertmanagerURL is the configured URL of a plain Alertmanager source
	// (empty for Grafana-managed and file sources). It is rendered without
	// userinfo.
	AlertmanagerURL string
	// GrafanaInstance is the configured grafana_instance and GrafanaURL the
	// URL of the referenced $GRAFANA_INSTANCES record.
	GrafanaInstance string
	GrafanaURL      string
	// GrafanaAlertmanager is the datasource of a Grafana-managed source.
	GrafanaAlertmanager string
}

// MetaFor returns the alert.md metadata of a configured source. It never
// includes credentials, auth configuration or credential commands. The
// Grafana URL is always the instance URL, even when polling uses an explicit
// url override.
func MetaFor(s config.Source) AlertMeta {
	var m AlertMeta
	if s.Type == config.SourceAlertmanager {
		if s.GrafanaAlertmanager != "" {
			m.GrafanaAlertmanager = s.GrafanaAlertmanager
		} else if s.URL != nil {
			m.AlertmanagerURL = *s.URL
		}
	}
	if s.GrafanaInstance != "" {
		m.GrafanaInstance = s.GrafanaInstance
		if s.Instance != nil {
			m.GrafanaURL = s.Instance.URL
		}
	}
	return m
}

// SanitizeURL removes the userinfo component (user:pass@ or user@, also
// when percent-encoded) from raw and keeps all other components. It reports
// false when raw cannot be parsed as an absolute hierarchical URL; the
// original must then not be used.
func SanitizeURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.Opaque != "" {
		return "", false
	}
	u.User = nil
	return u.String(), true
}

// AlertMarkdown renders alert.md in the Alertmanager.app "Copy as Markdown"
// structure, without credentials: Grafana access is described by instance
// name only. Missing optional data renders as empty values; the section
// structure is always kept.
func AlertMarkdown(a source.Alert, m AlertMeta) []byte {
	var fields []string
	field := func(name, value string) {
		fields = append(fields, strings.TrimRight("- **"+name+":** "+value, " "))
	}
	if m.AlertmanagerURL != "" {
		// Never fall back to the original (possibly credential-bearing) URL.
		if u, ok := SanitizeURL(m.AlertmanagerURL); ok {
			field("Alertmanager URL", u)
		}
	}
	if m.GrafanaInstance != "" {
		grafanaURL, _ := SanitizeURL(m.GrafanaURL)
		field("Grafana Instance", m.GrafanaInstance)
		field("Grafana URL", grafanaURL)
		field("Grafana Credentials", "resolve via `$GRAFANA_INSTANCES` instance `"+m.GrafanaInstance+"` (sre-grafana Option A)")
	}
	if m.GrafanaAlertmanager != "" {
		field("Grafana Alertmanager Datasource", m.GrafanaAlertmanager)
	}
	field("Severity", a.Labels["severity"])
	state := "Active"
	if a.Suppressed() {
		state = "Suppressed"
	}
	field("State", state)
	started := ""
	if !a.StartsAt.IsZero() {
		started = a.StartsAt.Format(time.RFC3339)
	}
	field("Started", started)
	field("Receivers", strings.Join(a.Receivers, ", "))
	field("Source", a.GeneratorURL)

	blocks := []string{
		strings.TrimRight("# "+a.Labels["alertname"], " "),
		strings.Join(fields, "\n"),
	}
	add := func(title, body string) {
		blocks = append(blocks, "## "+title)
		if body != "" {
			blocks = append(blocks, body)
		}
	}
	add("Summary", strings.TrimSpace(a.Annotations["summary"]))
	add("Description", strings.TrimSpace(a.Annotations["description"]))
	add("Labels", list(a.Labels))
	add("Annotations", list(a.Annotations, "summary", "description"))
	return []byte(strings.Join(blocks, "\n\n") + "\n")
}

// list renders key/value pairs sorted by key, excluding skip.
func list(kv map[string]string, skip ...string) string {
	var lines []string
	for _, k := range slices.Sorted(maps.Keys(kv)) {
		if !slices.Contains(skip, k) {
			lines = append(lines, "- `"+k+"`: `"+kv[k]+"`")
		}
	}
	return strings.Join(lines, "\n")
}
