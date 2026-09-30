package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ricoberger/tower/internal/item"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// fakeEngine records requests and returns configured errors.
type fakeEngine struct {
	mu      sync.Mutex
	calls   []string
	errs    map[string]error
	blockOn chan struct{}
}

func (f *fakeEngine) do(ctx context.Context, call string) error {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	err := f.errs[strings.Fields(call)[0]]
	block := f.blockOn
	f.mu.Unlock()
	if block != nil {
		// The fixture context is already cancelled to keep feed waits
		// synchronous, so only the test's channel releases a request.
		<-block
	}
	return err
}

func (f *fakeEngine) ManualRun(ctx context.Context, id string) error {
	return f.do(ctx, "manual "+id)
}

func (f *fakeEngine) Dismiss(ctx context.Context, id string) error {
	return f.do(ctx, "dismiss "+id)
}

func (f *fakeEngine) PollNow(ctx context.Context) error { return f.do(ctx, "poll") }

func (f *fakeEngine) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// driver runs a model synchronously: commands run in place, batches are
// expanded, ExecMsg commands are recorded (and run when run is set) and
// quitting is recorded.
type driver struct {
	t       *testing.T
	m       *Model
	quit    bool
	execs   [][]string
	execErr error
	// runExec executes ExecMsg commands instead of only recording them.
	runExec bool
}

func (d *driver) send(msg tea.Msg) {
	d.t.Helper()
	_, cmd := d.m.Update(msg)
	d.run(cmd)
}

func (d *driver) run(cmd tea.Cmd) {
	d.t.Helper()
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			d.run(c)
		}
	case tea.QuitMsg:
		d.quit = true
	case ExecMsg:
		args := msg.Cmd.Args()
		d.execs = append(d.execs, args)
		err := d.execErr
		if d.runExec {
			// The fixture's model context is already cancelled (see
			// newFixture), so a copy without it stands in for the
			// terminal handoff.
			err = exec.CommandContext(d.t.Context(), args[0], args[1:]...).Run() // #nosec G204 -- copy of the model's own command
		}
		d.send(msg.After(err))
	default:
		d.send(msg)
	}
}

func key(k string) tea.KeyPressMsg {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "ctrl+c":
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	r := []rune(k)[0]
	msg := tea.KeyPressMsg{Code: r, Text: k}
	if r >= 'A' && r <= 'Z' {
		msg = tea.KeyPressMsg{Code: r + 'a' - 'A', ShiftedCode: r, Mod: tea.ModShift, Text: k}
	}
	return msg
}

func (d *driver) keys(ks ...string) {
	d.t.Helper()
	for _, k := range ks {
		if got := key(k).String(); got != k {
			d.t.Fatalf("key %q is %q", k, got)
		}
		d.send(key(k))
	}
}

// fixture is a model over a real store in a temp directory.
type fixture struct {
	t      *testing.T
	store  *item.Store
	engine *fakeEngine
	now    time.Time
	d      *driver
	opts   Options
	resume []ResumeRequest
	opened []string
	items  map[string]*item.Item
	runs   map[string][]item.Run
}

