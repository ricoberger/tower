// Package pullrequests polls GitHub pull request searches via the gh CLI and
// turns the results into items.
package pullrequests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/ricoberger/tower/internal/config/helpers"
	"github.com/ricoberger/tower/internal/provider"
	"github.com/ricoberger/tower/internal/store"
)

// Kind is the item kind of pull requests.
const Kind = "pullrequest"

// searchLimit is the maximum number of results of the GitHub search API. A
// result of this size may be truncated, so it is treated as a failed poll:
// syncing it would resolve the cut-off pull requests.
const searchLimit = 1000

// searchFields are the gh search prs JSON fields used by Item.
const searchFields = "number,title,body,repository,author,state,isDraft,createdAt,updatedAt,url"

var _ provider.Provider = (*Provider)(nil)

// Config configures the pull request sources.
type Config struct {
	PollInterval helpers.Duration `yaml:"poll_interval"`
	// MaxAge limits the results to pull requests updated within this
	// duration.
	MaxAge  helpers.Duration `yaml:"max_age"`
	Sources []SourceConfig   `yaml:"sources"`
	Prompt  string           `yaml:"prompt"`
}

// SourceConfig is one gh search prs query.
type SourceConfig struct {
	Name string `yaml:"name"`
	// Query is split at whitespace into the search terms (no quoting).
	Query string `yaml:"query"`
}

// PullRequest is one result of gh search prs.
type PullRequest struct {
	Number     int    `json:"number"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	Repository struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	State     string    `json:"state"`
	IsDraft   bool      `json:"isDraft"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	URL       string    `json:"url"`
}

// runner runs gh with the given arguments and returns its stdout.
type runner func(ctx context.Context, args []string) ([]byte, error)

// Provider is the pull requests provider.
type Provider struct {
	cfg  Config
	tmpl *template.Template
	run  runner
	now  func() time.Time
}

// New creates the pull requests provider. Without sources it never polls and
// the other options are not required.
func New(cfg Config) (*Provider, error) {
	tmpl, err := template.New("prompt").Parse(cfg.Prompt)
	if err != nil {
		return nil, fmt.Errorf("parse prompt: %w", err)
	}

	if len(cfg.Sources) > 0 {
		if cfg.PollInterval.Duration <= 0 {
			return nil, errors.New("poll_interval must be positive")
		}
		if cfg.MaxAge.Duration <= 0 {
			return nil, errors.New("max_age must be positive")
		}
		if strings.TrimSpace(cfg.Prompt) == "" {
			return nil, errors.New("prompt must not be empty")
		}
		names := map[string]bool{}
		for i, s := range cfg.Sources {
			if s.Name == "" {
				return nil, fmt.Errorf("source %d: name must not be empty", i)
			}
			if names[s.Name] {
				return nil, fmt.Errorf("source %s: duplicate name", s.Name)
			}
			names[s.Name] = true
			if len(strings.Fields(s.Query)) == 0 {
				return nil, fmt.Errorf("source %s: query must not be empty", s.Name)
			}
		}
	}

	return &Provider{
		cfg:  cfg,
		tmpl: tmpl,
		run:  runGh,
		now:  time.Now,
	}, nil
}

// Kind implements provider.Provider.
func (p *Provider) Kind() string {
	return Kind
}

// Prompt renders providers.pullrequests.prompt for an item.
func (p *Provider) Prompt(it store.Item) (string, error) {
	return provider.RenderPrompt(p.tmpl, it)
}

