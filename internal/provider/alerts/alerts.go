package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/ricoberger/tower/internal/config/helpers"
	"github.com/ricoberger/tower/internal/provider"
	"github.com/ricoberger/tower/internal/store"
)

// Kind is the item kind of alerts.
const Kind = "alert"

var _ provider.Provider = (*Provider)(nil)

// Config configures the alert sources.
type Config struct {
	PollInterval helpers.Duration `yaml:"poll_interval"`
	ReopenWindow helpers.Duration `yaml:"reopen_window"`
	Sources      []SourceConfig   `yaml:"sources"`
	Prompt       string           `yaml:"prompt"`
}

// SourceConfig is a Grafana-managed Alertmanager.
type SourceConfig struct {
	Name                string   `yaml:"name"`
	GrafanaInstance     string   `yaml:"grafana_instance"`
	GrafanaAlertmanager string   `yaml:"grafana_alertmanager"`
	Filter              []string `yaml:"filter"`
	Receiver            string   `yaml:"receiver"`

	// URL and TokenCommand come from $GRAFANA_INSTANCES.
	URL          string `yaml:"-"`
	TokenCommand string `yaml:"-"`
}

type grafanaInstance struct {
	URL  string `json:"url"`
	Auth struct {
		TokenCommand string `json:"tokenCommand"`
	} `json:"auth"`
}

// Provider is the alerts provider.
type Provider struct {
	cfg     Config
	tmpl    *template.Template
	clients []*Client
}

// New creates the alerts provider.
func New(cfg Config) (*Provider, error) {
	tmpl, err := template.New("prompt").Parse(cfg.Prompt)
	if err != nil {
		return nil, fmt.Errorf("parse prompt: %w", err)
	}

	var clients []*Client
	var instances map[string]grafanaInstance
	if err := json.Unmarshal([]byte(os.Getenv("GRAFANA_INSTANCES")), &instances); err != nil {
		return nil, fmt.Errorf("parse $GRAFANA_INSTANCES: %w", err)
	}
	for i := range cfg.Sources {
		s := &cfg.Sources[i]
		inst, ok := instances[s.GrafanaInstance]
		if !ok {
			return nil, fmt.Errorf("%s not found in $GRAFANA_INSTANCES", s.GrafanaInstance)
		}
		s.URL = inst.URL
		s.TokenCommand = inst.Auth.TokenCommand
		clients = append(clients, NewClient(*s))
	}

	p := &Provider{
		cfg:     cfg,
		tmpl:    tmpl,
		clients: clients,
	}

	return p, nil
}

// Kind implements provider.Provider.
func (p *Provider) Kind() string {
	return Kind
}

// Prompt renders providers.alerts.prompt for an item.
func (p *Provider) Prompt(it store.Item) (string, error) {
	return provider.RenderPrompt(p.tmpl, it)
}

// Poll polls every source immediately, then every poll_interval and whenever
// trigger receives, and sends one Update per source and poll to out. It
// returns when ctx is done.
func (p *Provider) Poll(ctx context.Context, trigger <-chan struct{}, out chan<- store.Update) {
	interval := p.cfg.PollInterval.Duration
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		var wg sync.WaitGroup
		for _, c := range p.clients {
			wg.Go(func() {
				alerts, err := c.Fetch(ctx)
				u := store.Update{Kind: Kind, Source: c.Name(), At: time.Now(), ReopenWindow: p.cfg.ReopenWindow.Duration}
				if err == nil {
					u.Items, err = Items(c.source, alerts)
				}
				u.Err = err
				select {
				case out <- u:
				case <-ctx.Done():
				}
			})
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-trigger:
			ticker.Reset(interval)
		}
	}
}

// Items converts the alerts of a source into items.
func Items(s SourceConfig, alerts []Alert) ([]store.Incoming, error) {
	items := make([]store.Incoming, 0, len(alerts))
	for _, a := range alerts {
		it, err := Item(s, a)
		if err != nil {
			return nil, fmt.Errorf("alert %s: %w", a.Fingerprint, err)
		}
		items = append(items, it)
	}
	return items, nil
}

// Item converts an alert of a source into an item.
func Item(s SourceConfig, a Alert) (store.Incoming, error) {
	data := detailsData{Alert: a, Source: s}
	for _, r := range a.Receivers {
		data.ReceiverNames = append(data.ReceiverNames, r.Name)
	}
	var details bytes.Buffer
	if err := detailsTemplate.Execute(&details, data); err != nil {
		return store.Incoming{}, fmt.Errorf("render details: %w", err)
	}

	return store.Incoming{
		Key:         Kind + ":" + s.Name + ":" + a.Fingerprint,
		CreatedAt:   a.StartsAt,
		Title:       strings.Join(strings.Fields(a.Labels["alertname"]+" · "+a.Labels["severity"]+" · "+s.Name), " "),
		Description: strings.TrimSpace(strings.TrimSpace(a.Annotations["summary"]) + "\n" + strings.TrimSpace(a.Annotations["description"])),
		Details:     details.String(),
		URL:         strings.TrimSpace(a.GeneratorURL),
	}, nil
}

// detailsData is the data of detailsTemplate.
type detailsData struct {
	Alert
	Source        SourceConfig
	ReceiverNames []string
}

// detailsTemplate renders an alert as Markdown for the agent prompt.
var detailsTemplate = template.Must(template.New("details").Parse(`# {{index .Labels "alertname"}}

- **Grafana Instance:** {{.Source.GrafanaInstance}}
- **Grafana URL:** {{.Source.URL}}
- **Grafana Credentials:** resolve via ` + "`$GRAFANA_INSTANCES`" + ` instance ` + "`{{.Source.GrafanaInstance}}`" + `
- **Grafana Alertmanager Datasource:** {{.Source.GrafanaAlertmanager}}
- **Severity:** {{index .Labels "severity"}}
- **State:** {{.Status.State}}
- **Started:** {{.StartsAt.Format "2006-01-02T15:04:05Z07:00"}}
- **Receivers:** {{range $i, $r := .ReceiverNames}}{{if $i}}, {{end}}{{$r}}{{end}}
- **Source:** {{.GeneratorURL}}

## Summary

{{index .Annotations "summary"}}

## Description

{{index .Annotations "description"}}

## Labels

{{range $k, $v := .Labels -}}
- ` + "`{{$k}}`: `{{$v}}`" + `
{{end}}
## Annotations

{{range $k, $v := .Annotations -}}
{{if and (ne $k "summary") (ne $k "description") -}}
- ` + "`{{$k}}`: `{{$v}}`" + `
{{end -}}
{{end -}}
`))
