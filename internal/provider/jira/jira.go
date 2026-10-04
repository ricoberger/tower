// Package jira polls Jira JQL searches via the Atlassian CLI (acli) and turns
// the matching tickets into items.
package jira

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/ricoberger/tower/internal/config/helpers"
	"github.com/ricoberger/tower/internal/provider"
	"github.com/ricoberger/tower/internal/store"
)

// Kind is the item kind of Jira tickets.
const Kind = "jira"

// searchFields are the Jira fields requested from acli. The search's default
// display fields lack the description, reporter and labels. acli search
// rejects created and updated ("fields 'created, updated' are not allowed"),
// so the timestamps are only used when acli reports them anyway; otherwise
// the item's creation time is the poll time.
const searchFields = "key,summary,status,issuetype,priority,assignee,reporter,labels,description"

// doneCategory is the status category key of completed tickets. Workflow
// status names ("Closed", "Cancelled", …) and category colors are
// configurable in Jira and therefore not used.
const doneCategory = "done"

// timeout bounds every acli invocation, including all pages of a search.
const timeout = 30 * time.Second

// issueKey matches Jira issue keys such as DEMO-42.
var issueKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`)

var _ provider.Provider = (*Provider)(nil)

// Config configures the Jira sources.
type Config struct {
	PollInterval helpers.Duration `yaml:"poll_interval"`
	Sources      []SourceConfig   `yaml:"sources"`
	Prompt       string           `yaml:"prompt"`
}

// SourceConfig is one JQL search.
type SourceConfig struct {
	Name string `yaml:"name"`
	// JQL is passed unchanged as a single argument to acli.
	JQL string `yaml:"jql"`
}

// Issue is one decoded result of acli jira workitem search. Optional fields
// are zero when Jira omits them or returns an unexpected type.
type Issue struct {
	Key string
	// Status and StatusCategory are the workflow status name and the key of
	// its category (new, indeterminate or done).
	Status             string
	StatusCategory     string
	StatusCategoryName string
	Summary            string
	Type               string
	Priority           string
	Assignee           string
	Reporter           string
	Labels             []string
	Created            string
	Updated            string
	// Description is the raw description: null, a string or an ADF document.
	Description json.RawMessage
}

// Done reports whether the issue's status category is done.
func (i Issue) Done() bool {
	return i.StatusCategory == doneCategory
}

// runner runs acli with the given arguments and returns its stdout.
type runner func(ctx context.Context, args []string) ([]byte, error)

// Provider is the Jira provider.
type Provider struct {
	cfg  Config
	tmpl *template.Template
	run  runner
	// siteConfig is acli's Jira config file, the source of the site for the
	// browser links. Empty when the home directory is unknown.
	siteConfig string
	now        func() time.Time
	timeout    time.Duration
}

// New creates the Jira provider. Without sources it never polls and the
// other options are not required.
func New(cfg Config) (*Provider, error) {
	tmpl, err := template.New("prompt").Parse(cfg.Prompt)
	if err != nil {
		return nil, fmt.Errorf("parse prompt: %w", err)
	}

	if len(cfg.Sources) > 0 {
		if cfg.PollInterval.Duration <= 0 {
			return nil, errors.New("poll_interval must be positive")
		}
		if strings.TrimSpace(cfg.Prompt) == "" {
			return nil, errors.New("prompt must not be empty")
		}
		names := map[string]bool{}
		for i, s := range cfg.Sources {
			if strings.TrimSpace(s.Name) == "" {
				return nil, fmt.Errorf("source %d: name must not be empty", i)
			}
			if names[s.Name] {
				return nil, fmt.Errorf("source %s: duplicate name", s.Name)
			}
			names[s.Name] = true
			if strings.TrimSpace(s.JQL) == "" {
				return nil, fmt.Errorf("source %s: jql must not be empty", s.Name)
			}
		}
	}

	return &Provider{
		cfg:        cfg,
		tmpl:       tmpl,
		run:        runAcli,
		siteConfig: defaultSiteConfig(),
		now:        time.Now,
		timeout:    timeout,
	}, nil
}

// defaultSiteConfig returns the path of acli's Jira config file, or "" when
// the home directory is unknown.
func defaultSiteConfig() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "acli", "jira_config.yaml")
}

// Kind implements provider.Provider.
func (p *Provider) Kind() string {
	return Kind
}

// Prompt renders providers.jira.prompt for an item.
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

// update polls one source. Items stay retained while DONE, so a ticket that
// matches again is always reopened instead of being duplicated.
func (p *Provider) update(ctx context.Context, s SourceConfig) store.Update {
	u := store.Update{Kind: Kind, Source: s.Name, At: p.now(), ReopenWindow: time.Duration(math.MaxInt64)}
	issues, err := p.Fetch(ctx, s)
	if err == nil {
		// The site is read on every poll, so switching acli's profile takes
		// effect without restarting tower.
		u.Items = Items(s, issues, Site(p.siteConfig))
	}
	u.Err = err
	return u
}

// Fetch returns all tickets matching a source's JQL, including done ones.
func (p *Provider) Fetch(ctx context.Context, s SourceConfig) ([]Issue, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	out, err := p.run(ctx, Args(s))
	if err != nil {
		return nil, err
	}
	return Decode(out)
}

// Args returns the acli arguments of a source's search. The JQL is a single
// argument, and --paginate fetches every page: missing tickets are resolved,
// so a truncated result would wrongly move them to DONE.
func Args(s SourceConfig) []string {
	return []string{"jira", "workitem", "search", "--jql", s.JQL, "--fields", searchFields, "--paginate", "--json"}
}

// rawIssue is the part of a search result that is decoded strictly.
type rawIssue struct {
	Key    string                     `json:"key"`
	Fields map[string]json.RawMessage `json:"fields"`
}

// Decode decodes the output of acli jira workitem search. Empty output is no
// result. Every issue needs a key and a status category; otherwise the whole
// snapshot is rejected, because dropping or guessing would resolve or keep
// the wrong cards. The output is not part of errors: it is Jira data, not a
// diagnosis.
func Decode(data []byte) ([]Issue, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	var raw []rawIssue
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errors.New("output is not a JSON array of work items")
	}
	issues := make([]Issue, 0, len(raw))
	for i, r := range raw {
		if !issueKey.MatchString(r.Key) {
			return nil, fmt.Errorf("work item %d: missing or invalid key", i)
		}
		is, err := decodeFields(r)
		if err != nil {
			return nil, fmt.Errorf("work item %s: %w", r.Key, err)
		}
		issues = append(issues, is)
	}
	return issues, nil
}

// decodeFields decodes the status strictly and every optional field on its
// own, so an unexpected optional value cannot fail the poll.
func decodeFields(r rawIssue) (Issue, error) {
	is := Issue{Key: r.Key}
	var status struct {
		Name           string `json:"name"`
		StatusCategory struct {
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"statusCategory"`
	}
	if err := json.Unmarshal(r.Fields["status"], &status); err != nil || strings.TrimSpace(status.StatusCategory.Key) == "" {
		return Issue{}, errors.New("missing status category")
	}
	is.Status = strings.TrimSpace(status.Name)
	is.StatusCategory = strings.TrimSpace(status.StatusCategory.Key)
	is.StatusCategoryName = strings.TrimSpace(status.StatusCategory.Name)

	optional := func(field string, v any) {
		if data, ok := r.Fields[field]; ok {
			_ = json.Unmarshal(data, v)
		}
	}
	var named struct{ IssueType, Priority struct{ Name string } }
	var users struct{ Assignee, Reporter struct{ DisplayName string } }
	optional("summary", &is.Summary)
	optional("issuetype", &named.IssueType)
	optional("priority", &named.Priority)
	optional("assignee", &users.Assignee)
	optional("reporter", &users.Reporter)
	optional("labels", &is.Labels)
	optional("created", &is.Created)
	optional("updated", &is.Updated)
	is.Type = strings.TrimSpace(named.IssueType.Name)
	is.Priority = strings.TrimSpace(named.Priority.Name)
	is.Assignee = strings.TrimSpace(users.Assignee.DisplayName)
	is.Reporter = strings.TrimSpace(users.Reporter.DisplayName)
	is.Description = r.Fields["description"]
	return is, nil
}

