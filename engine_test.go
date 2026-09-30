package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/source"
)

// fakeAlertmanager is a local Alertmanager API returning a configurable
// response. 401/403 responses echo the request's Authorization header, like
// a misbehaving upstream might.
type fakeAlertmanager struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	body   string
	n      int
	auth   string
	gate   chan struct{}
}

func newFakeAlertmanager(t *testing.T) *fakeAlertmanager {
	t.Helper()
	f := &fakeAlertmanager{status: http.StatusOK, body: "[]"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.n++
		f.auth = r.Header.Get("Authorization")
		status, body, gate := f.status, f.body, f.gate
		f.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			body = `{"error":"invalid credentials","authorization":"` + r.Header.Get("Authorization") + `"}`
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body)) // #nosec G705 -- test server echoing synthetic values
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAlertmanager) set(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func (f *fakeAlertmanager) setGate(g chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate = g
}

func (f *fakeAlertmanager) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

func (f *fakeAlertmanager) lastAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auth
}

// fakeClock is a settable engine clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *fakeClock) add(d time.Duration) { c.set(c.now().Add(d)) }

// fakeSource is a controllable source.Source.
type fakeSource struct {
	name     string
	mu       sync.Mutex
	alerts   []source.Alert
	err      error
	gate     chan struct{}
	onReturn func()
	calls    int
	started  chan string
	finished chan string
}