func newFixture(t *testing.T, mut func(*Options)) *fixture {
	t.Helper()
	st, err := item.Open(filepath.Join(t.TempDir(), "state dir"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := &fixture{t: t, store: st, engine: &fakeEngine{errs: map[string]error{}}, now: t0,
		items: map[string]*item.Item{}, runs: map[string][]item.Run{}}
	ctx, cancel := context.WithCancel(context.Background())
	// A cancelled context makes the snapshot subscription return at once;
	// snapshots are sent directly.
	cancel()
	f.opts = Options{
		Feed:          NewFeed(),
		Engine:        f.engine,
		Store:         st,
		Now:           func() time.Time { return f.now },
		SeverityOrder: []string{"critical", "warning", "info"},
		PrepareAfter:  5 * time.Minute,
		Editor:        "/usr/bin/true",
		Placement:     "split",
		Resume: func(_ context.Context, r ResumeRequest) error {
			f.resume = append(f.resume, r)
			return nil
		},
		Open: func(_ context.Context, u string) error {
			f.opened = append(f.opened, u)
			return nil
		},
		Heartbeat: -1,
	}
	if mut != nil {
		mut(&f.opts)
	}
	f.d = &driver{t: t, m: New(ctx, f.opts)}
	f.d.run(f.d.m.Init())
	f.d.send(tea.WindowSizeMsg{Width: 140, Height: 40})
	return f
}

// add creates an item (and its runs) in the store and the next snapshot.
func (f *fixture) add(fp string, state item.State, severity string, startsAt time.Time, runs ...item.Run) string {
	f.t.Helper()
	k := item.Key{Source: "dev", Fingerprint: fp}
	id, _ := item.NewID(k, 1)
	status := item.AlertActive
	switch state {
	case item.StateSnoozed:
		status = item.AlertSuppressed
	case item.StateResolved, item.StateDone:
		status = item.AlertResolved
	}
	it := &item.Item{
		Version: item.Version, ID: id, Type: item.TypeAlert, State: state, Title: "Alert " + fp, Severity: severity,
		CreatedAt: startsAt, UpdatedAt: startsAt,
		Source: item.SourceRef{Name: "dev", Fingerprint: fp},
		Alert: item.AlertInfo{
			StartsAt: startsAt, LastSeenAt: startsAt, Status: status, Occurrences: 1,
			Labels: map[string]string{"alertname": "A" + fp},
		},
		History: []item.HistoryEntry{{At: startsAt, To: state, Reason: "seed"}},
	}
	if status == item.AlertResolved {
		r := startsAt
		it.Alert.ResolvedAt = &r
	}
	base := 0
	it.Runs.OccurrenceBase = &base
	for _, r := range runs {
		it.Runs.Current = max(it.Runs.Current, r.Number)
	}
	if err := f.store.Create(it, json.RawMessage(`{}`)); err != nil {
		f.t.Fatal(err)
	}
	for _, r := range runs {
		if err := f.store.CreateRun(id, r); err != nil {
			f.t.Fatal(err)
		}
	}
	f.items[id] = it
	f.runs[id] = runs
	return id
}

// snapshot sends the current fixture items to the model.
func (f *fixture) snapshot() {
	f.t.Helper()
	var s Snapshot
	ids := make([]string, 0, len(f.items))
	for id := range f.items {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		it := f.items[id]
		runs := make([]item.Run, 0, len(f.runs[id]))
		for _, r := range f.runs[id] {
			runs = append(runs, r.Clone())
		}
		s.Items = append(s.Items, ItemView{Item: it.Clone(), Runs: runs})
	}
	s.Sources = []SourceHealth{{Name: "dev", LastSuccess: f.now.Add(-5 * time.Second)}}
	s.Concurrency = 2
	f.d.send(SnapshotMsg(s))
}

func (f *fixture) disk(id string) *item.Item {
	f.t.Helper()
	it, err := f.store.Get(id)
	if err != nil {
		f.t.Fatal(err)
	}
	return it
}

func (f *fixture) writeRunFile(id string, n int, name, content string) {
	f.t.Helper()
	if err := f.store.WriteRunFile(id, n, name, []byte(content)); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) view() string { return ansi.Strip(f.d.m.Render()) }

func (f *fixture) groupLabels() []string {
	var out []string
	for _, g := range f.d.m.groups {
		out = append(out, fmt.Sprintf("%s:%d", g.label, len(g.items)))
	}
	return out
}

func (f *fixture) visibleIDs() []string {
	var out []string
	for _, v := range f.d.m.visible {
		out = append(out, v.Item.Title)
	}
	return out
}

func started(at time.Time) *time.Time { return &at }

func finishedRun(n int, outcome item.Outcome, at time.Time) item.Run {
	fin := at.Add(time.Minute)
	code := 0
	return item.Run{Number: n, SessionID: fmt.Sprintf("0000000%d-0000-4000-8000-000000000000", n), Reason: item.ReasonAuto,
		Skill: "sre-analyze-alert", QueuedAt: at, StartedAt: started(at), FinishedAt: &fin, PID: 4242, Outcome: outcome, ExitCode: &code}
}

func runningRun(n int, at time.Time) item.Run {
	return item.Run{Number: n, SessionID: fmt.Sprintf("0000000%d-0000-4000-8000-000000000000", n), Reason: item.ReasonAuto,
		Skill: "sre-analyze-alert", QueuedAt: at, StartedAt: started(at), PID: 4242, Outcome: item.OutcomeRunning}
}

func TestGroupsSortingAndCounts(t *testing.T) {
	f := newFixture(t, nil)
	f.add("n1", item.StateNeedsYou, "warning", t0.Add(-time.Hour))
	f.add("n2", item.StateNeedsYou, "critical", t0.Add(-time.Minute))
	f.add("n3", item.StateNeedsYou, "", t0.Add(-3*time.Hour))
	f.add("n4", item.StateNeedsYou, "critical", t0.Add(-2*time.Hour))
	f.add("n5", item.StateNeedsYou, "bogus", t0.Add(-4*time.Hour))
	f.add("q1", item.StateQueued, "info", t0.Add(-time.Hour))
	f.add("p1", item.StatePreparing, "critical", t0.Add(-time.Hour))
	f.add("i1", item.StateNew, "critical", t0.Add(-time.Minute))
	f.add("r1", item.StateResolved, "critical", t0.Add(-time.Hour))
	f.add("d1", item.StateDone, "critical", t0.Add(-time.Hour))
	f.snapshot()

	if got, want := f.groupLabels(), []string{"Needs you:5", "Preparing:2", "Incoming:1", "Resolved:1"}; !slices.Equal(got, want) {
		t.Fatalf("groups %v, want %v", got, want)
	}
	// Severity order, unknown/missing severities last, then oldest start
	// first (the two unknown severities keep start order).
	want := []string{"Alert n4", "Alert n2", "Alert n1", "Alert n5", "Alert n3", "Alert p1", "Alert q1", "Alert i1", "Alert r1"}
	if got := f.visibleIDs(); !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v", got, want)
	}
	view := f.view()
	for _, s := range []string{"NEEDS YOU (5)", "PREPARING (2)", "INCOMING (1)", "RESOLVED (1)"} {
		if !strings.Contains(view, s) {
			t.Errorf("view lacks %q", s)
		}
	}
	for _, s := range []string{"SNOOZED", "DONE ("} {
		if strings.Contains(view, s) {
			t.Errorf("empty group %q shown", s)
		}
	}
	f.add("s1", item.StateSnoozed, "warning", t0.Add(-time.Hour))
	f.snapshot()
	if got, want := f.groupLabels(), []string{"Needs you:5", "Preparing:2", "Incoming:1", "Snoozed:1", "Resolved:1"}; !slices.Equal(got, want) {
		t.Fatalf("groups %v, want %v", got, want)
	}
}

func TestDoneWindow(t *testing.T) {
	f := newFixture(t, nil)
	f.add("a", item.StateNeedsYou, "critical", t0)
	edge := f.add("edge", item.StateDone, "critical", t0.Add(-DoneWindow))
	old := f.add("old", item.StateDone, "critical", t0.Add(-DoneWindow-time.Second))
	f.snapshot()
	if got := f.groupLabels(); !slices.Equal(got, []string{"Needs you:1"}) {
		t.Fatalf("done shown initially: %v", got)
	}
	f.d.keys("d")
	if got := f.groupLabels(); !slices.Equal(got, []string{"Needs you:1", "Done:1"}) {
		t.Fatalf("groups %v", got)
	}
	if f.d.m.groups[1].items[0].Item.ID != edge {
		t.Fatal("the boundary item must be shown")
	}
	// The filter is display-only: the old item still exists.
	if f.disk(old).State != item.StateDone {
		t.Fatal("old done item changed")
	}
	f.d.keys("d")
	if got := f.groupLabels(); !slices.Equal(got, []string{"Needs you:1"}) {
		t.Fatalf("done not hidden again: %v", got)
	}
}

func TestSelectionFollowsIdentity(t *testing.T) {
	f := newFixture(t, nil)
	a := f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour))
	b := f.add("b", item.StateNeedsYou, "critical", t0.Add(-time.Minute))
	f.snapshot()
	if f.d.m.SelectedID() != a {
		t.Fatal("first item not selected")
	}
	f.d.keys("j")
	if f.d.m.SelectedID() != b {
		t.Fatal("j did not move")
	}
	// b moves to the top (critical sorts before warning) and stays
	// selected.
	f.items[a].Severity = "warning"
	f.snapshot()
	if f.d.m.SelectedID() != b || f.d.m.list.Selected != 0 {
		t.Fatalf("selection lost: %s at %d", f.d.m.SelectedID(), f.d.m.list.Selected)
	}
	// b leaves the visible groups: the selection falls back to a valid
	// item.
	f.items[b].State = item.StateDone
	f.snapshot()
	if f.d.m.SelectedID() != a {
		t.Fatalf("selection %q", f.d.m.SelectedID())
	}
	delete(f.items, a)
	delete(f.items, b)
	f.snapshot()
	if f.d.m.SelectedID() != "" {
		t.Fatal("empty list has a selection")
	}
}

