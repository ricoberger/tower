package pullrequests

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/config/helpers"
	"github.com/ricoberger/tower/internal/store"
)

const results = `[
 {"number": 42, "title": "Fix   the\nbug", "body": "Line one\r\nLine two",
  "repository": {"name": "tower", "nameWithOwner": "ricoberger/tower"},
  "author": {"login": "octocat", "is_bot": false}, "state": "open", "isDraft": true,
  "createdAt": "2026-01-02T03:04:05Z", "updatedAt": "2026-01-03T03:04:05Z",
  "url": "https://github.com/ricoberger/tower/pull/42"}
]`

var review = SourceConfig{Name: "review-requested", Query: "is:open  review-requested:@me -author:app/dependabot"}

func validConfig() Config {
	return Config{
		PollInterval: helpers.Duration{Duration: time.Minute},
		MaxAge:       helpers.Duration{Duration: 14 * 24 * time.Hour},
		Sources:      []SourceConfig{{Name: "authored", Query: "is:open author:@me"}, review},
		Prompt:       `{{if eq .Source "review-requested"}}review{{else}}address{{end}} {{.URL}}`,
	}
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Config)
		want   string
	}{
		{name: "valid", modify: func(*Config) {}},
		{name: "no sources needs nothing else", modify: func(c *Config) { *c = Config{} }},
		{name: "invalid prompt without sources", modify: func(c *Config) { *c = Config{Prompt: "{{"} }, want: "parse prompt"},
		{name: "zero poll interval", modify: func(c *Config) { c.PollInterval.Duration = 0 }, want: "poll_interval must be positive"},
		{name: "zero max age", modify: func(c *Config) { c.MaxAge.Duration = 0 }, want: "max_age must be positive"},
		{name: "empty prompt", modify: func(c *Config) { c.Prompt = " " }, want: "prompt must not be empty"},
		{name: "empty name", modify: func(c *Config) { c.Sources[0].Name = "" }, want: "source 0: name must not be empty"},
		{name: "duplicate name", modify: func(c *Config) { c.Sources[1].Name = "authored" }, want: "source authored: duplicate name"},
		{name: "empty query", modify: func(c *Config) { c.Sources[0].Query = "  " }, want: "source authored: query must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(&cfg)
			_, err := New(cfg)
			if tt.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestArgsSeparateTermsAndAddMaxAge(t *testing.T) {
	since := time.Date(2026, 1, 2, 23, 30, 0, 0, time.FixedZone("CEST", 2*3600))
	got := Args(review, since)
	want := []string{"search", "prs", "--json", searchFields, "--limit", "1000", "--",
		"is:open", "review-requested:@me", "-author:app/dependabot", "updated:>=2026-01-02"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args = %q\nwant   %q", got, want)
	}
}

func TestDecode(t *testing.T) {
	for _, empty := range []string{"", " \n"} {
		prs, err := Decode([]byte(empty))
		if err != nil || len(prs) != 0 {
			t.Errorf("Decode(%q) = %v, %v; want no results", empty, prs, err)
		}
	}
	if _, err := Decode([]byte(`{"message": "x"}`)); err == nil {
		t.Error("object decoded without error")
	}
	full := "[" + strings.Repeat(`{"number": 1},`, searchLimit-1) + `{"number": 1}]`
	if _, err := Decode([]byte(full)); err == nil || !strings.Contains(err.Error(), "search limit") {
		t.Errorf("full result err = %v", err)
	}
}

func TestItem(t *testing.T) {
	prs, err := Decode([]byte(results))
	if err != nil {
		t.Fatal(err)
	}
	it, err := Item(review, prs[0])
	if err != nil {
		t.Fatal(err)
	}
	if it.Key != "pullrequest:review-requested:ricoberger/tower#42" {
		t.Errorf("key = %q", it.Key)
	}
	if it.Title != "ricoberger/tower#42 · Fix the bug" {
		t.Errorf("title = %q", it.Title)
	}
	if it.Description != "@octocat · draft\nLine one\nLine two" {
		t.Errorf("description = %q", it.Description)
	}
	if it.URL != "https://github.com/ricoberger/tower/pull/42" || !it.CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Errorf("url, created = %q, %v", it.URL, it.CreatedAt)
	}
	for _, want := range []string{
		"- **Repository:** ricoberger/tower",
		"- **Number:** #42",
		"- **Author:** @octocat",
		"- **Source:** review-requested (query: `is:open  review-requested:@me -author:app/dependabot`)",
		"- **State:** open (draft)",
		"- **Updated:** 2026-01-03T03:04:05Z",
		"## Description\n\nLine one\r\nLine two",
	} {
		if !strings.Contains(it.Details, want) {
			t.Errorf("details missing %q:\n%s", want, it.Details)
		}
	}

	if _, err := Item(review, PullRequest{Number: 1}); err == nil {
		t.Error("pull request without repository converted")
	}
}

func TestUpdate(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		out     string
		err     error
		wantErr string
		items   int
	}{
		{name: "results", out: results, items: 1},
		{name: "no results", out: "", items: 0},
		{name: "gh failure", err: errors.New("gh: exit status 4: not logged in"), wantErr: "not logged in"},
		{name: "invalid output", out: "nope", wantErr: "not a JSON array"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(validConfig())
			if err != nil {
				t.Fatal(err)
			}
			var gotArgs []string
			p.run = func(_ context.Context, args []string) ([]byte, error) {
				gotArgs = args
				return []byte(tt.out), tt.err
			}
			p.now = func() time.Time { return now }

			u := p.update(context.Background(), review)
			if u.Kind != Kind || u.Source != "review-requested" || !u.At.Equal(now) || u.ReopenWindow != time.Duration(math.MaxInt64) {
				t.Errorf("update = %+v", u)
			}
			if gotArgs[len(gotArgs)-1] != "updated:>=2026-01-01" {
				t.Errorf("max age qualifier = %q", gotArgs[len(gotArgs)-1])
			}
			if tt.wantErr != "" {
				if u.Err == nil || !strings.Contains(u.Err.Error(), tt.wantErr) || u.Items != nil {
					t.Fatalf("err = %v, items = %v", u.Err, u.Items)
				}
				return
			}
			if u.Err != nil || len(u.Items) != tt.items {
				t.Fatalf("err = %v, items = %d, want %d", u.Err, len(u.Items), tt.items)
			}
		})
	}
}

