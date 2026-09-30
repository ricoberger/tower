package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/notify"
	"github.com/ricoberger/tower/internal/runner"
)

// runTest is an engine test environment with a file source "dev", the fake
// Copilot fixture and a fixture control directory.
type runTest struct {
	t        *testing.T
	dir      string
	cfgPath  string
	stateDir string
	alerts   string
	ctl      string
	cfg      *config.Config
	clock    *fakeClock
	t0       time.Time
}

// newRunTest writes the configuration (runs.concurrency, runs.timeout 20m,
// prepare_after 5m, resolved_linger 10m, plus extra top-level YAML) and
// sets the fixture mode to ready with the control directory.
func newRunTest(t *testing.T, concurrency int, extra string) *runTest {
	t.Helper()
	dir := t.TempDir()
	rt := &runTest{
		t: t, dir: dir, cfgPath: filepath.Join(dir, "config.yaml"), stateDir: filepath.Join(dir, "state"),
		alerts: filepath.Join(dir, "alerts.json"), ctl: filepath.Join(dir, "ctl"),
		t0: time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC),
	}
	rt.clock = &fakeClock{t: rt.t0}
	if err := os.Mkdir(rt.ctl, 0o700); err != nil {
		t.Fatal(err)
	}
	fixtureEnv(t, rt.stateDir, "ready")
	t.Setenv("FAKE_COPILOT_DIR", rt.ctl)
	content := fmt.Sprintf(`state_dir: ./state
alerts:
  prepare_after: 5m
  resolved_linger: 10m
runs:
  command: '%s'
  concurrency: %d
  timeout: 20m
sources:
  - name: dev
    type: file
    path: ./alerts.json
%s`, fakeCopilotPath(t), concurrency, extra)
	if err := os.WriteFile(rt.cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	rt.cfg = loadCfg(t, rt.cfgPath, nil)
	rt.setAlerts()
	return rt
}

// alert returns an active file-source alert.
func (rt *runTest) alert(fp, severity string, age time.Duration) string {
	return alertJSON(fp, "Alert"+fp, rt.t0.Add(-age), "active", map[string]string{"severity": severity})
}

func (rt *runTest) setAlerts(alerts ...string) {
	rt.t.Helper()
	writeFixture(rt.t, rt.alerts, "["+strings.Join(alerts, ",")+"]")
}

func (rt *runTest) start(mut func(*engineOptions)) *harness {
	rt.t.Helper()
	opts := engineOptions{cfg: rt.cfg, now: rt.clock.now, level: slog.LevelDebug}
	opts.runner.Grace = 200 * time.Millisecond
	if mut != nil {
		mut(&opts)
	}
	h := startEngine(rt.t, opts)
	h.wait(h.applied, "startup round")
	return h
}

func id(fp string) string { return "alert-dev-" + fp + "-1" }

func (rt *runTest) item(fp string) *item.Item { return readItem(rt.t, rt.stateDir, id(fp)) }

// runs reads the run metadata of an item.
func (rt *runTest) runs(fp string) []item.Run {
	rt.t.Helper()
	files, _ := filepath.Glob(filepath.Join(rt.stateDir, "items", id(fp), "runs", "*", "meta.yaml"))
	var out []item.Run
	for _, f := range files {
		data, err := os.ReadFile(f) // #nosec G304 -- test state
		if err != nil {
			continue
		}
		var r item.Run
		if err := yaml.Unmarshal(data, &r); err != nil {
			rt.t.Fatalf("%s: %v", f, err)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b item.Run) int { return a.Number - b.Number })
	return out
}

func (rt *runTest) run(fp string, n int) item.Run {
	rt.t.Helper()
	for _, r := range rt.runs(fp) {
		if r.Number == n {
			return r
		}
	}
	rt.t.Fatalf("%s has no run %d", fp, n)
	return item.Run{}
}

func (rt *runTest) runFile(fp string, n int, name string) string {
	return filepath.Join(rt.stateDir, "items", id(fp), "runs", strconv.Itoa(n), name)
}

func (rt *runTest) touch(name string) {
	rt.t.Helper()
	if err := os.WriteFile(filepath.Join(rt.ctl, name), nil, 0o600); err != nil {
		rt.t.Fatal(err)
	}
}

func (rt *runTest) hold()             { rt.touch("hold") }
func (rt *runTest) release(fp string) { rt.touch("release." + id(fp)) }
func (rt *runTest) mode(fp, m string) { rt.writeCtl("mode."+id(fp), m) }

func (rt *runTest) writeCtl(name, content string) {
	rt.t.Helper()
	if err := os.WriteFile(filepath.Join(rt.ctl, name), []byte(content), 0o600); err != nil {
		rt.t.Fatal(err)
	}
}

// log returns the fixture's start/end lines.
func (rt *runTest) log() []string {
	data, _ := os.ReadFile(filepath.Join(rt.ctl, "log")) // #nosec G304 -- test file
	return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(data)), " ", "_"))
}

func (rt *runTest) starts() []string {
	var out []string
	for _, l := range rt.log() {
		if s, ok := strings.CutPrefix(l, "start_"); ok {
			out = append(out, s)
		}
	}
	return out
}

func (rt *runTest) state(fp string) item.State {
	if it := rt.item(fp); it != nil {
		return it.State
	}
	return ""
}

func (rt *runTest) waitState(fp string, s item.State) {
	rt.t.Helper()
	eventually(rt.t, fp+" "+string(s), func() bool { return rt.state(fp) == s })
}

