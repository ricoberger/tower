package jira

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/config/helpers"
	"github.com/ricoberger/tower/internal/store"
)

// results is acli jira workitem search --json output: one active ticket with
// all metadata and an ADF description, one with plain text and missing
// optional fields, and one in a custom done status.
const results = `[
 {"id": "10001", "key": "DEMO-42", "self": "https://example.atlassian.net/rest/api/3/issue/10001",
  "fields": {
   "summary": "Fix   the\nlogin  bug",
   "status": {"name": "In Review", "statusCategory": {"key": "indeterminate", "name": "In Progress", "colorName": "yellow"}},
   "issuetype": {"name": "Bug"}, "priority": {"name": "High"},
   "assignee": {"accountId": "5b10ac8d82e05b22cc7d4ef5", "displayName": "Ada Lovelace", "emailAddress": "ada@example.com"},
   "reporter": {"accountId": "5b10ac8d82e05b22cc7d4ef6", "displayName": "Grace Hopper"},
   "labels": ["backend", "needs-attention"],
   "created": "2026-01-02T03:04:05.123+0100", "updated": "2026-01-03T10:00:00.000+0000",
   "description": {"type": "doc", "version": 1, "content": [
     {"type": "paragraph", "content": [{"type": "text", "text": "Users cannot "}, {"type": "text", "text": "log in", "marks": [{"type": "strong"}]}, {"type": "text", "text": "."}]},
     {"type": "paragraph", "content": [{"type": "text", "text": "Second paragraph"}]}
   ]},
   "comment": {"total": 1, "comments": [{"body": "SECRET COMMENT"}]},
   "subtasks": [{"key": "DEMO-43", "fields": {"summary": "SUBTASK SUMMARY"}}],
   "issuelinks": [{"type": {"outward": "blocks"}, "outwardIssue": {"key": "DEMO-44", "fields": {"summary": "LINKED SUMMARY"}}}]
  }},
 {"key": "DEMO-7", "fields": {
   "summary": "Plain", "status": {"name": "To Do", "statusCategory": {"key": "new"}},
   "assignee": null, "priority": null, "labels": [], "description": "Line one\r\nLine two"}},
 {"key": "DEMO-1", "self": "https://example.atlassian.net/rest/api/3/issue/10000", "fields": {
   "summary": "Won't fix", "status": {"name": "Cancelled", "statusCategory": {"key": "done", "name": "Done"}}}}
]`

var assigned = SourceConfig{Name: "assigned", JQL: `assignee = currentUser() AND (labels = "a b" OR summary ~ "x") ORDER BY updated DESC`}

func validConfig() Config {
	return Config{
		PollInterval: helpers.Duration{Duration: time.Minute},
		Sources:      []SourceConfig{assigned, {Name: "team", JQL: "project = DEMO"}},
		Prompt:       `{{if eq .Source "team"}}triage{{else}}work on{{end}} {{.Title}} ({{.Kind}})`,
	}
}