// Items converts the tickets of a source into items, linking them to the
// Jira site (see Site). Done tickets are left out, so that Sync resolves
// cards of tickets that were completed and does not create cards for already
// completed ones.
func Items(s SourceConfig, issues []Issue, site string) []store.Incoming {
	items := make([]store.Incoming, 0, len(issues))
	for _, is := range issues {
		if is.Done() {
			continue
		}
		items = append(items, Item(s, is, site))
	}
	return items
}

// Item converts a ticket of a source into an item, linking it to the Jira
// site (see Site).
func Item(s SourceConfig, is Issue, site string) store.Incoming {
	desc := Description(is.Description)
	assignee := is.Assignee
	if assignee == "" {
		assignee = "Unassigned"
	}
	meta := assignee
	if is.Status != "" {
		meta = is.Status + " · " + assignee
	}
	link := BrowseURL(site, is.Key)
	created, _ := parseTime(is.Created)
	return store.Incoming{
		Key:         Kind + ":" + s.Name + ":" + is.Key,
		CreatedAt:   created,
		Title:       strings.Join(strings.Fields(is.Key+" "+summaryTitle(is.Summary)), " "),
		Description: strings.TrimSpace(meta + "\n" + desc),
		Details:     details(s, is, desc, link),
		URL:         link,
	}
}

