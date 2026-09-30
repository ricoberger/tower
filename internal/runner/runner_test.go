package runner

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/prompt/prep"
	"github.com/ricoberger/tower/internal/result"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// fakeCopilot is the absolute path of the fake Copilot fixture.
func fakeCopilot(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/fake-copilot.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// safeBuffer is a concurrency-safe log sink.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// clock is a real-time clock that can be moved forward.
type clock struct{ offset atomic.Int64 }

func (c *clock) Now() time.Time          { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *clock) Advance(d time.Duration) { c.offset.Add(int64(d)) }

type env struct {
	store *item.Store
	dir   string
	logs  *safeBuffer
	clock *clock
}

// newEnv opens a store in a directory whose path contains spaces, quotes
// and shell metacharacters, and kills every fixture process group on
// cleanup.
func newEnv(t *testing.T) *env {
	t.Helper()
	dir := filepath.Join(t.TempDir(), `state dir 'q' $(touch pwned) ;&`)
	s, err := item.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{store: s, dir: dir, logs: &safeBuffer{}, clock: &clock{}}
	t.Cleanup(func() {
		killFixtures(dir)
		_ = s.Close()
	})
	t.Setenv("FAKE_COPILOT_MODE", "ready")
	t.Setenv("FAKE_COPILOT_DELAY", "0")
	t.Setenv("FAKE_COPILOT_DESCENDANT", "")
	t.Setenv("FAKE_COPILOT_EARLY", "")
	t.Setenv("FAKE_COPILOT_DIR", "")
	return e
}

// killFixtures kills the wrapper process groups (meta.yaml PIDs) and the
// process groups and descendants recorded by fake runs below dir, then
// waits until they are gone so that no fixture writes into a directory
// that is being removed.
func killFixtures(dir string) {
	var targets []int
	runs, _ := filepath.Glob(filepath.Join(dir, "items", "*", "runs", "*"))
	for _, run := range runs {
		if data, err := os.ReadFile(filepath.Join(run, "meta.yaml")); err == nil { // #nosec G304 -- test state
			for line := range strings.SplitSeq(string(data), "\n") {
				if v, ok := strings.CutPrefix(line, "pid: "); ok {
					if pid, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && pid > 1 {
						targets = append(targets, -pid)
					}
				}
			}
		}
		for _, name := range []string{"fake-pgid", "fake-descendant", "fake-pid"} {
			data, err := os.ReadFile(filepath.Join(run, name)) // #nosec G304 -- test fixture evidence
			if err != nil {
				continue
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || pid <= 1 {
				continue
			}
			if name == "fake-pgid" {
				pid = -pid
			}
			targets = append(targets, pid)
		}
	}
	for _, p := range targets {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, p := range targets {
		for time.Now().Before(deadline) && !errors.Is(syscall.Kill(p, 0), syscall.ESRCH) {
			_ = syscall.Kill(p, syscall.SIGKILL)
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func (e *env) runner(t *testing.T, mut func(*Options)) *Runner {
	t.Helper()
	opts := Options{
		Store:   e.store,
		Command: fakeCopilot(t),
		Args:    []string{"--yolo"},
		Timeout: time.Hour,
		Grace:   300 * time.Millisecond,
		Now:     e.clock.Now,
		Log:     slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	if mut != nil {
		mut(&opts)
	}
	r := New(opts)
	t.Cleanup(r.Close)
	return r
}

func (e *env) item(t *testing.T, fp string, n int) *item.Item {
	t.Helper()
	k := item.Key{Source: "dev", Fingerprint: fp}
	id, err := item.NewID(k, n)
	if err != nil {
		t.Fatal(err)
	}
	it := &item.Item{
		Version: item.Version, ID: id, Type: item.TypeAlert, State: item.StateQueued, Title: "KubePodCrashLooping: core/api",
		Severity:  "critical",
		CreatedAt: t0, UpdatedAt: t0,
		Source:  item.SourceRef{Name: k.Source, Fingerprint: k.Fingerprint},
		Alert:   item.AlertInfo{StartsAt: t0, LastSeenAt: t0, Status: item.AlertActive, Occurrences: 1, Labels: map[string]string{}},
		History: []item.HistoryEntry{{At: t0, To: item.StateNew, Reason: "alert firing"}},
	}
	if err := e.store.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.store.WriteAlertMarkdown(id, []byte("# KubePodCrashLooping\n\n`$(touch pwned)` \"quoted\" 'single'\n")); err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *env) runFile(t *testing.T, id string, n int, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.store.ItemDir(id), "runs", strconv.Itoa(n), name)) // #nosec G304 -- test file
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func (e *env) waitFile(t *testing.T, id string, n int, name string) string {
	t.Helper()
	p := filepath.Join(e.store.ItemDir(id), "runs", strconv.Itoa(n), name)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(p); err == nil && len(data) > 0 { // #nosec G304 -- test file
			return string(data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was not written", name)
	return ""
}

func (e *env) meta(t *testing.T, id string, n int) item.Run {
	t.Helper()
	runs, err := e.store.ReadRuns(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Number == n {
			return r
		}
	}
	t.Fatalf("run %d not found", n)
	return item.Run{}
}

func start(t *testing.T, r *Runner, it *item.Item, n int) item.Run {
	t.Helper()
	run, err := r.Start(Request{Item: it, Number: n, Reason: item.ReasonAuto, QueuedAt: t0})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return run
}

// drive processes Wait results, ownership check results and periodic checks
// until cond holds.
func drive(t *testing.T, r *Runner, useWait bool, cond func([]Completion) bool) []Completion {
	t.Helper()
	var all []Completion
	deadline := time.After(15 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	waitC := r.WaitC()
	if !useWait {
		waitC = nil
	}
	for !cond(all) {
		select {
		case w := <-waitC:
			all = append(all, r.HandleWait(w)...)
		case o := <-r.OwnershipC():
			all = append(all, r.HandleOwnership(o)...)
		case <-tick.C:
			all = append(all, r.Check()...)
		case <-deadline:
			t.Fatalf("condition not reached; completions %+v", all)
		}
	}
	return all
}

func completed(n int) func([]Completion) bool {
	return func(c []Completion) bool { return len(c) >= n }
}

func (r *Runner) idle([]Completion) bool { return r.Busy() == 0 }

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still alive", pid)
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

var uuidV4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestSessionIDAndHelpers(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id, err := newSessionID()
		if err != nil || !uuidV4.MatchString(id) || seen[id] {
			t.Fatalf("session ID %q %v", id, err)
		}
		seen[id] = true
	}
	for d, want := range map[time.Duration]string{
		20 * time.Minute: "20m", time.Hour: "1h", 90 * time.Minute: "1h30m", 90 * time.Second: "1m30s", time.Minute: "1m",
	} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%s) = %q, want %q", d, got, want)
		}
	}
	env := childEnv([]string{"A=1", "RUN=/bogus", "RUNNER=x", "RUN=/other"}, "/run dir")
	if !slices.Equal(env, []string{"A=1", "RUNNER=x", "RUN=/run dir"}) {
		t.Errorf("childEnv = %q", env)
	}
	if MonitorInterval != 2*time.Second || Grace != 10*time.Second {
		t.Error("production defaults changed")
	}
}

func TestQueueOrder(t *testing.T) {
	q := NewQueue([]string{"critical", "error", "warning", "info"})
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }
	q.Push(Entry{ItemID: "a", Reason: item.ReasonAuto, Severity: "warning", StartsAt: at(0)})
	q.Push(Entry{ItemID: "b", Reason: item.ReasonAuto, Severity: "critical", StartsAt: at(5)})
	q.Push(Entry{ItemID: "c", Reason: item.ReasonAuto, Severity: "critical", StartsAt: at(1)})
	q.Push(Entry{ItemID: "d", Reason: item.ReasonManual, Severity: "info", StartsAt: at(9)})
	q.Push(Entry{ItemID: "e", Reason: item.ReasonAuto, Severity: "", StartsAt: at(-10)})
	q.Push(Entry{ItemID: "f", Reason: item.ReasonAuto, Severity: "page", StartsAt: at(-20)})
	q.Push(Entry{ItemID: "g", Reason: item.ReasonRetry, Severity: "error", StartsAt: at(2)})
	q.Push(Entry{ItemID: "h", Reason: item.ReasonManual, Severity: "critical", StartsAt: at(3)})
	// A duplicate push replaces the earlier entry of the item.
	q.Push(Entry{ItemID: "a", Reason: item.ReasonAuto, Severity: "warning", StartsAt: at(0), Run: 2})
	var ids []string
	for _, e := range q.Ordered() {
		ids = append(ids, e.ItemID)
	}
	want := []string{"h", "d", "c", "b", "g", "a", "f", "e"}
	if !slices.Equal(ids, want) {
		t.Errorf("order = %v, want %v", ids, want)
	}
	if e, _ := q.Get("a"); e.Run != 2 || q.Len() != 8 {
		t.Errorf("dedupe failed: %+v %d", e, q.Len())
	}
	if !q.Remove("a") || q.Remove("a") || q.Len() != 7 {
		t.Error("Remove")
	}
}

func TestSpawnContract(t *testing.T) {
	e := newEnv(t)
	// The command lives in a path with spaces, quotes and metacharacters.
	cmdDir := filepath.Join(t.TempDir(), `bin dir 'x' $(touch pwned2) ;`)
	if err := os.MkdirAll(cmdDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(fakeCopilot(t))
	if err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(cmdDir, "copilot `id`")
	if err := os.WriteFile(command, fixture, 0o700); err != nil { // #nosec G306 G703 -- executable fixture
		t.Fatal(err)
	}
	args := []string{"--yolo", "--flag=a b", "$(touch pwned3)", "; echo hi", "", `"quoted" 'x'`}
	r := e.runner(t, func(o *Options) {
		o.Command = command
		o.Model = "model with space"
		o.Args = args
	})
	t.Setenv("FAKE_COPILOT_PROBE", "inherited value")
	t.Setenv("RUN", "/inherited/run")
	it := e.item(t, "abc", 1)

	run := start(t, r, it, 1)
	if run.PID <= 0 || run.Outcome != item.OutcomeRunning || run.StartedAt == nil || !uuidV4.MatchString(run.SessionID) ||
		run.Skill != Skill || run.Reason != item.ReasonAuto || run.RetryOf != 0 || !run.QueuedAt.Equal(t0) {
		t.Fatalf("run = %+v", run)
	}
	if m := e.meta(t, it.ID, 1); m.PID != run.PID || m.SessionID != run.SessionID || m.Outcome != item.OutcomeRunning || m.StartedAt == nil {
		t.Fatalf("persisted start = %+v", m)
	}
	if r.Busy() != 1 || !r.Executing(it.ID) {
		t.Fatal("started run is not tracked")
	}
	c := drive(t, r, true, completed(1))
	if c[0].Run.Outcome != item.OutcomeReady || c[0].ItemID != it.ID || c[0].Run.ExitCode == nil || *c[0].Run.ExitCode != 0 ||
		c[0].Run.FinishedAt == nil || c[0].Run.Error != "" {
		t.Fatalf("completion = %+v", c[0].Run)
	}
	drive(t, r, true, r.idle)

	prompt := e.runFile(t, it.ID, 1, item.PromptFile)
	argv := strings.Split(strings.TrimSuffix(e.runFile(t, it.ID, 1, "fake-argv"), "\x00"), "\x00")
	want := append([]string{"-p", prompt, "--session-id", run.SessionID, "--output-format", "json", "--no-ask-user", "--model", "model with space"}, args...)
	if !slices.Equal(argv, want) {
		t.Errorf("argv =\n%q\nwant\n%q", argv, want)
	}
	runDir, _ := e.store.RunPath(it.ID, 1)
	itemDir, _ := e.store.ItemPath(it.ID)
	if !filepath.IsAbs(runDir) || !strings.Contains(prompt, runDir+"/report.md") || !strings.Contains(prompt, "`$(touch pwned)`") {
		t.Errorf("prompt does not reference the run directory or alert: %q", prompt)
	}
	realItem, _ := filepath.EvalSymlinks(itemDir)
	if got := strings.TrimSpace(e.runFile(t, it.ID, 1, "fake-cwd")); got != realItem {
		t.Errorf("cwd = %q, want %q", got, realItem)
	}
	if got := e.runFile(t, it.ID, 1, "fake-run"); got != runDir {
		t.Errorf("RUN = %q, want %q", got, runDir)
	}
	if got := e.runFile(t, it.ID, 1, "fake-probe"); got != "inherited value" {
		t.Errorf("inherited env = %q", got)
	}
	pgid := atoi(t, e.runFile(t, it.ID, 1, "fake-pgid"))
	if pgid != run.PID || pgid == syscall.Getpgrp() {
		t.Errorf("fake pgid %d, wrapper pid %d, tower pgid %d", pgid, run.PID, syscall.Getpgrp())
	}
	if !strings.Contains(e.runFile(t, it.ID, 1, item.OutputFile), `"type":"result"`) ||
		!strings.Contains(e.runFile(t, it.ID, 1, item.StderrFile), "fake copilot running") ||
		e.runFile(t, it.ID, 1, item.ExitCodeFile) != "0\n" {
		t.Error("wrapper output, stderr or exit_code not published")
	}
	entries, _ := os.ReadDir(runDir)
	for _, en := range entries {
		info, _ := en.Info()
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v", en.Name(), info.Mode().Perm())
		}
		if strings.HasSuffix(en.Name(), ".tmp") {
			t.Errorf("temporary file %s left", en.Name())
		}
	}
	for _, d := range []string{runDir, itemDir, filepath.Dir(runDir)} {
		if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
			t.Errorf("%s mode = %v", d, fi.Mode().Perm())
		}
	}
	for _, p := range []string{"pwned", "pwned2", "pwned3"} {
		for _, d := range []string{realItem, runDir, cmdDir, "."} {
			if _, err := os.Stat(filepath.Join(d, p)); err == nil {
				t.Errorf("shell metacharacters were executed (%s in %s)", p, d)
			}
		}
	}

	// Without a model, --model is omitted; empty args are kept as they are.
	r2 := e.runner(t, func(o *Options) { o.Args = nil })
	it2 := e.item(t, "def", 1)
	run2 := start(t, r2, it2, 1)
	drive(t, r2, true, r2.idle)
	argv = strings.Split(strings.TrimSuffix(e.runFile(t, it2.ID, 1, "fake-argv"), "\x00"), "\x00")
	if slices.Contains(argv, "--model") || len(argv) != 7 || argv[3] != run2.SessionID {
		t.Errorf("argv without model = %q", argv)
	}
	if run2.SessionID == run.SessionID {
		t.Error("session IDs are reused")
	}
	// An existing run directory is never reused.
	if _, err := r2.Start(Request{Item: it2, Number: 1, Reason: item.ReasonAuto}); err == nil {
		t.Error("run directory reused")
	} else if se := (*StartError)(nil); !errors.As(err, &se) || se.Run != nil {
		t.Errorf("err = %v", err)
	}
	if m := e.meta(t, it2.ID, 1); m.Outcome != item.OutcomeReady {
		t.Errorf("existing run changed: %+v", m)
	}
}

func TestOutcomes(t *testing.T) {
	cases := []struct {
		mode    string
		outcome item.Outcome
		err     string
		exit    int
	}{
		{"ready", item.OutcomeReady, "", 0},
		{"blocked", item.OutcomeBlocked, "", 0},
		{"invalid", item.OutcomeFailed, "invalid result.json: ", 0},
		{"missing-result", item.OutcomeFailed, "result.json is missing", 0},
		{"missing-report", item.OutcomeFailed, "report.md is missing", 0},
		{"nonzero", item.OutcomeFailed, "copilot exited with status 3", 3},
	}
	e := newEnv(t)
	r := e.runner(t, nil)
	for i, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			t.Setenv("FAKE_COPILOT_MODE", c.mode)
			it := e.item(t, "fp"+strconv.Itoa(i), 1)
			start(t, r, it, 1)
			got := drive(t, r, true, completed(1))[0].Run
			drive(t, r, true, r.idle)
			if got.Outcome != c.outcome || !strings.HasPrefix(got.Error, c.err) || (c.err == "") != (got.Error == "") ||
				got.ExitCode == nil || *got.ExitCode != c.exit {
				t.Errorf("run = %+v", got)
			}
			if m := e.meta(t, it.ID, 1); m.Outcome != got.Outcome || m.Error != got.Error {
				t.Errorf("persisted %+v", m)
			}
			// Artifacts remain in place.
			for _, f := range []string{item.PromptFile, item.OutputFile, item.StderrFile, item.ExitCodeFile} {
				e.runFile(t, it.ID, 1, f)
			}
			if c.mode == "nonzero" {
				if _, err := result.Parse([]byte(e.runFile(t, it.ID, 1, item.ResultFile))); err != nil {
					t.Error("nonzero fixture must leave a valid result")
				}
			}
		})
	}
}