func TestNavigationFocusAndSizes(t *testing.T) {
	f := newFixture(t, nil)
	// Keys on an empty model never panic.
	f.d.keys("j", "k", "down", "up", "g", "G", "tab", "j", "tab", "d", "d", "?", "?", "enter", "o", "c", "C", "p", "x", "l", "b", "a")
	if len(f.engine.got()) != 0 || f.d.m.confirm != nil {
		t.Fatalf("empty model dispatched %v", f.engine.got())
	}
	if !strings.Contains(f.d.m.Footer(), "no item selected") {
		t.Fatalf("footer %q", f.d.m.Footer())
	}
	for i := range 5 {
		f.add(fmt.Sprintf("i%d", i), item.StateNeedsYou, "critical", t0.Add(-time.Duration(10-i)*time.Minute))
	}
	f.snapshot()
	ids := f.d.m.visible
	f.d.keys("G")
	if f.d.m.SelectedID() != ids[4].Item.ID {
		t.Fatal("G")
	}
	f.d.keys("k", "up")
	if f.d.m.SelectedID() != ids[2].Item.ID {
		t.Fatal("k/up")
	}
	f.d.keys("g", "j", "down")
	if f.d.m.SelectedID() != ids[2].Item.ID {
		t.Fatal("j/down")
	}
	// Preview focus scrolls instead of moving the selection.
	f.d.keys("tab", "j", "j", "down")
	if f.d.m.SelectedID() != ids[2].Item.ID || f.d.m.scroll != 3 {
		t.Fatalf("preview scroll: sel=%s scroll=%d", f.d.m.SelectedID(), f.d.m.scroll)
	}
	f.d.keys("k")
	if f.d.m.scroll != 2 {
		t.Fatal("k in preview")
	}
	// g and G select the first and last item regardless of focus.
	f.d.keys("G")
	if f.d.m.SelectedID() != ids[4].Item.ID || f.d.m.scroll != 0 || f.d.m.focus != focusPreview {
		t.Fatalf("G in preview: sel=%s scroll=%d", f.d.m.SelectedID(), f.d.m.scroll)
	}
	f.d.keys("g")
	if f.d.m.SelectedID() != ids[0].Item.ID {
		t.Fatal("g in preview")
	}
	f.d.keys("tab", "j")
	if f.d.m.SelectedID() != ids[1].Item.ID {
		t.Fatal("tab back to list")
	}
	for _, size := range [][2]int{{0, 0}, {1, 1}, {5, 3}, {10, 5}, {20, 6}, {30, 10}, {80, 24}, {200, 60}} {
		f.d.send(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		out := f.d.m.Render()
		if size[0] > 0 && size[1] > 0 {
			for _, l := range strings.Split(out, "\n") {
				if w := ansi.StringWidth(l); w > size[0] {
					t.Fatalf("%v: line wider than the terminal (%d): %q", size, w, l)
				}
			}
			if n := strings.Count(out, "\n") + 1; n > size[1] {
				t.Fatalf("%v: %d lines", size, n)
			}
		}
	}
	f.d.keys("q")
	if !f.d.quit {
		t.Fatal("q did not quit")
	}
}

func TestHelpAndConfirmationCaptureKeys(t *testing.T) {
	f := newFixture(t, nil)
	a := f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeReady, t0.Add(-time.Hour)))
	b := f.add("b", item.StateNeedsYou, "critical", t0.Add(-time.Minute))
	f.snapshot()

	f.d.keys("?")
	if !strings.Contains(f.view(), "confirmation") {
		t.Fatal("help not shown")
	}
	f.d.keys("p", "x", "c", "j", "d", "r", "enter")
	if len(f.engine.got()) != 0 || len(f.resume) != 0 || len(f.d.execs) != 0 || f.d.m.SelectedID() != a || f.d.m.showDone {
		t.Fatalf("keys leaked through help: %v", f.engine.got())
	}
	f.d.keys("esc")
	if f.d.m.help {
		t.Fatal("esc did not close help")
	}

	// At an ordinary terminal size every help line is reachable by
	// scrolling, and scrolling moves neither the selection nor the preview.
	f.d.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	f.d.keys("?")
	seen := ""
	for range 40 {
		seen += ansi.Strip(f.view())
		f.d.keys("j")
	}
	for _, l := range helpLines() {
		for _, word := range strings.Fields(ansi.Strip(l)) {
			if !strings.Contains(seen, word) {
				t.Fatalf("help word %q never visible at 80x24", word)
			}
		}
	}
	if !strings.Contains(ansi.Strip(f.view()), "closes it.") {
		t.Fatal("the end of the help is not reachable")
	}
	f.d.keys("g")
	if !strings.Contains(ansi.Strip(f.view()), "Navigation") || f.d.m.SelectedID() != a || f.d.m.scroll != 0 {
		t.Fatal("g in help")
	}
	f.d.keys("G", "?")
	if f.d.m.help || f.d.m.SelectedID() != a {
		t.Fatal("G in help moved the selection")
	}
	f.d.keys("?")
	if !strings.Contains(ansi.Strip(f.view()), "Navigation") {
		t.Fatal("reopened help does not start at the top")
	}
	f.d.keys("?")

	// A dismissal waits for y and ignores other keys.
	f.d.keys("x")
	if !strings.Contains(f.view(), `Dismiss "Alert a"?`) {
		t.Fatalf("no confirmation prompt: %s", f.d.m.footerText())
	}
	f.d.keys("p", "j", "c", "enter", "x", "d")
	if len(f.engine.got()) != 0 || f.d.m.SelectedID() != a || len(f.resume) != 0 {
		t.Fatalf("keys leaked through confirmation: %v", f.engine.got())
	}
	f.d.keys("n")
	if f.d.m.confirm != nil || len(f.engine.got()) != 0 {
		t.Fatal("n did not cancel")
	}
	f.d.keys("x", "esc")
	if f.d.m.confirm != nil || len(f.engine.got()) != 0 {
		t.Fatal("esc did not cancel")
	}

	// The confirmation stays bound to the item, even when a refresh
	// reorders the list.
	f.d.keys("x")
	f.items[b].Severity = ""
	f.items[a].Severity = "info"
	f.add("c", item.StateNeedsYou, "critical", t0)
	f.snapshot()
	f.d.keys("y")
	if got := f.engine.got(); !slices.Equal(got, []string{"dismiss " + a}) {
		t.Fatalf("calls %v", got)
	}
	if h := f.disk(a).History; len(h) != 1 {
		t.Fatalf("the UI wrote history itself: %+v", h)
	}
	// Quitting stays available from a confirmation and from help.
	f.d.keys("x", "q")
	if !f.d.quit {
		t.Fatal("q in confirmation")
	}
	f.d.quit = false
	f.d.keys("?", "q")
	if !f.d.quit {
		t.Fatal("q in help")
	}
	f.d.quit = false
	f.d.keys("?", "ctrl+c")
	if !f.d.quit {
		t.Fatal("ctrl+c in help")
	}
}

