package jira

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/provider"
	"github.com/ricoberger/tower/internal/store"
)

// issueJSON returns a search result for one ticket.
func issueJSON(key, summary, status, category string) string {
	return `{"key": "` + key + `", "self": "https://example.atlassian.net/rest/api/3/issue/1", "fields": {"summary": "` + summary +
		`", "status": {"name": "` + status + `", "statusCategory": {"key": "` + category + `"}}, "created": "2025-12-01T00:00:00.000+0000"}}`
}

// syncer polls sources with a fake acli and syncs the updates into a
// temporary store, like the app does.
type syncer struct {
	t   *testing.T
	s   *store.Store
	p   *Provider
	out map[string]string
	err map[string]error
	now time.Time
}

func newSyncer(t *testing.T) *syncer {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	y := &syncer{t: t, s: s, p: p, out: map[string]string{}, err: map[string]error{}, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	p.run = func(_ context.Context, args []string) ([]byte, error) {
		return []byte(y.out[args[4]]), y.err[args[4]]
	}
	p.now = func() time.Time { return y.now }
	return y
}

// poll polls a source with the given issues (or error) one hour later and
// syncs a successful update.
func (y *syncer) poll(src SourceConfig, err error, issues ...string) []int64 {
	y.t.Helper()
	y.now = y.now.Add(time.Hour)
	y.out[src.JQL] = "[" + strings.Join(issues, ",") + "]"
	y.err[src.JQL] = err
	u := y.p.update(context.Background(), src)
	if u.Err != nil {
		if err == nil {
			y.t.Fatalf("poll %s: %v", src.Name, u.Err)
		}
		return nil
	}
	added, serr := y.s.Sync(context.Background(), u)
	if serr != nil {
		y.t.Fatal(serr)
	}
	return added
}

func (y *syncer) items() map[string]store.Item {
	y.t.Helper()
	list, err := y.s.List(context.Background())
	if err != nil {
		y.t.Fatal(err)
	}
	m := map[string]store.Item{}
	for _, it := range list {
		if _, dup := m[it.Key]; dup {
			y.t.Fatalf("duplicate item %s", it.Key)
		}
		m[it.Key] = it
	}
	return m
}

var team = SourceConfig{Name: "team", JQL: "project = DEMO"}

func TestSyncLifecycle(t *testing.T) {
	y := newSyncer(t)
	ctx := context.Background()
	active := issueJSON("DEMO-1", "first", "In Progress", "indeterminate")
	other := issueJSON("DEMO-2", "second", "To Do", "new")
	done := issueJSON("DEMO-3", "old", "Done", "done")

	// Active tickets enter TO DO; first-seen done tickets produce no cards.
	if added := y.poll(assigned, nil, active, other, done); len(added) != 2 {
		t.Fatalf("added = %v", added)
	}
	items := y.items()
	first := items["jira:assigned:DEMO-1"]
	if len(items) != 2 || first.State != store.StateTodo || items["jira:assigned:DEMO-2"].State != store.StateTodo {
		t.Fatalf("items = %+v", items)
	}
	if !first.CreatedAt.Equal(time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)) || first.URL != "https://example.atlassian.net/browse/DEMO-1" {
		t.Errorf("created, url = %v, %q", first.CreatedAt, first.URL)
	}

	// The same ticket in another source is an independent card.
	y.poll(team, nil, active)
	if it, ok := y.items()["jira:team:DEMO-1"]; !ok || it.ID == first.ID {
		t.Fatalf("team card = %+v", it)
	}

	// Repeated snapshots create no duplicates and keep IDs.
	if added := y.poll(assigned, nil, active, other, done); len(added) != 0 {
		t.Fatalf("repeated snapshot added %v", added)
	}

	// A custom done status resolves the card although the JQL still
	// returns it; disappearance resolves the other card. The team card is
	// untouched.
	y.poll(assigned, nil, issueJSON("DEMO-1", "first", "Won't Do", "done"))
	items = y.items()
	if it := items["jira:assigned:DEMO-1"]; it.State != store.StateDone || it.ResolvedAt == nil || it.ID != first.ID {
		t.Errorf("done category = %+v", it)
	}
	if it := items["jira:assigned:DEMO-2"]; it.State != store.StateDone || it.ResolvedAt == nil {
		t.Errorf("disappeared = %+v", it)
	}
	if it := items["jira:team:DEMO-1"]; it.State != store.StateTodo || it.ResolvedAt != nil {
		t.Errorf("other source changed: %+v", it)
	}

	// A user move back to TO DO is not undone by continued absence.
	if err := y.s.Move(ctx, items["jira:assigned:DEMO-2"].ID, store.StateTodo, y.now); err != nil {
		t.Fatal(err)
	}
	y.poll(assigned, nil)
	if it := y.items()["jira:assigned:DEMO-2"]; it.State != store.StateTodo {
		t.Errorf("user move undone: %+v", it)
	}

	// Long after, a reopened ticket reuses its card and session.
	if err := y.s.Move(ctx, first.ID, store.StateTodo, y.now); err != nil {
		t.Fatal(err)
	}
	if err := y.s.Start(ctx, first.ID, "session-1", y.now); err != nil {
		t.Fatal(err)
	}
	if _, err := y.s.Finish(ctx, first.ID, "session-1", false, y.now); err != nil {
		t.Fatal(err)
	}
	if err := y.s.Move(ctx, first.ID, store.StateDone, y.now); err != nil {
		t.Fatal(err)
	}
	y.poll(assigned, nil) // resolved again while DONE
	y.now = y.now.Add(365 * 24 * time.Hour)
	added := y.poll(assigned, nil, issueJSON("DEMO-1", "reopened", "Reopened", "new"))
	if len(added) != 1 || added[0] != first.ID {
		t.Fatalf("reopen added = %v, want [%d]", added, first.ID)
	}
	if it := y.items()["jira:assigned:DEMO-1"]; it.State != store.StateTodo || it.SessionID != "session-1" || it.Title != "DEMO-1 · reopened" {
		t.Errorf("reopened = %+v", it)
	}

	// After retention deleted it, a later match is a new item.
	y.poll(assigned, nil)
	if _, err := y.s.DeleteDone(ctx, y.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	added = y.poll(assigned, nil, active)
	if len(added) != 1 || added[0] == first.ID {
		t.Errorf("after retention added = %v", added)
	}
}

func TestSyncManuallyClosedCardStaysDoneWithFreshFields(t *testing.T) {
	y := newSyncer(t)
	ctx := context.Background()
	id := y.poll(assigned, nil, issueJSON("DEMO-1", "before", "To Do", "new"))[0]
	closedAt := y.now
	if err := y.s.Move(ctx, id, store.StateDone, closedAt); err != nil {
		t.Fatal(err)
	}
	before, _ := y.s.Get(ctx, id)
	y.poll(assigned, nil, issueJSON("DEMO-1", "after", "In Progress", "indeterminate"))
	it, err := y.s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if it.State != store.StateDone || it.Title != "DEMO-1 · after" || !strings.HasPrefix(it.Description, "In Progress · ") {
		t.Errorf("item = %+v", it)
	}
	if !it.UpdatedAt.Equal(before.UpdatedAt) || !it.CreatedAt.Equal(before.CreatedAt) || it.ResolvedAt != nil {
		t.Errorf("timestamps changed: %+v, before %+v", it, before)
	}
}

func TestSyncFieldUpdatesPreserveRunningSession(t *testing.T) {
	y := newSyncer(t)
	ctx := context.Background()
	id := y.poll(assigned, nil, issueJSON("DEMO-1", "before", "To Do", "new"))[0]
	if err := y.s.Start(ctx, id, "session-1", y.now); err != nil {
		t.Fatal(err)
	}
	if err := y.s.SetPID(ctx, id, "session-1", 4242); err != nil {
		t.Fatal(err)
	}
	before, _ := y.s.Get(ctx, id)

	// Jira workflow changes only refresh fields.
	y.poll(assigned, nil, issueJSON("DEMO-1", "after", "In Review", "indeterminate"))
	it, _ := y.s.Get(ctx, id)
	if it.State != store.StateInProgress || it.SessionID != "session-1" || it.PID != 4242 || it.Title != "DEMO-1 · after" ||
		!it.UpdatedAt.Equal(before.UpdatedAt) || !it.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("field update = %+v, before %+v", it, before)
	}

	// Source resolution moves the card but keeps the running session.
	y.poll(assigned, nil, issueJSON("DEMO-1", "after", "Done", "done"))
	it, _ = y.s.Get(ctx, id)
	if it.State != store.StateDone || it.SessionID != "session-1" || it.PID != 4242 {
		t.Errorf("resolved = %+v", it)
	}
}