func TestExitFileCompletionAndSingleFinalization(t *testing.T) {
	e := newEnv(t)
	r := e.runner(t, nil)
	ctl := t.TempDir()
	t.Setenv("FAKE_COPILOT_DIR", ctl)
	t.Setenv("FAKE_COPILOT_EARLY", "1")
	if err := os.WriteFile(filepath.Join(ctl, "hold"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, "abc", 1)
	run := start(t, r, it, 1)
	// A valid result written while the run is still executing does not
	// finish it.
	e.waitFile(t, it.ID, 1, item.ResultFile)
	for range 5 {
		if c := r.Check(); len(c) != 0 {
			t.Fatalf("running run finished: %+v", c)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(ctl, "release.all"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Completion through the exit file (Wait results are not consumed).
	e.waitFile(t, it.ID, 1, item.ExitCodeFile)
	c := drive(t, r, false, completed(1))
	if len(c) != 1 || c[0].Run.Outcome != item.OutcomeReady {
		t.Fatalf("completions = %+v", c)
	}
	// The Wait result is still collected (the child is reaped) but cannot
	// finalize the run again.
	w := <-r.WaitC()
	if dup := r.HandleWait(w); len(dup) != 0 {
		t.Fatalf("duplicate finalization %+v", dup)
	}
	if !w.state.Exited() || w.state.Pid() != run.PID {
		t.Errorf("wait state %v", w.state)
	}
	drive(t, r, true, r.idle)
	if extra := r.Check(); len(extra) != 0 {
		t.Fatal("finalized again")
	}
	if strings.Count(e.logs.String(), "run finished") != 1 {
		t.Errorf("logs:\n%s", e.logs)
	}
}

func TestWaitCompletionWithoutExitFile(t *testing.T) {
	e := newEnv(t)
	r := e.runner(t, nil)
	ctl := t.TempDir()
	t.Setenv("FAKE_COPILOT_DIR", ctl)
	t.Setenv("FAKE_COPILOT_EARLY", "1")
	if err := os.WriteFile(filepath.Join(ctl, "hold"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, "abc", 1)
	run := start(t, r, it, 1)
	e.waitFile(t, it.ID, 1, item.ResultFile)
	fake := atoi(t, e.waitFile(t, it.ID, 1, "fake-pid"))
	// Kill only the wrapper: no exit_code is published, Wait reports the
	// signal, and the fake keeps running in the group until cleanup.
	if err := syscall.Kill(run.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	c := drive(t, r, true, completed(1))
	got := c[0].Run
	if got.Outcome != item.OutcomeFailed || got.ExitCode != nil || !strings.Contains(got.Error, "killed") {
		t.Fatalf("run = %+v", got)
	}
	// The surviving fake is terminated by the group cleanup before the
	// slot is released.
	if r.Busy() != 1 {
		t.Fatal("slot released before the group cleanup")
	}
	drive(t, r, true, r.idle)
	waitDead(t, fake)
}

func TestTimeoutLocal(t *testing.T) {
	e := newEnv(t)
	r := e.runner(t, func(o *Options) { o.Timeout = 20 * time.Minute })
	t.Setenv("FAKE_COPILOT_MODE", "hang")
	t.Setenv("FAKE_COPILOT_DESCENDANT", "1")
	it := e.item(t, "abc", 1)
	start(t, r, it, 1)
	desc := atoi(t, e.waitFile(t, it.ID, 1, "fake-descendant"))
	if c := r.Check(); len(c) != 0 || r.Busy() != 1 {
		t.Fatal("run finished before its timeout")
	}
	e.clock.Advance(20 * time.Minute)
	c := drive(t, r, true, completed(1))
	got := c[0].Run
	if got.Outcome != item.OutcomeFailed || got.Error != "timeout after 20m" {
		t.Fatalf("run = %+v", got)
	}
	// The descendant ignores TERM and still occupies the slot until KILL.
	if !pidAlive(desc) || r.Busy() != 1 {
		t.Fatal("descendant must survive TERM and keep the slot")
	}
	drive(t, r, true, r.idle)
	waitDead(t, desc)
	logs := e.logs.String()
	if !strings.Contains(logs, "run timed out") || !strings.Contains(logs, "sent KILL") {
		t.Errorf("logs:\n%s", logs)
	}
	if m := e.meta(t, it.ID, 1); m.Outcome != item.OutcomeFailed || m.Error != "timeout after 20m" {
		t.Errorf("persisted %+v", m)
	}
}

func TestNormalFinishDescendantCleanup(t *testing.T) {
	e := newEnv(t)
	r := e.runner(t, nil)
	t.Setenv("FAKE_COPILOT_MODE", "descendant")
	it := e.item(t, "abc", 1)
	start(t, r, it, 1)
	c := drive(t, r, true, completed(1))
	if c[0].Run.Outcome != item.OutcomeReady || c[0].Run.Error != "" || *c[0].Run.ExitCode != 0 {
		t.Fatalf("run = %+v", c[0].Run)
	}
	desc := atoi(t, e.runFile(t, it.ID, 1, "fake-descendant"))
	if !pidAlive(desc) || r.Busy() != 1 {
		t.Fatal("slot released while the descendant is alive")
	}
	drive(t, r, true, r.idle)
	waitDead(t, desc)
	if m := e.meta(t, it.ID, 1); m.Outcome != item.OutcomeReady || m.Error != "" {
		t.Errorf("cleanup changed the outcome: %+v", m)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "processes remain in the run's group after completion") || !strings.Contains(logs, "cleanup complete") {
		t.Errorf("logs:\n%s", logs)
	}
}

func TestCancelLocal(t *testing.T) {
	e := newEnv(t)
	r := e.runner(t, nil)
	t.Setenv("FAKE_COPILOT_MODE", "ready")
	t.Setenv("FAKE_COPILOT_EARLY", "1")
	t.Setenv("FAKE_COPILOT_DELAY", "30")
	it := e.item(t, "abc", 1)
	run := start(t, r, it, 1)
	e.waitFile(t, it.ID, 1, item.ResultFile)
	r.Cancel(it.ID, 2) // wrong run: ignored
	r.Cancel(it.ID, 1)
	r.Cancel(it.ID, 1) // repeated: harmless
	c := drive(t, r, true, completed(1))
	if c[0].Run.Outcome != item.OutcomeCancelled || c[0].Run.Error != ErrCancelled {
		t.Fatalf("run = %+v", c[0].Run)
	}
	drive(t, r, true, r.idle)
	if alive(syscall.Kill, -run.PID) {
		t.Error("group still alive")
	}
}

// startDetached starts a run with a first runner that is then closed,
// simulating a tower restart with the child still running.
func startDetached(t *testing.T, e *env, it *item.Item) item.Run {
	t.Helper()
	old := e.runner(t, nil)
	run := start(t, old, it, 1)
	old.Close()
	e.waitFile(t, it.ID, 1, "fake-pid")
	return run
}

func TestReattachAndComplete(t *testing.T) {
	e := newEnv(t)
	ctl := t.TempDir()
	t.Setenv("FAKE_COPILOT_DIR", ctl)
	if err := os.WriteFile(filepath.Join(ctl, "hold"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, "abc", 1)
	run := startDetached(t, e, it)
	if !pidAlive(run.PID) {
		t.Fatal("closing the runner stopped the child")
	}
	r := e.runner(t, nil)
	adopted, _ := r.Recover(it.ID, "dev", e.meta(t, it.ID, 1))
	if !adopted || r.Busy() != 1 || !r.Executing(it.ID) {
		t.Fatal("live owned run not adopted")
	}
	if c := r.Check(); len(c) != 0 {
		t.Fatal("adopted run finished early")
	}
	if err := os.WriteFile(filepath.Join(ctl, "release.all"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := drive(t, r, true, completed(1))
	if c[0].Run.Outcome != item.OutcomeReady || c[0].Run.PID != run.PID || c[0].Run.SessionID != run.SessionID {
		t.Fatalf("run = %+v", c[0].Run)
	}
	drive(t, r, true, r.idle)
	log, _ := os.ReadFile(filepath.Join(ctl, "log")) // #nosec G304 -- test file
	if strings.Count(string(log), "start ") != 1 {
		t.Errorf("fixture log:\n%s", log)
	}
}

func TestReattachedPIDDeathWithoutExitFile(t *testing.T) {
	e := newEnv(t)
	ctl := t.TempDir()
	t.Setenv("FAKE_COPILOT_DIR", ctl)
	t.Setenv("FAKE_COPILOT_EARLY", "1")
	if err := os.WriteFile(filepath.Join(ctl, "hold"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	it := e.item(t, "abc", 1)
	run := startDetached(t, e, it)
	e.waitFile(t, it.ID, 1, item.ResultFile)
	fake := atoi(t, e.runFile(t, it.ID, 1, "fake-pid"))
	var signals atomic.Int32
	r := e.runner(t, func(o *Options) {
		o.Signal = func(pid int, sig syscall.Signal) error {
			if sig != 0 {
				signals.Add(1)
			}
			return syscall.Kill(pid, sig)
		}
	})
	if adopted, _ := r.Recover(it.ID, "dev", e.meta(t, it.ID, 1)); !adopted {
		t.Fatal("not adopted")
	}
	// The wrapper dies without publishing exit_code; the fake stays in the
	// group.
	if err := syscall.Kill(run.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	c := drive(t, r, true, completed(1))
	got := c[0].Run
	if got.Outcome != item.OutcomeReady || got.ExitCode != nil {
		t.Fatalf("run = %+v", got)
	}
	// Post-completion cleanup is skipped because the dead wrapper cannot be
	// verified, and the slot is released anyway.
	drive(t, r, true, r.idle)
	if signals.Load() != 0 {
		t.Error("an unverified group was signalled")
	}
	if !pidAlive(fake) {
		t.Error("the unverified descendant must not be signalled")
	}
	if !strings.Contains(e.logs.String(), "ownership could not be verified") {
		t.Errorf("logs:\n%s", e.logs)
	}
	if m := e.meta(t, it.ID, 1); m.ExitCode != nil || m.Outcome != item.OutcomeReady {
		t.Errorf("persisted %+v", m)
	}
}

func TestRecoverInterrupted(t *testing.T) {
	e := newEnv(t)
	var signals []int
	r := e.runner(t, func(o *Options) {
		o.Signal = func(pid int, sig syscall.Signal) error {
			if sig != 0 {
				signals = append(signals, pid)
			}
			return syscall.Kill(pid, sig)
		}
	})
	// An unrelated live process reuses the PID.
	unrelated := exec.CommandContext(t.Context(), "sleep", "30")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() })
	dead := exec.CommandContext(t.Context(), "true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	base := item.Run{Number: 1, SessionID: "9b7a1c1e-6f0f-4c9e-8a57-1f6f2d8e0b11", Reason: item.ReasonAuto, Skill: Skill, StartedAt: &t0, Outcome: item.OutcomeRunning}
	cases := map[string]struct {
		mut  func(*item.Run)
		want string
	}{
		"pid reuse":  {func(r *item.Run) { r.PID = unrelated.Process.Pid }, "could not be verified"},
		"dead":       {func(r *item.Run) { r.PID = dead.Process.Pid }, "no longer running"},
		"no pid":     {func(r *item.Run) { r.PID = 0 }, "no process was recorded"},
		"no session": {func(r *item.Run) { r.PID = unrelated.Process.Pid; r.SessionID = "" }, "no session ID"},
	}
	i := 0
	for name, c := range cases {
		i++
		it := e.item(t, "fp"+strconv.Itoa(i), 1)
		run := base.Clone()
		c.mut(&run)
		if err := e.store.CreateRun(it.ID, run); err != nil {
			t.Fatal(err)
		}
		adopted, got := r.Recover(it.ID, "dev", run)
		if adopted || got.Outcome != item.OutcomeInterrupted || got.FinishedAt == nil || !strings.Contains(got.Error, c.want) ||
			got.PID != run.PID || got.SessionID != run.SessionID {
			t.Errorf("%s: adopted=%v run=%+v", name, adopted, got)
		}
	}
	if r.Busy() != 0 || len(signals) != 0 {
		t.Errorf("busy=%d signals=%v", r.Busy(), signals)
	}
	if !pidAlive(unrelated.Process.Pid) {
		t.Error("unrelated process was affected")
	}

	// A run that published exit_code is adopted even though its PID is gone.
	it := e.item(t, "done", 1)
	run := base.Clone()
	run.PID = dead.Process.Pid
	if err := e.store.CreateRun(it.ID, run); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{item.ExitCodeFile: "0\n", item.ResultFile: `{"version":1,"status":"blocked","summary":"s","assumptions":[],"gate":{"question":"q?"},"proposed_actions":[]}`, item.ReportFile: "# r"} {
		if err := e.store.WriteRunFile(it.ID, 1, name, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if adopted, _ := r.Recover(it.ID, "dev", run); !adopted {
		t.Fatal("completed run not adopted")
	}
	c := r.Check()
	if len(c) != 1 || c[0].Run.Outcome != item.OutcomeBlocked || *c[0].Run.ExitCode != 0 {
		t.Fatalf("completions = %+v", c)
	}
	if r.Busy() != 0 || len(signals) != 0 {
		t.Errorf("busy=%d signals=%v", r.Busy(), signals)
	}
}

// ownsRecorder wraps ProcessOwns: every check is counted and a switch can
// simulate the PID being taken over by an unrelated process.
type ownsRecorder struct {
	calls   atomic.Int32
	foreign atomic.Bool
}

func (o *ownsRecorder) owns(pid int, sid string) bool {
	o.calls.Add(1)
	if o.foreign.Load() {
		return false
	}
	return ProcessOwns(pid, sid)
}

type signalRecorder struct {
	mu   sync.Mutex
	sigs []syscall.Signal
}

func (s *signalRecorder) signal(pid int, sig syscall.Signal) error {
	if sig != 0 {
		s.mu.Lock()
		s.sigs = append(s.sigs, sig)
		s.mu.Unlock()
	}
	return syscall.Kill(pid, sig)
}

func (s *signalRecorder) list() []syscall.Signal {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.sigs)
}

func TestReattachedFreshOwnershipChecks(t *testing.T) {
	for _, action := range []string{"timeout", "cancel"} {
		t.Run(action, func(t *testing.T) {
			e := newEnv(t)
			t.Setenv("FAKE_COPILOT_MODE", "hang")
			it := e.item(t, "abc", 1)
			startDetached(t, e, it)
			owns, sigs := &ownsRecorder{}, &signalRecorder{}
			r := e.runner(t, func(o *Options) {
				o.Owns = owns.owns
				o.Signal = sigs.signal
				o.Timeout = time.Hour
			})
			if adopted, _ := r.Recover(it.ID, "dev", e.meta(t, it.ID, 1)); !adopted {
				t.Fatal("not adopted")
			}
			startupChecks := owns.calls.Load()
			// The PID now represents an unrelated process.
			owns.foreign.Store(true)
			if action == "timeout" {
				e.clock.Advance(2 * time.Hour)
			} else {
				r.Cancel(it.ID, 1)
			}
			c := drive(t, r, true, completed(1))
			want := item.OutcomeCancelled
			if action == "timeout" {
				want = item.OutcomeFailed
			}
			if c[0].Run.Outcome != want {
				t.Fatalf("run = %+v", c[0].Run)
			}
			if r.Busy() != 0 {
				t.Error("slot not released")
			}
			if len(sigs.list()) != 0 {
				t.Errorf("signals sent: %v", sigs.list())
			}
			if owns.calls.Load() != startupChecks+1 {
				t.Errorf("ownership checks = %d (startup %d)", owns.calls.Load(), startupChecks)
			}
			if !strings.Contains(e.logs.String(), "ownership could not be verified") {
				t.Errorf("logs:\n%s", e.logs)
			}
		})
	}
}

func TestReattachedOwnershipLossAfterTerm(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_COPILOT_MODE", "hang")
	t.Setenv("FAKE_COPILOT_DESCENDANT", "1")
	it := e.item(t, "abc", 1)
	startDetached(t, e, it)
	desc := atoi(t, e.waitFile(t, it.ID, 1, "fake-descendant"))
	owns, sigs := &ownsRecorder{}, &signalRecorder{}
	r := e.runner(t, func(o *Options) {
		o.Owns = owns.owns
		o.Signal = sigs.signal
		o.Timeout = time.Hour
	})
	if adopted, _ := r.Recover(it.ID, "dev", e.meta(t, it.ID, 1)); !adopted {
		t.Fatal("not adopted")
	}
	before := owns.calls.Load()
	// The timeout is measured from the original start.
	e.clock.Advance(59 * time.Minute)
	if c := r.Check(); len(c) != 0 || len(sigs.list()) != 0 {
		t.Fatal("timed out early")
	}
	e.clock.Advance(2 * time.Minute)
	c := drive(t, r, true, completed(1))
	if c[0].Run.Outcome != item.OutcomeFailed || c[0].Run.Error != "timeout after 1h" || c[0].Run.ExitCode != nil {
		t.Fatalf("run = %+v", c[0].Run)
	}
	drive(t, r, true, r.idle)
	// TERM was sent after a fresh check; the wrapper exited and the second
	// check before KILL failed, so no KILL was sent.
	if got := sigs.list(); !slices.Equal(got, []syscall.Signal{syscall.SIGTERM}) {
		t.Errorf("signals = %v", got)
	}
	if owns.calls.Load() != before+2 {
		t.Errorf("ownership checks = %d, want %d", owns.calls.Load(), before+2)
	}
	if !pidAlive(desc) {
		t.Error("the TERM-ignoring descendant should remain (killed by the test cleanup)")
	}
	if !strings.Contains(e.logs.String(), "ownership could not be verified") {
		t.Errorf("logs:\n%s", e.logs)
	}
}

func TestReattachedCleanupWithOwnership(t *testing.T) {
	// A re-attached run whose wrapper survives TERM (simulated by
	// swallowing the TERM) receives KILL after a fresh ownership check.
	e := newEnv(t)
	t.Setenv("FAKE_COPILOT_MODE", "hang")
	it := e.item(t, "abc", 1)
	run := startDetached(t, e, it)
	owns, sigs := &ownsRecorder{}, &signalRecorder{}
	r := e.runner(t, func(o *Options) {
		o.Owns = owns.owns
		// TERM is swallowed so KILL escalation is needed.
		o.Signal = func(pid int, sig syscall.Signal) error {
			if sig == syscall.SIGTERM {
				_ = sigs.signal(pid, 0)
				sigs.mu.Lock()
				sigs.sigs = append(sigs.sigs, sig)
				sigs.mu.Unlock()
				return nil
			}
			return sigs.signal(pid, sig)
		}
	})
	if adopted, _ := r.Recover(it.ID, "dev", e.meta(t, it.ID, 1)); !adopted {
		t.Fatal("not adopted")
	}
	before := owns.calls.Load()
	e.clock.Advance(2 * time.Hour)
	c := drive(t, r, true, completed(1))
	if c[0].Run.Outcome != item.OutcomeFailed {
		t.Fatalf("run = %+v", c[0].Run)
	}
	drive(t, r, true, r.idle)
	if got := sigs.list(); !slices.Equal(got, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Errorf("signals = %v", got)
	}
	if owns.calls.Load() != before+2 {
		t.Errorf("ownership checks = %d, want %d", owns.calls.Load(), before+2)
	}
	if alive(syscall.Kill, -run.PID) {
		t.Error("group survived")
	}
}

// A run finalized before tower stopped whose process group was still being
// cleaned up keeps its slot after a restart until the cleanup completes.
func TestRecoverCleanup(t *testing.T) {
	t.Run("owned", func(t *testing.T) {
		e := newEnv(t)
		t.Setenv("FAKE_COPILOT_MODE", "hang")
		it := e.item(t, "abc", 1)
		run := startDetached(t, e, it)
		run.Outcome = item.OutcomeReady
		owns, sigs := &ownsRecorder{}, &signalRecorder{}
		r := e.runner(t, func(o *Options) { o.Owns, o.Signal = owns.owns, sigs.signal })
		if !r.RecoverCleanup(it.ID, "dev", run) {
			t.Fatal("not tracked")
		}
		if r.Busy() != 1 || r.Executing(it.ID) || !r.Tracking(it.ID) {
			t.Fatalf("busy=%d executing=%v", r.Busy(), r.Executing(it.ID))
		}
		// A second recovery of the same item is a no-op.
		if r.RecoverCleanup(it.ID, "dev", run) {
			t.Fatal("tracked twice")
		}
		c := drive(t, r, false, r.idle)
		if len(c) != 0 {
			t.Fatalf("completion reported again: %+v", c)
		}
		if got := sigs.list(); len(got) == 0 || got[0] != syscall.SIGTERM {
			t.Errorf("signals = %v", got)
		}
		if alive(syscall.Kill, -run.PID) {
			t.Error("group survived")
		}
		if !strings.Contains(e.logs.String(), "needs cleanup") {
			t.Errorf("logs:\n%s", e.logs.String())
		}
	})
	t.Run("not owned", func(t *testing.T) {
		e := newEnv(t)
		t.Setenv("FAKE_COPILOT_MODE", "hang")
		it := e.item(t, "abc", 1)
		run := startDetached(t, e, it)
		run.Outcome = item.OutcomeFailed
		owns, sigs := &ownsRecorder{}, &signalRecorder{}
		owns.foreign.Store(true)
		r := e.runner(t, func(o *Options) { o.Owns, o.Signal = owns.owns, sigs.signal })
		if r.RecoverCleanup(it.ID, "dev", run) || r.Busy() != 0 {
			t.Fatal("unverified process tracked")
		}
		r.Check()
		if got := sigs.list(); len(got) != 0 {
			t.Errorf("signals = %v", got)
		}
		if !alive(syscall.Kill, run.PID) {
			t.Error("unverified process was affected")
		}
	})
	t.Run("wrapper gone", func(t *testing.T) {
		e := newEnv(t)
		t.Setenv("FAKE_COPILOT_MODE", "hang")
		t.Setenv("FAKE_COPILOT_DESCENDANT", "1")
		it := e.item(t, "abc", 1)
		run := startDetached(t, e, it)
		e.waitFile(t, it.ID, 1, "fake-descendant")
		if err := syscall.Kill(run.PID, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		waitDead(t, run.PID)
		run.Outcome = item.OutcomeReady
		sigs := &signalRecorder{}
		r := e.runner(t, func(o *Options) { o.Signal = sigs.signal })
		if r.RecoverCleanup(it.ID, "dev", run) || r.Busy() != 0 {
			t.Fatal("unverifiable group tracked")
		}
		if got := sigs.list(); len(got) != 0 {
			t.Errorf("signals = %v", got)
		}
		if !alive(syscall.Kill, -run.PID) {
			t.Fatal("fixture group already gone")
		}
		if !strings.Contains(e.logs.String(), "skipping process group cleanup") {
			t.Errorf("logs:\n%s", e.logs.String())
		}
	})
}

func TestStartFailures(t *testing.T) {
	e := newEnv(t)
	it := e.item(t, "abc", 1)
	// A missing alert.md is a retained, failed attempt.
	if err := e.store.RemoveAlertMarkdown(it.ID); err != nil {
		t.Fatal(err)
	}
	r := e.runner(t, nil)
	_, err := r.Start(Request{Item: it, Number: 1, Reason: item.ReasonAuto, QueuedAt: t0})
	var se *StartError
	if !errors.As(err, &se) || se.Run == nil || se.Run.Outcome != item.OutcomeFailed || !strings.Contains(se.Run.Error, "alert.md") {
		t.Fatalf("err = %v", err)
	}
	if m := e.meta(t, it.ID, 1); m.Outcome != item.OutcomeFailed || m.PID != 0 || m.FinishedAt == nil {
		t.Errorf("persisted %+v", m)
	}
	if r.Busy() != 0 {
		t.Error("failed start occupies a slot")
	}

	// A process start error is retained as well.
	it2 := e.item(t, "def", 1)
	r2 := e.runner(t, func(o *Options) { o.Shell = filepath.Join(t.TempDir(), "missing-sh") })
	_, err = r2.Start(Request{Item: it2, Number: 1, Reason: item.ReasonManual, QueuedAt: t0})
	if !errors.As(err, &se) || se.Run == nil || !strings.HasPrefix(se.Run.Error, "start run: ") || se.Run.Reason != item.ReasonManual {
		t.Fatalf("err = %v", err)
	}

	// A runtime template error is a run error, not a crash.
	it3 := e.item(t, "ghi", 1)
	tmpl, perr := prep.Parse("{{.RunDir}}{{template \"missing\"}}")
	if perr != nil {
		t.Fatal(perr)
	}
	r3 := e.runner(t, func(o *Options) { o.Template = tmpl })
	_, err = r3.Start(Request{Item: it3, Number: 1, Reason: item.ReasonAuto})
	if !errors.As(err, &se) || se.Run == nil || !strings.HasPrefix(se.Run.Error, "render prompt: ") {
		t.Fatalf("err = %v", err)
	}
}

func TestPreviousReports(t *testing.T) {
	e := newEnv(t)
	prev := e.item(t, "abc", 1)
	it := e.item(t, "abc", 2)
	it.PreviousItem = &prev.ID
	mk := func(id string, n int, report bool) {
		run := item.Run{Number: n, SessionID: "s", Reason: item.ReasonAuto, Skill: Skill, Outcome: item.OutcomeFailed}
		if err := e.store.CreateRun(id, run); err != nil {
			t.Fatal(err)
		}
		if report {
			if err := e.store.WriteRunFile(id, n, item.ReportFile, []byte("# r")); err != nil {
				t.Fatal(err)
			}
		}
	}
	path := func(id string, n int) string {
		d, err := e.store.RunPath(id, n)
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(d, item.ReportFile)
	}

	if got := PreviousReports(e.store, it, 1); len(got) != 0 {
		t.Errorf("no reports: %v", got)
	}
	mk(prev.ID, 1, true)
	mk(prev.ID, 2, false)
	if got := PreviousReports(e.store, it, 1); !slices.Equal(got, []string{path(prev.ID, 1)}) {
		t.Errorf("previous item only: %v", got)
	}
	for n, report := range map[int]bool{1: true, 2: true, 3: false, 4: true, 5: true, 6: true} {
		mk(it.ID, n, report)
	}
	want := []string{path(it.ID, 5), path(it.ID, 4), path(it.ID, 2), path(prev.ID, 1)}
	if got := PreviousReports(e.store, it, 6); !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	want = []string{path(it.ID, 6), path(it.ID, 5), path(it.ID, 4), path(prev.ID, 1)}
	if got := PreviousReports(e.store, it, 7); !slices.Equal(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
	// A pruned previous item is skipped.
	missing := "dev-0000000000000000-9"
	it.PreviousItem = &missing
	if got := PreviousReports(e.store, it, 7); len(got) != 3 {
		t.Errorf("pruned previous item: %v", got)
	}
	bad := "../x"
	it.PreviousItem = &bad
	if got := PreviousReports(e.store, it, 7); len(got) != 3 {
		t.Errorf("invalid previous item: %v", got)
	}
}

func TestCloseDoesNotWaitOrSignal(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_COPILOT_MODE", "hang")
	r := e.runner(t, nil)
	it := e.item(t, "abc", 1)
	run := start(t, r, it, 1)
	done := make(chan struct{})
	go func() { r.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked")
	}
	time.Sleep(100 * time.Millisecond)
	if !pidAlive(run.PID) {
		t.Error("Close terminated the run")
	}
	if err := syscall.Kill(-run.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
}

// gatedOwns is an ownership check that, once gated, blocks until the test
// releases it with a verdict (true: the real check decides).
type gatedOwns struct {
	calls   atomic.Int32
	gated   atomic.Bool
	entered chan struct{}
	release chan bool
}

func newGatedOwns() *gatedOwns {
	return &gatedOwns{entered: make(chan struct{}, 8), release: make(chan bool)}
}

func (g *gatedOwns) owns(pid int, sid string) bool {
	g.calls.Add(1)
	if !g.gated.Load() {
		return ProcessOwns(pid, sid)
	}
	g.entered <- struct{}{}
	if !<-g.release {
		return false
	}
	return ProcessOwns(pid, sid)
}

func (g *gatedOwns) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("ownership check not started")
	}
}

// release unblocks the in-flight check after delay and returns its result.
func (g *gatedOwns) resolve(t *testing.T, r *Runner, delay time.Duration, owned bool) OwnershipResult {
	t.Helper()
	go func() {
		time.Sleep(delay)
		g.release <- owned
	}()
	select {
	case res := <-r.OwnershipC():
		return res
	case <-time.After(delay + 10*time.Second):
		t.Fatal("no ownership result")
	}
	return OwnershipResult{}
}

// swallowTerm records signals; TERM is recorded but not delivered so KILL
// escalation is needed.
func swallowTerm(sigs *signalRecorder) func(int, syscall.Signal) error {
	return func(pid int, sig syscall.Signal) error {
		if sig == syscall.SIGTERM {
			sigs.mu.Lock()
			sigs.sigs = append(sigs.sigs, sig)
			sigs.mu.Unlock()
			return nil
		}
		return sigs.signal(pid, sig)
	}
}

// TestReattachedOwnershipCheckOffLoop verifies that the ownership check
// before signalling a re-attached run does not block Check, HandleWait or
// Cancel, that only one check is in flight per run, and that every signal
// (TERM and the KILL escalation) is preceded by its own fresh check.
func TestReattachedOwnershipCheckOffLoop(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_COPILOT_MODE", "hang")
	it := e.item(t, "abc", 1)
	run := startDetached(t, e, it)
	owns, sigs := newGatedOwns(), &signalRecorder{}
	r := e.runner(t, func(o *Options) {
		o.Owns = owns.owns
		o.Signal = swallowTerm(sigs)
	})
	if adopted, _ := r.Recover(it.ID, "dev", e.meta(t, it.ID, 1)); !adopted {
		t.Fatal("not adopted")
	}
	before := owns.calls.Load()
	owns.gated.Store(true)
	e.clock.Advance(2 * time.Hour)

	// The timed-out run starts one check; repeated checks, cancellation
	// and another run's Wait result are handled while it blocks.
	for range 10 {
		begin := time.Now()
		if c := r.Check(); len(c) != 0 {
			t.Fatalf("completions while verifying: %+v", c)
		}
		if d := time.Since(begin); d > time.Second {
			t.Fatalf("Check blocked for %s", d)
		}
	}
	r.Cancel(it.ID, 1)
	owns.waitEntered(t)
	t.Setenv("FAKE_COPILOT_MODE", "ready")
	other := e.item(t, "def", 1)
	start(t, r, other, 1)
	var got []Completion
	deadline := time.After(15 * time.Second)
	for len(got) == 0 {
		select {
		case w := <-r.WaitC():
			got = append(got, r.HandleWait(w)...)
		case <-time.After(20 * time.Millisecond):
			got = append(got, r.Check()...)
		case <-deadline:
			t.Fatal("the other run did not complete")
		}
	}
	if got[0].ItemID != other.ID || got[0].Run.Outcome != item.OutcomeReady {
		t.Fatalf("completions = %+v", got)
	}
	if n := owns.calls.Load(); n != before+1 {
		t.Fatalf("ownership checks = %d, want %d", n, before+1)
	}
	if s := sigs.list(); len(s) != 0 {
		t.Fatalf("signalled before the check returned: %v", s)
	}

	// TERM is sent only when the check returns true.
	c := r.HandleOwnership(owns.resolve(t, r, 100*time.Millisecond, true))
	if s := sigs.list(); !slices.Equal(s, []syscall.Signal{syscall.SIGTERM}) {
		t.Fatalf("signals = %v", s)
	}
	if len(c) != 0 {
		t.Fatalf("completions = %+v", c)
	}
	if r.Check(); owns.calls.Load() != before+1 {
		t.Fatal("a check started before the grace elapsed")
	}

	// KILL escalation starts its own fresh check.
	e.clock.Advance(time.Second)
	r.Check()
	r.Check()
	owns.waitEntered(t)
	if n := owns.calls.Load(); n != before+2 || len(sigs.list()) != 1 {
		t.Fatalf("ownership checks = %d, signals = %v", n, sigs.list())
	}
	r.HandleOwnership(owns.resolve(t, r, 100*time.Millisecond, true))
	if s := sigs.list(); !slices.Equal(s, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Fatalf("signals = %v", s)
	}
	owns.gated.Store(false)
	c = drive(t, r, true, completed(1))
	if c[0].ItemID != it.ID || c[0].Run.Outcome != item.OutcomeFailed || c[0].Run.Error != "timeout after 1h" {
		t.Fatalf("run = %+v", c[0].Run)
	}
	drive(t, r, true, r.idle)
	if alive(syscall.Kill, -run.PID) {
		t.Error("group survived")
	}
	if n := owns.calls.Load(); n != before+2 {
		t.Errorf("ownership checks = %d, want %d", n, before+2)
	}
}

func TestReattachedOwnershipCheckFailsAfterDelay(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_COPILOT_MODE", "hang")
	it := e.item(t, "abc", 1)
	startDetached(t, e, it)
	owns, sigs := newGatedOwns(), &signalRecorder{}
	r := e.runner(t, func(o *Options) {
		o.Owns = owns.owns
		o.Signal = sigs.signal
	})
	if adopted, _ := r.Recover(it.ID, "dev", e.meta(t, it.ID, 1)); !adopted {
		t.Fatal("not adopted")
	}
	owns.gated.Store(true)
	e.clock.Advance(2 * time.Hour)
	if c := r.Check(); len(c) != 0 {
		t.Fatalf("completions = %+v", c)
	}
	owns.waitEntered(t)
	res := owns.resolve(t, r, 300*time.Millisecond, false)
	c := r.HandleOwnership(res)
	if len(c) != 1 || c[0].Run.Outcome != item.OutcomeFailed || c[0].Run.Error != "timeout after 1h" {
		t.Fatalf("completions = %+v", c)
	}
	if r.Busy() != 0 {
		t.Error("slot not released")
	}
	if s := sigs.list(); len(s) != 0 {
		t.Errorf("signals sent: %v", s)
	}
	if m := e.meta(t, it.ID, 1); m.Outcome != item.OutcomeFailed || m.Error != "timeout after 1h" {
		t.Errorf("persisted %+v", m)
	}
	if !strings.Contains(e.logs.String(), "ownership could not be verified") {
		t.Errorf("logs:\n%s", e.logs)
	}
	// A late duplicate of the result is ignored.
	if c := r.HandleOwnership(res); len(c) != 0 {
		t.Errorf("stale result produced %+v", c)
	}
}

func TestCloseDuringOwnershipCheck(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_COPILOT_MODE", "hang")
	it := e.item(t, "abc", 1)
	startDetached(t, e, it)
	owns, sigs := newGatedOwns(), &signalRecorder{}
	r := e.runner(t, func(o *Options) {
		o.Owns = owns.owns
		o.Signal = sigs.signal
	})
	if adopted, _ := r.Recover(it.ID, "dev", e.meta(t, it.ID, 1)); !adopted {
		t.Fatal("not adopted")
	}
	owns.gated.Store(true)
	r.Cancel(it.ID, 1)
	owns.waitEntered(t)
	done := make(chan struct{})
	go func() { r.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked on the in-flight check")
	}
	owns.release <- true
	finished := make(chan struct{})
	go func() { r.checks.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the check goroutine leaked in a blocked send")
	}
	if s := sigs.list(); len(s) != 0 {
		t.Errorf("signals sent after Close: %v", s)
	}
}

func TestLocalSignalsWithoutOwnershipCheck(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_COPILOT_MODE", "hang")
	var calls atomic.Int32
	sigs := &signalRecorder{}
	r := e.runner(t, func(o *Options) {
		o.Owns = func(int, string) bool { calls.Add(1); return false }
		o.Signal = sigs.signal
	})
	it := e.item(t, "abc", 1)
	start(t, r, it, 1)
	r.Cancel(it.ID, 1)
	if s := sigs.list(); !slices.Equal(s, []syscall.Signal{syscall.SIGTERM}) {
		t.Fatalf("signals = %v", s)
	}
	drive(t, r, true, r.idle)
	if calls.Load() != 0 {
		t.Errorf("ownership checks for a local run: %d", calls.Load())
	}
}

// A failed ownership check abandons the run even when completion evidence
// arrived while it was in flight (moving the run from termination to
// cleanup): no second check, no signal, the timeout outcome is kept and
// the slot is released.
func TestOwnershipFailureAcrossCompletion(t *testing.T) {
	e := newEnv(t)
	t.Setenv("FAKE_COPILOT_MODE", "hang")
	it := e.item(t, "abc", 1)
	startDetached(t, e, it)
	owns, sigs := newGatedOwns(), &signalRecorder{}
	r := e.runner(t, func(o *Options) {
		o.Owns = owns.owns
		o.Signal = sigs.signal
	})
	if adopted, _ := r.Recover(it.ID, "dev", e.meta(t, it.ID, 1)); !adopted {
		t.Fatal("not adopted")
	}
	before := owns.calls.Load()
	owns.gated.Store(true)
	e.clock.Advance(2 * time.Hour)
	if c := r.Check(); len(c) != 0 {
		t.Fatalf("completions = %+v", c)
	}
	owns.waitEntered(t)

	// Completion evidence arrives while the pre-TERM check is pending; the
	// wrapper's group is still alive, so cleanup would need a signal.
	exitFile := filepath.Join(e.store.ItemDir(it.ID), "runs", "1", item.ExitCodeFile)
	if err := os.WriteFile(exitFile, []byte("0\n"), 0o600); err != nil { // #nosec G703 -- test file
		t.Fatal(err)
	}
	c := r.Check()
	if len(c) != 1 || c[0].Run.Outcome != item.OutcomeFailed || c[0].Run.Error != "timeout after 1h" {
		t.Fatalf("completions = %+v", c)
	}
	if r.Busy() != 1 {
		t.Fatal("slot released before the pending check returned")
	}

	res := owns.resolve(t, r, 100*time.Millisecond, false)
	// A retry would now be verified and signal.
	owns.gated.Store(false)
	if c := r.HandleOwnership(res); len(c) != 0 {
		t.Fatalf("completions = %+v", c)
	}
	if r.Busy() != 0 {
		t.Fatal("slot not released after the failed check")
	}
	for range 5 {
		if c := r.Check(); len(c) != 0 {
			t.Fatalf("completions = %+v", c)
		}
		select {
		case o := <-r.OwnershipC():
			r.HandleOwnership(o)
		case <-time.After(20 * time.Millisecond):
		}
	}
	if n := owns.calls.Load(); n != before+1 {
		t.Errorf("ownership checks = %d, want %d (retried)", n, before+1)
	}
	if s := sigs.list(); len(s) != 0 {
		t.Errorf("signals sent after the failed check: %v", s)
	}
	if m := e.meta(t, it.ID, 1); m.Outcome != item.OutcomeFailed || m.Error != "timeout after 1h" {
		t.Errorf("persisted %+v", m)
	}
	if !strings.Contains(e.logs.String(), "ownership could not be verified") {
		t.Errorf("logs:\n%s", e.logs)
	}
}