func (rt *runTest) waitOutcome(fp string, n int, o item.Outcome) item.Run {
	rt.t.Helper()
	var got item.Run
	eventually(rt.t, fmt.Sprintf("%s run %d %s", fp, n, o), func() bool {
		for _, r := range rt.runs(fp) {
			if r.Number == n {
				got = r
				return r.Outcome == o
			}
		}
		return false
	})
	return got
}

// wrapperPID waits for the persisted wrapper PID of a run.
func (rt *runTest) wrapperPID(fp string, n int) int {
	rt.t.Helper()
	var pid int
	eventually(rt.t, "wrapper pid", func() bool {
		for _, r := range rt.runs(fp) {
			if r.Number == n && r.PID > 0 {
				pid = r.PID
				return true
			}
		}
		return false
	})
	return pid
}

// fixtureStarted waits until the fixture of a run is executing (its pid
// evidence is written).
func (rt *runTest) fixtureStarted(fp string, n int) {
	rt.t.Helper()
	eventually(rt.t, "fixture started", func() bool {
		data, err := os.ReadFile(rt.runFile(fp, n, "fake-pgid")) // #nosec G304 -- test state
		return err == nil && len(strings.TrimSpace(string(data))) > 0
	})
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ownedProcess starts a live process in its own group whose command line
// contains the session ID, like a detached run wrapper. It is reaped and its
// group killed when the test ends; kill ends it early.
func ownedProcess(t *testing.T, sid string) (pid int, kill func()) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "sleep 30; : "+sid) // #nosec G204 -- test process
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	kill = func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	t.Cleanup(kill)
	return cmd.Process.Pid, kill
}

// seed creates an item with the given state and runs in the state
// directory (while no engine runs).
func (rt *runTest) seed(fp string, state item.State, current int, pending *item.RunReason, runs ...item.Run) string {
	rt.t.Helper()
	st, err := item.Open(rt.stateDir)
	if err != nil {
		rt.t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	k := item.Key{Source: "dev", Fingerprint: fp}
	iid, _ := item.NewID(k, 1)
	at := rt.t0.Add(-time.Hour)
	base := 0
	status := item.AlertActive
	switch state {
	case item.StateSnoozed:
		status = item.AlertSuppressed
	case item.StateResolved, item.StateDone:
		status = item.AlertResolved
	}
	it := &item.Item{
		Version: item.Version, ID: iid, Type: item.TypeAlert, State: state, Title: "Seeded " + fp, Severity: "critical",
		CreatedAt: at, UpdatedAt: at,
		Source: item.SourceRef{Name: "dev", Fingerprint: fp},
		Alert: item.AlertInfo{
			StartsAt: at, LastSeenAt: at, Status: status, Occurrences: 1,
			Labels: map[string]string{"alertname": "Seeded", "severity": "critical"},
		},
		Runs:    item.RunsInfo{Current: current, PendingReason: pending, OccurrenceBase: &base},
		History: []item.HistoryEntry{{At: at, To: state, Reason: "seed"}},
	}
	if status == item.AlertResolved {
		it.Alert.ResolvedAt = &at
	}
	if err := st.Create(it, []byte(alertJSON(fp, "Seeded", at, "active", nil))); err != nil {
		rt.t.Fatal(err)
	}
	for _, r := range runs {
		if err := st.CreateRun(iid, r); err != nil {
			rt.t.Fatal(err)
		}
	}
	return iid
}

func runningRun(n int, pid int, sid string, reason item.RunReason, started time.Time) item.Run {
	return item.Run{
		Number: n, PID: pid, SessionID: sid, Reason: reason, Skill: runner.Skill,
		QueuedAt: started, StartedAt: &started, Outcome: item.OutcomeRunning,
	}
}

func waitDelivery(t *testing.T, h *harness) notify.Notification {
	t.Helper()
	select {
	case n := <-h.deliveries.ch:
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a notification")
	}
	return notify.Notification{}
}

func noDelivery(t *testing.T, h *harness) {
	t.Helper()
	select {
	case n := <-h.deliveries.ch:
		t.Fatalf("unexpected notification %+v", n)
	case <-time.After(300 * time.Millisecond):
	}
}

// AC1: the headless ready flow.
func TestRunReadyFlow(t *testing.T) {
	rt := newRunTest(t, 2, "")
	rt.setAlerts(rt.alert("aaa", "critical", time.Hour))
	h := rt.start(nil)

	rt.waitState("aaa", item.StateNeedsYou)
	n := waitDelivery(t, h)
	it := rt.item("aaa")
	if n.ItemID != id("aaa") || n.Outcome != item.OutcomeReady || n.Subtitle != it.Title || n.Message != "Fake summary of the alert." || n.Title() != notify.TitleReady {
		t.Errorf("notification = %+v", n)
	}
	runs := rt.runs("aaa")
	if len(runs) != 1 || runs[0].Outcome != item.OutcomeReady || it.Runs.Current != 1 || runs[0].Reason != item.ReasonAuto ||
		runs[0].ExitCode == nil || *runs[0].ExitCode != 0 || runs[0].FinishedAt == nil || runs[0].Error != "" {
		t.Fatalf("runs = %+v item = %+v", runs, it.Runs)
	}
	for _, f := range []string{"alert.json", "alert.md", "item.yaml"} {
		if _, err := os.Stat(filepath.Join(rt.stateDir, "items", id("aaa"), f)); err != nil {
			t.Error(err)
		}
	}
	for _, f := range []string{"meta.yaml", "prompt.md", "output.jsonl", "stderr.log", "exit_code", "report.md", "result.json"} {
		fi, err := os.Stat(rt.runFile("aaa", 1, f))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", f, err, fi)
		}
	}
	if fi := fileStat(t, filepath.Dir(rt.runFile("aaa", 1, "x"))); fi.Mode().Perm() != 0o700 {
		t.Errorf("run dir mode %v", fi.Mode().Perm())
	}

	// Unchanged polls and ticks start nothing else.
	for range 3 {
		h.send(h.poll)
		h.wait(h.applied, "round")
		h.doTick()
	}
	time.Sleep(100 * time.Millisecond)
	if len(rt.runs("aaa")) != 1 || len(rt.starts()) != 1 || rt.state("aaa") != item.StateNeedsYou {
		t.Fatalf("runs = %+v starts = %v", rt.runs("aaa"), rt.starts())
	}
	noDelivery(t, h)
}