func TestEngineRequestsAndErrorHints(t *testing.T) {
	errUnknown := errors.New("unknown item")
	f := newFixture(t, func(o *Options) {
		o.Hint = func(err error) string { return "hint: " + err.Error() }
	})
	a := f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeReady, t0.Add(-time.Hour)))
	f.snapshot()

	f.d.keys("p")
	if got := f.engine.got(); !slices.Equal(got, []string{"manual " + a}) || f.d.m.Footer() != "preparation run requested" {
		t.Fatalf("calls %v footer %q", got, f.d.m.Footer())
	}
	f.d.keys("r")
	if f.d.m.Footer() != "poll started" {
		t.Fatalf("footer %q", f.d.m.Footer())
	}
	for _, tc := range []struct {
		keys []string
		op   string
		err  error
	}{
		{[]string{"p"}, "manual", errUnknown},
		{[]string{"p"}, "manual", errors.New("a manual run is not allowed while the item is queued")},
		{[]string{"p"}, "manual", errors.New("persist item: disk full")},
		{[]string{"x", "y"}, "dismiss", errors.New("the item is already done")},
		{[]string{"x", "y"}, "dismiss", context.Canceled},
		{[]string{"r"}, "poll", errors.New("a poll is already in progress")},
	} {
		f.engine.errs = map[string]error{tc.op: tc.err}
		f.d.keys(tc.keys...)
		if f.d.m.Footer() != "hint: "+tc.err.Error() || !f.d.m.footerErr {
			t.Errorf("%v: footer %q", tc.err, f.d.m.Footer())
		}
	}
	// No request wrote history or changed the item locally.
	it := f.disk(a)
	if len(it.History) != 1 || it.Seen || it.State != item.StateNeedsYou {
		t.Fatalf("item changed: %+v", it)
	}
}

func TestRequestsDoNotBlockInput(t *testing.T) {
	f := newFixture(t, nil)
	f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour))
	f.add("b", item.StateNeedsYou, "critical", t0)
	f.snapshot()
	block := make(chan struct{})
	f.engine.blockOn = block
	_, cmd := f.d.m.Update(key("p"))
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	// Input keeps working while the request waits for the engine.
	f.d.keys("j")
	if f.d.m.SelectedID() == "" || f.d.m.list.Selected != 1 {
		t.Fatal("input blocked")
	}
	close(block)
	f.d.send(<-done)
	if f.d.m.Footer() != "preparation run requested" {
		t.Fatalf("footer %q", f.d.m.Footer())
	}
}

