package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/notify"
)

// tuiTower is a running `tower` in terminal UI mode over the real engine,
// store, runner and fake Copilot. Poll and refresh ticks are driven by the
// test; the engine and the UI read the run test's fake clock.
type tuiTower struct {
	t          *testing.T
	rt         *runTest
	d          *tuiDriver
	deliveries *deliveries
	tick, poll chan time.Time
	ticked     chan struct{}
	applied    chan struct{}
	exit       chan int
	stderr     syncBuffer
	engineLog  syncBuffer
	stopped    bool
}

// startTUI starts tower in terminal UI mode and waits for its startup
// round. deliver replaces the recording notifier when set.
func startTUI(t *testing.T, rt *runTest, deliver func(context.Context, notify.Notification) error) *tuiTower {
	t.Helper()
	tw := &tuiTower{
		t: t, rt: rt, d: newTUIDriver(t), deliveries: newDeliveries(),
		tick: make(chan time.Time), poll: make(chan time.Time),
		ticked: make(chan struct{}, 64), applied: make(chan struct{}, 64), exit: make(chan int, 1),
	}
	signal := func(ch chan struct{}) func() {
		return func() {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
	e := testEnv(rt.dir, map[string]string{"PATH": "/usr/bin:/bin"}, rt.t0)
	e.now = rt.clock.now
	e.lookPath = exec.LookPath
	e.deliver = tw.deliveries.deliver
	if deliver != nil {
		e.deliver = deliver
	}
	e.tui = tw.d.run
	e.engineOptions = func(o *engineOptions) {
		o.tickC, o.pollC = tw.tick, tw.poll
		monitor := time.NewTicker(20 * time.Millisecond)
		t.Cleanup(monitor.Stop)
		o.monitorC = monitor.C
		o.runner.Grace = 200 * time.Millisecond
		o.stderr = &tw.engineLog
		o.hooks = engineHooks{roundApplied: signal(tw.applied), ticked: signal(tw.ticked)}
	}
	go func() {
		tw.exit <- run(context.Background(), []string{"--config", rt.cfgPath}, &tw.stderr, &tw.stderr, e)
	}()
	select {
	case <-tw.d.running:
	case code := <-tw.exit:
		t.Fatalf("tower exited with %d: %s", code, tw.stderr.String())
	case <-time.After(15 * time.Second):
		t.Fatal("UI not started")
	}
	tw.wait(tw.applied, "startup round")
	t.Cleanup(tw.quit)
	return tw
}

func (tw *tuiTower) wait(ch chan struct{}, what string) {
	tw.t.Helper()
	select {
	case <-ch:
	case code := <-tw.exit:
		tw.stopped = true
		tw.t.Fatalf("tower exited with %d while waiting for %s: %s", code, what, tw.stderr.String())
	case <-time.After(10 * time.Second):
		tw.t.Fatalf("timed out waiting for %s\n%s", what, tw.engineLog.String())
	}
}

func (tw *tuiTower) send(ch chan time.Time) {
	tw.t.Helper()
	select {
	case ch <- time.Now():
	case <-time.After(10 * time.Second):
		tw.t.Fatal("engine loop not receiving")
	}
}

// refresh sends the regular refresh tick and waits until it was handled.
func (tw *tuiTower) refresh() {
	tw.t.Helper()
	tw.send(tw.tick)
	tw.wait(tw.ticked, "refresh tick")
}

// pollRound sends the regular poll tick and waits until its round was
// applied.
func (tw *tuiTower) pollRound() {
	tw.t.Helper()
	tw.send(tw.poll)
	tw.wait(tw.applied, "poll round")
}

// quit presses q and waits for tower to exit cleanly. It is idempotent so
// it can also run as cleanup after a failed test.
func (tw *tuiTower) quit() {
	tw.t.Helper()
	if tw.stopped {
		return
	}
	tw.stopped = true
	q := tuiKey("q")
	select {
	case tw.d.in <- q:
	case code := <-tw.exit:
		tw.t.Errorf("tower exited early with %d: %s", code, tw.stderr.String())
		return
	case <-time.After(10 * time.Second):
		tw.t.Error("UI not processing q")
		return
	}
	select {
	case code := <-tw.exit:
		if code != 0 {
			tw.t.Errorf("exit %d: %s", code, tw.stderr.String())
		}
	case <-time.After(15 * time.Second):
		tw.t.Error("tower did not exit")
	}
}

// row finds the list row whose title contains title and returns the group
// it is listed under and the raw (styled) row. The group is empty when the
// item is not listed.
func (tw *tuiTower) row(title string) (group, raw string) {
	screen, _, _ := tw.d.view()
	return listRow(screen, title)
}

func listRow(screen, title string) (group, raw string) {
	for _, line := range strings.Split(screen, "\n") {
		// The list is the left column (60 cells at the driver's width).
		left := ansi.Cut(line, 0, 60)
		plain := ansi.Strip(left)
		for _, l := range []string{"NEEDS YOU", "PREPARING", "INCOMING", "SNOOZED", "RESOLVED", "DONE"} {
			if strings.Contains(plain, l+" (") {
				group = l
			}
		}
		if strings.Contains(plain, title) {
			return group, left
		}
	}
	return "", ""
}

// waitRow waits until the item row is listed under group and its plain
// text contains every want.
func (tw *tuiTower) waitRow(title, group string, want ...string) {
	tw.t.Helper()
	tw.d.waitFor(title+" in "+group+" "+strings.Join(want, ","), func(s, _, _ string) bool {
		g, raw := listRow(s, title)
		if g != group {
			return false
		}
		for _, w := range want {
			if !strings.Contains(ansi.Strip(raw), w) {
				return false
			}
		}
		return true
	})
}

// waitScreen waits until the plain screen contains every want.
func (tw *tuiTower) waitScreen(what string, want ...string) {
	tw.t.Helper()
	tw.d.waitFor(what, func(s, _, _ string) bool {
		plain := ansi.Strip(s)
		for _, w := range want {
			if !strings.Contains(plain, w) {
				return false
			}
		}
		return true
	})
}

// selectItem moves the selection to the item with the given ID.
func (tw *tuiTower) selectItem(id string) {
	tw.t.Helper()
	tw.d.keys("g")
	for range 20 {
		if _, _, sel := tw.d.view(); sel == id {
			return
		}
		tw.d.keys("j")
	}
	tw.t.Fatalf("item %s not selectable", id)
}

func tuiTitle(fp string) string { return "Alert" + fp }

// TestTUILifecycle drives the alert lifecycle through the terminal UI:
// every scenario asserts what the list and preview show as well as the
// persisted state.
func TestTUILifecycle(t *testing.T) {
	t.Run("threshold", func(t *testing.T) {
		rt := newRunTest(t, 2, "")
		rt.setAlerts(rt.alert("young", "critical", time.Minute))
		tw := startTUI(t, rt, nil)
		tw.waitRow(tuiTitle("young"), "INCOMING", "prep in 4m")
		tw.refresh()
		if s := rt.state("young"); s != item.StateNew || len(rt.runs("young")) != 0 {
			t.Fatalf("state %s runs %+v before the threshold", s, rt.runs("young"))
		}
		tw.waitRow(tuiTitle("young"), "INCOMING")

		rt.clock.add(4 * time.Minute)
		tw.refresh()
		tw.waitRow(tuiTitle("young"), "NEEDS YOU", "ready")
		if len(rt.starts()) != 1 {
			t.Fatalf("starts %v", rt.starts())
		}
	})

	t.Run("restart re-attaches", func(t *testing.T) {
		rt := newRunTest(t, 1, "")
		rt.hold()
		rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
		tw := startTUI(t, rt, nil)
		tw.waitRow(tuiTitle("aaa"), "PREPARING", "⟳")
		pid := rt.wrapperPID("aaa", 1)
		rt.fixtureStarted("aaa", 1)
		tw.quit()
		if !pidAlive(pid) {
			t.Fatal("quitting stopped the run")
		}

		tw2 := startTUI(t, rt, nil)
		tw2.waitRow(tuiTitle("aaa"), "PREPARING", "⟳")
		if !strings.Contains(tw2.engineLog.String(), "run re-attached") {
			t.Fatalf("not re-attached:\n%s", tw2.engineLog.String())
		}
		rt.release("aaa")
		tw2.waitRow(tuiTitle("aaa"), "NEEDS YOU", "ready")
		if n := waitDeliveryFrom(t, tw2.deliveries); n.ItemID != id("aaa") {
			t.Fatalf("notification %+v", n)
		}
		if len(rt.runs("aaa")) != 1 || len(rt.starts()) != 1 {
			t.Fatalf("runs %+v starts %v", rt.runs("aaa"), rt.starts())
		}
	})

	t.Run("interrupted run is retried once", func(t *testing.T) {
		rt := newRunTest(t, 1, "")
		rt.hold()
		rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
		tw := startTUI(t, rt, nil)
		tw.waitRow(tuiTitle("aaa"), "PREPARING")
		pid := rt.wrapperPID("aaa", 1)
		rt.fixtureStarted("aaa", 1)
		tw.quit()
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		eventually(t, "run killed", func() bool { return !pidAlive(pid) })

		tw2 := startTUI(t, rt, nil)
		rt.waitOutcome("aaa", 1, item.OutcomeInterrupted)
		tw2.waitRow(tuiTitle("aaa"), "PREPARING")
		tw2.waitScreen("retry preview", "Preparing run 2")
		pid2 := rt.wrapperPID("aaa", 2)
		rt.fixtureStarted("aaa", 2)
		if r := rt.run("aaa", 2); r.RetryOf != 1 || r.Reason != item.ReasonRetry {
			t.Fatalf("retry %+v", r)
		}
		tw2.quit()
		_ = syscall.Kill(-pid2, syscall.SIGKILL)
		eventually(t, "retry killed", func() bool { return !pidAlive(pid2) })

		tw3 := startTUI(t, rt, nil)
		tw3.waitRow(tuiTitle("aaa"), "NEEDS YOU", "✗", "interrupted")
		tw3.waitScreen("failure preview", "interrupted while tower was stopped")
		tw3.refresh()
		if len(rt.runs("aaa")) != 2 {
			t.Fatalf("runs %+v", rt.runs("aaa"))
		}
	})

	t.Run("timeout", func(t *testing.T) {
		rt := newRunTest(t, 1, "")
		rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
		rt.mode("aaa", "hang")
		tw := startTUI(t, rt, nil)
		tw.waitRow(tuiTitle("aaa"), "PREPARING")
		pid := rt.wrapperPID("aaa", 1)
		rt.fixtureStarted("aaa", 1)

		rt.clock.add(20*time.Minute + time.Second)
		eventually(t, "TERM", func() bool { return strings.Contains(tw.engineLog.String(), "run timed out") })
		rt.clock.add(time.Second)
		tw.waitRow(tuiTitle("aaa"), "NEEDS YOU", "✗", "failed")
		tw.waitScreen("timeout preview", "timeout after 20m")
		eventually(t, "process group killed", func() bool { return syscall.Kill(-pid, 0) != nil })
		if r := rt.run("aaa", 1); r.Outcome != item.OutcomeFailed || r.Error != "timeout after 20m" {
			t.Fatalf("run %+v", r)
		}
	})

	t.Run("invalid and missing result", func(t *testing.T) {
		rt := newRunTest(t, 2, "")
		rt.setAlerts(rt.alert("inv", "critical", 2*time.Hour), rt.alert("mis", "critical", time.Hour))
		rt.mode("inv", "invalid")
		rt.mode("mis", "missing-result")
		tw := startTUI(t, rt, nil)
		tw.waitRow(tuiTitle("inv"), "NEEDS YOU", "✗", "failed")
		tw.waitRow(tuiTitle("mis"), "NEEDS YOU", "✗", "failed")
		inv := rt.waitOutcome("inv", 1, item.OutcomeFailed)
		if !strings.Contains(inv.Error, "result.json") {
			t.Fatalf("invalid run %+v", inv)
		}
		tw.selectItem(id("inv"))
		tw.waitScreen("validation error preview", firstLine(inv.Error))
		tw.selectItem(id("mis"))
		tw.waitScreen("missing result preview", "result.json is missing")
	})

	t.Run("source error and resolution", func(t *testing.T) {
		rt := newRunTest(t, 2, "")
		rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour), rt.alert("bbb", "critical", time.Hour))
		tw := startTUI(t, rt, nil)
		tw.waitRow(tuiTitle("aaa"), "NEEDS YOU", "ready")
		tw.waitRow(tuiTitle("bbb"), "NEEDS YOU", "ready")

		// A failing source keeps its items and shows the error.
		writeFixture(t, rt.alerts, "not json")
		tw.pollRound()
		tw.waitScreen("source error header", "dev ✗")
		tw.waitRow(tuiTitle("aaa"), "NEEDS YOU")
		tw.waitRow(tuiTitle("bbb"), "NEEDS YOU")
		if rt.state("aaa") != item.StateNeedsYou || rt.state("bbb") != item.StateNeedsYou {
			t.Fatal("a failing source resolved items")
		}

		// A successful poll without bbb resolves it; after the linger it is
		// done and hidden.
		rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
		tw.pollRound()
		tw.waitRow(tuiTitle("bbb"), "RESOLVED", "✓", "resolved")
		if _, raw := tw.row(tuiTitle("bbb")); !strings.Contains(raw, "\x1b[2m") && !strings.Contains(raw, ";2m") {
			t.Fatalf("resolved row not dimmed: %q", raw)
		}
		rt.clock.add(10 * time.Minute)
		tw.refresh()
		tw.d.waitFor("done item hidden", func(s, _, _ string) bool { g, _ := listRow(s, tuiTitle("bbb")); return g == "" })
		if rt.state("bbb") != item.StateDone || rt.state("aaa") != item.StateNeedsYou {
			t.Fatalf("aaa %s bbb %s", rt.state("aaa"), rt.state("bbb"))
		}
	})

	t.Run("reopen and new occurrence", func(t *testing.T) {
		rt := newRunTest(t, 1, "")
		rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
		tw := startTUI(t, rt, nil)
		tw.waitRow(tuiTitle("aaa"), "NEEDS YOU", "ready")

		resolveToDone := func() {
			t.Helper()
			rt.setAlerts()
			tw.pollRound()
			tw.waitRow(tuiTitle("aaa"), "RESOLVED")
			rt.clock.add(10 * time.Minute)
			tw.refresh()
			tw.d.waitFor("done item hidden", func(s, _, _ string) bool { g, _ := listRow(s, tuiTitle("aaa")); return g == "" })
		}

		// Re-firing within the reopen window reopens the same item; its next
		// run gets the previous report as context.
		resolveToDone()
		rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
		tw.pollRound()
		rt.waitOutcome("aaa", 2, item.OutcomeReady)
		tw.waitRow(tuiTitle("aaa"), "NEEDS YOU", "ready")
		tw.waitScreen("second run preview", "run 2")
		if it := rt.item("aaa"); it.Alert.Occurrences != 2 {
			t.Fatalf("occurrences %d", it.Alert.Occurrences)
		}
		if prompt := readString(t, rt.runFile("aaa", 2, item.PromptFile)); !strings.Contains(prompt, rt.runFile("aaa", 1, item.ReportFile)) {
			t.Fatal("the reopened run does not reference the previous report")
		}

		// Re-firing after the window creates a new item.
		resolveToDone()
		rt.clock.add(25 * time.Hour)
		tw.refresh()
		rt.setAlerts(rt.alert("aaa", "critical", time.Hour))
		tw.pollRound()
		next := strings.TrimSuffix(id("aaa"), "-1") + "-2"
		tw.d.waitFor("new occurrence listed", func(s, _, _ string) bool { g, _ := listRow(s, tuiTitle("aaa")); return g != "" })
		it := readItem(t, rt.stateDir, next)
		if it.PreviousItem == nil || *it.PreviousItem != id("aaa") {
			t.Fatalf("previous item %v", it.PreviousItem)
		}
		if rt.state("aaa") != item.StateDone {
			t.Fatal("the old occurrence was revived")
		}
	})

	t.Run("snoozed", func(t *testing.T) {
		rt := newRunTest(t, 1, "")
		start := rt.t0.Add(-2 * time.Hour)
		rt.setAlerts(alertJSON("zzz", "Alertzzz", start, "suppressed", nil, "silence-1"))
		tw := startTUI(t, rt, nil)
		tw.waitRow(tuiTitle("zzz"), "SNOOZED", "☾")
		rt.clock.add(time.Hour)
		tw.refresh()
		tw.pollRound()
		tw.waitRow(tuiTitle("zzz"), "SNOOZED")
		if len(rt.runs("zzz")) != 0 || len(rt.starts()) != 0 || rt.state("zzz") != item.StateSnoozed {
			t.Fatalf("runs %+v starts %v", rt.runs("zzz"), rt.starts())
		}
	})
}

