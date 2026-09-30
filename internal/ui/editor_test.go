package ui

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestEditorStopsOnShutdown runs the real Bubble Tea program: an editor
// handoff runs for as long as the user needs, but shutting down the UI ends
// the whole editor invocation, also when it ignores SIGTERM and when the
// configured editor is a wrapper that starts the real editor as a child.
func TestEditorStopsOnShutdown(t *testing.T) {
	// Each script runs after the editor recorded its PID; $CHILD is the
	// path a started child's PID is written to.
	for _, tc := range []struct {
		name   string
		script string
		child  bool
	}{
		{"editor exits on SIGTERM", "exec sleep 60", false},
		{"editor ignores SIGTERM", "trap '' TERM\nexec sleep 60", false},
		{"wrapper with a child", "sleep 60 &\necho $! >\"$CHILD.tmp\"; mv \"$CHILD.tmp\" \"$CHILD\"\nwait", true},
		{"wrapper with a child ignoring SIGTERM", "(trap '' TERM; exec sleep 60) &\necho $! >\"$CHILD.tmp\"; mv \"$CHILD.tmp\" \"$CHILD\"\nwait", true},
		{"wrapper and child ignoring SIGTERM", "trap '' TERM\nsleep 60 &\necho $! >\"$CHILD.tmp\"; mv \"$CHILD.tmp\" \"$CHILD\"\nwait", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			childFile := filepath.Join(dir, "child")
			editor := filepath.Join(dir, "fake editor")
			script := "#!/bin/sh\nCHILD=" + shQuote(childFile) + "\necho $$ >" + shQuote(pidFile+".tmp") + "\nmv " + shQuote(pidFile+".tmp") + " " + shQuote(pidFile) + "\n" + tc.script + "\n"
			if err := os.WriteFile(editor, []byte(script), 0o700); err != nil { // #nosec G306 -- executable test fixture
				t.Fatal(err)
			}
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
			p.Send(ExecMsg{Cmd: m.editorCmd(filepath.Join(dir, "report.md")), After: func(error) tea.Msg { return nil }})

			pids := []int{readPID(t, pidFile)}
			if tc.child {
				pids = append(pids, readPID(t, childFile))
			}
			t.Cleanup(func() {
				for _, pid := range pids {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})

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
			// The program returned only after the invocation was stopped;
			// an orphaned child may still need to be reaped by init.
			for _, pid := range pids {
				gone := false
				for range 100 {
					if gone = processGone(pid); gone {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				if !gone {
					t.Fatalf("process %d survived the shutdown", pid)
				}
			}
		})
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

// processGone reports whether pid no longer runs (it is absent or a zombie
// waiting to be reaped).
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	table, err := processTable()
	if err != nil {
		return false
	}
	p, ok := table[pid]
	return !ok || p.zombie
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