func TestRowStylesAndStatus(t *testing.T) {
	f := newFixture(t, nil)
	unseen := f.add("u", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeReady, t0.Add(-time.Hour)))
	seen := f.add("s", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeBlocked, t0.Add(-time.Hour)))
	res := f.add("r", item.StateResolved, "critical", t0.Add(-time.Hour))
	exec := f.add("e", item.StatePreparing, "critical", t0.Add(-time.Hour), runningRun(1, t0.Add(-65*time.Second)))
	inc := f.add("i", item.StateNew, "critical", t0.Add(-2*time.Minute))
	fail := f.add("f", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeFailed, t0.Add(-time.Hour)))
	intr := f.add("x", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeInterrupted, t0.Add(-time.Hour)))
	q := f.add("q", item.StateQueued, "critical", t0.Add(-time.Hour))
	sn := f.add("z", item.StateSnoozed, "critical", t0.Add(-time.Hour), runningRun(1, t0.Add(-10*time.Second)))
	f.items[seen].Seen = true
	f.snapshot()
	row := func(id string) string {
		v, _ := f.d.m.findView(id)
		return f.d.m.row(v, false, 80)
	}
	if !strings.Contains(row(unseen), "\x1b[1m") || strings.Contains(row(seen), "\x1b[1m") {
		t.Fatalf("bold: unseen=%q seen=%q", row(unseen), row(seen))
	}
	if !strings.Contains(row(res), "\x1b[2m") && !strings.Contains(row(res), ";2m") {
		t.Fatalf("resolved not dimmed: %q", row(res))
	}
	for id, want := range map[string]string{
		unseen: "ready", seen: "blocked", exec: "1m05s", inc: "prep in 3m00s", fail: "failed",
		intr: "interrupted", q: "queued", sn: "10s", res: "resolved",
	} {
		if got := ansi.Strip(row(id)); !strings.Contains(got, want) {
			t.Errorf("%s: row %q lacks %q", id, got, want)
		}
	}
	// The heartbeat updates elapsed time and countdowns.
	f.now = t0.Add(2 * time.Minute)
	f.d.send(HeartbeatMsg(f.now))
	if got := ansi.Strip(row(exec)); !strings.Contains(got, "3m05s") {
		t.Fatalf("elapsed not updated: %q", got)
	}
	if got := ansi.Strip(row(inc)); !strings.Contains(got, "prep in 1m00s") {
		t.Fatalf("countdown not updated: %q", got)
	}
	f.now = t0.Add(4 * time.Minute)
	f.d.send(HeartbeatMsg(f.now))
	if got := ansi.Strip(row(inc)); !strings.Contains(got, "prep due") {
		t.Fatalf("countdown: %q", got)
	}
}

func TestHeader(t *testing.T) {
	f := newFixture(t, nil)
	if !strings.Contains(f.view(), "starting") {
		t.Fatal("no starting header")
	}
	s := Snapshot{
		Sources: []SourceHealth{
			{Name: "fresh"},
			{Name: "ok", LastSuccess: t0.Add(-90 * time.Second)},
			{Name: "broken", LastSuccess: t0.Add(-time.Hour), LastErr: "fetch: HTTP 500"},
		},
		Running: 3, Concurrency: 2, Queued: 4, Unreadable: 2,
	}
	f.d.send(SnapshotMsg(s))
	h := ansi.Strip(f.d.m.headerText())
	for _, want := range []string{"fresh … never polled", "ok ✓ 1m ago", "broken ✗ fetch: HTTP 500", "running 3/2", "queued 4", "2 unreadable"} {
		if !strings.Contains(h, want) {
			t.Errorf("header %q lacks %q", h, want)
		}
	}
	// Recovery clears the error; ages advance with the heartbeat.
	s.Sources[2].LastErr, s.Sources[2].LastSuccess = "", t0
	s.Unreadable = 0
	f.d.send(SnapshotMsg(s))
	f.now = t0.Add(2 * time.Minute)
	f.d.send(HeartbeatMsg(f.now))
	h = ansi.Strip(f.d.m.headerText())
	if strings.Contains(h, "✗") || !strings.Contains(h, "broken ✓ 2m ago") || strings.Contains(h, "unreadable") {
		t.Fatalf("header %q", h)
	}
	// A failed notification delivery is shown, sanitized, until a later
	// delivery succeeds.
	s.Notify = NotifyHealth{LastErr: "terminal-notifier failed:\x1b[31m exit status 1", ItemID: "x", ItemTitle: "Alert\nx"}
	f.d.send(SnapshotMsg(s))
	h = ansi.Strip(f.view())
	if !strings.Contains(h, "notification ✗ terminal-notifier failed:[31m exit status 1 (Alert x)") {
		t.Fatalf("header %q", h)
	}
	s.Notify = NotifyHealth{}
	f.d.send(SnapshotMsg(s))
	if h = ansi.Strip(f.d.m.headerText()); strings.Contains(h, "notification") {
		t.Fatalf("header %q", h)
	}
}

const readyResult = `{"version":1,"status":"ready","summary":"Pods crash on start.","confidence":"high",
"root_cause":"Bad config.","assumptions":["The deploy at 10:00 caused it."],
"gate":{"question":"Roll back?","options":["Roll back","Wait"]},
"proposed_actions":[{"id":1,"type":"rollback","title":"Roll back api","description":"Revert to v1.","preview":"kubectl rollout undo"}]}`

const blockedResult = `{"version":1,"status":"blocked","summary":"Missing access.","assumptions":[],
"gate":{"question":"Which cluster?"},"proposed_actions":[]}`