// isBold reports whether a styled row is rendered bold (unseen).
func isBold(raw string) bool {
	return strings.Contains(raw, "\x1b[1m") || strings.Contains(raw, "\x1b[1;")
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

// TestTUINotificationFailure shows a failed desktop notification in the
// header while the run result is kept, until a later delivery succeeds.
func TestTUINotificationFailure(t *testing.T) {
	rt := newRunTest(t, 1, "")
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
	var mu sync.Mutex
	fail := true
	delivered := newDeliveries()
	tw := startTUI(t, rt, func(ctx context.Context, n notify.Notification) error {
		_ = delivered.deliver(ctx, n)
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return errors.New("terminal-notifier failed: exit status 1")
		}
		return nil
	})
	tw.waitRow(tuiTitle("aaa"), "NEEDS YOU", "ready")
	waitDeliveryFrom(t, delivered)
	tw.waitScreen("failure header", "notification ✗ terminal-notifier failed: exit status 1 ("+tuiTitle("aaa"))
	if r := rt.run("aaa", 1); r.Outcome != item.OutcomeReady || rt.state("aaa") != item.StateNeedsYou {
		t.Fatalf("run %+v", r)
	}

	mu.Lock()
	fail = false
	mu.Unlock()
	tw.d.keys("p")
	tw.d.waitFooter("preparation run requested")
	rt.waitOutcome("aaa", 2, item.OutcomeReady)
	waitDeliveryFrom(t, delivered)
	tw.d.waitFor("failure cleared", func(s, _, _ string) bool { return !strings.Contains(ansi.Strip(s), "notification ✗") })
}