func TestSyncFailedPollsChangeNothing(t *testing.T) {
	y := newSyncer(t)
	y.poll(assigned, nil, issueJSON("DEMO-1", "a", "To Do", "new"), issueJSON("DEMO-2", "b", "To Do", "new"))
	before := y.items()

	failures := []struct {
		name string
		err  error
		out  []string
	}{
		{name: "acli error", err: errors.New("acli: exit status 1: unauthorized")},
		{name: "missing category", out: []string{issueJSON("DEMO-1", "a", "To Do", ""), issueJSON("DEMO-3", "c", "To Do", "new")}},
		{name: "missing key", out: []string{issueJSON("", "a", "To Do", "new")}},
	}
	for _, f := range failures {
		y.now = y.now.Add(time.Hour)
		y.out[assigned.JQL] = "[" + strings.Join(f.out, ",") + "]"
		y.err[assigned.JQL] = f.err
		u := y.p.update(context.Background(), assigned)
		if u.Err == nil || u.Items != nil {
			t.Fatalf("%s: update = %+v", f.name, u)
		}
		if _, err := y.s.Sync(context.Background(), u); err == nil {
			t.Fatalf("%s: failed update synced", f.name)
		}
	}
	// Partial (truncated) output.
	y.out[assigned.JQL] = `[` + issueJSON("DEMO-1", "a", "To Do", "new") + `,{"key": "DEMO-2", "fi`
	y.err[assigned.JQL] = nil
	if u := y.p.update(context.Background(), assigned); u.Err == nil {
		t.Fatal("partial output succeeded")
	}
	after := y.items()
	if len(after) != len(before) {
		t.Fatalf("items = %v, before %v", after, before)
	}
	for k, it := range before {
		if a := after[k]; a.State != it.State || a.ResolvedAt != nil || a.Title != it.Title {
			t.Errorf("%s changed: %+v", k, a)
		}
	}
}

func TestRegisteredProviderServesStoredItems(t *testing.T) {
	p, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	set := provider.Set{p}
	got, err := set.Get(Kind)
	if err != nil {
		t.Fatal(err)
	}
	prompt, err := got.Prompt(store.Item{Kind: Kind, Source: "team", Title: "DEMO-1 · x"})
	if err != nil || prompt != "triage DEMO-1 · x (jira)" {
		t.Errorf("prompt = %q, %v", prompt, err)
	}
}