func TestPollSendsOneUpdatePerSource(t *testing.T) {
	p, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	p.run = func(context.Context, []string) ([]byte, error) { return []byte(results), nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan store.Update)
	done := make(chan struct{})
	go func() {
		p.Poll(ctx, nil, out)
		close(done)
	}()
	sources := map[string]bool{}
	for range 2 {
		u := <-out
		sources[u.Source] = true
	}
	if !sources["authored"] || !sources["review-requested"] {
		t.Errorf("sources = %v", sources)
	}
	cancel()
	<-done
}

func TestPollWithoutSourcesReturns(t *testing.T) {
	p, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	p.Poll(context.Background(), nil, nil)
}

func TestPromptBranchesOnSource(t *testing.T) {
	p, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	for source, want := range map[string]string{"review-requested": "review u", "authored": "address u"} {
		got, err := p.Prompt(store.Item{Kind: Kind, Source: source, URL: "u"})
		if err != nil || got != want {
			t.Errorf("Prompt(%s) = %q, %v; want %q", source, got, err, want)
		}
	}
}

func TestSyncReopensAgedOutPullRequest(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	prs, _ := Decode([]byte(results))
	items, err := Items(review, prs)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	u := func(items []store.Incoming, at time.Time) store.Update {
		return store.Update{Kind: Kind, Source: review.Name, Items: items, At: at, ReopenWindow: time.Duration(math.MaxInt64)}
	}

	added, err := s.Sync(ctx, u(items, at))
	if err != nil || len(added) != 1 {
		t.Fatalf("first sync = %v, %v", added, err)
	}
	if _, err := s.Sync(ctx, u(nil, at.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.Get(ctx, added[0]); it.State != store.StateDone {
		t.Fatalf("aged out state = %s", it.State)
	}
	reopened, err := s.Sync(ctx, u(items, at.Add(365*24*time.Hour)))
	if err != nil || fmt.Sprint(reopened) != fmt.Sprint(added) {
		t.Fatalf("reopen = %v, %v; want %v", reopened, err, added)
	}
	if it, _ := s.Get(ctx, added[0]); it.State != store.StateTodo {
		t.Errorf("reopened state = %s", it.State)
	}
}
