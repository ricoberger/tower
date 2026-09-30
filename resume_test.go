package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/loginenv"
	"github.com/ricoberger/tower/internal/notify"
	"github.com/ricoberger/tower/internal/runner"
	"github.com/ricoberger/tower/internal/ui"
)

// fakeGhostty is a fake ghostty-new helper in its own bin directory. It
// records its arguments and environment and fails while the fail file
// exists.
type fakeGhostty struct {
	bin, dir string
}

func newFakeGhostty(t *testing.T) *fakeGhostty {
	t.Helper()
	g := &fakeGhostty{bin: filepath.Join(t.TempDir(), "login bin"), dir: t.TempDir()}
	if err := os.Mkdir(g.bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
d='` + g.dir + `'
if [ -e "$d/fail" ]; then echo "ghostty broke" >&2; exit 1; fi
n=$(ls "$d" | grep -c '^args\.')
: > "$d/args.$n"
for a in "$@"; do printf '%s\0' "$a" >> "$d/args.$n"; done
/usr/bin/env > "$d/env.$n"
echo "helper noise"
`
	if err := os.WriteFile(filepath.Join(g.bin, "ghostty-new"), []byte(script), 0o700); err != nil { // #nosec G306 -- executable test fixture
		t.Fatal(err)
	}
	return g
}

// calls returns the recorded argument lists in call order.
func (g *fakeGhostty) calls(t *testing.T) [][]string {
	t.Helper()
	var out [][]string
	for i := 0; ; i++ {
		data, err := os.ReadFile(filepath.Join(g.dir, "args."+strconv.Itoa(i))) // #nosec G304 -- test file
		if err != nil {
			return out
		}
		out = append(out, strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"))
	}
}

func (g *fakeGhostty) env(t *testing.T, n int) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(g.dir, "env."+strconv.Itoa(n))) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

func (g *fakeGhostty) setFail(t *testing.T, fail bool) {
	t.Helper()
	p := filepath.Join(g.dir, "fail")
	if fail {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	_ = os.Remove(p)
}

// resumeCLI is an in-process `tower resume` environment: the captured login
// environment provides the PATH that contains the fake helper.
type resumeCLI struct {
	rt      *runTest
	ghostty *fakeGhostty
	env     env
	mu      sync.Mutex
	errors  []string
}

func newResumeCLI(t *testing.T, rt *runTest) *resumeCLI {
	t.Helper()
	c := &resumeCLI{rt: rt, ghostty: newFakeGhostty(t)}
	vars := map[string]string{"PATH": "/usr/bin:/bin", "HOME": rt.dir, "CALLER_ONLY": "kept"}
	c.env = testEnv(rt.dir, vars, rt.t0)
	c.env.lookPath = func(string) (string, error) { return "", errors.New("the caller PATH must not be used") }
	c.env.captureEnv = func(context.Context) (loginenv.Env, error) {
		return loginenv.Env{Vars: map[string]string{"PATH": c.ghostty.bin + ":/usr/bin:/bin", "LOGIN_ONLY": "yes", "CALLER_ONLY": "login"}}, nil
	}
	c.env.resumeError = func(_ context.Context, _ config.LookPath, msg string) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.errors = append(c.errors, msg)
	}
	return c
}

func (c *resumeCLI) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runCLI(t, c.env, append([]string{"--config", c.rt.cfgPath, "resume"}, args...)...)
}

func (c *resumeCLI) notified() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.errors)
}

// newResumeTest is a run test whose configuration uses the bare helper
// name, resolved through the login PATH.
func newResumeTest(t *testing.T) (*runTest, *resumeCLI) {
	t.Helper()
	rt := newRunTest(t, 1, "ghostty:\n  command: ghostty-new\n  placement: split\n  direction: down\n")
	return rt, newResumeCLI(t, rt)
}

func finishedRun(n int, sid string, o item.Outcome, at time.Time) item.Run {
	fin := at.Add(time.Minute)
	code := 0
	return item.Run{
		Number: n, SessionID: sid, Reason: item.ReasonAuto, Skill: runner.Skill,
		QueuedAt: at, StartedAt: &at, FinishedAt: &fin, ExitCode: &code, Outcome: o,
	}
}

func resumedEntries(it *item.Item) []item.HistoryEntry {
	var out []item.HistoryEntry
	for _, h := range it.History {
		if h.Action == item.ActionResumedSession {
			out = append(out, h)
		}
	}
	return out
}

func TestParseResumeArgs(t *testing.T) {
	valid, _ := item.NewID(item.Key{Source: "dev", Fingerprint: "abc"}, 1)
	for _, tc := range []struct {
		args      []string
		placement string
		err       string
	}{
		{[]string{valid}, "", ""},
		{[]string{valid, "--placement", "tab"}, "tab", ""},
		{[]string{"--placement", "window", valid}, "window", ""},
		{[]string{"--placement=split", valid}, "split", ""},
		{nil, "", "expects an item ID"},
		{[]string{valid, "other"}, "", "exactly one item ID"},
		{[]string{"../etc"}, "", "invalid item ID"},
		{[]string{valid, "--placement", "pane"}, "", "--placement must be one of"},
		{[]string{valid, "--unknown"}, "", "flag provided but not defined"},
	} {
		var stderr syncBuffer
		gotID, placement, err := parseResumeArgs(&globalFlags{}, tc.args, &stderr)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q: err %v, want %q", tc.args, err, tc.err)
			}
			continue
		}
		if err != nil || gotID != valid || placement != tc.placement {
			t.Errorf("%q: %q %q %v", tc.args, gotID, placement, err)
		}
	}
}

func TestResumeCLI(t *testing.T) {
	rt, c := newResumeTest(t)
	ready := rt.seed("ready", item.StateNeedsYou, 2, nil,
		finishedRun(1, "sess-old", item.OutcomeFailed, rt.t0.Add(-time.Hour)),
		finishedRun(2, "sess-ready", item.OutcomeReady, rt.t0.Add(-30*time.Minute)))
	blocked := rt.seed("blocked", item.StateNeedsYou, 1, nil, finishedRun(1, "sess-blocked", item.OutcomeBlocked, rt.t0.Add(-time.Hour)))

	code, stdout, stderr := c.run(t, ready)
	if code != 0 || stdout != "resumed run 2 of "+ready+" in Ghostty (split)\n" {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	code, _, stderr = c.run(t, "--placement", "window", blocked)
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}
	code, _, stderr = c.run(t, blocked, "--placement", "tab")
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr)
	}

	st, err := item.Open(rt.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	readyDir, _ := st.ItemPath(ready)
	blockedDir, _ := st.ItemPath(blocked)
	copilot := fakeCopilotPath(t)
	want := [][]string{
		{"--placement", "split", "--direction", "down", "--working-dir", readyDir, "--title", "tower: Seeded ready",
			"--command", "'" + copilot + "' '--resume' 'sess-ready'"},
		{"--placement", "window", "--working-dir", blockedDir, "--title", "tower: Seeded blocked",
			"--command", "'" + copilot + "' '--resume' 'sess-blocked'"},
		{"--placement", "tab", "--working-dir", blockedDir, "--title", "tower: Seeded blocked",
			"--command", "'" + copilot + "' '--resume' 'sess-blocked'"},
	}
	if got := c.ghostty.calls(t); !slices.EqualFunc(got, want, slices.Equal) {
		t.Fatalf("helper calls\n%q\nwant\n%q", got, want)
	}
	// The helper runs with the merged login environment: the login PATH,
	// login-only variables and caller values.
	e := c.ghostty.env(t, 0)
	if e["PATH"] != c.ghostty.bin+":/usr/bin:/bin" || e["LOGIN_ONLY"] != "yes" || e["CALLER_ONLY"] != "kept" {
		t.Fatalf("helper env %v", e)
	}

	// The resume is recorded as an action on the resumed run and marks the
	// item seen, without changing its state.
	it := rt.item("ready")
	if h := resumedEntries(it); len(h) != 1 || h[0].Run != 2 || !h[0].At.Equal(rt.t0) || !it.Seen || it.State != item.StateNeedsYou {
		t.Fatalf("history %+v seen %v", it.History, it.Seen)
	}
	if h := resumedEntries(rt.item("blocked")); len(h) != 2 || h[0].Run != 1 {
		t.Fatalf("history %+v", h)
	}
	if n := c.notified(); len(n) != 0 {
		t.Fatalf("error notifications %q", n)
	}
}

func TestResumeCLIFailures(t *testing.T) {
	rt, c := newResumeTest(t)
	noRun := rt.seed("norun", item.StateNew, 0, nil)
	noSession := rt.seed("nosession", item.StateNeedsYou, 1, nil, item.Run{
		Number: 1, Reason: item.ReasonAuto, Skill: runner.Skill, QueuedAt: rt.t0, Outcome: item.OutcomeFailed,
	})
	missingMeta := rt.seed("nometa", item.StateNeedsYou, 3, nil, finishedRun(1, "s1", item.OutcomeReady, rt.t0))
	pid, _ := ownedProcess(t, "sess-exec")
	executing := rt.seed("exec", item.StatePreparing, 1, nil, runningRun(1, pid, "sess-exec", item.ReasonAuto, rt.t0))
	ready := rt.seed("ready", item.StateNeedsYou, 1, nil, finishedRun(1, "sess-ready", item.OutcomeReady, rt.t0))
	unknown, _ := item.NewID(item.Key{Source: "dev", Fingerprint: "gone"}, 1)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no run", []string{noRun}, "no preparation run"},
		{"no session", []string{noSession}, "no session"},
		{"missing run metadata", []string{missingMeta}, "metadata of its latest run 3 is missing"},
		{"executing", []string{executing}, "still executing"},
		{"unknown item", []string{unknown}, "load item"},
		{"invalid ID", []string{"not/an/id"}, "invalid item ID"},
		{"invalid placement", []string{ready, "--placement", "pane"}, "--placement must be one of"},
		{"extra argument", []string{ready, ready}, "exactly one item ID"},
		{"missing ID", nil, "expects an item ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(c.notified())
			code, stdout, stderr := c.run(t, tc.args...)
			if code != 1 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
			}
			n := c.notified()
			if len(n) != before+1 || !strings.Contains(n[len(n)-1], tc.want) {
				t.Fatalf("notifications %q", n)
			}
		})
	}
	if got := c.ghostty.calls(t); len(got) != 0 {
		t.Fatalf("helper launched: %q", got)
	}
	for _, fp := range []string{"norun", "nosession", "nometa", "exec"} {
		if h := resumedEntries(rt.item(fp)); len(h) != 0 {
			t.Fatalf("%s: history %+v", fp, h)
		}
	}

	// A run whose exit code was written is resumable although the engine
	// has not finalized it yet.
	if err := os.WriteFile(rt.runFile("exec", 1, item.ExitCodeFile), []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := c.run(t, executing); code != 0 {
		t.Fatalf("finished run: %s", stderr)
	}

	// A helper failure is reported and nothing is recorded.
	c.ghostty.setFail(t, true)
	code, _, stderr := c.run(t, ready)
	if code != 1 || !strings.Contains(stderr, "ghostty.command: ghostty-new failed") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if h := resumedEntries(rt.item("ready")); len(h) != 0 {
		t.Fatalf("history %+v", h)
	}
	c.ghostty.setFail(t, false)

	// An unresolvable helper is reported before anything is launched.
	c.env.captureEnv = func(context.Context) (loginenv.Env, error) {
		return loginenv.Env{Vars: map[string]string{"PATH": "/usr/bin:/bin"}}, nil
	}
	code, _, stderr = c.run(t, ready)
	if code != 1 || !strings.Contains(stderr, "ghostty.command: executable cannot be resolved") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}

	// A failed login-shell capture is reported.
	c.env.captureEnv = func(context.Context) (loginenv.Env, error) { return loginenv.Env{}, errors.New("shell timed out") }
	code, _, stderr = c.run(t, ready)
	if code != 1 || !strings.Contains(stderr, "recover the login shell environment: shell timed out") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if n := c.notified(); !strings.Contains(n[len(n)-1], "shell timed out") {
		t.Fatalf("notifications %q", n)
	}
}

func TestResumeCLIPersistenceFailure(t *testing.T) {
	rt, c := newResumeTest(t)
	ready := rt.seed("ready", item.StateNeedsYou, 1, nil, finishedRun(1, "sess-ready", item.OutcomeReady, rt.t0))
	dir := filepath.Join(rt.stateDir, "items", ready)
	if err := os.Chmod(dir, 0o500); err != nil { // #nosec G302 -- test makes the item directory read-only
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // #nosec G302 -- restore for cleanup
	code, _, stderr := c.run(t, ready)
	if code != 1 || !strings.Contains(stderr, "the Ghostty surface was opened, but recording the resume failed") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if len(c.ghostty.calls(t)) != 1 {
		t.Fatal("helper not launched")
	}
}

func TestResumeCLIInvalidConfig(t *testing.T) {
	rt, c := newResumeTest(t)
	if err := os.WriteFile(rt.cfgPath, []byte("state_dir: ./state\nghostty:\n  placement: diagonal\nsources: []\nruns:\n  concurrency: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	valid, _ := item.NewID(item.Key{Source: "dev", Fingerprint: "abc"}, 1)
	code, _, stderr := c.run(t, valid)
	if code != 1 || !strings.Contains(stderr, "invalid configuration") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	if n := c.notified(); len(n) != 1 || !strings.Contains(n[0], "invalid configuration") {
		t.Fatalf("notifications %q", n)
	}
	// The configuration is resolved with the login environment.
	c.env.lookup = func(string) (string, bool) { return "", false }
	c.env.captureEnv = func(context.Context) (loginenv.Env, error) {
		return loginenv.Env{Vars: map[string]string{"PATH": "/usr/bin", "TOWER_CONFIG": filepath.Join(rt.dir, "missing.yaml")}}, nil
	}
	code, _, stderr = runCLI(t, c.env, "resume", valid)
	if code != 1 || !strings.Contains(stderr, "missing.yaml") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
}

// Resuming while the engine runs does not need the instance lock and does
// not disturb the engine's writes to the same item.
func TestResumeCLIWhileEngineRuns(t *testing.T) {
	rt, c := newResumeTest(t)
	rt.setAlerts(rt.alert("aaa", "critical", time.Hour))
	h := rt.start(nil)
	rt.waitState("aaa", item.StateNeedsYou)
	rt.waitOutcome("aaa", 1, item.OutcomeReady)

	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if code, _, stderr := c.run(t, id("aaa")); code != 0 {
				t.Errorf("resume: %s", stderr)
			}
		})
	}
	for range 3 {
		h.doTick()
	}
	wg.Wait()
	it := rt.item("aaa")
	if len(resumedEntries(it)) != 3 || it.State != item.StateNeedsYou || !it.Seen {
		t.Fatalf("item %+v", it)
	}
	h.doTick()
	if it := rt.item("aaa"); len(resumedEntries(it)) != 3 {
		t.Fatalf("history lost: %+v", it.History)
	}
}

// TestResumeFromNotificationClick runs the exact click command of a
// notification through /bin/sh in a subprocess without a terminal, with a
// minimal PATH and a fake login shell whose interactive startup provides
// PATH and a configuration variable.
func TestResumeFromNotificationClick(t *testing.T) {
	// The Grafana-managed source is only valid with the login shell's
	// $GRAFANA_INSTANCES; its credential command must never run.
	rt := newRunTest(t, 1, "")
	g := newFakeGhostty(t)
	tokenMarker := filepath.Join(t.TempDir(), "token-command-ran")
	ready := rt.seed("ready", item.StateNeedsYou, 1, nil, finishedRun(1, "sess-ready", item.OutcomeReady, rt.t0))

	// A copilot on the login PATH is the configured bare runs.command.
	cfg := strings.Replace(readString(t, rt.cfgPath), "command: '"+fakeCopilotPath(t)+"'", "command: copilot", 1) + `  - name: prod
    type: alertmanager
    grafana_instance: prod
    grafana_alertmanager: grafana
ghostty:
  command: ghostty-new
`
	writeFixture(t, rt.cfgPath, cfg)
	if err := os.Symlink(fakeCopilotPath(t), filepath.Join(g.bin, "copilot")); err != nil {
		t.Fatal(err)
	}

	shellDir := t.TempDir()
	shell := filepath.Join(shellDir, "login shell")
	shellScript := `#!/bin/sh
[ "$1" = "-l" ] && [ "$2" = "-i" ] && [ "$3" = "-c" ] || exit 64
echo "motd noise"
echo "rc noise" >&2
export PATH='` + g.bin + `:/usr/bin:/bin'
export GRAFANA_INSTANCES='{"prod":{"url":"https://grafana.invalid","auth":{"tokenCommand":"touch ` + tokenMarker + `; echo ` + credentialSentinel + `"}}}'
exec /bin/sh -c "$4"
`
	if err := os.WriteFile(shell, []byte(shellScript), 0o700); err != nil { // #nosec G306 -- executable test fixture
		t.Fatal(err)
	}

	// Error notifications may only reach a fake terminal-notifier; it is
	// only on the caller's PATH.
	fakeBin := t.TempDir()
	notifierLog := filepath.Join(t.TempDir(), "notifier")
	if err := os.WriteFile(filepath.Join(fakeBin, "terminal-notifier"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >> '"+notifierLog+"'\n"), 0o700); err != nil { // #nosec G306 -- executable test fixture
		t.Fatal(err)
	}

	click, err := notify.ResumeCommand(os.Args[0], rt.cfgPath, ready)
	if err != nil {
		t.Fatal(err)
	}
	runClick := func() (string, error) {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", click) // #nosec G204 G702 -- the generated click command of the test binary
		cmd.Env = []string{
			"PATH=" + fakeBin + ":/usr/bin:/bin", "HOME=" + rt.dir, "SHELL=" + shell, "CALLER_ONLY=kept",
			"TOWER_TEST_RUN_MAIN=1", "TOWER_TEST_FAKE_BIN=" + fakeBin,
		}
		cmd.Dir = t.TempDir()
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := runClick()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	calls := g.calls(t)
	if len(calls) != 1 || !slices.Contains(calls[0], "'"+filepath.Join(g.bin, "copilot")+"' '--resume' 'sess-ready'") {
		t.Fatalf("helper calls %q", calls)
	}
	e := g.env(t, 0)
	if e["PATH"] != g.bin+":/usr/bin:/bin" || e["CALLER_ONLY"] != "kept" || e["SHELL"] != shell {
		t.Fatalf("helper env PATH=%q CALLER_ONLY=%q", e["PATH"], e["CALLER_ONLY"])
	}
	if strings.Contains(out, credentialSentinel) {
		t.Fatal("secret in the output")
	}
	if h := resumedEntries(rt.item("ready")); len(h) != 1 {
		t.Fatalf("history %+v", h)
	}
	if _, err := os.Stat(tokenMarker); err == nil {
		t.Fatal("a credential command was executed")
	}
	assertNoSentinel(t, rt.stateDir)

	// A failure is shown as an error notification without a click action.
	g.setFail(t, true)
	out, err = runClick()
	if err == nil || !strings.Contains(out, "ghostty-new failed") {
		t.Fatalf("%v: %s", err, out)
	}
	logged := readString(t, notifierLog)
	if !strings.Contains(logged, notify.TitleResumeFailed) || !strings.Contains(logged, "ghostty-new failed") ||
		strings.Contains(logged, "-execute") || strings.Contains(logged, credentialSentinel) {
		t.Fatalf("notification %q", logged)
	}
}

func TestAPIHint(t *testing.T) {
	for err, want := range map[error]string{
		ErrUnknownItem: "the item no longer exists or cannot be read",
		&ManualRunNotAllowedError{State: "snoozed"}: "cannot prepare now: the item is snoozed",
		ErrRunExecuting:                         "a preparation run is still executing; wait for it to finish",
		ErrItemDone:                             "the item is already done",
		ErrEngineStopped:                        "the engine is not running; restart tower",
		ErrPollInProgress:                       "a poll is already in progress",
		context.Canceled:                        "the request was cancelled",
		errors.New("persist item: disk full\n"): "request failed: persist item: disk full",
	} {
		if got := apiHint(err); got != want {
			t.Errorf("%v: %q", err, got)
		}
	}
}

// The resumable check of the TUI and the CLI is the same.
func TestResumableRuns(t *testing.T) {
	at := time.Now()
	if err := ui.Resumable(finishedRun(1, "s", item.OutcomeReady, at)); err != nil {
		t.Fatal(err)
	}
	if err := ui.Resumable(item.Run{Number: 1, Outcome: item.OutcomeRunning}); err == nil {
		t.Fatal("a run without a started session is resumable")
	}
}
