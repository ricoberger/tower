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
// it, also when the editor ignores SIGTERM.
func TestEditorStopsOnShutdown(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		{"editor exits on SIGTERM", "exec sleep 60"},
		{"editor ignores SIGTERM", "trap '' TERM\nexec sleep 60"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			editor := filepath.Join(dir, "fake editor")
			script := "#!/bin/sh\necho $$ >" + shQuote(pidFile+".tmp") + "\nmv " + shQuote(pidFile+".tmp") + " " + shQuote(pidFile) + "\n" + tc.script + "\n"
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

			var pid int
			deadline := time.Now().Add(10 * time.Second)
			for pid == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the editor did not start")
				}
				if data, err := os.ReadFile(pidFile); err == nil {
					pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

			// Normal editor use is not time limited.
			time.Sleep(2 * stopDelay)
			if err := syscall.Kill(pid, 0); err != nil {
				t.Fatalf("the editor ended without a shutdown: %v", err)
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
				t.Fatalf("shutdown is blocked by the editor (signal probe: %v)", syscall.Kill(pid, 0))
			}
			if took := time.Since(start); took > stopDelay+5*time.Second {
				t.Fatalf("shutdown took %s", took)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("the editor survived the shutdown: %v", err)
			}
		})
	}
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
