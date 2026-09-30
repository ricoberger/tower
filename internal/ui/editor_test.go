package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestEditorStopsOnShutdown runs the real Bubble Tea program: an editor
// handoff runs for as long as the user needs, but shutting down the UI ends
// the whole editor invocation, also when it ignores SIGTERM, when the
// configured editor is a wrapper that starts the real editor as a child and
// when the editor starts a child while it exits. The editor's process group
// is only signalled while the editor is not reaped yet, so the group ID
// cannot belong to an unrelated process.
func TestEditorStopsOnShutdown(t *testing.T) {
	// Each script runs after the editor recorded its PID; $CHILD is the
	// path a started child's PID is written to.
	const writeChild = `echo $! >"$CHILD.tmp"; mv "$CHILD.tmp" "$CHILD"`
	for _, tc := range []struct {
		name   string
		script string
		// child is set when the script starts a child before the
		// shutdown, lateChild when it starts one while shutting down.
		child, lateChild bool
	}{
		{"editor exits on SIGTERM", "exec sleep 60", false, false},
		{"editor ignores SIGTERM", "trap '' TERM\nexec sleep 60", false, false},
		{"wrapper with a child", "sleep 60 &\n" + writeChild + "\nwait", true, false},
		{"wrapper with a child ignoring SIGTERM", "(trap '' TERM; exec sleep 60) &\n" + writeChild + "\nwait", true, false},
		{"wrapper and child ignoring SIGTERM", "trap '' TERM\nsleep 60 &\n" + writeChild + "\nwait", true, false},
		{"editor starts a child on SIGTERM and exits", "trap 'sleep 60 & " + writeChild + "; exit 0' TERM\nwhile :; do sleep 1 & wait $!; done", false, true},
		{"editor starts a child ignoring SIGTERM on SIGTERM and exits", "trap '(trap \"\" TERM; exec sleep 60) & " + writeChild + "; exit 0' TERM\nwhile :; do sleep 1 & wait $!; done", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			childFile := filepath.Join(dir, "child")
			editor := writeEditor(t, dir, "CHILD="+shQuote(childFile)+"\necho $$ >"+shQuote(pidFile+".tmp")+"\nmv "+shQuote(pidFile+".tmp")+" "+shQuote(pidFile)+"\n"+tc.script)
			const stopDelay = 500 * time.Millisecond
			f := newFixture(t, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opts := f.opts
			opts.Editor = editor
			opts.EditorStopDelay = stopDelay
			m := New(ctx, opts)

			// Without an input reader the editor's stdin is /dev/null; the
			// terminal handoff is otherwise the one of a real session.
			p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutSignalHandler())
			done := make(chan error, 1)
			go func() {
				_, err := p.Run()
				done <- err
			}()
			cmd := m.editorCmd(filepath.Join(dir, "report.md"))
			signals := recordGroupSignals(cmd)
			p.Send(ExecMsg{Cmd: cmd, After: func(error) tea.Msg { return nil }})

			pid := readPID(t, pidFile)
			pids := []int{pid}
			if tc.child {
				pids = append(pids, readPID(t, childFile))
			}
			cleanupOnFailure(t, &pids)

			// Normal editor use is not time limited.
			time.Sleep(2 * stopDelay)
			for _, pid := range pids {
				if err := syscall.Kill(pid, 0); err != nil {
					t.Fatalf("process %d ended without a shutdown: %v", pid, err)
				}
			}
			select {
			case err := <-done:
				t.Fatalf("the program ended while the editor runs: %v", err)
			default:
			}
			if got := signals.get(); len(got) != 0 {
				t.Fatalf("signals before the shutdown: %v", got)
			}

			start := time.Now()
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, tea.ErrProgramKilled) {
					t.Fatalf("program: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("shutdown is blocked by the editor")
			}
			if took := time.Since(start); took > stopDelay+5*time.Second {
				t.Fatalf("shutdown took %s", took)
			}
			if tc.lateChild {
				pids = append(pids, readPID(t, childFile))
			}
			for _, pid := range pids {
				waitGone(t, pid)
			}
			got := signals.get()
			if len(got) == 0 || got[0].sig != syscall.SIGTERM || got[len(got)-1].sig != syscall.SIGKILL {
				t.Fatalf("signals %v, want SIGTERM first and SIGKILL last", got)
			}
			for _, s := range got {
				if s.pgid != pid || s.err != nil {
					t.Fatalf("signal %v: group %d of editor %d, editor PID held: %v", s.sig, s.pgid, pid, s.err)
				}
			}
		})
	}
}