// AC2: an alert younger than prepare_after is not started until then.
func TestRunThreshold(t *testing.T) {
	rt := newRunTest(t, 2, "")
	rt.setAlerts(rt.alert("young", "critical", time.Minute))
	h := rt.start(nil)
	h.doTick()
	if s := rt.state("young"); s != item.StateNew {
		t.Fatalf("state = %s", s)
	}
	if _, err := os.Stat(filepath.Join(rt.stateDir, "items", id("young"), "runs")); !os.IsNotExist(err) {
		t.Fatal("run directory created before the threshold")
	}
	rt.clock.add(4 * time.Minute)
	h.doTick()
	rt.waitState("young", item.StateNeedsYou)
	if len(rt.starts()) != 1 {
		t.Fatalf("starts = %v", rt.starts())
	}
}

// AC3 and priority: concurrency limit and severity order.
func TestRunConcurrencyAndSeverity(t *testing.T) {
	rt := newRunTest(t, 2, "")
	rt.hold()
	rt.setAlerts(
		rt.alert("warna", "warning", 4*time.Hour),
		rt.alert("warnb", "warning", 3*time.Hour),
		rt.alert("crita", "critical", 2*time.Hour),
		rt.alert("critb", "critical", time.Hour),
	)
	rt.start(nil)

	eventually(t, "two starts", func() bool { return len(rt.starts()) == 2 })
	time.Sleep(300 * time.Millisecond)
	first := rt.starts()
	slices.Sort(first)
	if len(first) != 2 || first[0] != id("crita")+"_1" || first[1] != id("critb")+"_1" {
		t.Fatalf("first starts = %v", rt.starts())
	}
	if rt.state("warna") != item.StateQueued || rt.state("warnb") != item.StateQueued {
		t.Fatal("warnings not queued")
	}

	// The next free slot goes to the oldest warning.
	rt.release("crita")
	eventually(t, "third start", func() bool { return len(rt.starts()) == 3 })
	time.Sleep(200 * time.Millisecond)
	if s := rt.starts(); len(s) != 3 || s[2] != id("warna")+"_1" {
		t.Fatalf("starts = %v", s)
	}
	rt.touch("release.all")
	for _, fp := range []string{"warna", "warnb", "crita", "critb"} {
		rt.waitState(fp, item.StateNeedsYou)
	}

	// Never more than two executions at a time.
	active, most := 0, 0
	for _, l := range rt.log() {
		if strings.HasPrefix(l, "start_") {
			active++
		} else {
			active--
		}
		most = max(most, active)
	}
	if most > 2 || len(rt.starts()) != 4 {
		t.Fatalf("max concurrency %d, log %v", most, rt.log())
	}
}

// AC4: shutdown leaves the run alive; a new instance re-attaches it without
// a new process, counts it and processes its completion.
func TestRunRestartReattach(t *testing.T) {
	rt := newRunTest(t, 1, "")
	rt.hold()
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
	h := rt.start(nil)
	rt.waitState("aaa", item.StatePreparing)
	pid := rt.wrapperPID("aaa", 1)
	rt.fixtureStarted("aaa", 1)
	sid := rt.run("aaa", 1).SessionID
	h.stop()
	if !pidAlive(pid) {
		t.Fatal("shutdown killed the run")
	}

	// A second eligible alert waits for the re-attached run's slot.
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour), rt.alert("bbb", "critical", time.Hour))
	h2 := rt.start(nil)
	h2.doTick()
	time.Sleep(200 * time.Millisecond)
	if s := rt.state("bbb"); s != item.StateQueued {
		t.Fatalf("bbb = %s while the re-attached run occupies the slot", s)
	}
	if !strings.Contains(h2.stderr.String(), "run re-attached") || len(rt.starts()) != 1 {
		t.Fatalf("not re-attached: starts=%v\n%s", rt.starts(), h2.stderr.String())
	}
	if r := rt.run("aaa", 1); r.PID != pid || r.SessionID != sid || r.Outcome != item.OutcomeRunning {
		t.Fatalf("run = %+v", r)
	}

	rt.release("aaa")
	rt.waitState("aaa", item.StateNeedsYou)
	if r := rt.run("aaa", 1); r.Outcome != item.OutcomeReady || r.ExitCode == nil || *r.ExitCode != 0 {
		t.Fatalf("run = %+v", r)
	}
	if n := waitDelivery(t, h2); n.ItemID != id("aaa") {
		t.Fatalf("notification = %+v", n)
	}
	// The slot is released: bbb starts.
	rt.release("bbb")
	rt.waitState("bbb", item.StateNeedsYou)
	if len(rt.runs("aaa")) != 1 {
		t.Fatal("duplicate run")
	}
}