func decode(t *testing.T, data string) []Issue {
	t.Helper()
	issues, err := Decode([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	return issues
}

func TestNewValidation(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Config)
		want   string
	}{
		{name: "valid", modify: func(*Config) {}},
		{name: "no sources needs nothing else", modify: func(c *Config) { *c = Config{} }},
		{name: "empty sources need nothing else", modify: func(c *Config) { *c = Config{Sources: []SourceConfig{}} }},
		{name: "invalid prompt without sources", modify: func(c *Config) { *c = Config{Prompt: "{{"} }, want: "parse prompt"},
		{name: "invalid prompt", modify: func(c *Config) { c.Prompt = "{{.Title" }, want: "parse prompt"},
		{name: "zero poll interval", modify: func(c *Config) { c.PollInterval.Duration = 0 }, want: "poll_interval must be positive"},
		{name: "negative poll interval", modify: func(c *Config) { c.PollInterval.Duration = -time.Second }, want: "poll_interval must be positive"},
		{name: "blank prompt", modify: func(c *Config) { c.Prompt = " \n" }, want: "prompt must not be empty"},
		{name: "empty name", modify: func(c *Config) { c.Sources[1].Name = "" }, want: "source 1: name must not be empty"},
		{name: "blank name", modify: func(c *Config) { c.Sources[0].Name = "  " }, want: "source 0: name must not be empty"},
		{name: "duplicate name", modify: func(c *Config) { c.Sources[1].Name = "assigned" }, want: "source assigned: duplicate name"},
		{name: "blank jql", modify: func(c *Config) { c.Sources[1].JQL = " \t" }, want: "source team: jql must not be empty"},
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

func TestPollWithoutSourcesRunsNothing(t *testing.T) {
	p, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	p.run = func(context.Context, []string) ([]byte, error) {
		t.Error("acli ran without sources")
		return nil, nil
	}
	p.Poll(context.Background(), nil, nil)
}

func TestArgsPassJQLUnchangedAndPaginate(t *testing.T) {
	got := Args(assigned)
	want := []string{"jira", "workitem", "search", "--jql", assigned.JQL, "--fields", searchFields, "--paginate", "--json"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %q\nwant   %q", got, want)
	}
	if slices.Contains(got, "--limit") {
		t.Error("search is limited")
	}
	for _, f := range []string{"key", "summary", "status", "issuetype", "priority", "assignee", "reporter", "labels", "created", "updated", "description"} {
		if !slices.Contains(strings.Split(searchFields, ","), f) {
			t.Errorf("field %s not requested", f)
		}
	}
	for _, f := range []string{"comment", "subtasks", "issuelinks"} {
		if slices.Contains(strings.Split(searchFields, ","), f) {
			t.Errorf("field %s requested", f)
		}
	}
}

func TestFetchRunsAcliWithSourceArgs(t *testing.T) {
	p, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	p.run = func(ctx context.Context, args []string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("acli runs without timeout")
		}
		got = args
		return []byte(results), nil
	}
	issues, err := p.Fetch(context.Background(), assigned)
	if err != nil || len(issues) != 3 {
		t.Fatalf("fetch = %d, %v", len(issues), err)
	}
	if !reflect.DeepEqual(got, Args(assigned)) {
		t.Errorf("args = %q", got)
	}
}

func TestDecode(t *testing.T) {
	for _, empty := range []string{"", " \n", "[]", "null"} {
		issues, err := Decode([]byte(empty))
		if err != nil || len(issues) != 0 {
			t.Errorf("Decode(%q) = %v, %v; want no results", empty, issues, err)
		}
	}
	tests := []struct{ name, data, want string }{
		{name: "malformed", data: `[{"key": "DEMO-1",`, want: "not a JSON array"},
		{name: "object", data: `{"errorMessages": ["x"]}`, want: "not a JSON array"},
		{name: "text", data: "Error: unauthorized", want: "not a JSON array"},
		{name: "missing key", data: `[{"fields": {"status": {"statusCategory": {"key": "new"}}}}]`, want: "work item 0: missing or invalid key"},
		{name: "invalid key", data: `[{"key": "DEMO 1", "fields": {"status": {"statusCategory": {"key": "new"}}}}]`, want: "invalid key"},
		{name: "missing status", data: `[{"key": "DEMO-1", "fields": {"summary": "x"}}]`, want: "DEMO-1: missing status category"},
		{name: "missing fields", data: `[{"key": "DEMO-1"}]`, want: "DEMO-1: missing status category"},
		{name: "missing category", data: `[{"key": "DEMO-1", "fields": {"status": {"name": "Done"}}}]`, want: "missing status category"},
		{name: "color only", data: `[{"key": "DEMO-1", "fields": {"status": {"name": "Done", "statusCategory": {"colorName": "green"}}}}]`, want: "missing status category"},
		{name: "bad status", data: `[{"key": "DEMO-1", "fields": {"status": "Done"}}]`, want: "missing status category"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues, err := Decode([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) || issues != nil {
				t.Fatalf("Decode = %v, %v; want %q", issues, err, tt.want)
			}
		})
	}
}

func TestDecodeErrorOmitsOutput(t *testing.T) {
	_, err := Decode([]byte("token=s3cr3t not json"))
	if err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("err = %v", err)
	}
}

func TestDecodeToleratesUnexpectedOptionalFields(t *testing.T) {
	issues := decode(t, `[{"key": "DEMO-1", "fields": {"status": {"name": "Open", "statusCategory": {"key": "new"}},
		"summary": 5, "labels": "x", "assignee": "nobody", "priority": [], "created": 1}}]`)
	it := Item(assigned, issues[0])
	if it.Title != "DEMO-1" || it.Description != "Open · Unassigned" || !it.CreatedAt.IsZero() {
		t.Errorf("item = %+v", it)
	}
}