// TestEditorExitSignalsNothing checks that an editor exiting by itself is
// neither signalled nor its group, so what it left running is left alone,
// and that its exit status is reported.
func TestEditorExitSignalsNothing(t *testing.T) {
	dir := t.TempDir()
	childFile := filepath.Join(dir, "child")
	editor := writeEditor(t, dir, "sleep 60 &\necho $! >"+shQuote(childFile+".tmp")+"\nmv "+shQuote(childFile+".tmp")+" "+shQuote(childFile)+"\nexit 3")
	cmd := newEditorCommand(t.Context(), time.Second, editor)
	signals := recordGroupSignals(cmd)
	if err := cmd.Run(); err == nil || err.Error() != "exit status 3" {
		t.Fatalf("Run: %v", err)
	}
	child := readPID(t, childFile)
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	if err := syscall.Kill(child, 0); err != nil {
		t.Fatalf("the editor's child was stopped: %v", err)
	}
	if got := signals.get(); len(got) != 0 {
		t.Fatalf("signals: %v", got)
	}
}

// TestEditorTerminalJobControl runs an editor on a pseudo terminal: the
// editor is the terminal's foreground job while it runs, suspending it with
// ctrl+z suspends tower like any job (or is a no-op without job control),
// continuing tower gives the editor the terminal back, and tower is the
// foreground job again once the editor exited.
func TestEditorTerminalJobControl(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shell bool
	}{
		{"tower run by a job control shell", true},
		{"tower without job control", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave := openPTY(t)
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			editor := writeEditor(t, dir, "echo $$ >"+shQuote(pidFile+".tmp")+"\nmv "+shQuote(pidFile+".tmp")+" "+shQuote(pidFile)+
				"\nread first\necho \"$first\" >"+shQuote(filepath.Join(dir, "first"))+
				"\nread second\necho \"$second\" >"+shQuote(filepath.Join(dir, "second")))
			role := "tower"
			if tc.shell {
				role = "shell"
			}
			tty, err := os.OpenFile(slave, os.O_RDWR, 0) // #nosec G304 -- pseudo terminal opened by the test
			if err != nil {
				t.Fatal(err)
			}
			helper := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEditorTerminalHelper$") // #nosec G204 G702 -- the test binary itself
			helper.Env = append(os.Environ(), "TOWER_UI_TEST_ROLE="+role, "TOWER_UI_TEST_DIR="+dir, "TOWER_UI_TEST_EDITOR="+editor)
			helper.Stdin, helper.Stdout, helper.Stderr = tty, tty, tty
			helper.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
			if err := helper.Start(); err != nil {
				t.Fatal(err)
			}
			_ = tty.Close()
			var out syncBuffer
			go func() { _, _ = io.Copy(&out, master) }()
			// exited is closed once the helper ended with waitErr.
			exited := make(chan struct{})
			var waitErr error
			go func() {
				waitErr = helper.Wait()
				close(exited)
			}()
			var pids []int
			t.Cleanup(func() {
				select {
				case <-exited:
				default:
					// The helper session and the editor's group are
					// still running: remove them.
					_ = syscall.Kill(-helper.Process.Pid, syscall.SIGKILL)
					for _, pid := range pids {
						_ = syscall.Kill(-pid, syscall.SIGKILL)
					}
					select {
					case <-exited:
					case <-time.After(10 * time.Second):
						t.Errorf("helper %d did not end", helper.Process.Pid)
					}
				}
				if t.Failed() {
					t.Logf("terminal output:\n%s", out.String())
				}
			})
			type2 := func(s string) {
				t.Helper()
				if _, err := master.WriteString(s); err != nil {
					t.Fatal(err)
				}
			}
			pids = append(pids, readPID(t, pidFile))

			// The editor reads the terminal: it is the foreground job.
			type2("one\n")
			waitFile(t, filepath.Join(dir, "first"), "one\n")
			type2("\x1a")
			if tc.shell {
				// The shell saw tower stop and continued it.
				waitFile(t, filepath.Join(dir, "stopped"), "stopped\n")
			}
			type2("two\n")
			waitFile(t, filepath.Join(dir, "second"), "two\n")
			select {
			case <-exited:
				if waitErr != nil {
					t.Fatalf("helper: %v", waitErr)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("tower did not return from the editor")
			}
			waitFile(t, filepath.Join(dir, "result"), "err=<nil> foreground=true\n")
		})
	}
}