// A run that finished while tower was stopped completes on restart; a
// further restart does not notify again.
func TestRunStartupCompleted(t *testing.T) {
	rt := newRunTest(t, 1, "")
	rt.hold()
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
	h := rt.start(nil)
	rt.waitState("aaa", item.StatePreparing)
	pid := rt.wrapperPID("aaa", 1)
	h.stop()
	rt.release("aaa")
	eventually(t, "exit_code", func() bool {
		_, err := os.Stat(rt.runFile("aaa", 1, "exit_code"))
		return err == nil && !pidAlive(pid)
	})

	h2 := rt.start(nil)
	rt.waitState("aaa", item.StateNeedsYou)
	if r := rt.run("aaa", 1); r.Outcome != item.OutcomeReady {
		t.Fatalf("run = %+v", r)
	}
	waitDelivery(t, h2)
	h2.stop()

	h3 := rt.start(nil)
	h3.doTick()
	noDelivery(t, h3)
	if rt.state("aaa") != item.StateNeedsYou || len(rt.runs("aaa")) != 1 {
		t.Fatal("restart changed the item")
	}
}

// AC5: an interrupted run is retried once, the queued retry survives a
// restart without duplication, and an interrupted retry needs the user.
func TestRunInterruptionRetry(t *testing.T) {
	rt := newRunTest(t, 2, "")
	rt.hold()
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour), rt.alert("ccc", "critical", time.Hour))
	h := rt.start(nil)
	rt.waitState("aaa", item.StatePreparing)
	rt.waitState("ccc", item.StatePreparing)
	pidA := rt.wrapperPID("aaa", 1)
	rt.fixtureStarted("aaa", 1)
	rt.fixtureStarted("ccc", 1)
	h.stop()
	_ = syscall.Kill(-pidA, syscall.SIGKILL)
	eventually(t, "run killed", func() bool { return !pidAlive(pidA) })

	// With one slot occupied by the re-attached ccc run, the retry stays
	// queued.
	rt.cfg.Runs.Concurrency = 1
	h2 := rt.start(nil)
	rt.waitState("aaa", item.StateQueued)
	r1 := rt.run("aaa", 1)
	if r1.Outcome != item.OutcomeInterrupted || !strings.HasPrefix(r1.Error, "interrupted while tower was stopped") || r1.FinishedAt == nil {
		t.Fatalf("run 1 = %+v", r1)
	}
	it := rt.item("aaa")
	if it.Runs.PendingReason == nil || *it.Runs.PendingReason != item.ReasonRetry || it.Runs.Current != 1 {
		t.Fatalf("item = %+v", it.Runs)
	}
	history := len(it.History)
	h2.stop()

	h3 := rt.start(nil)
	h3.doTick()
	if it := rt.item("aaa"); it.State != item.StateQueued || len(it.History) != history || len(rt.runs("aaa")) != 1 {
		t.Fatalf("restart duplicated the retry: %+v %+v", it.History, rt.runs("aaa"))
	}

	// Freeing the slot starts the retry.
	rt.release("ccc")
	rt.waitState("aaa", item.StatePreparing)
	r2 := rt.run("aaa", 2)
	if r2.Reason != item.ReasonRetry || r2.RetryOf != 1 || r2.SessionID == r1.SessionID {
		t.Fatalf("retry = %+v", r2)
	}
	pid2 := rt.wrapperPID("aaa", 2)
	rt.fixtureStarted("aaa", 2)
	waitDelivery(t, h3) // ccc
	h3.stop()
	_ = syscall.Kill(-pid2, syscall.SIGKILL)
	eventually(t, "retry killed", func() bool { return !pidAlive(pid2) })

	h4 := rt.start(nil)
	rt.waitState("aaa", item.StateNeedsYou)
	h4.doTick()
	if r := rt.run("aaa", 2); r.Outcome != item.OutcomeInterrupted || r.Error == "" {
		t.Fatalf("retry = %+v", r)
	}
	if len(rt.runs("aaa")) != 2 || rt.item("aaa").Runs.Current != 2 {
		t.Fatal("a third attempt was made")
	}
	noDelivery(t, h4)
}

// A live unrelated process with the stored PID is neither re-attached nor
// signalled.
func TestRunPIDReuse(t *testing.T) {
	rt := newRunTest(t, 1, "")
	rt.hold()
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
	other, _ := ownedProcess(t, "unrelated")
	sid := "0f3d2c4b-1a2b-4c3d-8e9f-0123456789ab"
	rt.seed("aaa", item.StatePreparing, 1, nil, runningRun(1, other, sid, item.ReasonAuto, rt.t0.Add(-time.Minute)))

	rt.start(nil)
	rt.waitOutcome("aaa", 1, item.OutcomeInterrupted)
	if r := rt.run("aaa", 1); !strings.Contains(r.Error, "could not be verified") {
		t.Fatalf("run = %+v", r)
	}
	// The retry starts; the unrelated process is untouched.
	eventually(t, "retry run", func() bool { return len(rt.runs("aaa")) == 2 && rt.item("aaa").Runs.Current == 2 })
	rt.waitState("aaa", item.StatePreparing)
	if r := rt.run("aaa", 2); r.RetryOf != 1 {
		t.Fatalf("retry = %+v", r)
	}
	if !pidAlive(other) {
		t.Fatal("unrelated process was signalled")
	}
}