func newFakeSource(name string) *fakeSource {
	return &fakeSource{name: name, started: make(chan string, 64), finished: make(chan string, 64)}
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Fetch(ctx context.Context) ([]source.Alert, error) {
	f.mu.Lock()
	f.calls++
	gate, alerts, err, onReturn := f.gate, f.alerts, f.err, f.onReturn
	f.mu.Unlock()
	f.started <- f.name
	defer func() { f.finished <- f.name }()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if onReturn != nil {
		onReturn()
	}
	if err != nil {
		return nil, err
	}
	return alerts, nil
}

func (f *fakeSource) set(alerts []source.Alert, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts, f.err = alerts, err
}

func (f *fakeSource) setGate(g chan struct{}, onReturn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gate, f.onReturn = g, onReturn
}

func (f *fakeSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// alertJSON returns an Alertmanager API alert.
func alertJSON(fp, name string, startsAt time.Time, state string, extra map[string]string, silencedBy ...string) string {
	labels := map[string]string{"alertname": name, "severity": "critical"}
	for k, v := range extra {
		labels[k] = v
	}
	if silencedBy == nil {
		silencedBy = []string{}
	}
	a := map[string]any{
		"fingerprint":  fp,
		"labels":       labels,
		"annotations":  map[string]string{"summary": "synthetic summary", "runbook_url": "https://runbooks.example.com/x"},
		"startsAt":     startsAt.Format(time.RFC3339),
		"updatedAt":    startsAt.Format(time.RFC3339),
		"endsAt":       startsAt.Add(time.Hour).Format(time.RFC3339),
		"generatorURL": "https://prometheus.example.com/graph",
		"status":       map[string]any{"state": state, "silencedBy": silencedBy, "inhibitedBy": []string{}},
		"receivers":    []map[string]string{{"name": "team"}},
	}
	data, _ := json.Marshal(a)
	return string(data)
}

func mkAlert(t *testing.T, src, fp string, startsAt time.Time) source.Alert {
	t.Helper()
	a, err := source.DecodeAlert(src, json.RawMessage(alertJSON(fp, "Synthetic", startsAt, "active", nil)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// harness runs the engine with injected tick/poll channels and hooks.
type harness struct {
	t                                         *testing.T
	tick, poll                                chan time.Time
	started, applied, skipped, ticked, pruned chan struct{}
	monitored                                 chan struct{}
	deliveries                                *deliveries
	api                                       *engineAPI
	cancel                                    context.CancelFunc
	done                                      chan error
	stderr                                    *syncBuffer
	stopped                                   bool
}

func startEngine(t *testing.T, opts engineOptions) *harness {
	t.Helper()
	h := &harness{
		t: t, tick: make(chan time.Time), poll: make(chan time.Time),
		started: make(chan struct{}, 64), applied: make(chan struct{}, 64), skipped: make(chan struct{}, 64),
		ticked: make(chan struct{}, 64), pruned: make(chan struct{}, 64), monitored: make(chan struct{}, 1),
		done: make(chan error, 1), stderr: &syncBuffer{}, deliveries: newDeliveries(),
	}
	signal := func(ch chan struct{}) func() { return func() { ch <- struct{}{} } }
	opts.hooks = engineHooks{
		roundStarted: signal(h.started), roundApplied: signal(h.applied), pollSkipped: signal(h.skipped),
		ticked: signal(h.ticked), pruneApplied: signal(h.pruned),
		monitored: func() {
			select {
			case h.monitored <- struct{}{}:
			default:
			}
		},
	}
	if opts.deliver == nil {
		opts.deliver = h.deliveries.deliver
	}
	if opts.api == nil {
		opts.api = newEngineAPI()
	}
	h.api = opts.api
	if opts.monitorC == nil {
		monitor := time.NewTicker(20 * time.Millisecond)
		t.Cleanup(monitor.Stop)
		opts.monitorC = monitor.C
	}
	opts.tickC, opts.pollC = h.tick, h.poll
	opts.stderr = h.stderr
	if opts.lookPath == nil {
		opts.lookPath = found
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- runEngine(ctx, opts) }()
	t.Cleanup(h.stop)
	return h
}

func (h *harness) wait(ch chan struct{}, what string) {
	h.t.Helper()
	select {
	case <-ch:
	case err := <-h.done:
		h.t.Fatalf("engine stopped while waiting for %s: %v\n%s", what, err, h.stderr.String())
	case <-time.After(10 * time.Second):
		h.t.Fatalf("timed out waiting for %s\n%s", what, h.stderr.String())
	}
}

func (h *harness) none(ch chan struct{}, what string) {
	h.t.Helper()
	select {
	case <-ch:
		h.t.Fatalf("unexpected %s", what)
	case <-time.After(200 * time.Millisecond):
	}
}

func (h *harness) send(ch chan time.Time) {
	h.t.Helper()
	select {
	case ch <- time.Now():
	case <-time.After(10 * time.Second):
		h.t.Fatal("engine loop not receiving")
	}
}

// doTick sends a refresh/reconcile tick and waits until it was handled.
func (h *harness) doTick() {
	h.t.Helper()
	h.send(h.tick)
	h.wait(h.ticked, "tick")
}

// stop cancels the engine and returns how long it took to stop.
func (h *harness) stop() {
	if h.stopped {
		return
	}
	h.stopped = true
	h.cancel()
	select {
	case err := <-h.done:
		if err != nil {
			h.t.Errorf("engine: %v", err)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatal("engine did not stop promptly")
	}
}

func waitFor(t *testing.T, ch chan string, what string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	return ""
}

// fakeConfig loads a configuration with file sources named after the fake
// sources (their metadata is used for alert.md).
func fakeConfig(t *testing.T, names ...string) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("state_dir: ./state\n" + fakeRuns(t, filepath.Join(dir, "state")) + "sources:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  - name: %s\n    type: file\n    path: ./%s.json\n", n, n)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return loadCfg(t, path, nil), filepath.Join(dir, "state")
}

// seedResolved creates a resolved (or done) item of src with alert.json.
func seedResolved(t *testing.T, stateDir, src, fp string, state item.State, resolvedAt time.Time) string {
	t.Helper()
	st, err := item.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	id, _ := item.NewID(item.Key{Source: src, Fingerprint: fp}, 1)
	base := 0
	it := &item.Item{
		Version: item.Version, ID: id, Type: item.TypeAlert, State: state, Title: "seed",
		CreatedAt: resolvedAt, UpdatedAt: resolvedAt,
		Source: item.SourceRef{Name: src, Fingerprint: fp},
		Alert: item.AlertInfo{
			StartsAt: resolvedAt, LastSeenAt: resolvedAt, Status: item.AlertResolved, ResolvedAt: &resolvedAt,
			Occurrences: 1, Labels: map[string]string{"alertname": "Seeded"},
		},
		Runs:    item.RunsInfo{OccurrenceBase: &base},
		History: []item.HistoryEntry{{At: resolvedAt, To: state, Reason: "seed"}},
	}
	if err := st.Create(it, json.RawMessage(alertJSON(fp, "Seeded", resolvedAt, "active", nil))); err != nil {
		t.Fatal(err)
	}
	return id
}

func itemState(t *testing.T, stateDir, id string) item.State {
	t.Helper()
	it := readItem(t, stateDir, id)
	if it == nil {
		return ""
	}
	return it.State
}

func TestEngineBackgroundRounds(t *testing.T) {
	t0 := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	cfg, stateDir := fakeConfig(t, "fast", "slow")
	oldDone := seedResolved(t, stateDir, "fast", "old", item.StateDone, t0.Add(-2*8760*time.Hour))

	fast, slow := newFakeSource("fast"), newFakeSource("slow")
	fast.set([]source.Alert{mkAlert(t, "fast", "aaa", t0)}, nil)
	slow.set([]source.Alert{mkAlert(t, "slow", "bbb", t0)}, nil)
	gate := make(chan struct{})
	// The slow poll completes 5s later than the fast one.
	slow.setGate(gate, func() { clock.add(5 * time.Second) })

	h := startEngine(t, engineOptions{cfg: cfg, now: clock.now, sources: []source.Source{fast, slow}})

	// The startup round runs in the background: both polls start without
	// waiting for each other, and the fast one completes while the slow one
	// is blocked.
	h.wait(h.started, "startup round")
	waitFor(t, slow.started, "slow poll start")
	waitFor(t, fast.started, "fast poll start")
	waitFor(t, fast.finished, "fast poll completion")

	// The engine loop keeps servicing ticks and prune results; the fast
	// result is not applied before the round completes.
	h.doTick()
	h.doTick()
	h.wait(h.pruned, "prune result")
	if _, err := os.Stat(filepath.Join(stateDir, "items", oldDone)); !os.IsNotExist(err) {
		t.Fatal("old done item not pruned")
	}
	h.none(h.applied, "round applied")
	if readItem(t, stateDir, "alert-fast-aaa-1") != nil {
		t.Fatal("an individual poll result was applied before the round completed")
	}

	// Poll ticks during the round are skipped.
	h.send(h.poll)
	h.wait(h.skipped, "skipped poll")
	h.send(h.poll)
	h.wait(h.skipped, "skipped poll")
	if fast.callCount() != 1 || slow.callCount() != 1 {
		t.Fatalf("calls = %d/%d", fast.callCount(), slow.callCount())
	}

	// Completing the round applies both results at once, each observed at
	// its own completion time.
	close(gate)
	h.wait(h.applied, "round applied")
	a, b := readItem(t, stateDir, "alert-fast-aaa-1"), readItem(t, stateDir, "alert-slow-bbb-1")
	if a == nil || b == nil {
		t.Fatal("round not applied")
	}
	if !a.Alert.LastSeenAt.Equal(t0) || !b.Alert.LastSeenAt.Equal(t0.Add(5*time.Second)) {
		t.Errorf("last_seen_at = %s / %s", a.Alert.LastSeenAt, b.Alert.LastSeenAt)
	}
	for _, id := range []string{"alert-fast-aaa-1", "alert-slow-bbb-1"} {
		if _, err := os.Stat(filepath.Join(stateDir, "items", id, "alert.md")); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}

	// No catch-up round is queued for the skipped ticks.
	h.none(h.started, "catch-up round")
	if fast.callCount() != 1 {
		t.Fatalf("fast calls = %d", fast.callCount())
	}
	// The next normal poll tick starts the next round.
	h.send(h.poll)
	h.wait(h.started, "next round")
	h.wait(h.applied, "next round applied")
	if fast.callCount() != 2 || slow.callCount() != 2 {
		t.Fatalf("calls = %d/%d", fast.callCount(), slow.callCount())
	}
}

func TestEnginePollBudgetsDoNotAccumulate(t *testing.T) {
	cfg, _ := fakeConfig(t, "one", "two")
	one, two := newFakeSource("one"), newFakeSource("two")
	// Both polls hang until their budget expires.
	one.setGate(make(chan struct{}), nil)
	two.setGate(make(chan struct{}), nil)
	start := time.Now()
	h := startEngine(t, engineOptions{cfg: cfg, sources: []source.Source{one, two}, fetchTimeout: 500 * time.Millisecond})
	h.wait(h.applied, "round applied")
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Errorf("round took %s; independent 500ms budgets must not add up", elapsed)
	}
	for _, s := range []*fakeSource{one, two} {
		if !strings.Contains(h.stderr.String(), `msg="source poll failed" source=`+s.name) {
			t.Errorf("no failure logged for %s", s.name)
		}
	}
	if !strings.Contains(h.stderr.String(), "last_success=never") {
		t.Error("missing last_success=never")
	}
}

func TestEngineTicksUseAppliedSnapshots(t *testing.T) {
	t0 := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	cfg, stateDir := fakeConfig(t, "a", "b", "c")
	oldA := seedResolved(t, stateDir, "a", "olda", item.StateResolved, t0.Add(-10*time.Minute))
	oldC := seedResolved(t, stateDir, "c", "oldc", item.StateResolved, t0.Add(-10*time.Minute))

	a, b, c := newFakeSource("a"), newFakeSource("b"), newFakeSource("c")
	a.set([]source.Alert{mkAlert(t, "a", "aaa", t0)}, nil)
	b.set([]source.Alert{mkAlert(t, "b", "bbb", t0)}, nil)
	c.set(nil, errors.New("synthetic failure")) // never successful

	h := startEngine(t, engineOptions{cfg: cfg, now: clock.now, sources: []source.Source{a, b, c}})
	h.wait(h.applied, "startup round")
	if s := itemState(t, stateDir, "alert-a-aaa-1"); s != item.StateNew {
		t.Fatalf("a item = %s", s)
	}

	// b fails in the next round.
	b.set(nil, errors.New("synthetic failure"))
	clock.add(time.Minute)
	h.send(h.poll)
	h.wait(h.applied, "second round")

	// A third round is blocked on a.
	gate := make(chan struct{})
	a.setGate(gate, nil)
	h.send(h.poll)
	h.wait(h.started, "third round")
	waitFor(t, a.started, "a poll")
	for len(a.started) > 0 {
		<-a.started
	}

	// Past prepare_after and resolved_linger: ticks during the blocked round
	// advance the previously healthy source only.
	clock.add(5 * time.Hour)
	h.doTick()
	if s := itemState(t, stateDir, "alert-a-aaa-1"); s != item.StatePreparing {
		t.Errorf("healthy source item = %s, want preparing (T2, then started)", s)
	}
	if s := itemState(t, stateDir, oldA); s != item.StateDone {
		t.Errorf("healthy source resolved item = %s, want done (T8)", s)
	}
	if s := itemState(t, stateDir, "alert-b-bbb-1"); s != item.StateNew {
		t.Errorf("failed source item = %s, want frozen new", s)
	}
	if s := itemState(t, stateDir, oldC); s != item.StateResolved {
		t.Errorf("never-successful source item = %s, want frozen resolved", s)
	}
	close(gate)
	h.wait(h.applied, "third round")

	out := h.stderr.String()
	if !strings.Contains(out, `msg="source poll failed" source=b error="synthetic failure" last_success=`+t0.Format(time.RFC3339)) {
		t.Errorf("failure log lacks the last success:\n%s", out)
	}
	if !strings.Contains(out, `msg="source poll failed" source=c error="synthetic failure" last_success=never`) {
		t.Errorf("failure log lacks last_success=never:\n%s", out)
	}
}

func fileStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func sameStamp(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}

func readString(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// httpSetup writes a config with a plain Alertmanager source "legacy" (URL
// with userinfo, bearer token command reading a token file) and a file
// source "dev".
func httpSetup(t *testing.T, am *fakeAlertmanager) (cfgPath, stateDir, fixture, tokenFile string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.yaml")
	stateDir = filepath.Join(dir, "state")
	fixture = filepath.Join(dir, "alerts.json")
	tokenFile = filepath.Join(dir, "token")
	withUser := strings.Replace(am.URL, "http://", "http://syn-user:syn-url-password@", 1)
	content := `state_dir: ./state
` + fakeRuns(t, stateDir) + `sources:
  - name: dev
    type: file
    path: ./alerts.json
  - name: legacy
    type: alertmanager
    url: ` + withUser + `
    grafana_instance: prod
    auth:
      type: bearer
      token_command: cat "` + tokenFile + `"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte(credentialSentinel+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, fixture, "[]")
	return
}

func grafanaEnv() map[string]string {
	return map[string]string{config.GrafanaInstancesEnv: `{"prod": {"url": "https://grafana.example.com", "auth": {"tokenCommand": "exit 1"}}}`}
}

func TestEngineHTTPSource(t *testing.T) {
	t0 := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	clock := &fakeClock{t: t0}
	am := newFakeAlertmanager(t)
	cfgPath, stateDir, fixture, _ := httpSetup(t, am)
	cfg := loadCfg(t, cfgPath, grafanaEnv())

	start := t0.Add(-time.Hour)
	firing := alertJSON("aaa111", "HighLatency", start, "active", nil)
	am.set(http.StatusOK, "["+firing+","+alertJSON("uuu999", "Unprocessed", start, "unprocessed", nil)+"]")
	writeFixture(t, fixture, "["+alertJSON("fff000", "FileAlert", start, "active", nil)+"]")

	h := startEngine(t, engineOptions{cfg: cfg, now: clock.now, level: slog.LevelDebug})
	h.wait(h.applied, "startup round")

	id := "alert-legacy-aaa111-1"
	itemDir := filepath.Join(stateDir, "items", id)
	if s := itemState(t, stateDir, id); s != item.StatePreparing {
		t.Fatalf("http item = %s", s)
	}
	if am.lastAuth() != "Bearer "+credentialSentinel {
		t.Errorf("Authorization = %q", am.lastAuth())
	}
	// An unprocessed new fingerprint creates no item.
	if readItem(t, stateDir, "alert-legacy-uuu999-1") != nil {
		t.Fatal("unprocessed alert created an item")
	}
	md := readString(t, filepath.Join(itemDir, "alert.md"))
	for _, want := range []string{
		"# HighLatency\n",
		"- **Alertmanager URL:** " + am.URL + "\n",
		"- **Grafana Instance:** prod\n",
		"- **Grafana URL:** https://grafana.example.com\n",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("alert.md lacks %q:\n%s", want, md)
		}
	}
	for _, leak := range []string{"syn-user", "syn-url-password", credentialSentinel, "cat "} {
		if strings.Contains(md, leak) {
			t.Errorf("alert.md contains %q", leak)
		}
	}
	if fileMD := readString(t, filepath.Join(stateDir, "items", "alert-dev-fff000-1", "alert.md")); !strings.HasPrefix(fileMD, "# FileAlert\n") {
		t.Errorf("file source alert.md = %q", fileMD)
	}
	if m := fileStat(t, filepath.Join(itemDir, "alert.md")).Mode().Perm(); m != 0o600 {
		t.Errorf("alert.md mode %v", m)
	}

	// Unrelated-field-only changes leave artifacts untouched.
	mdStat, jsonStat := fileStat(t, filepath.Join(itemDir, "alert.md")), fileStat(t, filepath.Join(itemDir, "alert.json"))
	unrelated := strings.Replace(firing, `"updatedAt":"`+start.Format(time.RFC3339), `"updatedAt":"`+t0.Format(time.RFC3339), 1)
	if unrelated == firing {
		t.Fatal("fixture replacement failed")
	}
	clock.add(time.Minute)
	am.set(http.StatusOK, "["+unrelated+"]")
	h.send(h.poll)
	h.wait(h.applied, "unrelated change round")
	if !sameStamp(mdStat, fileStat(t, filepath.Join(itemDir, "alert.md"))) || !sameStamp(jsonStat, fileStat(t, filepath.Join(itemDir, "alert.json"))) {
		t.Error("artifacts rewritten for an unrelated change")
	}

	// An existing fingerprint reported unprocessed (even with silencedBy)
	// stays unchanged rather than resolving or snoozing.
	before := readString(t, filepath.Join(itemDir, "item.yaml"))
	am.set(http.StatusOK, "["+alertJSON("aaa111", "HighLatency", start, "unprocessed", nil, "silence-1")+"]")
	clock.add(time.Minute)
	h.send(h.poll)
	h.wait(h.applied, "unprocessed round")
	h.doTick()
	if readString(t, filepath.Join(itemDir, "item.yaml")) != before {
		t.Error("unprocessed observation changed the item")
	}
	if !sameStamp(mdStat, fileStat(t, filepath.Join(itemDir, "alert.md"))) {
		t.Error("unprocessed observation rewrote alert.md")
	}

	// A label change rewrites alert.md with alert.json.
	am.set(http.StatusOK, "["+alertJSON("aaa111", "HighLatency", start, "active", map[string]string{"pod": "api-2"})+"]")
	clock.add(time.Minute)
	h.send(h.poll)
	h.wait(h.applied, "label change round")
	md = readString(t, filepath.Join(itemDir, "alert.md"))
	if !strings.Contains(md, "- `pod`: `api-2`") || !strings.Contains(readString(t, filepath.Join(itemDir, "alert.json")), "api-2") {
		t.Errorf("alert.md not rewritten:\n%s", md)
	}
	if strings.Contains(md, "syn-url-password") {
		t.Error("rewritten alert.md contains URL credentials")
	}

	// Successful absence resolves the HTTP and file items.
	am.set(http.StatusOK, "[]")
	writeFixture(t, fixture, "[]")
	clock.add(time.Minute)
	lastSuccess := clock.now()
	h.send(h.poll)
	h.wait(h.applied, "absence round")
	if s := itemState(t, stateDir, id); s != item.StateResolved {
		t.Fatalf("http item = %s, want resolved", s)
	}

	// 401 freezes the HTTP source; the file source continues.
	am.set(http.StatusUnauthorized, "")
	clock.add(time.Minute)
	h.send(h.poll)
	h.wait(h.applied, "401 round")
	clock.add(5 * time.Hour) // past resolved_linger
	h.doTick()
	if s := itemState(t, stateDir, id); s != item.StateResolved {
		t.Errorf("http item = %s, want frozen resolved", s)
	}
	if s := itemState(t, stateDir, "alert-dev-fff000-1"); s != item.StateDone {
		t.Errorf("file item = %s, want done", s)
	}
	out := h.stderr.String()
	wantFail := `msg="source poll failed" source=legacy error="HTTP 401: credentials rejected (token expired?)`
	if !strings.Contains(out, wantFail) || !strings.Contains(out, "last_success="+lastSuccess.Format(time.RFC3339)) {
		t.Errorf("stderr lacks the 401 diagnostic with last success:\n%s", out)
	}

	// Recovery resumes normal behavior and clears the error.
	am.set(http.StatusOK, "[]")
	h.send(h.poll)
	h.wait(h.applied, "recovery round")
	if s := itemState(t, stateDir, id); s != item.StateDone {
		t.Errorf("http item = %s, want done after recovery", s)
	}
	out = h.stderr.String()
	if !strings.Contains(out, `msg="source recovered" source=legacy`) || !strings.Contains(out, `previous_error="HTTP 401: credentials rejected (token expired?)`) {
		t.Errorf("no recovery log:\n%s", out)
	}
	h.stop()

	// Only normal polling happened: no retries on 401.
	if n := am.requests(); n != 7 {
		t.Errorf("requests = %d, want 7", n)
	}
	// The credential never leaks into logs or state.
	if strings.Contains(out, credentialSentinel) || strings.Contains(out, "cat ") {
		t.Error("stderr leaks the credential or command")
	}
	assertNoSentinel(t, stateDir)
	var sawHealth bool
	for _, line := range strings.Split(readString(t, filepath.Join(stateDir, "tower.log")), "\n") {
		if strings.Contains(line, `"msg":"source poll failed"`) && strings.Contains(line, `"source":"legacy"`) {
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatal(err)
			}
			sawHealth = rec["level"] == "WARN" && rec["last_success"] == lastSuccess.Format(time.RFC3339) &&
				strings.Contains(rec["error"].(string), "credentials rejected (token expired?)")
		}
	}
	if !sawHealth {
		t.Error("tower.log lacks structured health information")
	}
}

func TestEngineInitializesMissingMarkdown(t *testing.T) {
	am := newFakeAlertmanager(t)
	cfgPath, stateDir, _, _ := httpSetup(t, am)
	marker := filepath.Join(t.TempDir(), "command-ran")
	// Replace the token command with one that leaves a marker.
	data := strings.Replace(readString(t, cfgPath), `token_command: cat`, `token_command: touch `+marker+`; cat`, 1)
	if err := os.WriteFile(cfgPath, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadCfg(t, cfgPath, grafanaEnv())
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("config loading ran the credential command")
	}

	t0 := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	missing := seedResolved(t, stateDir, "legacy", "aaa", item.StateResolved, t0)
	fileItem := seedResolved(t, stateDir, "dev", "fff", item.StateResolved, t0)
	existing := seedResolved(t, stateDir, "legacy", "bbb", item.StateResolved, t0)
	unconfigured := seedResolved(t, stateDir, "gone", "ccc", item.StateResolved, t0)
	existingMD := filepath.Join(stateDir, "items", existing, "alert.md")
	if err := os.WriteFile(existingMD, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	corruptDir := filepath.Join(stateDir, "items", "alert-legacy-bad-1")
	if err := os.MkdirAll(corruptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "item.yaml"), []byte("{{ nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "alert.json"), []byte(alertJSON("bad", "X", t0, "active", nil)), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := func(id string) (string, string) {
		d := filepath.Join(stateDir, "items", id)
		return readString(t, filepath.Join(d, "item.yaml")), readString(t, filepath.Join(d, "alert.json"))
	}
	beforeYAML, beforeJSON := snapshot(missing)
	existingStat := fileStat(t, existingMD)

	// Polls never complete, so nothing but initialization touches items.
	block := newFakeSource("legacy")
	block.setGate(make(chan struct{}), nil)
	h := startEngine(t, engineOptions{cfg: cfg, sources: []source.Source{block}})
	h.wait(h.started, "startup round")

	md := readString(t, filepath.Join(stateDir, "items", missing, "alert.md"))
	if !strings.HasPrefix(md, "# Seeded\n") || !strings.Contains(md, "- **Alertmanager URL:** "+am.URL+"\n") || strings.Contains(md, "syn-url-password") {
		t.Errorf("initialized alert.md:\n%s", md)
	}
	if !strings.HasPrefix(readString(t, filepath.Join(stateDir, "items", fileItem, "alert.md")), "# Seeded\n") {
		t.Error("file source alert.md not initialized")
	}
	if y, j := snapshot(missing); y != beforeYAML || j != beforeJSON {
		t.Error("initialization changed item.yaml or alert.json")
	}
	if got := readString(t, existingMD); got != "# mine\n" || !sameStamp(existingStat, fileStat(t, existingMD)) {
		t.Error("existing alert.md changed")
	}
	for _, p := range []string{filepath.Join(stateDir, "items", unconfigured, "alert.md"), filepath.Join(corruptDir, "alert.md")} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s created", p)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("initialization ran the credential command")
	}
	if !strings.Contains(h.stderr.String(), "initialized missing alert.md") {
		t.Error("no initialization log")
	}
	h.stop()

	// Restarting leaves existing Markdown untouched.
	initialized := filepath.Join(stateDir, "items", missing, "alert.md")
	st := fileStat(t, initialized)
	h = startEngine(t, engineOptions{cfg: cfg, sources: []source.Source{block}})
	h.wait(h.started, "startup round")
	h.stop()
	if !sameStamp(st, fileStat(t, initialized)) {
		t.Error("restart rewrote alert.md")
	}
}

func TestEngineShutdownCancelsBlockedRound(t *testing.T) {
	for _, later := range []bool{false, true} {
		name := "startup round"
		if later {
			name = "later round"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile, runs := filepath.Join(dir, "pid"), filepath.Join(dir, "runs")
			if !later {
				if err := os.WriteFile(runs, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			slowHTTP := newFakeAlertmanager(t)
			slowHTTP.setGate(make(chan struct{}))
			cmdAM := newFakeAlertmanager(t)
			// The command blocks once runs exists (i.e. on the first call
			// for the startup case, on the second otherwise).
			cmd := `if [ -f ` + runs + ` ]; then sleep 30 & echo $! > ` + pidFile + `; wait; fi; touch ` + runs + `; echo ` + credentialSentinel
			cfgPath := filepath.Join(dir, "config.yaml")
			content := fmt.Sprintf(`state_dir: ./state
runs:
  command: sh
sources:
  - name: cmd
    type: alertmanager
    url: %s
    auth:
      type: bearer
      token_command: '%s'
  - name: http
    type: alertmanager
    url: %s
`, cmdAM.URL, cmd, slowHTTP.URL)
			if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if later {
				slowHTTP.setGate(nil)
			}
			h := startEngine(t, engineOptions{cfg: loadCfg(t, cfgPath, nil)})
			if later {
				h.wait(h.applied, "startup round")
				slowHTTP.setGate(make(chan struct{}))
				h.send(h.poll)
				h.wait(h.started, "later round")
			}
			pid := 0
			eventually(t, "credential command started", func() bool {
				data, err := os.ReadFile(pidFile) // #nosec G304 -- test file
				if err != nil {
					return false
				}
				_, err = fmt.Sscan(string(data), &pid)
				return err == nil && pid > 0
			})
			eventually(t, "blocked HTTP request", func() bool { return slowHTTP.requests() > 0 })

			start := time.Now()
			h.stop()
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Errorf("shutdown took %s", elapsed)
			}
			eventually(t, "credential process group killed", func() bool {
				return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
			})
			if strings.Contains(h.stderr.String(), credentialSentinel) {
				t.Error("stderr leaks the credential")
			}
		})
	}
}

func TestInvalidInstanceReferences(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	write := func(content string) {
		if err := os.WriteFile(cfgPath, []byte("state_dir: ./state\nruns:\n  command: sh\nsources:\n"+content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	instances := map[string]string{config.GrafanaInstancesEnv: `{"prod": {"url": "https://grafana.example.com", "auth": {"tokenCommand": "echo ` + credentialSentinel + `"}}}`}

	write("  - {name: a, type: alertmanager, grafana_alertmanager: grafana, grafana_instance: missing}\n")
	e := testEnv(dir, instances, time.Now())
	code, out, _ := runCLI(t, e, "--config", cfgPath, "config", "validate")
	if code != 1 || !strings.Contains(out, "sources[0].grafana_instance: instance not found") {
		t.Fatalf("validate: code=%d out=%q", code, out)
	}
	code, _, stderr := runCLI(t, e, "--config", cfgPath)
	if code != 1 || !strings.Contains(stderr, "instance not found") {
		t.Fatalf("startup: code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
		t.Fatal("startup touched state with an invalid config")
	}

	// Absent variable.
	write("  - {name: a, type: file, path: ./x.json, grafana_instance: prod}\n")
	code, out, _ = runCLI(t, testEnv(dir, nil, time.Now()), "--config", cfgPath, "config", "validate")
	if code != 1 || !strings.Contains(out, "GRAFANA_INSTANCES") {
		t.Fatalf("validate: code=%d out=%q", code, out)
	}

	// Valid references; the missing-instance warning stays for both kinds
	// of Alertmanager source.
	write(`  - {name: a, type: alertmanager, grafana_alertmanager: grafana, grafana_instance: prod}
  - {name: b, type: alertmanager, url: https://am.example.com}
  - {name: c, type: alertmanager, grafana_alertmanager: grafana, url: https://grafana.example.com}
`)
	code, out, _ = runCLI(t, e, "--config", cfgPath, "config", "validate")
	if code != 0 || strings.Count(out, "alertmanager source without grafana_instance") != 2 {
		t.Fatalf("validate: code=%d out=%q", code, out)
	}
	if strings.Contains(out, credentialSentinel) {
		t.Fatal("validation ran or disclosed the instance token command")
	}
}

// TestEngineStartupWarnsAboutMissingInstance checks the startup warning and
// the absence of the obsolete polling warning.
func TestEngineStartupWarnsAboutMissingInstance(t *testing.T) {
	am := newFakeAlertmanager(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := "state_dir: ./state\nruns:\n  command: sh\nsources:\n" +
		"  - {name: a, type: alertmanager, url: '" + am.URL + "'}\n" +
		"  - {name: b, type: alertmanager, grafana_alertmanager: grafana, url: '" + am.URL + "'}\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	h := startEngine(t, engineOptions{cfg: loadCfg(t, cfgPath, nil)})
	h.wait(h.applied, "startup round")
	h.stop()
	out := h.stderr.String()
	if strings.Count(out, "alertmanager source without grafana_instance") != 2 || strings.Contains(out, "polling is not available") {
		t.Errorf("stderr:\n%s", out)
	}
	if am.requests() != 2 {
		t.Errorf("requests = %d", am.requests())
	}
}
