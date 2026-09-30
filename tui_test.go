package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/notify"
	"github.com/ricoberger/tower/internal/ui"
)

// tuiDriver runs the real model asynchronously like Bubble Tea: commands run
// in goroutines, batches are expanded, editor handoffs run synchronously
// and the model is only touched by the driver loop. Tests send keys and
// inspect the model through the loop.
type tuiDriver struct {
	t       *testing.T
	in      chan tea.Msg
	inspect chan func(*ui.Model)
	running chan struct{}
}

func newTUIDriver(t *testing.T) *tuiDriver {
	return &tuiDriver{t: t, in: make(chan tea.Msg), inspect: make(chan func(*ui.Model)), running: make(chan struct{})}
}

func (d *tuiDriver) run(ctx context.Context, tm tea.Model) error {
	m := tm.(*ui.Model)
	done := make(chan struct{})
	defer close(done)
	msgs := make(chan tea.Msg)
	var deliver func(tea.Msg)
	exec := func(cmd tea.Cmd) {
		if cmd != nil {
			go func() { deliver(cmd()) }()
		}
	}
	deliver = func(msg tea.Msg) {
		switch msg := msg.(type) {
		case nil:
			return
		case tea.BatchMsg:
			for _, c := range msg {
				exec(c)
			}
			return
		case ui.ExecMsg:
			deliver(msg.After(msg.Cmd.Run()))
			return
		}
		select {
		case msgs <- msg:
		case <-done:
		}
	}
	update := func(msg tea.Msg) bool {
		if _, ok := msg.(tea.QuitMsg); ok {
			return false
		}
		_, cmd := m.Update(msg)
		exec(cmd)
		return true
	}
	exec(m.Init())
	update(tea.WindowSizeMsg{Width: 160, Height: 50})
	close(d.running)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case msg := <-msgs:
			if !update(msg) {
				return nil
			}
		case msg := <-d.in:
			if !update(msg) {
				return nil
			}
		case f := <-d.inspect:
			f(m)
		}
	}
}

func (d *tuiDriver) keys(ks ...string) {
	d.t.Helper()
	for _, k := range ks {
		select {
		case d.in <- tuiKey(k):
		case <-time.After(10 * time.Second):
			d.t.Fatalf("UI not processing key %q", k)
		}
	}
}

// tuiKey is the key press of a single character or "enter".
func tuiKey(k string) tea.KeyPressMsg {
	if k == "enter" {
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	}
	return tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
}

// view renders the screen on the driver loop.
func (d *tuiDriver) view() (screen, footer, selected string) {
	d.t.Helper()
	res := make(chan [3]string, 1)
	select {
	case d.inspect <- func(m *ui.Model) { res <- [3]string{m.Render(), m.Footer(), m.SelectedID()} }:
	case <-time.After(10 * time.Second):
		d.t.Fatal("UI loop not responding")
	}
	r := <-res
	return r[0], r[1], r[2]
}