// Partial starts: run metadata ahead of runs.current is recovered before
// scheduling, without duplicates; unidentifiable evidence is preserved.
func TestRunPartialStartRecovery(t *testing.T) {
	rt := newRunTest(t, 2, "")
	rt.hold()
	auto := item.ReasonAuto
	sid := "1f3d2c4b-1a2b-4c3d-8e9f-0123456789ab"
	pid, kill := ownedProcess(t, sid)
	rt.seed("live", item.StateQueued, 0, &auto, runningRun(1, pid, sid, item.ReasonAuto, rt.t0.Add(-time.Minute)))
	rt.seed("dead", item.StateQueued, 0, &auto, runningRun(1, 0, "", item.ReasonAuto, rt.t0.Add(-time.Minute)))
	// The source fails: recovery does not depend on it.
	writeFixture(t, rt.alerts, "not json")

	h := rt.start(nil)
	rt.waitState("live", item.StatePreparing)
	if it := rt.item("live"); it.Runs.Current != 1 || it.Runs.PendingReason != nil {
		t.Fatalf("live = %+v", it.Runs)
	}
	if r := rt.run("live", 1); r.PID != pid || r.Outcome != item.OutcomeRunning {
		t.Fatalf("live run = %+v", r)
	}
	// The unidentifiable start is interrupted and retried with run 2.
	eventually(t, "dead retry", func() bool { return len(rt.runs("dead")) == 2 })
	if r := rt.run("dead", 1); r.Outcome != item.OutcomeInterrupted || !strings.Contains(r.Error, "no process") {
		t.Fatalf("dead run 1 = %+v", r)
	}
	if r := rt.run("dead", 2); r.RetryOf != 1 || r.Reason != item.ReasonRetry {
		t.Fatalf("dead run 2 = %+v", r)
	}
	for _, s := range rt.starts() {
		if strings.HasPrefix(s, id("live")) {
			t.Fatal("duplicate process for the live start")
		}
	}
	h.stop()

	// Idempotent on restart.
	h2 := rt.start(nil)
	h2.doTick()
	if len(rt.runs("live")) != 1 || len(rt.runs("dead")) != 2 || rt.item("live").Runs.Current != 1 || rt.item("dead").Runs.Current != 2 {
		t.Fatalf("restart changed recovery: %+v %+v", rt.runs("live"), rt.runs("dead"))
	}

	// PID death without exit_code completes the re-attached run from its
	// (missing) artifacts.
	kill()
	r := rt.waitOutcome("live", 1, item.OutcomeFailed)
	if r.ExitCode != nil || r.Error != "result.json is missing" {
		t.Fatalf("live run = %+v", r)
	}
	rt.waitState("live", item.StateNeedsYou)
}

// Recovery of runs of snoozed, resolved and done items.
func TestRunRecoveryNonPreparing(t *testing.T) {
	rt := newRunTest(t, 1, "")
	sidS := "2f3d2c4b-1a2b-4c3d-8e9f-0123456789ab"
	sidD := "3f3d2c4b-1a2b-4c3d-8e9f-0123456789ab"
	pidS, killS := ownedProcess(t, sidS)
	pidD, _ := ownedProcess(t, sidD)
	started := rt.t0.Add(-time.Minute)
	rt.seed("snoozed", item.StateSnoozed, 1, nil, runningRun(1, pidS, sidS, item.ReasonAuto, started))
	rt.seed("resolved", item.StateResolved, 1, nil, runningRun(1, 999999, "4f3d2c4b-1a2b-4c3d-8e9f-0123456789ab", item.ReasonAuto, started))
	rt.seed("done", item.StateDone, 1, nil, runningRun(1, pidD, sidD, item.ReasonAuto, started))
	manual := item.ReasonManual
	rt.seed("queued", item.StateQueued, 0, &manual)
	writeFixture(t, rt.alerts, "not json") // frozen source

	h := rt.start(nil)
	// The done item's verified group is cancelled without revival.
	rt.waitOutcome("done", 1, item.OutcomeCancelled)
	eventually(t, "done group terminated", func() bool { return !pidAlive(pidD) })
	// The resolved item's dead run is interrupted without a retry.
	rt.waitOutcome("resolved", 1, item.OutcomeInterrupted)
	h.doTick()
	if rt.state("done") != item.StateDone || rt.state("resolved") != item.StateResolved || len(rt.runs("resolved")) != 1 {
		t.Fatal("recovery revived or retried a non-preparing item")
	}
	// The snoozed item's live run occupies the only slot.
	time.Sleep(200 * time.Millisecond)
	if rt.state("queued") != item.StateQueued {
		t.Fatal("queued item started although the re-attached run occupies the slot")
	}
	killS()
	rt.waitOutcome("snoozed", 1, item.OutcomeFailed)
	rt.waitState("queued", item.StateNeedsYou)
	if rt.state("snoozed") != item.StateSnoozed {
		t.Fatal("snoozed item revived")
	}
	// Only the queued item's completion notifies.
	if n := waitDelivery(t, h); n.ItemID != id("queued") {
		t.Fatalf("notification = %+v", n)
	}
	noDelivery(t, h)
}