func TestItemsExcludeDoneCategory(t *testing.T) {
	items := Items(assigned, decode(t, results))
	var keys []string
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	want := []string{"jira:assigned:DEMO-42", "jira:assigned:DEMO-7"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
	if items == nil {
		t.Error("successful empty snapshot must not be nil")
	}
	if got := Items(assigned, nil); got == nil || len(got) != 0 {
		t.Errorf("no issues = %#v", got)
	}
}

func TestItem(t *testing.T) {
	issues := decode(t, results)
	it := Item(assigned, issues[0])
	if it.Key != "jira:assigned:DEMO-42" {
		t.Errorf("key = %q", it.Key)
	}
	if it.Title != "DEMO-42 · Fix the login bug" {
		t.Errorf("title = %q", it.Title)
	}
	if it.Description != "In Review · Ada Lovelace\nUsers cannot **log in**.\n\nSecond paragraph" {
		t.Errorf("description = %q", it.Description)
	}
	if it.URL != "https://example.atlassian.net/browse/DEMO-42" {
		t.Errorf("url = %q", it.URL)
	}
	if want := time.Date(2026, 1, 2, 2, 4, 5, 123e6, time.UTC); !it.CreatedAt.Equal(want) {
		t.Errorf("created = %v, want %v", it.CreatedAt, want)
	}
	for _, want := range []string{
		"# DEMO-42 · Fix the login bug\n",
		"- **Key:** DEMO-42\n",
		"- **URL:** https://example.atlassian.net/browse/DEMO-42\n",
		"- **Source:** assigned (JQL: `" + assigned.JQL + "`)\n",
		"- **Status:** In Review (category: In Progress)\n",
		"- **Type:** Bug\n",
		"- **Priority:** High\n",
		"- **Assignee:** Ada Lovelace\n",
		"- **Reporter:** Grace Hopper\n",
		"- **Labels:** `backend` `needs-attention`\n",
		"- **Created:** 2026-01-02T03:04:05+01:00\n",
		"- **Updated:** 2026-01-03T10:00:00Z\n",
		"## Description\n\nUsers cannot **log in**.\n\nSecond paragraph\n",
	} {
		if !strings.Contains(it.Details, want) {
			t.Errorf("details missing %q:\n%s", want, it.Details)
		}
	}
	for _, unwanted := range []string{"SECRET COMMENT", "SUBTASK", "LINKED", "Comments", "Sub-tasks", "Links", "5b10ac8d", "ada@example.com", "null"} {
		if strings.Contains(it.Details+it.Description, unwanted) {
			t.Errorf("item contains %q:\n%s", unwanted, it.Details)
		}
	}
}

func TestItemMissingOptionalMetadata(t *testing.T) {
	it := Item(SourceConfig{Name: "team", JQL: "project = DEMO"}, decode(t, results)[1])
	if it.Key != "jira:team:DEMO-7" || it.Title != "DEMO-7 · Plain" {
		t.Errorf("key, title = %q, %q", it.Key, it.Title)
	}
	if it.Description != "To Do · Unassigned\nLine one\nLine two" {
		t.Errorf("description = %q", it.Description)
	}
	if it.URL != "" || !it.CreatedAt.IsZero() {
		t.Errorf("url, created = %q, %v", it.URL, it.CreatedAt)
	}
	if !strings.Contains(it.Details, "- **Status:** To Do (category: new)\n") {
		t.Errorf("status missing:\n%s", it.Details)
	}
	for _, unwanted := range []string{"Type:", "Priority:", "Assignee:", "Reporter:", "Labels:", "Created:", "Updated:", "URL:", "null"} {
		if strings.Contains(it.Details, unwanted) {
			t.Errorf("details contain %q:\n%s", unwanted, it.Details)
		}
	}

	bare := Item(assigned, Issue{Key: "DEMO-9", StatusCategory: "new"})
	if bare.Title != "DEMO-9" || bare.Description != "Unassigned" {
		t.Errorf("bare = %+v", bare)
	}
	if !strings.Contains(bare.Details, "## Description\n\n"+noDescription+"\n") {
		t.Errorf("empty description:\n%s", bare.Details)
	}
}

func TestParseTime(t *testing.T) {
	tests := []struct {
		in   string
		want time.Time
	}{
		{"2026-01-02T03:04:05.000+0100", time.Date(2026, 1, 2, 2, 4, 5, 0, time.UTC)},
		{"2026-01-02T03:04:05-0530", time.Date(2026, 1, 2, 8, 34, 5, 0, time.UTC)},
		{"2026-01-02T03:04:05Z", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)},
		{"2026-01-02T03:04:05.5+02:00", time.Date(2026, 1, 2, 1, 4, 5, 5e8, time.UTC)},
	}
	for _, tt := range tests {
		got, ok := parseTime(tt.in)
		if !ok || !got.Equal(tt.want) {
			t.Errorf("parseTime(%q) = %v, %v; want %v", tt.in, got, ok, tt.want)
		}
	}
	for _, bad := range []string{"", "yesterday", "2026-01-02"} {
		if _, ok := parseTime(bad); ok {
			t.Errorf("parseTime(%q) succeeded", bad)
		}
	}
}