func TestPreparedPreview(t *testing.T) {
	f := newFixture(t, nil)
	a := f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeReady, t0.Add(-time.Hour)))
	b := f.add("b", item.StateNeedsYou, "critical", t0, finishedRun(1, item.OutcomeBlocked, t0.Add(-time.Hour)))
	report := "# Report\n\n" + strings.Repeat("line of the report\n", 80) + "END OF REPORT\n"
	f.writeRunFile(a, 1, item.ResultFile, readyResult)
	f.writeRunFile(a, 1, item.ReportFile, report)
	f.writeRunFile(b, 1, item.ResultFile, blockedResult)
	f.writeRunFile(b, 1, item.ReportFile, "blocked report\n")
	f.snapshot()

	lines := strings.Join(f.d.m.previewLines(), "\n")
	lines = ansi.Strip(lines)
	order := []string{"Alert a", "critical · dev · firing 1h", "run 1 ready", "high confidence",
		"Summary", "Pods crash on start.", "Root cause", "Question", "Roll back?", "1) Roll back", "2) Wait",
		"Proposed actions", "1. rollback  Roll back api", "Revert to v1.", "kubectl rollout undo",
		"Assumptions", "The deploy at 10:00 caused it.", "Report", "# Report", "END OF REPORT"}
	pos := 0
	for _, s := range order {
		i := strings.Index(lines[pos:], s)
		if i < 0 {
			t.Fatalf("preview lacks %q after position %d:\n%s", s, pos, lines)
		}
		pos += i + len(s)
	}
	// The report is scrollable in preview focus; G reaches its end.
	if strings.Contains(f.view(), "END OF REPORT") {
		t.Fatal("report end visible without scrolling")
	}
	f.d.keys("tab")
	f.d.keys(slices.Repeat([]string{"j"}, 200)...)
	if !strings.Contains(f.view(), "END OF REPORT") || f.d.m.SelectedID() != a {
		t.Fatal("j did not scroll to the end")
	}
	// Resizing recomputes wrapping and the scroll bounds.
	f.d.send(tea.WindowSizeMsg{Width: 60, Height: 20})
	f.d.keys(slices.Repeat([]string{"j"}, 200)...)
	if !strings.Contains(f.view(), "END OF REPORT") {
		t.Fatal("scrolling after a resize")
	}
	f.d.send(tea.WindowSizeMsg{Width: 200, Height: 60})
	if v := f.view(); !strings.Contains(v, "END OF REPORT") || !strings.Contains(v, "line of the report") {
		t.Fatal("scroll bound not recomputed after growing")
	}

	f.d.keys("tab", "j")
	lines = ansi.Strip(strings.Join(f.d.m.previewLines(), "\n"))
	for _, s := range []string{"Blocked", "Missing access.", "Which cluster?", "Proposed actions\nnone", "Assumptions\nnone", "blocked report"} {
		if !strings.Contains(lines, s) {
			t.Errorf("blocked preview lacks %q:\n%s", s, lines)
		}
	}
	if strings.Contains(lines, "confidence") {
		t.Error("blocked preview without confidence shows one")
	}
	// Previewing and scrolling never mark an item seen.
	if f.disk(a).Seen || f.disk(b).Seen {
		t.Fatal("preview marked seen")
	}
}

func TestPreviewArtifactErrors(t *testing.T) {
	f := newFixture(t, nil)
	f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeReady, t0.Add(-time.Hour)))
	f.add("b", item.StateNew, "critical", t0)
	f.snapshot()
	lines := ansi.Strip(strings.Join(f.d.m.previewLines(), "\n"))
	if !strings.Contains(lines, "result.json:") || !strings.Contains(lines, "report.md:") {
		t.Fatalf("missing artifacts not shown:\n%s", lines)
	}
	f.d.keys("j")
	lines = ansi.Strip(strings.Join(f.d.m.previewLines(), "\n"))
	if !strings.Contains(lines, "No preparation run yet") || !strings.Contains(lines, "no run") {
		t.Fatalf("no-run preview:\n%s", lines)
	}
}

func TestFailurePreview(t *testing.T) {
	f := newFixture(t, nil)
	r := finishedRun(1, item.OutcomeFailed, t0.Add(-time.Hour))
	r.Error = "copilot exited with code 1"
	a := f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour), r)
	var log strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&log, "stderr line %02d\n", i)
	}
	f.writeRunFile(a, 1, item.StderrFile, log.String())
	ir := finishedRun(2, item.OutcomeInterrupted, t0.Add(-time.Hour))
	ir.Error = "tower was not running when the run ended"
	b := f.add("b", item.StateNeedsYou, "critical", t0, ir)
	f.writeRunFile(b, 2, item.StderrFile, "only\nthree\nlines")
	f.snapshot()

	lines := ansi.Strip(strings.Join(f.d.m.previewLines(), "\n"))
	if !strings.Contains(lines, "Run 1 failed: copilot exited with code 1") {
		t.Fatalf("no error:\n%s", lines)
	}
	if strings.Contains(lines, "stderr line 10") || !strings.Contains(lines, "stderr line 11") || !strings.Contains(lines, "stderr line 30") {
		t.Fatalf("not the last 20 lines:\n%s", lines)
	}
	if got := len(f.d.m.art.stderr); got != 20 {
		t.Fatalf("%d stderr lines", got)
	}
	if strings.Contains(lines, "Summary") {
		t.Fatal("failed run shown as a report")
	}
	f.d.keys("j")
	lines = ansi.Strip(strings.Join(f.d.m.previewLines(), "\n"))
	if !strings.Contains(lines, "Run 2 interrupted: tower was not running") || !strings.Contains(lines, "only\nthree\nlines") {
		t.Fatalf("interrupted preview:\n%s", lines)
	}
	// A missing stderr.log is a visible, nonfatal condition.
	c := f.add("c", item.StateNeedsYou, "critical", t0.Add(time.Minute), finishedRun(1, item.OutcomeCancelled, t0))
	f.snapshot()
	f.d.keys("G")
	if f.d.m.SelectedID() != c {
		t.Fatal("select c")
	}
	lines = ansi.Strip(strings.Join(f.d.m.previewLines(), "\n"))
	if !strings.Contains(lines, "stderr.log:") {
		t.Fatalf("missing log not shown:\n%s", lines)
	}
}

func assistant(content string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant.message", "data": map[string]string{"content": content}})
	return string(b) + "\n"
}