// AC6: timeout of a locally started run with a TERM-ignoring descendant.
func TestRunTimeout(t *testing.T) {
	rt := newRunTest(t, 1, "")
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
	rt.mode("aaa", "hang")
	t.Setenv("FAKE_COPILOT_DESCENDANT", "1")
	h := rt.start(nil)
	rt.waitState("aaa", item.StatePreparing)
	pid := rt.wrapperPID("aaa", 1)
	rt.fixtureStarted("aaa", 1)
	eventually(t, "descendant", func() bool { _, err := os.Stat(rt.runFile("aaa", 1, "fake-descendant")); return err == nil })

	rt.clock.add(20*time.Minute + time.Second)
	eventually(t, "TERM", func() bool { return strings.Contains(h.stderr.String(), "run timed out") })
	rt.clock.add(time.Second) // past the grace: KILL
	r := rt.waitOutcome("aaa", 1, item.OutcomeFailed)
	if r.Error != "timeout after 20m" {
		t.Fatalf("run = %+v", r)
	}
	rt.waitState("aaa", item.StateNeedsYou)
	// The TERM-ignoring descendant is killed on a monitor pass after the grace.
	eventually(t, "process group killed", func() bool { return syscall.Kill(-pid, 0) != nil })
	if n := waitDelivery(t, h); n.Outcome != item.OutcomeFailed || n.Message != "timeout after 20m" || n.Title() != notify.TitleFailed {
		t.Fatalf("notification = %+v", n)
	}
}

// Cancellation of queued and running work through dismissal and resolved
// linger expiry.
func TestRunCancellation(t *testing.T) {
	rt := newRunTest(t, 1, "")
	rt.hold()
	rt.setAlerts(rt.alert("aaa", "critical", 3*time.Hour), rt.alert("bbb", "critical", 2*time.Hour), rt.alert("ccc", "warning", time.Hour))
	h := rt.start(nil)
	rt.waitState("aaa", item.StatePreparing)
	pidA := rt.wrapperPID("aaa", 1)
	rt.fixtureStarted("aaa", 1)
	ctx := t.Context()

	// Queued cancellation: no process, no run directory.
	if err := h.api.Dismiss(ctx, id("bbb")); err != nil {
		t.Fatal(err)
	}
	if rt.state("bbb") != item.StateDone {
		t.Fatal("bbb not dismissed")
	}
	if err := h.api.Dismiss(ctx, id("bbb")); !errors.Is(err, ErrItemDone) {
		t.Fatalf("repeated dismissal = %v", err)
	}

	// Running cancellation by dismissal: the group is terminated, the run
	// cancelled without notification, and the slot is then used by ccc.
	if err := h.api.Dismiss(ctx, id("aaa")); err != nil {
		t.Fatal(err)
	}
	r := rt.waitOutcome("aaa", 1, item.OutcomeCancelled)
	if r.Error != runner.ErrCancelled || rt.state("aaa") != item.StateDone {
		t.Fatalf("run = %+v", r)
	}
	eventually(t, "group gone", func() bool { return syscall.Kill(-pidA, 0) != nil })
	rt.waitState("ccc", item.StatePreparing)
	if _, err := os.Stat(filepath.Join(rt.stateDir, "items", id("bbb"), "runs")); !os.IsNotExist(err) {
		t.Fatal("cancelled queued work created a run")
	}

	// Resolved-linger cancellation: ccc resolves while running (the run
	// continues) and is cancelled once the linger expires.
	rt.fixtureStarted("ccc", 1)
	rt.setAlerts(rt.alert("aaa", "critical", 3*time.Hour), rt.alert("bbb", "critical", 2*time.Hour))
	h.send(h.poll)
	h.wait(h.applied, "round")
	rt.waitState("ccc", item.StateResolved)
	if r := rt.run("ccc", 1); r.Outcome != item.OutcomeRunning {
		t.Fatalf("resolution cancelled the run: %+v", r)
	}
	rt.clock.add(11 * time.Minute)
	h.doTick()
	rt.waitOutcome("ccc", 1, item.OutcomeCancelled)
	rt.waitState("ccc", item.StateDone)
	noDelivery(t, h)
	if len(rt.starts()) != 2 {
		t.Fatalf("starts = %v", rt.starts())
	}
}

// AC10: suppressed alerts do not start; newly snoozed queued work is
// removed; executing work finishes without notification.
func TestRunSnoozeResolve(t *testing.T) {
	rt := newRunTest(t, 1, "")
	rt.hold()
	silenced := alertJSON("sss", "Silenced", rt.t0.Add(-4*time.Hour), "suppressed", nil, "silence-1")
	rt.setAlerts(silenced, rt.alert("aaa", "critical", 3*time.Hour), rt.alert("bbb", "critical", 2*time.Hour))
	h := rt.start(nil)
	rt.waitState("aaa", item.StatePreparing)
	rt.fixtureStarted("aaa", 1)
	if rt.state("sss") != item.StateSnoozed || rt.state("bbb") != item.StateQueued {
		t.Fatalf("sss=%s bbb=%s", rt.state("sss"), rt.state("bbb"))
	}

	// bbb is silenced while queued; aaa resolves while executing.
	rt.setAlerts(silenced, alertJSON("bbb", "Alertbbb", rt.t0.Add(-2*time.Hour), "suppressed", nil, "silence-2"))
	h.send(h.poll)
	h.wait(h.applied, "round")
	if rt.state("bbb") != item.StateSnoozed || rt.state("aaa") != item.StateResolved {
		t.Fatalf("bbb=%s aaa=%s\n%s", rt.state("bbb"), rt.state("aaa"), h.stderr.String())
	}
	rt.release("aaa")
	rt.waitOutcome("aaa", 1, item.OutcomeReady)
	h.doTick()
	time.Sleep(200 * time.Millisecond)
	if rt.state("aaa") != item.StateResolved || len(rt.starts()) != 1 {
		t.Fatalf("aaa=%s starts=%v", rt.state("aaa"), rt.starts())
	}
	noDelivery(t, h)
}

