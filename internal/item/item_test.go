package item

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

func newItem(t *testing.T, key Key, n int) *Item {
	t.Helper()
	id, err := NewID(key, n)
	if err != nil {
		t.Fatal(err)
	}
	return &Item{
		Version: Version, ID: id, Type: TypeAlert, State: StateNew, Title: "T",
		CreatedAt: t0, UpdatedAt: t0,
		Source:  SourceRef{Name: key.Source, Fingerprint: key.Fingerprint},
		Alert:   AlertInfo{StartsAt: t0, LastSeenAt: t0, Status: AlertActive, Occurrences: 1, Labels: map[string]string{}},
		History: []HistoryEntry{{At: t0, From: "", To: StateNew, Reason: "alert firing"}},
	}
}

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

var key = Key{Source: "dev", Fingerprint: "abc"}

func TestItemRoundTrip(t *testing.T) {
	full := newItem(t, Key{Source: "my-src", Fingerprint: "F00"}, 12)
	full.State = StateDone
	full.Title = "A: ns/pod"
	full.Severity = "critical"
	full.Seen = true
	full.Dismissed = true
	full.Alert = AlertInfo{
		StartsAt: t0, LastSeenAt: t0.Add(time.Minute), ResolvedAt: ptr(t0.Add(time.Hour)), Status: AlertResolved,
		Occurrences: 3, Labels: map[string]string{"a": "b"}, GeneratorURL: "g", RunbookURL: "r",
	}
	full.Runs = RunsInfo{Current: 2, PendingReason: ptr(ReasonRetry), OccurrenceBase: ptr(1)}
	full.PreviousItem = ptr("alert-my-src-F00-11")
	full.History = append(full.History,
		HistoryEntry{At: t0, Action: ActionOpenedReport, Run: 2},
		HistoryEntry{At: t0, Action: ActionDismissed},
		HistoryEntry{At: t0, From: StateNew, To: StateDone, Reason: "dismissed"},
	)
	empty := newItem(t, key, 1)
	empty.History = nil
	empty.Alert.Labels = nil

	for name, it := range map[string]*Item{"full": full, "empty": empty} {
		t.Run(name, func(t *testing.T) {
			data, err := yaml.Marshal(it)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"version: 1", "type: alert", "resolved_at:", "pending_reason:", "previous_item:", "occurrence_base:", "occurrences:", "runbook_url:", "generator_url:", "last_seen_at:", "seen:", "dismissed:"} {
				if !strings.Contains(string(data), field) {
					t.Errorf("missing %q in\n%s", field, data)
				}
			}
			got, err := decodeItem(it.ID, data)
			if err != nil {
				t.Fatal(err)
			}
			again, _ := yaml.Marshal(got)
			if !bytes.Equal(data, again) {
				t.Errorf("round trip changed document:\n%s\n---\n%s", data, again)
			}
		})
	}
	data, _ := yaml.Marshal(empty)
	for _, want := range []string{"resolved_at: null", "pending_reason: null", "previous_item: null", "current: 0"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q", want)
		}
	}
	data, _ = yaml.Marshal(full)
	for _, want := range []string{"action: opened-report\n      run: 2", "action: dismissed\n", "from: \"\"", "to: done"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %q in\n%s", want, data)
		}
	}
}