func TestBrowseURL(t *testing.T) {
	tests := []struct{ self, want string }{
		{"https://example.atlassian.net/rest/api/3/issue/10001", "https://example.atlassian.net/browse/DEMO-42"},
		{"https://example.atlassian.net/rest/api/2/issue/10001", "https://example.atlassian.net/browse/DEMO-42"},
		{"https://jira.example.com/jira/rest/api/2/issue/10001", "https://jira.example.com/jira/browse/DEMO-42"},
		{"https://api.atlassian.com/ex/jira/1324a495-1645-4b0d-a8b3-ed3a2f9e2b1f/rest/api/3/issue/10001", ""},
		{"https://gateway.example.com/ex/jira/1324a495/rest/api/3/issue/10001", ""},
		{"https://example.atlassian.net/rest/agile/1.0/issue/10001", ""},
		{"https://example.atlassian.net/browse/DEMO-42", ""},
		{"https://user:pass@example.atlassian.net/rest/api/3/issue/10001", ""},
		{"file:///rest/api/3/issue/1", ""},
		{"/rest/api/3/issue/1", ""},
		{"", ""},
		{"::", ""},
	}
	for _, tt := range tests {
		if got := BrowseURL(tt.self, "DEMO-42"); got != tt.want {
			t.Errorf("BrowseURL(%q) = %q, want %q", tt.self, got, tt.want)
		}
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
		{name: "results", out: results, items: 2},
		{name: "no results", out: "", items: 0},
		{name: "empty array", out: "[]", items: 0},
		{name: "acli failure", err: errors.New("acli: exit status 1: unauthorized, run acli jira auth login"), wantErr: "unauthorized"},
		{name: "missing acli", err: errors.New(`acli: exec: "acli": executable file not found in $PATH`), wantErr: "not found"},
		{name: "invalid output", out: "nope", wantErr: "not a JSON array"},
		{name: "truncated output", out: results[:len(results)/2], wantErr: "not a JSON array"},
		{name: "missing category", out: `[{"key": "DEMO-1", "fields": {"status": {"name": "Open"}}}]`, wantErr: "missing status category"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(validConfig())
			if err != nil {
				t.Fatal(err)
			}
			p.run = func(context.Context, []string) ([]byte, error) { return []byte(tt.out), tt.err }
			p.now = func() time.Time { return now }

			u := p.update(context.Background(), assigned)
			if u.Kind != Kind || u.Source != "assigned" || !u.At.Equal(now) || u.ReopenWindow != time.Duration(math.MaxInt64) {
				t.Errorf("update = %+v", u)
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

func TestUpdateIncludesAllPages(t *testing.T) {
	// --paginate makes acli print every page as one array; the snapshot must
	// contain all of them, not a first page.
	var b strings.Builder
	b.WriteString("[")
	const n = 237
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"key": "DEMO-%d", "fields": {"summary": "s", "status": {"name": "Open", "statusCategory": {"key": "new"}}}}`, i+1)
	}
	b.WriteString("]")
	p, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	p.run = func(_ context.Context, args []string) ([]byte, error) {
		if !slices.Contains(args, "--paginate") {
			return nil, errors.New("not paginated")
		}
		return []byte(b.String()), nil
	}
	u := p.update(context.Background(), assigned)
	if u.Err != nil || len(u.Items) != n {
		t.Fatalf("items = %d, err = %v; want %d", len(u.Items), u.Err, n)
	}
	if u.Items[n-1].Key != fmt.Sprintf("jira:assigned:DEMO-%d", n) {
		t.Errorf("last key = %q", u.Items[n-1].Key)
	}
}

// fakeAcli puts an acli shell script with the given body first in PATH.
func fakeAcli(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acli"), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil { // #nosec G306 -- test executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestRunAcli(t *testing.T) {
	t.Run("passes arguments without a shell", func(t *testing.T) {
		fakeAcli(t, `for a in "$@"; do printf '%s\n' "$a"; done`)
		out, err := runAcli(context.Background(), Args(assigned))
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n"); !reflect.DeepEqual(got, Args(assigned)) {
			t.Errorf("args = %q", got)
		}
	})
	t.Run("failure includes stderr", func(t *testing.T) {
		fakeAcli(t, `echo 'not logged in' >&2; exit 1`)
		if _, err := runAcli(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "not logged in") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("missing executable", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if _, err := runAcli(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "acli") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		fakeAcli(t, `exec /bin/sleep 10`)
		p, err := New(validConfig())
		if err != nil {
			t.Fatal(err)
		}
		p.timeout = 50 * time.Millisecond
		start := time.Now()
		u := p.update(context.Background(), assigned)
		if u.Err == nil || !strings.Contains(u.Err.Error(), "timed out") || u.Items != nil {
			t.Errorf("err = %v, items = %v", u.Err, u.Items)
		}
		if time.Since(start) > 5*time.Second {
			t.Error("timeout not honored")
		}
	})
	t.Run("cancel", func(t *testing.T) {
		fakeAcli(t, `exec /bin/sleep 10`)
		ctx, cancel := context.WithCancel(context.Background())
		time.AfterFunc(50*time.Millisecond, cancel)
		if _, err := runAcli(ctx, nil); err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestPollSourcesImmediatelyOnTriggerAndPeriodically(t *testing.T) {
	cfg := validConfig()
	cfg.PollInterval.Duration = time.Hour
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.run = func(_ context.Context, args []string) ([]byte, error) {
		if args[4] == "project = DEMO" {
			return nil, errors.New("acli: exit status 1: invalid JQL")
		}
		return []byte(results), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	trigger := make(chan struct{}, 1)
	out := make(chan store.Update)
	done := make(chan struct{})
	go func() {
		p.Poll(ctx, trigger, out)
		close(done)
	}()

	receive := func() map[string]store.Update {
		t.Helper()
		got := map[string]store.Update{}
		for range 2 {
			select {
			case u := <-out:
				got[u.Source] = u
			case <-time.After(5 * time.Second):
				t.Fatal("no update")
			}
		}
		return got
	}
	// Immediately: one source fails, the other still succeeds.
	got := receive()
	if u := got["assigned"]; u.Err != nil || len(u.Items) != 2 {
		t.Errorf("assigned = %+v", u)
	}
	if u := got["team"]; u.Err == nil || u.Items != nil {
		t.Errorf("team = %+v", u)
	}
	select {
	case u := <-out:
		t.Fatalf("unexpected update before trigger: %+v", u)
	case <-time.After(50 * time.Millisecond):
	}
	// Manual trigger (R).
	trigger <- struct{}{}
	if got := receive(); len(got) != 2 {
		t.Errorf("triggered sources = %v", got)
	}
	cancel()
	<-done

	// Periodic polling.
	cfg.PollInterval.Duration = 20 * time.Millisecond
	p, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.run = func(context.Context, []string) ([]byte, error) { return []byte("[]"), nil }
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() {
		p.Poll(ctx, nil, out)
		close(done)
	}()
	for range 3 {
		receive()
	}
	cancel()
	<-done
}

func TestPollStopsWhenDeliveryIsBlocked(t *testing.T) {
	p, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	ran := make(chan struct{}, 2)
	p.run = func(context.Context, []string) ([]byte, error) {
		ran <- struct{}{}
		return []byte(results), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Poll(ctx, nil, make(chan store.Update))
		close(done)
	}()
	<-ran
	<-ran
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Poll did not stop")
	}
}

func TestPrompt(t *testing.T) {
	p, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	for source, want := range map[string]string{"team": "triage DEMO-1 · x (jira)", "assigned": "work on DEMO-1 · x (jira)"} {
		got, err := p.Prompt(store.Item{Kind: Kind, Source: source, Title: "DEMO-1 · x"})
		if err != nil || got != want {
			t.Errorf("Prompt(%s) = %q, %v; want %q", source, got, err, want)
		}
	}

	cfg := validConfig()
	cfg.Prompt = "{{.ID}} {{.Kind}} {{.Source}} {{.Title}} {{.Description}} {{.Details}} {{.URL}}"
	p, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Prompt(store.Item{ID: 3, Kind: Kind, Source: "team", Title: "T", Description: "D", Details: "M", URL: "U"})
	if err != nil || got != "3 jira team T D M U" {
		t.Errorf("Prompt = %q, %v", got, err)
	}

	cfg.Prompt = "{{.Assignee}}"
	p, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Prompt(store.Item{Kind: Kind}); err == nil || !strings.Contains(err.Error(), "render prompt") {
		t.Errorf("unknown field err = %v", err)
	}
}