func TestRunningPreviewProgress(t *testing.T) {
	f := newFixture(t, nil)
	a := f.add("a", item.StatePreparing, "critical", t0.Add(-time.Hour), runningRun(1, t0.Add(-90*time.Second)))
	f.snapshot()
	lines := ansi.Strip(strings.Join(f.d.m.previewLines(), "\n"))
	if !strings.Contains(lines, "Preparing run 1 · elapsed 1m30s") || !strings.Contains(lines, "No assistant message yet") {
		t.Fatalf("running preview:\n%s", lines)
	}
	// Progress refreshes with the heartbeat, without an item change.
	f.writeRunFile(a, 1, item.OutputFile, `{"type":"session.start"}`+"\n"+assistant("\n  Checking the pods\nsecond line")+
		`{"type":"assistant.message_delta","data":{"content":"delta"}}`+"\n"+
		`{"type":"tool.execution_start","data":{"content":"tool"}}`+"\n"+`{"type":"assistant.message","data":{"content":"partial`)
	f.now = t0.Add(time.Second)
	f.d.send(HeartbeatMsg(f.now))
	lines = ansi.Strip(strings.Join(f.d.m.previewLines(), "\n"))
	if !strings.Contains(lines, "› Checking the pods") || !strings.Contains(lines, "elapsed 1m31s") {
		t.Fatalf("progress:\n%s", lines)
	}
}

func TestStaleArtifactResultsAreDropped(t *testing.T) {
	f := newFixture(t, nil)
	a := f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeReady, t0.Add(-time.Hour)))
	b := f.add("b", item.StateNeedsYou, "critical", t0, finishedRun(1, item.OutcomeReady, t0.Add(-time.Hour)))
	f.writeRunFile(a, 1, item.ReportFile, "report A\n")
	f.writeRunFile(b, 1, item.ReportFile, "report B\n")
	f.snapshot()
	// A load for a arrives after the selection moved to b.
	late := loadArtifacts(f.store, previewKey{id: a, run: 1, outcome: item.OutcomeReady})
	f.d.keys("j")
	f.d.send(artifactsMsg(late))
	lines := strings.Join(f.d.m.previewLines(), "\n")
	if strings.Contains(lines, "report A") || !strings.Contains(lines, "report B") {
		t.Fatalf("stale preview:\n%s", lines)
	}
	// A late progress read for another run is dropped as well.
	f.d.send(progressMsg(progress{id: a, run: 1, message: "stale"}))
	if f.d.m.prog.message == "stale" {
		t.Fatal("stale progress applied")
	}
}

func TestOpenReport(t *testing.T) {
	var editorArgs string
	f := newFixture(t, nil)
	dir := t.TempDir()
	editor := filepath.Join(dir, "my editor")
	argsFile := filepath.Join(dir, "args")
	writeScript(t, editor, "printf '%s\\n' \"$@\" > '"+argsFile+"'\n")
	f.d.m.opts.Editor = editor
	f.d.runExec = true
	r1 := finishedRun(1, item.OutcomeReady, t0.Add(-2*time.Hour))
	r2 := finishedRun(2, item.OutcomeReady, t0.Add(-time.Hour))
	a := f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour), r1, r2)
	f.writeRunFile(a, 1, item.ReportFile, "old\n")
	f.writeRunFile(a, 2, item.ReportFile, "new\n")
	f.snapshot()

	f.now = t0.Add(time.Minute)
	f.d.keys("enter")
	runPath, _ := f.store.RunPath(a, 2)
	data, err := os.ReadFile(argsFile) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	editorArgs = string(data)
	if editorArgs != filepath.Join(runPath, item.ReportFile)+"\n" {
		t.Fatalf("editor args %q", editorArgs)
	}
	it := f.disk(a)
	last := it.History[len(it.History)-1]
	if !it.Seen || last.Action != item.ActionOpenedReport || last.Run != 2 || len(it.History) != 2 {
		t.Fatalf("history %+v seen=%v", it.History, it.Seen)
	}

	// o works as well; a failing editor records nothing.
	writeScript(t, editor, "exit 3\n")
	f.d.keys("o")
	if !strings.Contains(f.d.m.Footer(), "editor (report)") || len(f.disk(a).History) != 2 {
		t.Fatalf("failed editor: footer %q history %d", f.d.m.Footer(), len(f.disk(a).History))
	}

	// A missing report is not opened (and not created).
	b := f.add("b", item.StateNeedsYou, "critical", t0, finishedRun(1, item.OutcomeBlocked, t0))
	f.snapshot()
	f.d.keys("j")
	if f.d.m.SelectedID() != b {
		t.Fatal("select b")
	}
	execs := len(f.d.execs)
	f.d.keys("enter")
	if len(f.d.execs) != execs || !strings.Contains(f.d.m.Footer(), "no report.md") {
		t.Fatalf("missing report: footer %q", f.d.m.Footer())
	}
	if ok, _ := f.store.HasRunFile(b, 1, item.ReportFile); ok {
		t.Fatal("report created")
	}
	if it := f.disk(b); it.Seen || len(it.History) != 1 {
		t.Fatal("missing report recorded")
	}
	// No run: a hint.
	f.add("c", item.StateNew, "critical", t0.Add(time.Minute))
	f.snapshot()
	f.d.keys("G", "enter")
	if !strings.Contains(f.d.m.Footer(), "no run") {
		t.Fatalf("footer %q", f.d.m.Footer())
	}
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil { // #nosec G306 -- test executable
		t.Fatal(err)
	}
}