// TestTUIExternalWritesDuringTransition resumes a session with `tower
// resume` from other processes while the engine resolves the same item.
// Both histories are kept, and the running UI shows the item as seen after
// its regular refresh, without a manual poll or a restart.
func TestTUIExternalWritesDuringTransition(t *testing.T) {
	rt, c := newResumeTest(t)
	rt.setAlerts(rt.alert("aaa", "critical", 2*time.Hour))
	tw := startTUI(t, rt, nil)
	tw.waitRow(tuiTitle("aaa"), "NEEDS YOU", "ready")
	if _, raw := tw.row(tuiTitle("aaa")); !isBold(raw) {
		t.Fatalf("unseen row not bold: %q", raw)
	}

	// Resumes from other processes race with engine transitions of the
	// same item: the alert disappears and re-fires on every poll round.
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			for range 2 {
				if code, _, stderr := c.run(t, id("aaa")); code != 0 {
					t.Errorf("resume: %s", stderr)
				}
			}
		})
	}
	firing := rt.alert("aaa", "critical", 2*time.Hour)
	for i := range 5 {
		if i%2 == 0 {
			rt.setAlerts()
		} else {
			rt.setAlerts(firing)
		}
		tw.pollRound()
	}
	wg.Wait()
	tw.refresh()

	it := rt.item("aaa")
	if len(resumedEntries(it)) != 6 || !it.Seen || it.State != item.StateResolved {
		t.Fatalf("item state %s seen %v history %+v", it.State, it.Seen, it.History)
	}
	resolved := 0
	for _, h := range it.History {
		if h.To == item.StateResolved {
			resolved++
		}
	}
	if resolved != 3 {
		t.Fatalf("engine transitions lost: %+v", it.History)
	}
	tw.d.waitFor("seen resolved row", func(s, _, _ string) bool {
		g, raw := listRow(s, tuiTitle("aaa"))
		return g == "RESOLVED" && !isBold(raw)
	})

	// Later refreshes keep both histories.
	tw.refresh()
	if it := rt.item("aaa"); len(resumedEntries(it)) != 6 || it.State != item.StateResolved {
		t.Fatalf("history lost: %+v", it.History)
	}
}
