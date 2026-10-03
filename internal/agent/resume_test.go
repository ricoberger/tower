package agent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResumeBoundsInheritedOutputPipes(t *testing.T) {
	for _, wait := range []bool{false, true} {
		name := "launcher exits"
		if wait {
			name = "launcher cancelled"
		}
		t.Run(name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "child.pid")
			script := `sleep 10 & printf '%s' "$!" > "$0"; `
			if wait {
				script += "wait"
			} else {
				script += "exit 0"
			}
			a, s, _ := setup(t, "true {{.Prompt}} {{.SessionID}}", "sh -c "+shellQuote(script)+" "+shellQuote(pidFile)+" {{.SessionID}}")
			it := createTask(t, s)
			it.SessionID = "session"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- a.Resume(ctx, it) }()

			// Cancel only once the fake descendant holds the output pipes.
			var pid int
			deadline := time.Now().Add(5 * time.Second)
			for pid == 0 {
				data, _ := os.ReadFile(pidFile)
				pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
				if time.Now().After(deadline) {
					t.Fatal("fake descendant did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			child, err := os.FindProcess(pid)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = child.Kill() })
			if wait {
				cancel()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("expected an output-pipe or cancellation error")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("resume blocked on descendant output pipes")
			}
		})
	}
}