func (d *tuiDriver) waitFor(what string, cond func(screen, footer, selected string) bool) {
	d.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		s, f, sel := d.view()
		if cond(s, f, sel) {
			return
		}
		if time.Now().After(deadline) {
			d.t.Fatalf("timed out waiting for %s\nfooter: %q\n%s", what, f, s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (d *tuiDriver) waitFooter(want string) {
	d.t.Helper()
	d.waitFor("footer "+want, func(_, f, _ string) bool { return strings.Contains(f, want) })
}

// fakeEditor records every invocation's arguments.
func fakeEditor(t *testing.T) (path string, calls func() [][]string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "log")
	path = filepath.Join(dir, "editor")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\" >> '" + log + "'; done\nprintf '\\n' >> '" + log + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { // #nosec G306 -- executable test fixture
		t.Fatal(err)
	}
	return path, func() [][]string {
		data, _ := os.ReadFile(log) // #nosec G304 -- test file
		var out [][]string
		for line := range strings.SplitSeq(strings.TrimSuffix(string(data), "\n"), "\n") {
			if line != "" {
				out = append(out, strings.Split(strings.TrimSuffix(line, "\x00"), "\x00"))
			}
		}
		return out
	}
}

// TestTUIWithEngine drives the terminal UI over the real engine, store,
// runner and fake Copilot: progress, notification, report, resume, manual
// run, poll, dismissal, the instance lock and quitting while a run is
// detached.
func TestTUIWithEngine(t *testing.T) {
	g := newFakeGhostty(t)
	editor, editorCalls := fakeEditor(t)
	rt := newRunTest(t, 2, "editor: '"+editor+"'\nghostty:\n  command: '"+filepath.Join(g.bin, "ghostty-new")+"'\n")
	rt.setAlerts(rt.alert("aaa", "critical", time.Hour), rt.alert("bbb", "warning", time.Hour))
	rt.hold()

	d := newTUIDriver(t)
	deliveries := newDeliveries()
	e := testEnv(rt.dir, map[string]string{"PATH": "/usr/bin:/bin"}, rt.t0)
	e.lookPath = exec.LookPath
	e.deliver = deliveries.deliver
	e.tui = d.run
	var stderr syncBuffer
	exit := make(chan int, 1)
	go func() {
		exit <- run(context.Background(), []string{"--config", rt.cfgPath}, &stderr, &stderr, e)
	}()
	select {
	case <-d.running:
	case code := <-exit:
		t.Fatalf("tower exited with %d: %s", code, stderr.String())
	case <-time.After(15 * time.Second):
		t.Fatal("UI not started")
	}

	// A second instance fails on the instance lock.
	code, _, errOut := runCLI(t, testEnv(rt.dir, nil, rt.t0), "--config", rt.cfgPath)
	if code != 1 || !strings.Contains(errOut, filepath.Join(rt.stateDir, "tower.lock")) {
		t.Fatalf("second instance: %d %q", code, errOut)
	}

	// Both runs execute; the critical item is selected first and shows its
	// progress.
	aaa, bbb := id("aaa"), id("bbb")
	rt.fixtureStarted("aaa", 1)
	rt.fixtureStarted("bbb", 1)
	d.waitFor("preparing preview", func(s, _, sel string) bool {
		return sel == aaa && strings.Contains(s, "Preparing run 1") && strings.Contains(s, "running 2/2")
	})

	// Resuming an executing run asks for confirmation; n cancels.
	d.keys("c")
	d.waitFor("resume confirmation", func(s, _, _ string) bool { return strings.Contains(s, "still executing") })
	d.keys("n")
	d.waitFooter("cancelled")
	if len(g.calls(t)) != 0 {
		t.Fatal("helper launched without confirmation")
	}

	// The run finishes: the preview shows the result and the engine
	// notifies.
	rt.release("aaa")
	d.waitFor("ready preview", func(s, _, _ string) bool {
		return strings.Contains(s, "Fake summary of the alert.") && strings.Contains(s, "Create the fix?")
	})
	if n := waitDeliveryFrom(t, deliveries); n.ItemID != aaa || n.Outcome != item.OutcomeReady {
		t.Fatalf("notification %+v", n)
	}

	// Enter opens the report in the editor and records it.
	d.keys("enter")
	d.waitFooter("opened report of run 1")
	if calls := editorCalls(); len(calls) != 1 || !slices.Equal(calls[0], []string{rt.runFile("aaa", 1, item.ReportFile)}) {
		t.Fatalf("editor calls %q", calls)
	}
	if it := rt.item("aaa"); !it.Seen || it.History[len(it.History)-1].Action != item.ActionOpenedReport {
		t.Fatalf("item %+v", it)
	}

	// c resumes the finished session in Ghostty.
	d.keys("c")
	d.waitFooter("resumed run 1 in Ghostty (split)")
	calls := g.calls(t)
	if len(calls) != 1 || !slices.Contains(calls[0], "'"+fakeCopilotPath(t)+"' '--resume' '"+rt.run("aaa", 1).SessionID+"'") {
		t.Fatalf("helper calls %q", calls)
	}
	if h := resumedEntries(rt.item("aaa")); len(h) != 1 || h[0].Run != 1 {
		t.Fatalf("history %+v", rt.item("aaa").History)
	}

	// p requests a second run whose prompt references the first report.
	d.keys("p")
	d.waitFooter("preparation run requested")
	rt.waitOutcome("aaa", 2, item.OutcomeReady)
	if prompt := readString(t, rt.runFile("aaa", 2, item.PromptFile)); !strings.Contains(prompt, rt.runFile("aaa", 1, item.ReportFile)) {
		t.Fatal("manual run prompt does not reference the previous report")
	}
	d.waitFor("run 2 preview", func(s, _, _ string) bool { return strings.Contains(s, "run 2") })

	// r polls all sources now.
	d.keys("r")
	d.waitFor("poll result", func(_, f, _ string) bool {
		return strings.Contains(f, "poll started") || strings.Contains(f, "a poll is already in progress")
	})

	// An action recorded by another process (a notification click) shows
	// up after the engine's refresh.
	st, err := item.Open(rt.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendAction(aaa, item.ActionResumedSession, 2, rt.t0); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	eventually(t, "external action kept", func() bool { return len(resumedEntries(rt.item("aaa"))) == 2 })

	// x asks for confirmation; y dismisses.
	d.keys("x")
	d.waitFor("dismiss confirmation", func(s, _, _ string) bool { return strings.Contains(s, "Dismiss") })
	d.keys("y")
	d.waitFooter("dismissed")
	rt.waitState("aaa", item.StateDone)
	d.waitFor("selection moves on", func(_, _, sel string) bool { return sel == bbb })

	// Quitting stops the engine but leaves the detached run executing.
	pid := rt.wrapperPID("bbb", 1)
	d.keys("q")
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("tower did not exit")
	}
	if !pidAlive(pid) {
		t.Fatal("the detached run was stopped")
	}
	if r := rt.run("bbb", 1); r.Outcome != item.OutcomeRunning {
		t.Fatalf("bbb run %+v", r)
	}
	if s := stderr.String(); s != "" {
		t.Fatalf("stderr %q", s)
	}
	if log := readString(t, filepath.Join(rt.stateDir, "tower.log")); !strings.Contains(log, "tower engine starting (tui)") {
		t.Fatalf("log %q", log)
	}
}

func waitDeliveryFrom(t *testing.T, d *deliveries) notify.Notification {
	t.Helper()
	select {
	case n := <-d.ch:
		return n
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a notification")
	}
	return notify.Notification{}
}

// TestTUIStartupFailures covers configuration and UI errors of the default
// mode.
func TestTUIStartupFailures(t *testing.T) {
	rt := newRunTest(t, 1, "")
	e := testEnv(rt.dir, nil, rt.t0)
	var mu sync.Mutex
	calls := 0
	e.tui = func(ctx context.Context, m tea.Model) error {
		mu.Lock()
		calls++
		mu.Unlock()
		return context.DeadlineExceeded
	}
	code, _, stderr := runCLI(t, e, "--config", rt.cfgPath)
	if code != 1 || !strings.Contains(stderr, "terminal UI") || calls != 1 {
		t.Fatalf("code %d stderr %q calls %d", code, stderr, calls)
	}
	// The instance lock is released after the UI failed.
	e.tui = func(context.Context, tea.Model) error { return nil }
	if code, _, stderr := runCLI(t, e, "--config", rt.cfgPath); code != 0 {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
}