func TestAuxiliaryActions(t *testing.T) {
	f := newFixture(t, nil)
	a := f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeFailed, t0.Add(-time.Hour)))
	f.items[a].Alert.RunbookURL = "https://runbooks.example.com/a"
	f.items[a].Alert.GeneratorURL = "https://prom.example.com/graph"
	f.snapshot()
	runPath, _ := f.store.RunPath(a, 1)

	f.d.keys("l")
	if !strings.Contains(f.d.m.Footer(), "no readable output.jsonl or stderr.log") || len(f.d.execs) != 0 {
		t.Fatalf("no logs: %q", f.d.m.Footer())
	}
	f.writeRunFile(a, 1, item.StderrFile, "err\n")
	f.d.keys("l")
	if len(f.d.execs) != 1 || !slices.Equal(f.d.execs[0][1:], []string{filepath.Join(runPath, item.StderrFile)}) {
		t.Fatalf("execs %v", f.d.execs)
	}
	f.writeRunFile(a, 1, item.OutputFile, "{}\n")
	f.d.keys("l")
	if !slices.Equal(f.d.execs[1][1:], []string{filepath.Join(runPath, item.OutputFile), filepath.Join(runPath, item.StderrFile)}) {
		t.Fatalf("execs %v", f.d.execs)
	}
	f.d.keys("a")
	itemPath, _ := f.store.ItemPath(a)
	if !slices.Equal(f.d.execs[2], []string{"/usr/bin/true", itemPath}) {
		t.Fatalf("execs %v", f.d.execs)
	}
	f.d.keys("b")
	f.items[a].Alert.RunbookURL = ""
	f.snapshot()
	f.d.keys("b")
	f.items[a].Alert.GeneratorURL = ""
	f.snapshot()
	f.d.keys("b")
	if !slices.Equal(f.opened, []string{"https://runbooks.example.com/a", "https://prom.example.com/graph"}) || !strings.Contains(f.d.m.Footer(), "no runbook or source URL") {
		t.Fatalf("opened %v footer %q", f.opened, f.d.m.Footer())
	}
	f.items[a].Alert.RunbookURL = "file:///etc/passwd"
	f.snapshot()
	f.d.keys("b")
	if len(f.opened) != 2 {
		t.Fatal("non-http URL opened")
	}
	// A failing editor is an error hint.
	f.d.execErr = errors.New("exit status 1")
	f.d.keys("a")
	if !strings.Contains(f.d.m.Footer(), "editor (item directory): exit status 1") {
		t.Fatalf("footer %q", f.d.m.Footer())
	}
	if it := f.disk(a); it.Seen || len(it.History) != 1 {
		t.Fatal("auxiliary actions recorded history")
	}
}

func TestResumeKeys(t *testing.T) {
	f := newFixture(t, nil)
	ready := finishedRun(2, item.OutcomeReady, t0.Add(-time.Hour))
	a := f.add("a", item.StateNeedsYou, "critical", t0.Add(-time.Hour), finishedRun(1, item.OutcomeReady, t0.Add(-2*time.Hour)), ready)
	f.snapshot()
	f.d.keys("c", "C")
	want := []ResumeRequest{
		{ItemID: a, Run: 2, SessionID: ready.SessionID, Placement: "split"},
		{ItemID: a, Run: 2, SessionID: ready.SessionID, Placement: "tab"},
	}
	if !slices.Equal(f.resume, want) {
		t.Fatalf("resume %+v", f.resume)
	}

	// A failed resume is shown as an error.
	f.d.m.opts.Resume = func(context.Context, ResumeRequest) error { return errors.New("ghostty.command: exit status 1") }
	f.d.keys("c")
	if !f.d.m.footerErr || !strings.Contains(f.d.m.Footer(), "ghostty.command") {
		t.Fatalf("footer %q", f.d.m.Footer())
	}
}

func TestResumeGuards(t *testing.T) {
	f := newFixture(t, nil)
	noSession := finishedRun(1, item.OutcomeFailed, t0)
	noSession.SessionID = ""
	setup := finishedRun(1, item.OutcomeFailed, t0)
	setup.StartedAt = nil
	setup.PID = 0
	a := f.add("a", item.StateNeedsYou, "critical", t0.Add(-4*time.Hour), noSession)
	b := f.add("b", item.StateNeedsYou, "critical", t0.Add(-3*time.Hour), setup)
	c := f.add("c", item.StateNew, "critical", t0.Add(-2*time.Hour))
	// An executing run of a snoozed item still needs a confirmation.
	d := f.add("d", item.StateSnoozed, "critical", t0.Add(-time.Hour), runningRun(1, t0.Add(-time.Minute)))
	f.snapshot()
	for _, id := range []string{a, b, c} {
		if f.d.m.SelectedID() != id {
			t.Fatalf("selected %s, want %s", f.d.m.SelectedID(), id)
		}
		f.d.keys("c")
		if !f.d.m.footerErr || len(f.resume) != 0 {
			t.Fatalf("%s: footer %q", id, f.d.m.Footer())
		}
		f.d.keys("j")
	}
	if f.d.m.SelectedID() != d {
		t.Fatal("select d")
	}
	f.d.keys("C")
	if len(f.resume) != 0 || !strings.Contains(f.d.m.footerText(), "still executing") {
		t.Fatalf("no confirmation: %q", f.d.m.footerText())
	}
	f.d.keys("n")
	if len(f.resume) != 0 {
		t.Fatal("cancelled resume launched")
	}
	f.d.keys("C", "y")
	if len(f.resume) != 1 || f.resume[0].Placement != "tab" || f.resume[0].Run != 1 {
		t.Fatalf("resume %+v", f.resume)
	}
	// A refresh that replaces the run invalidates the confirmation.
	f.d.keys("c")
	r2 := runningRun(2, t0)
	f.runs[d] = append(f.runs[d], r2)
	f.items[d].Runs.Current = 2
	f.snapshot()
	f.d.keys("y")
	if len(f.resume) != 1 || !strings.Contains(f.d.m.Footer(), "resume cancelled") {
		t.Fatalf("stale confirmation resumed: %+v footer %q", f.resume, f.d.m.Footer())
	}
}