// summaryTitle is the title part after the key.
func summaryTitle(summary string) string {
	if strings.TrimSpace(summary) == "" {
		return ""
	}
	return "· " + summary
}

// siteLine matches the site entries of acli's Jira config file, e.g.
// "site: example.atlassian.net" or "  - site: \"example.atlassian.net\"",
// like the fzfjira script does.
var siteLine = regexp.MustCompile(`^[[:space:]]*-?[[:space:]]*site:[[:space:]]*(.*)$`)

// Site returns the first site value of acli's Jira config file at path, with
// double quotes removed, e.g. example.atlassian.net. A missing or unreadable
// file or one without a site yields "": the ticket links are only a
// convenience and must not fail the poll. Only the site is used; the file
// contents are never logged.
func Site(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := siteLine.FindStringSubmatch(sc.Text()); m != nil {
			return strings.TrimSpace(strings.ReplaceAll(m[1], `"`, ""))
		}
	}
	return ""
}

// BrowseURL returns the ticket's browser link on a Jira site host, e.g.
// example.atlassian.net and DEMO-42 → https://example.atlassian.net/browse/DEMO-42.
// Sites that are not a plain host (empty, with a scheme, path, user info or
// whitespace) yield no link.
func BrowseURL(site, key string) string {
	if site == "" || strings.ContainsAny(site, "/\\?#@ \t\r\n") {
		return ""
	}
	u, err := url.Parse("https://" + site)
	if err != nil || u.Host != site {
		return ""
	}
	return "https://" + site + "/browse/" + key
}

// Jira timestamps use numeric offsets without a colon
// (2026-01-02T03:04:05.000+0100); fractional seconds are accepted by
// time.Parse without being part of the layout.
var timeLayouts = []string{"2006-01-02T15:04:05-0700", time.RFC3339Nano}

// parseTime parses a Jira or RFC3339 timestamp.
func parseTime(s string) (time.Time, bool) {
	for _, layout := range timeLayouts {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// formatTime formats a timestamp for the details, keeping unparsable values
// as they are.
func formatTime(s string) string {
	if t, ok := parseTime(s); ok {
		return t.Format(time.RFC3339)
	}
	return strings.TrimSpace(s)
}

// details renders a ticket as Markdown for the agent prompt. Absent optional
// metadata is left out.
func details(s SourceConfig, is Issue, desc, link string) string {
	var b strings.Builder
	title := is.Key
	if summary := strings.Join(strings.Fields(is.Summary), " "); summary != "" {
		title += " · " + summary
	}
	fmt.Fprintf(&b, "# %s\n\n", title)
	line := func(name, value string) {
		if value != "" {
			fmt.Fprintf(&b, "- **%s:** %s\n", name, value)
		}
	}
	line("Key", is.Key)
	line("URL", link)
	line("Source", fmt.Sprintf("%s (JQL: `%s`)", s.Name, s.JQL))
	category := is.StatusCategoryName
	if category == "" {
		category = is.StatusCategory
	}
	status := is.Status
	if status == "" {
		status = "-"
	}
	line("Status", fmt.Sprintf("%s (category: %s)", status, category))
	line("Type", is.Type)
	line("Priority", is.Priority)
	line("Assignee", is.Assignee)
	line("Reporter", is.Reporter)
	var labels []string
	for _, l := range is.Labels {
		if l = strings.TrimSpace(l); l != "" {
			labels = append(labels, "`"+l+"`")
		}
	}
	line("Labels", strings.Join(labels, " "))
	line("Created", formatTime(is.Created))
	line("Updated", formatTime(is.Updated))
	b.WriteString("\n## Description\n\n")
	if desc == "" {
		desc = noDescription
	}
	b.WriteString(desc)
	b.WriteString("\n")
	return b.String()
}

// runAcli runs acli without a shell, using its active Jira login. Its stderr
// is part of the error because acli explains failures (not logged in,
// invalid JQL) there.
func runAcli(ctx context.Context, args []string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "acli", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 250 * time.Millisecond
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, errors.New("acli timed out")
	}
	if ctx.Err() != nil {
		return nil, errors.New("acli canceled")
	}
	if err != nil {
		if msg := strings.Join(strings.Fields(stderr.String()), " "); msg != "" {
			return nil, fmt.Errorf("acli: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("acli: %w", err)
	}
	return stdout.Bytes(), nil
}