// A failed source does not stop completion or manual requests.
func TestRunSourceFailureIndependence(t *testing.T) {
	rt := newRunTest(t, 2, "")
	rt.hold()
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
	h := rt.start(nil)
	rt.waitState("aaa", item.StatePreparing)
	writeFixture(t, rt.alerts, "not json")
	h.send(h.poll)
	h.wait(h.applied, "failed round")
	rt.release("aaa")
	rt.waitState("aaa", item.StateNeedsYou)
	waitDelivery(t, h)

	rt.touch("release.all")
	if err := h.api.ManualRun(t.Context(), id("aaa")); err != nil {
		t.Fatal(err)
	}
	rt.waitOutcome("aaa", 2, item.OutcomeReady)
	rt.waitState("aaa", item.StateNeedsYou)
}

// API requests that cannot be reconciled report an error and change
// nothing.
func TestRunAPIReconcileFailure(t *testing.T) {
	rt := newRunTest(t, 2, "")
	rt.setAlerts(rt.alert("young", "critical", time.Minute))
	h := rt.start(nil)
	rt.waitState("young", item.StateNew)
	counters := filepath.Join(rt.stateDir, "counters.yaml")
	orig, err := os.ReadFile(counters) // #nosec G304 -- test state
	if err != nil {
		t.Fatal(err)
	}
	writeFixture(t, counters, "{not yaml")
	before := len(rt.item("young").History)
	ctx := t.Context()
	for name, call := range map[string]func() error{
		"manual run": func() error { return h.api.ManualRun(ctx, id("young")) },
		"dismiss":    func() error { return h.api.Dismiss(ctx, id("young")) },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "counters") {
			t.Fatalf("%s = %v", name, err)
		}
		if it := rt.item("young"); it.State != item.StateNew || len(it.History) != before || it.Runs.PendingReason != nil {
			t.Fatalf("%s changed the item: %+v", name, it)
		}
	}
	if len(rt.runs("young")) != 0 || len(rt.starts()) != 0 {
		t.Fatal("a failed request started work")
	}
	// Once the counters are readable again, the request is accepted.
	writeFixture(t, counters, string(orig))
	if err := h.api.ManualRun(ctx, id("young")); err != nil {
		t.Fatal(err)
	}
	rt.waitState("young", item.StateNeedsYou)
}

// AC12: the manual run API.
func TestRunManualAPI(t *testing.T) {
	rt := newRunTest(t, 2, "")
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour), rt.alert("bbb", "critical", time.Hour))
	h := rt.start(nil)
	rt.waitState("aaa", item.StateNeedsYou)
	rt.waitState("bbb", item.StateNeedsYou)
	waitDelivery(t, h)
	waitDelivery(t, h)
	ctx := t.Context()

	// Rejections leave no trace.
	if err := h.api.ManualRun(ctx, "alert-dev-unknown-1"); !errors.Is(err, ErrUnknownItem) {
		t.Fatalf("unknown = %v", err)
	}
	if err := h.api.ManualRun(ctx, "../etc"); !errors.Is(err, ErrUnknownItem) {
		t.Fatalf("invalid = %v", err)
	}

	// A second run of a needs-you item, independent of prepare_after,
	// referencing the first report.
	rt.hold()
	before := len(rt.item("aaa").History)
	if err := h.api.ManualRun(ctx, id("aaa")); err != nil {
		t.Fatal(err)
	}
	rt.waitState("aaa", item.StatePreparing)
	it := rt.item("aaa")
	var action *item.HistoryEntry
	for i := range it.History[before:] {
		if h := it.History[before+i]; h.Action == item.ActionManualRun {
			action = &h
		}
	}
	if action == nil || action.Run != 2 {
		t.Fatalf("history = %+v", it.History[before:])
	}
	r2 := rt.run("aaa", 2)
	if r2.Reason != item.ReasonManual || it.Seen {
		t.Fatalf("run 2 = %+v seen=%v", r2, it.Seen)
	}
	prompt := readString(t, rt.runFile("aaa", 2, "prompt.md"))
	if !strings.Contains(prompt, rt.runFile("aaa", 1, "report.md")) {
		t.Fatal("prompt does not reference the first report")
	}

	// Rejected while preparing and while done; nothing changes.
	var notAllowed *ManualRunNotAllowedError
	if err := h.api.ManualRun(ctx, id("aaa")); !errors.As(err, &notAllowed) || notAllowed.State != item.StatePreparing {
		t.Fatalf("preparing = %v", err)
	}
	history := len(rt.item("aaa").History)
	rt.release("aaa")
	rt.waitState("aaa", item.StateNeedsYou)
	waitDelivery(t, h)

	// Snoozed with a still-executing run: rejected.
	rt.mode("bbb", "hang")
	if err := h.api.ManualRun(ctx, id("bbb")); err != nil {
		t.Fatal(err)
	}
	rt.waitState("bbb", item.StatePreparing)
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour),
		alertJSON("bbb", "Alertbbb", rt.t0.Add(-time.Hour), "suppressed", map[string]string{"severity": "critical"}, "silence-1"))
	h.send(h.poll)
	h.wait(h.applied, "round")
	rt.waitState("bbb", item.StateSnoozed)
	bh := len(rt.item("bbb").History)
	if err := h.api.ManualRun(ctx, id("bbb")); !errors.Is(err, ErrRunExecuting) {
		t.Fatalf("executing = %v", err)
	}
	if len(rt.item("bbb").History) != bh {
		t.Fatal("rejection changed the item")
	}
	if err := h.api.Dismiss(ctx, id("bbb")); err != nil {
		t.Fatal(err)
	}
	rt.waitOutcome("bbb", 2, item.OutcomeCancelled)
	if err := h.api.ManualRun(ctx, id("bbb")); !errors.As(err, &notAllowed) || notAllowed.State != item.StateDone {
		t.Fatalf("done = %v", err)
	}
	if len(rt.item("aaa").History) <= history {
		t.Fatal("history not advanced by the run")
	}

	// A manual request from resolved survives the unchanged snapshot.
	rt.setAlerts()
	h.send(h.poll)
	h.wait(h.applied, "round")
	rt.waitState("aaa", item.StateResolved)
	if err := h.api.ManualRun(ctx, id("aaa")); err != nil {
		t.Fatal(err)
	}
	rt.release("aaa")
	rt.waitOutcome("aaa", 3, item.OutcomeReady)
	rt.waitState("aaa", item.StateNeedsYou)
	h.send(h.poll)
	h.wait(h.applied, "round")
	h.doTick()
	if it := rt.item("aaa"); it.State != item.StateNeedsYou || it.Alert.Status != item.AlertResolved {
		t.Fatalf("aaa = %s %s", it.State, it.Alert.Status)
	}
	if err := h.api.ManualRun(context.Background(), id("aaa")); err != nil {
		t.Fatal(err)
	}
	h.stop()
	if err := h.api.ManualRun(context.Background(), id("aaa")); !errors.Is(err, ErrEngineStopped) {
		t.Fatalf("stopped = %v", err)
	}
}