// TestEditorTerminalHelper is the process side of
// TestEditorTerminalJobControl; it does nothing in a normal test run.
func TestEditorTerminalHelper(t *testing.T) {
	dir := os.Getenv("TOWER_UI_TEST_DIR")
	switch os.Getenv("TOWER_UI_TEST_ROLE") {
	case "tower":
		// tower handing the terminal to the editor.
		cmd := newEditorCommand(context.Background(), time.Second, os.Getenv("TOWER_UI_TEST_EDITOR"))
		cmd.SetStdin(os.Stdin)
		cmd.SetStdout(os.Stdout)
		cmd.SetStderr(os.Stderr)
		err := cmd.Run()
		result := fmt.Sprintf("err=%v foreground=%v\n", err, isForeground(0))
		if werr := os.WriteFile(filepath.Join(dir, "result"), []byte(result), 0o600); werr != nil { // #nosec G703 -- test directory
			t.Fatal(werr)
		}
	case "shell":
		// A job control shell running tower as a foreground job.
		tower := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestEditorTerminalHelper$") // #nosec G204 G702 -- the test binary itself
		tower.Env = append(os.Environ(), "TOWER_UI_TEST_ROLE=tower")
		tower.Stdin, tower.Stdout, tower.Stderr = os.Stdin, os.Stdout, os.Stderr
		tower.SysProcAttr = &syscall.SysProcAttr{Foreground: true, Ctty: 0}
		if err := tower.Start(); err != nil {
			t.Fatal(err)
		}
		pid := tower.Process.Pid
		for {
			var ws syscall.WaitStatus
			if _, err := syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil); errors.Is(err, syscall.EINTR) {
				continue
			} else if err != nil {
				t.Fatal(err)
			}
			if !ws.Stopped() {
				if ws.ExitStatus() != 0 {
					t.Fatalf("tower: %v", exitError(ws))
				}
				return
			}
			// "fg": the shell takes the terminal, then gives it back to
			// the stopped job and continues it.
			if err := setForeground(0, syscall.Getpgrp()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "stopped"), []byte("stopped\n"), 0o600); err != nil { // #nosec G703 -- test directory
				t.Fatal(err)
			}
			if err := setForeground(0, pid); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(-pid, syscall.SIGCONT); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// groupSignal is one signal sent to the editor's process group; err tells
// whether the editor's PID (the group ID) was still held at that time.
type groupSignal struct {
	pgid int
	sig  syscall.Signal
	err  error
}

type groupSignals struct {
	mu   sync.Mutex
	sent []groupSignal
}

func (g *groupSignals) get() []groupSignal {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.sent)
}

// recordGroupSignals records the group signals of cmd. With each signal it
// checks that the group ID still is the PID of the unreaped editor.
func recordGroupSignals(cmd *EditorCommand) *groupSignals {
	g := &groupSignals{}
	send := cmd.signalGroup
	cmd.signalGroup = func(pgid int, sig syscall.Signal) error {
		held := syscall.Kill(pgid, 0)
		g.mu.Lock()
		g.sent = append(g.sent, groupSignal{pgid: pgid, sig: sig, err: held})
		g.mu.Unlock()
		return send(pgid, sig)
	}
	return g
}

// writeEditor writes a fake editor shell script.
func writeEditor(t *testing.T, dir, script string) string {
	t.Helper()
	editor := filepath.Join(dir, "fake editor")
	if err := os.WriteFile(editor, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { // #nosec G306 -- executable test fixture
		t.Fatal(err)
	}
	return editor
}

// cleanupOnFailure kills the recorded processes if the test failed; a
// passing test verified that they are gone.
func cleanupOnFailure(t *testing.T, pids *[]int) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, pid := range *pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// waitGone waits for pid to be gone. The editor itself is reaped by the
// editor command; orphaned children are reaped by init.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatalf("process %d survived the shutdown", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitFile waits for path to have the content want.
func waitFile(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path) // #nosec G304 -- test file
		if err == nil && string(data) == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %q, %v; want %q", filepath.Base(path), data, err, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// readPID waits for a PID file written by the fake editor.
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil { // #nosec G304 -- test file
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was not written", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// syncBuffer is a bytes.Buffer safe for concurrent use.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