// Poll polls every source immediately, then every poll_interval and whenever
// trigger receives, and sends one Update per source and poll to out. It
// returns when ctx is done, or immediately without sources.
func (p *Provider) Poll(ctx context.Context, trigger <-chan struct{}, out chan<- store.Update) {
	if len(p.cfg.Sources) == 0 {
		return
	}
	interval := p.cfg.PollInterval.Duration
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		var wg sync.WaitGroup
		for _, s := range p.cfg.Sources {
			wg.Go(func() {
				u := p.update(ctx, s)
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

// update polls one source. Items stay retained while DONE, so a pull request
// that matches again is always reopened instead of being duplicated.
func (p *Provider) update(ctx context.Context, s SourceConfig) store.Update {
	now := p.now()
	u := store.Update{Kind: Kind, Source: s.Name, At: now, ReopenWindow: time.Duration(math.MaxInt64)}
	prs, err := p.Fetch(ctx, s, now)
	if err == nil {
		u.Items, err = Items(s, prs)
	}
	u.Err = err
	return u
}

// Fetch returns the complete current snapshot of a source: the open search
// results updated within max_age.
func (p *Provider) Fetch(ctx context.Context, s SourceConfig, now time.Time) ([]PullRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	out, err := p.run(ctx, Args(s, now.Add(-p.cfg.MaxAge.Duration)))
	if err != nil {
		return nil, err
	}
	return Decode(out)
}

// Args returns the gh arguments of a source's search for pull requests
// updated since. The terms follow "--" so that exclusions such as
// -author:app/dependabot are not parsed as flags.
func Args(s SourceConfig, since time.Time) []string {
	args := []string{"search", "prs", "--json", searchFields, "--limit", fmt.Sprint(searchLimit), "--"}
	args = append(args, strings.Fields(s.Query)...)
	return append(args, "updated:>="+since.UTC().Format(time.DateOnly))
}

// Decode decodes the output of gh search prs. gh prints nothing when there
// are no results.
func Decode(data []byte) ([]PullRequest, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var prs []PullRequest
	if err := json.Unmarshal(data, &prs); err != nil {
		return nil, errors.New("output is not a JSON array of pull requests")
	}
	if len(prs) >= searchLimit {
		return nil, fmt.Errorf("%d results reached the search limit, narrow the query or max_age", len(prs))
	}
	return prs, nil
}

// Items converts the pull requests of a source into items.
func Items(s SourceConfig, prs []PullRequest) ([]store.Incoming, error) {
	items := make([]store.Incoming, 0, len(prs))
	for _, pr := range prs {
		it, err := Item(s, pr)
		if err != nil {
			return nil, fmt.Errorf("pull request %s#%d: %w", pr.Repository.NameWithOwner, pr.Number, err)
		}
		items = append(items, it)
	}
	return items, nil
}

// Item converts a pull request of a source into an item.
func Item(s SourceConfig, pr PullRequest) (store.Incoming, error) {
	if pr.Repository.NameWithOwner == "" || pr.Number <= 0 {
		return store.Incoming{}, errors.New("missing repository or number")
	}
	var details bytes.Buffer
	if err := detailsTemplate.Execute(&details, detailsData{PullRequest: pr, Source: s}); err != nil {
		return store.Incoming{}, fmt.Errorf("render details: %w", err)
	}

	ref := fmt.Sprintf("%s#%d", pr.Repository.NameWithOwner, pr.Number)
	meta := "@" + pr.Author.Login
	if pr.IsDraft {
		meta += " · draft"
	}
	return store.Incoming{
		Key:         Kind + ":" + s.Name + ":" + ref,
		CreatedAt:   pr.CreatedAt,
		Title:       strings.Join(strings.Fields(ref+" · "+pr.Title), " "),
		Description: strings.TrimSpace(meta + "\n" + strings.TrimSpace(strings.ReplaceAll(pr.Body, "\r\n", "\n"))),
		Details:     details.String(),
		URL:         strings.TrimSpace(pr.URL),
	}, nil
}

// runGh runs gh without a shell. Its stderr is part of the error because gh
// explains failures (not logged in, invalid query) there.
func runGh(ctx context.Context, args []string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 250 * time.Millisecond
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, errors.New("gh timed out")
	}
	if err != nil {
		if msg := strings.Join(strings.Fields(stderr.String()), " "); msg != "" {
			return nil, fmt.Errorf("gh: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("gh: %w", err)
	}
	return stdout.Bytes(), nil
}

// detailsData is the data of detailsTemplate.
type detailsData struct {
	PullRequest
	Source SourceConfig
}

// detailsTemplate renders a pull request as Markdown for the agent prompt.
var detailsTemplate = template.Must(template.New("details").Parse(`# {{.Title}}

- **Repository:** {{.Repository.NameWithOwner}}
- **Number:** #{{.Number}}
- **URL:** {{.URL}}
- **Author:** @{{.Author.Login}}
- **Source:** {{.Source.Name}} (query: ` + "`{{.Source.Query}}`" + `)
- **State:** {{.State}}{{if .IsDraft}} (draft){{end}}
- **Created:** {{.CreatedAt.Format "2006-01-02T15:04:05Z07:00"}}
- **Updated:** {{.UpdatedAt.Format "2006-01-02T15:04:05Z07:00"}}

## Description

{{.Body}}
`))