// Notifications: disabled delivery invokes nothing; a hanging or failing
// delivery never blocks the engine.
func TestRunNotifications(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		rt := newRunTest(t, 1, "notifications:\n  enabled: false\n")
		rt.setAlerts(rt.alert("aaa", "critical", time.Hour))
		called := make(chan struct{}, 1)
		h := rt.start(func(o *engineOptions) {
			o.deliver = func(context.Context, notify.Notification) error { called <- struct{}{}; return nil }
		})
		rt.waitState("aaa", item.StateNeedsYou)
		h.doTick()
		select {
		case <-called:
			t.Fatal("disabled notifications delivered")
		case <-time.After(200 * time.Millisecond):
		}
	})
	t.Run("hanging", func(t *testing.T) {
		rt := newRunTest(t, 1, "")
		rt.mode("bbb", "blocked")
		rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour), rt.alert("bbb", "critical", time.Hour))
		d := newDeliveries()
		d.block = true
		h := rt.start(func(o *engineOptions) { o.deliver = d.deliver })
		h.deliveries = d
		// Both runs complete and ticks are processed while the first
		// delivery hangs.
		rt.waitState("aaa", item.StateNeedsYou)
		rt.waitState("bbb", item.StateNeedsYou)
		h.doTick()
		h.send(h.poll)
		h.wait(h.applied, "round")
		got := []notify.Notification{waitDelivery(t, h), waitDelivery(t, h)}
		slices.SortFunc(got, func(a, b notify.Notification) int { return strings.Compare(a.ItemID, b.ItemID) })
		if got[1].Outcome != item.OutcomeBlocked || got[1].Message != "Which cluster is affected?" || got[1].Title() != notify.TitleBlocked {
			t.Fatalf("blocked notification = %+v", got[1])
		}
		if len(h.deliveries.all()) != 2 {
			t.Fatal("unexpected deliveries")
		}
		// Shutdown is not held up by the hanging deliveries.
		start := time.Now()
		h.stop()
		if time.Since(start) > 3*time.Second {
			t.Fatal("shutdown waited for deliveries")
		}
		if !strings.Contains(h.stderr.String(), "notification delivery failed") {
			t.Fatalf("no warning:\n%s", h.stderr.String())
		}
	})
	t.Run("failing", func(t *testing.T) {
		rt := newRunTest(t, 1, "")
		rt.setAlerts(rt.alert("aaa", "critical", time.Hour))
		h := rt.start(func(o *engineOptions) {
			o.deliver = func(context.Context, notify.Notification) error { return notify.ErrTimeout }
		})
		rt.waitState("aaa", item.StateNeedsYou)
		eventually(t, "warning", func() bool {
			return strings.Contains(h.stderr.String(), `level=WARN msg="notification delivery failed"`) &&
				strings.Contains(h.stderr.String(), notify.ErrTimeout.Error())
		})
		h.doTick()
		if r := rt.run("aaa", 1); r.Outcome != item.OutcomeReady {
			t.Fatal("delivery failure changed the outcome")
		}
	})
}

// The resume target of the default notifier is the running binary and
// the active configuration.
func TestRunDefaultNotifierTarget(t *testing.T) {
	rt := newRunTest(t, 1, "")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	n := &notify.Notifier{ConfigPath: rt.cfg.Path, Executable: os.Executable, LookPath: func(string) (string, error) { return "/bin/terminal-notifier", nil }}
	_, args, err := n.Command(notify.Notification{ItemID: id("aaa"), Outcome: item.OutcomeReady, Message: "m"})
	if err != nil {
		t.Fatal(err)
	}
	want := notify.ShellQuote(exe) + " --config " + notify.ShellQuote(rt.cfgPath) + " resume " + id("aaa")
	if !slices.Contains(args, want) || !filepath.IsAbs(rt.cfg.Path) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}