func TestHistoryEntryDecodeErrors(t *testing.T) {
	for name, doc := range map[string]string{
		"mixed":          "at: 2026-01-01T00:00:00Z\naction: dismissed\nto: done\n",
		"unknown action": "at: 2026-01-01T00:00:00Z\naction: deleted\n",
		"unknown state":  "at: 2026-01-01T00:00:00Z\nfrom: new\nto: gone\n",
		"no to":          "at: 2026-01-01T00:00:00Z\nfrom: new\n",
		"run on trans":   "at: 2026-01-01T00:00:00Z\nto: new\nrun: 1\n",
		"unknown field":  "at: 2026-01-01T00:00:00Z\nto: new\nextra: 1\n",
		"not a mapping":  "[1]\n",
	} {
		var h HistoryEntry
		if err := yaml.Unmarshal([]byte(doc), &h); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestItemValidate(t *testing.T) {
	cases := map[string]func(it *Item){
		"version":        func(it *Item) { it.Version = 2 },
		"id mismatch":    func(it *Item) { it.ID = "alert-dev-abc-2" },
		"source":         func(it *Item) { it.Source.Name = "other" },
		"type":           func(it *Item) { it.Type = "task" },
		"state":          func(it *Item) { it.State = "open" },
		"status":         func(it *Item) { it.Alert.Status = "firing" },
		"current":        func(it *Item) { it.Runs.Current = -1 },
		"pending reason": func(it *Item) { it.Runs.PendingReason = ptr(RunReason("x")) },
		"base negative":  func(it *Item) { it.Runs.OccurrenceBase = ptr(-1) },
		"base > current": func(it *Item) { it.Runs.Current, it.Runs.OccurrenceBase = 1, ptr(2) },
		"previous":       func(it *Item) { it.PreviousItem = ptr("../../etc") },
	}
	for name, mutate := range cases {
		it := newItem(t, key, 1)
		mutate(it)
		if err := it.Validate("alert-dev-abc-1"); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestLegacyItemWithoutOccurrenceBase(t *testing.T) {
	it := newItem(t, key, 1)
	it.Runs.OccurrenceBase = ptr(0)
	data, err := yaml.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "occurrence_base: 0") {
		t.Fatalf("explicit 0 not written:\n%s", data)
	}
	got, err := decodeItem(it.ID, data)
	if err != nil || got.Runs.OccurrenceBase == nil || *got.Runs.OccurrenceBase != 0 {
		t.Fatalf("explicit 0 decoded as %v (%v)", got.Runs.OccurrenceBase, err)
	}
	for name, doc := range map[string]string{
		"absent": strings.Replace(string(data), "    occurrence_base: 0\n", "", 1),
		"null":   strings.Replace(string(data), "occurrence_base: 0", "occurrence_base: null", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if doc == string(data) {
				t.Fatal("fixture unchanged")
			}
			got, err := decodeItem(it.ID, []byte(doc))
			if err != nil {
				t.Fatal(err)
			}
			if got.Runs.OccurrenceBase != nil {
				t.Fatalf("occurrence_base = %d, want unset", *got.Runs.OccurrenceBase)
			}
			if c := got.Clone(); c.Runs.OccurrenceBase != nil {
				t.Fatal("clone invented occurrence_base")
			}
		})
	}
	c := got.Clone()
	*c.Runs.OccurrenceBase = 5
	if *got.Runs.OccurrenceBase != 0 {
		t.Fatal("clone shares occurrence_base")
	}
}

func TestDeriveOccurrenceBase(t *testing.T) {
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	tr := func(m int, from, to State) HistoryEntry {
		return HistoryEntry{At: at(m), From: from, To: to, Reason: "r"}
	}
	runs := []Run{{Number: 1, QueuedAt: at(1)}, {Number: 2, QueuedAt: at(10)}, {Number: 3, QueuedAt: at(30)}}
	cases := []struct {
		name    string
		history []HistoryEntry
		runs    []Run
		current int
		want    int
	}{
		{"created only", []HistoryEntry{tr(0, "", StateNew)}, runs, 3, 0},
		{"T10 reopen", []HistoryEntry{tr(0, "", StateNew), tr(5, StateResolved, StateDone), tr(20, StateDone, StateNew)}, runs, 3, 2},
		{"latest T9 wins", []HistoryEntry{tr(0, "", StateNew), tr(5, StateResolved, StateNew), tr(25, StateResolved, StateNew)}, runs, 3, 2},
		{"T9 restoring needs-you is no start", []HistoryEntry{tr(0, "", StateNew), tr(20, StateResolved, StateNeedsYou)}, runs, 3, 0},
		{"T6 into new is no start", []HistoryEntry{tr(0, "", StateNew), tr(20, StateSnoozed, StateNew)}, runs, 3, 0},
		{"actions ignored", []HistoryEntry{tr(0, "", StateNew), tr(5, StateDone, StateNew), {At: at(40), Action: ActionDismissed}}, runs, 3, 1},
		{"start evicted", []HistoryEntry{tr(20, StateNew, StateQueued)}, runs, 3, 0},
		{"capped at current", []HistoryEntry{tr(50, StateDone, StateNew)}, runs, 2, 2},
		{"no runs", []HistoryEntry{tr(50, StateDone, StateNew)}, nil, 2, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			it := newItem(t, key, 1)
			it.History, it.Runs.Current = c.history, c.current
			if got := DeriveOccurrenceBase(it, c.runs); got != c.want {
				t.Fatalf("base = %d, want %d", got, c.want)
			}
		})
	}
	it := newItem(t, key, 1)
	it.Runs.Current, it.Runs.OccurrenceBase = 3, ptr(3)
	if got := DeriveOccurrenceBase(it, runs); got != 3 {
		t.Fatalf("recorded base overridden: %d", got)
	}
}

func TestRunRoundTrip(t *testing.T) {
	outcomes := []Outcome{OutcomeRunning, OutcomeReady, OutcomeBlocked, OutcomeFailed, OutcomeCancelled, OutcomeInterrupted}
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	var want []Run
	for i, o := range outcomes {
		r := Run{Number: i + 1, SessionID: "0b5c3c2e-4f0e-4a8e-9d4c-6d7f1b2a3c4d", Reason: ReasonAuto, Skill: "", QueuedAt: t0, Outcome: o}
		if i%2 == 1 {
			r.Reason = ReasonRetry
			r.Skill = "sre-analyze-alert"
			r.StartedAt = ptr(t0.Add(time.Second))
			r.FinishedAt = ptr(t0.Add(time.Minute))
			r.PID = 4242
			r.Error = "boom"
			r.ExitCode = ptr(0)
			r.RetryOf = i
		}
		if err := s.WriteRun(it.ID, r); err != nil {
			t.Fatal(err)
		}
		want = append(want, r)
	}
	got, err := s.ReadRuns(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := yaml.Marshal(want)
	b, _ := yaml.Marshal(got)
	if !bytes.Equal(a, b) {
		t.Fatalf("runs differ:\n%s\n---\n%s", a, b)
	}
	if got[0].ExitCode != nil || got[0].StartedAt != nil || got[0].FinishedAt != nil {
		t.Error("nulls not preserved")
	}
	if got[1].ExitCode == nil || *got[1].ExitCode != 0 {
		t.Error("zero exit code not preserved")
	}
	for _, f := range []string{"number:", "session_id:", "reason:", "skill:", "queued_at:", "started_at: null", "finished_at: null", "pid:", "outcome:", "error:", "exit_code: null", "retry_of:"} {
		if !strings.Contains(string(a), f) {
			t.Errorf("run yaml lacks %q", f)
		}
	}
	if err := s.WriteRun(it.ID, Run{Number: 0, Reason: ReasonAuto, Outcome: OutcomeReady}); err == nil {
		t.Error("run number 0 must be rejected")
	}
	if err := s.WriteRun(it.ID, Run{Number: 9, Reason: ReasonAuto, Outcome: "done"}); err == nil {
		t.Error("unknown outcome must be rejected")
	}
}

func TestHistoryAndActions(t *testing.T) {
	it := newItem(t, key, 1)
	it.State = StateNeedsYou
	if err := it.ApplyAction(ActionManualRun, 2, t0.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if it.Seen || it.State != StateNeedsYou {
		t.Fatal("manual-run action must not mark seen or change state")
	}
	if err := it.ApplyAction(ActionOpenedReport, 1, t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !it.Seen || it.State != StateNeedsYou {
		t.Fatal("opened-report must mark seen without changing state")
	}
	it.Seen = false
	if err := it.ApplyAction(ActionResumedSession, 1, t0.Add(3*time.Second)); err != nil || !it.Seen {
		t.Fatal("resumed-session must mark seen")
	}
	if err := it.ApplyAction(ActionDismissed, 7, t0.Add(4*time.Second)); err != nil || it.State != StateNeedsYou || it.Dismissed {
		t.Fatal("appending dismissed action alone must not change lifecycle")
	}
	if err := it.ApplyAction("bogus", 0, t0); err == nil {
		t.Fatal("unknown action must be rejected")
	}
	h := it.History
	if len(h) != 5 || h[1].Action != ActionManualRun || h[1].Run != 2 || h[2].Action != ActionOpenedReport ||
		h[3].Action != ActionResumedSession || h[4].Action != ActionDismissed || h[4].Run != 0 {
		t.Fatalf("history = %+v", h)
	}
	for i := 1; i < len(h); i++ {
		if h[i].At.Before(h[i-1].At) {
			t.Fatal("history not chronological")
		}
	}
	if !it.UpdatedAt.Equal(t0.Add(4 * time.Second)) {
		t.Error("updated_at not bumped")
	}

	// Limit: the oldest entries are dropped.
	var entries []HistoryEntry
	for i := range 600 {
		entries = append(entries, HistoryEntry{At: t0.Add(time.Duration(i) * time.Second), Action: ActionOpenedReport, Run: i})
	}
	got := AppendHistory(nil, entries...)
	if len(got) != MaxHistory || got[0].Run != 100 || got[MaxHistory-1].Run != 599 {
		t.Fatalf("limit: len=%d first=%d last=%d", len(got), got[0].Run, got[len(got)-1].Run)
	}
	got = AppendHistory(got, HistoryEntry{At: t0, Action: ActionDismissed})
	if len(got) != MaxHistory || got[0].Run != 101 || got[MaxHistory-1].Action != ActionDismissed {
		t.Fatal("append beyond limit must drop the oldest entry")
	}
}

func TestIDsAndTitles(t *testing.T) {
	id, err := NewID(Key{Source: "my-src-2", Fingerprint: "aB9"}, 17)
	if err != nil || id != "alert-my-src-2-aB9-17" {
		t.Fatalf("NewID = %q %v", id, err)
	}
	k, n, err := ParseID(id)
	if err != nil || k != (Key{Source: "my-src-2", Fingerprint: "aB9"}) || n != 17 {
		t.Fatalf("ParseID = %v %d %v", k, n, err)
	}
	for _, bad := range []string{"", "alert-dev-abc-0", "alert-dev-abc-01", "alert--abc-1", "alert-dev-a/b-1", "alert-dev-../x-1", "../alert-dev-abc-1", "alert-dev-abc-1/..", "alert-Dev-abc-1", "item-dev-abc-1", "alert-dev-abc-1-"} {
		if _, _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) must fail", bad)
		}
	}
	for _, k := range []Key{{"", "a"}, {"a", ""}, {"a/b", "c"}, {"a", "b-c"}, {"a", "../x"}, {"A", "b"}} {
		if _, err := NewID(k, 1); err == nil {
			t.Errorf("NewID(%v) must fail", k)
		}
	}
	if _, err := NewID(key, 0); err == nil {
		t.Error("n=0 must fail")
	}

	titles := []struct {
		labels map[string]string
		want   string
	}{
		{map[string]string{"alertname": "A"}, "A"},
		{map[string]string{"alertname": "A", "namespace": "ns"}, "A"},
		{map[string]string{"alertname": "A", "job": "j", "service": "s", "deployment": "d", "pod": "p", "namespace": "ns"}, "A: ns/p"},
		{map[string]string{"alertname": "A", "job": "j", "service": "s", "deployment": "d"}, "A: d"},
		{map[string]string{"alertname": "A", "job": "j", "service": "s", "namespace": "ns"}, "A: ns/s"},
		{map[string]string{"alertname": "A", "job": "j"}, "A: j"},
		{map[string]string{"alertname": "A", "pod": "", "job": "j"}, "A: j"},
	}
	for _, tt := range titles {
		if got := Title(tt.labels); got != tt.want {
			t.Errorf("Title(%v) = %q, want %q", tt.labels, got, tt.want)
		}
	}
}

func mode(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestStorePermissions(t *testing.T) {
	parent := t.TempDir()
	stateDir := filepath.Join(parent, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil { // #nosec G301 -- verifies tightening
		t.Fatal(err)
	}
	s, err := Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	it := newItem(t, key, 1)
	if err := s.Create(it, json.RawMessage(`{"fingerprint":"abc"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteRun(it.ID, Run{Number: 1, Reason: ReasonAuto, Outcome: OutcomeReady, QueuedAt: t0}); err != nil {
		t.Fatal(err)
	}
	l, err := s.TryLockInstance()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	dir := s.ItemDir(it.ID)
	for p, want := range map[string]os.FileMode{
		stateDir:                         0o700,
		s.ItemsDir():                     0o700,
		dir:                              0o700,
		filepath.Join(dir, "runs"):       0o700,
		filepath.Join(dir, "runs", "1"):  0o700,
		filepath.Join(dir, "item.yaml"):  0o600,
		filepath.Join(dir, "alert.json"): 0o600,
		filepath.Join(dir, ".lock"):      0o600,
		filepath.Join(dir, "runs", "1", "meta.yaml"): 0o600,
		filepath.Join(stateDir, "counters.yaml"):     0o600,
		filepath.Join(stateDir, "tower.lock"):        0o600,
	} {
		if got := mode(t, p); got != want {
			t.Errorf("%s: mode %v, want %v", p, got, want)
		}
	}
}

func TestCreateLoadAndRawAlert(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	raw := json.RawMessage(`{"fingerprint":"abc","labels":{}}`)
	if err := s.Create(it, raw); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(it, raw); err == nil {
		t.Fatal("creating over an existing item must fail")
	}
	l, err := s.Load(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(l.RawAlert) != string(raw) || l.Item.ID != it.ID || len(l.Runs) != 0 {
		t.Fatalf("loaded = %+v", l)
	}
	if _, err := os.Stat(filepath.Join(s.ItemDir(it.ID), "runs")); !os.IsNotExist(err) {
		t.Error("runs directory must not be created without a run")
	}
	if err := s.WriteRawAlert(it.ID, json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	l, _ = s.Load(it.ID)
	if string(l.RawAlert) != `{"x":1}` {
		t.Fatalf("raw = %s", l.RawAlert)
	}
	// Operations on nonexistent items never create directories.
	if err := s.WriteRawAlert("alert-dev-zzz-1", raw); err == nil {
		t.Fatal("want error for missing item")
	}
	if _, err := os.Stat(s.ItemDir("alert-dev-zzz-1")); !os.IsNotExist(err) {
		t.Fatal("item directory was created")
	}
}

func TestStampChangesOnWrite(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	a, err := s.Stamp(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendAction(it.ID, ActionOpenedReport, 0, t0); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Stamp(it.ID)
	if a == b {
		t.Fatal("stamp did not change after an atomic replacement")
	}
}

// TestHelperAppendActions is run as a subprocess by TestCrossProcessLocking.
func TestHelperAppendActions(t *testing.T) {
	dir := os.Getenv("TOWER_TEST_STATE_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	n, _ := strconv.Atoi(os.Getenv("TOWER_TEST_COUNT"))
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if _, err := s.AppendAction(os.Getenv("TOWER_TEST_ID"), ActionOpenedReport, 1000+i, t0); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConcurrentAppendActions(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	const goroutines, each = 8, 15
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			// Separate stores open separate lock descriptors, like separate
			// processes would.
			other, err := Open(s.Dir())
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = other.Close() }()
			for i := range each {
				if _, err := other.AppendAction(it.ID, ActionOpenedReport, g*100+i, t0); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	got, err := s.Get(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.History) != 1+goroutines*each {
		t.Fatalf("lost updates: %d entries", len(got.History))
	}
}

func TestCrossProcessLocking(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	const n = 30
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestHelperAppendActions$") // #nosec G204 G702 -- re-executes the test binary
	cmd.Env = append(os.Environ(), "TOWER_TEST_STATE_DIR="+s.Dir(), "TOWER_TEST_ID="+it.ID, "TOWER_TEST_COUNT="+strconv.Itoa(n))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if _, err := s.AppendAction(it.ID, ActionResumedSession, i, t0); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper: %v\n%s", err, out.String())
	}
	got, _ := s.Get(it.ID)
	if len(got.History) != 1+2*n {
		t.Fatalf("lost updates across processes: %d entries", len(got.History))
	}
}

func TestItemLockBlocksUpdate(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lockItem(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = s.AppendAction(it.ID, ActionOpenedReport, 1, t0)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("update did not wait for the item lock")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("update did not proceed after unlock")
	}
}

func TestAtomicReplacementFailure(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.ItemDir(it.ID), "item.yaml")
	before, _ := os.ReadFile(path) // #nosec G304 -- test file
	var sawTemp bool
	beforeRename = func(tmp, dst string) error {
		// The temporary file is complete and in the destination directory
		// while the old document is still intact.
		data, err := s.root.ReadFile(tmp)
		sawTemp = err == nil && strings.Contains(string(data), "opened-report") && filepath.Dir(tmp) == filepath.Dir(dst)
		cur, _ := os.ReadFile(path) // #nosec G304 -- test file
		if !bytes.Equal(cur, before) {
			t.Error("destination modified before rename")
		}
		return errors.New("simulated crash")
	}
	t.Cleanup(func() { beforeRename = nil })
	if _, err := s.AppendAction(it.ID, ActionOpenedReport, 1, t0); err == nil {
		t.Fatal("want failure")
	}
	beforeRename = nil
	if !sawTemp {
		t.Error("temporary file was not written completely")
	}
	after, _ := os.ReadFile(path) // #nosec G304 -- test file
	if !bytes.Equal(before, after) {
		t.Fatal("old document was modified by a failed replacement")
	}
	entries, _ := os.ReadDir(s.ItemDir(it.ID))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
	if _, err := s.Get(it.ID); err != nil {
		t.Fatal(err)
	}
}

func writeCorrupt(t *testing.T, s *Store, id string, content string) string {
	t.Helper()
	dir := s.ItemDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "item.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCorruptItemsSkipped(t *testing.T) {
	s := openStore(t)
	good := newItem(t, key, 1)
	if err := s.Create(good, nil); err != nil {
		t.Fatal(err)
	}
	corrupt := map[string]string{
		"alert-dev-abc-2": "{{{ not yaml",
		"alert-dev-def-1": "version: 1\nid: alert-dev-other-1\n",
		"alert-dev-ghi-1": "version: 1\nunknown_field: x\n",
		"alert-dev-jkl-1": "",
		"alert-dev-mno-1": "version: 1\nid: alert-dev-mno-1\ntype: alert\nstate: bogus\n",
	}
	contents := map[string][]byte{}
	for id, c := range corrupt {
		p := writeCorrupt(t, s, id, c)
		contents[p] = []byte(c)
	}
	// Directories with invalid names and stray files are ignored entirely.
	_ = os.MkdirAll(filepath.Join(s.ItemsDir(), "not-an-item"), 0o700)
	_ = os.WriteFile(filepath.Join(s.ItemsDir(), "alert-dev-file-1"), []byte("x"), 0o600)

	loaded, bad, err := s.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].Item.ID != good.ID || len(bad) != len(corrupt) {
		t.Fatalf("loaded=%d bad=%d", len(loaded), len(bad))
	}
	for _, b := range bad {
		if b.Path != s.ItemDir(b.ID) || b.Err == nil {
			t.Errorf("unreadable = %+v", b)
		}
	}
	// Creation must not overwrite a corrupt directory: its number is reserved.
	counters, err := s.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if counters[key] != 2 || counters[Key{"dev", "def"}] != 1 {
		t.Fatalf("counters = %v", counters)
	}
	if err := s.Create(newItem(t, key, 2), nil); err == nil {
		t.Fatal("creation over a corrupt directory must fail")
	}
	// Updates and pruning never touch them.
	if _, err := s.AppendAction("alert-dev-abc-2", ActionOpenedReport, 0, t0); err == nil {
		t.Fatal("update of corrupt item must fail")
	}
	if deleted, _ := s.PruneItem("alert-dev-abc-2", t0.Add(1000*time.Hour)); deleted {
		t.Fatal("corrupt item pruned")
	}
	cands, _ := s.PruneCandidates(t0.Add(1000 * time.Hour))
	if len(cands) != 0 {
		t.Fatalf("candidates = %v", cands)
	}
	for p, c := range contents {
		got, err := os.ReadFile(p) // #nosec G304 -- test file
		if err != nil || !bytes.Equal(got, c) {
			t.Errorf("%s changed", p)
		}
	}
}

func TestCorruptRunMakesItemUnreadable(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.ItemDir(it.ID), "runs", "1"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.ItemDir(it.ID), "runs", "1", "meta.yaml"), []byte("number: 2\nreason: auto\noutcome: ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, bad, _ := s.LoadAll(); len(bad) != 1 {
		t.Fatal("mismatched run number must make the item unreadable")
	}
}

func TestCountersSurvivePruneAndRestart(t *testing.T) {
	s := openStore(t)
	other := Key{Source: "prod", Fingerprint: "abc"}
	for n := 1; n <= 3; n++ {
		it := newItem(t, key, n)
		it.State = StateDone
		if err := s.Create(it, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Create(newItem(t, other, 1), nil); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 3; n++ {
		id, _ := NewID(key, n)
		deleted, err := s.PruneItem(id, t0.Add(time.Hour))
		if err != nil || !deleted {
			t.Fatalf("prune %s: %v %v", id, deleted, err)
		}
	}
	// Restart: a fresh store over the same directory.
	s2, err := Open(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	counters, err := s2.Counters()
	if err != nil {
		t.Fatal(err)
	}
	if counters[key] != 3 || counters[other] != 1 {
		t.Fatalf("counters = %v", counters)
	}
	// A counters read error is returned, never silently reset.
	if err := os.WriteFile(filepath.Join(s.Dir(), "counters.yaml"), []byte("{{bad"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Counters(); err == nil {
		t.Fatal("want counters read error")
	}
	if err := s2.Create(newItem(t, key, 4), nil); err == nil {
		t.Fatal("creation must fail when counters are unreadable")
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "counters.yaml"), []byte("dev/abc: 3\nbad key: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Counters(); err == nil {
		t.Fatal("want error for invalid counter key")
	}
}

// TestPruneSkipsItemsWithUnreadableRuns verifies that pruning uses the same
// readability rules as loading (review round 1, B1).
func TestPruneSkipsItemsWithUnreadableRuns(t *testing.T) {
	s := openStore(t)
	cutoff := t0.Add(time.Hour)
	it := newItem(t, key, 1)
	it.State = StateDone
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(s.ItemDir(it.ID), "runs", "1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	meta := filepath.Join(runDir, "meta.yaml")
	corrupt := []byte("{{not yaml")
	if err := os.WriteFile(meta, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, bad, err := s.LoadAll(); err != nil || len(bad) != 1 || bad[0].ID != it.ID {
		t.Fatalf("LoadAll must skip the item: bad=%v err=%v", bad, err)
	}
	cands, err := s.PruneCandidates(cutoff)
	if err != nil || len(cands) != 0 {
		t.Fatalf("candidates = %+v, err = %v", cands, err)
	}
	if deleted, err := s.PruneItem(it.ID, cutoff); deleted || err != nil {
		t.Fatalf("deleted=%v err=%v", deleted, err)
	}
	if got, err := os.ReadFile(meta); err != nil || !bytes.Equal(got, corrupt) {
		t.Fatalf("run metadata changed: %q %v", got, err)
	}
}

func TestPruneSelection(t *testing.T) {
	s := openStore(t)
	cutoff := t0.Add(time.Hour)
	mk := func(fp string, state State, updated time.Time) string {
		it := newItem(t, Key{"dev", fp}, 1)
		it.State = state
		it.UpdatedAt = updated
		it.CreatedAt = t0.Add(-1000 * time.Hour)
		if err := s.Create(it, nil); err != nil {
			t.Fatal(err)
		}
		return it.ID
	}
	old := mk("old", StateDone, cutoff.Add(-time.Second))
	equal := mk("equal", StateDone, cutoff)
	newer := mk("newer", StateDone, cutoff.Add(time.Second))
	notDone := mk("resolved", StateResolved, cutoff.Add(-time.Hour))
	cands, err := s.PruneCandidates(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 1 || cands[0].ID != old || cands[0].Path != s.ItemDir(old) {
		t.Fatalf("candidates = %+v", cands)
	}
	for _, id := range []string{equal, newer, notDone} {
		if deleted, err := s.PruneItem(id, cutoff); deleted || err != nil {
			t.Errorf("%s: deleted=%v err=%v", id, deleted, err)
		}
	}
	// Recheck under the lock: an item that is no longer done is kept even
	// if it was listed.
	if _, err := s.Update(old, func(it *Item) error { it.State = StateNew; return nil }); err != nil {
		t.Fatal(err)
	}
	if deleted, _ := s.PruneItem(old, cutoff); deleted {
		t.Fatal("stale candidate deleted")
	}
	if _, err := os.Stat(s.ItemDir(old)); err != nil {
		t.Fatal(err)
	}
}

func TestPruneContainment(t *testing.T) {
	s := openStore(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "alert-dev-abc-1")
	it := newItem(t, key, 1)
	it.State = StateDone
	data, _ := yaml.Marshal(it)
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "item.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlinked item directory pointing outside the items directory.
	if err := os.Symlink(victim, s.ItemDir(it.ID)); err != nil {
		t.Fatal(err)
	}
	ids, _ := s.List()
	if len(ids) != 0 {
		t.Fatalf("symlinked directory listed: %v", ids)
	}
	if deleted, _ := s.PruneItem(it.ID, t0.Add(time.Hour)); deleted {
		t.Fatal("symlink target deleted")
	}
	if _, err := os.Stat(filepath.Join(victim, "item.yaml")); err != nil {
		t.Fatal("outside directory was modified")
	}
	// Traversal IDs are rejected before touching the file system.
	for _, id := range []string{"../x", "alert-dev-abc-1/../../x", "/etc", "alert-dev-a.b-1"} {
		if deleted, err := s.PruneItem(id, t0.Add(time.Hour)); deleted || err == nil {
			t.Errorf("PruneItem(%q) = %v, %v", id, deleted, err)
		}
		if _, err := s.Get(id); err == nil {
			t.Errorf("Get(%q) must fail", id)
		}
	}
	// A state-directory-internal symlink inside the item directory must not
	// let item.yaml reads escape the root either.
	it2 := newItem(t, Key{"dev", "x"}, 1)
	if err := os.MkdirAll(s.ItemDir(it2.ID), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(victim, "item.yaml"), filepath.Join(s.ItemDir(it2.ID), "item.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(it2.ID); err == nil {
		t.Fatal("read escaped the state directory through a symlink")
	}
}

func TestInstanceLock(t *testing.T) {
	s := openStore(t)
	l, err := s.TryLockInstance()
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Open(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s2.Close() }()
	_, err = s2.TryLockInstance()
	if !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), "tower.lock") {
		t.Fatalf("err = %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	// The file still exists, but it is not ownership.
	if _, err := os.Stat(filepath.Join(s.Dir(), "tower.lock")); err != nil {
		t.Fatal(err)
	}
	l2, err := s2.TryLockInstance()
	if err != nil {
		t.Fatal(err)
	}
	_ = l2.Release()
}
