package bounded

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func script(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cmd")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil { // #nosec G306 -- test executable
		t.Fatal(err)
	}
	return path
}

func run(t *testing.T, ctx context.Context, opts Options, path string, args ...string) error {
	t.Helper()
	cmd, cctx, cancel := Command(ctx, opts, path, args...)
	defer cancel()
	return Run(cctx, cmd)
}

func TestRun(t *testing.T) {
	if err := run(t, context.Background(), Options{}, script(t, "exit 0\n")); err != nil {
		t.Fatal(err)
	}
	err := run(t, context.Background(), Options{}, script(t, "exit 3\n"), "secret-arg")
	if err == nil || !strings.Contains(err.Error(), "cmd failed") || strings.Contains(err.Error(), "secret-arg") {
		t.Fatalf("err %v", err)
	}
	err = run(t, context.Background(), Options{}, filepath.Join(t.TempDir(), "missing"))
	if err == nil || !strings.HasPrefix(err.Error(), "start missing:") || strings.Count(err.Error(), "/") > 0 {
		t.Fatalf("err %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(t, ctx, Options{}, script(t, "sleep 5\n")); err == nil {
		t.Fatal("cancelled command succeeded")
	}
}

func TestTimeoutKillsGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	// The timeout leaves the script time to record its descendant even on a
	// loaded machine; the descendant runs far longer than the timeout.
	err := run(t, context.Background(), Options{Timeout: 2 * time.Second, WaitDelay: 100 * time.Millisecond},
		script(t, "sleep 30 &\necho $! > '"+pidFile+"'\nwait\n"))
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout not enforced")
	}
	assertGone(t, pidFile)
}

// A successful command whose descendant keeps the output pipe open returns
// after the wait delay and the descendant is killed.
func TestDescendantHoldingOutput(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd, cctx, cancel := Command(context.Background(), Options{Timeout: 10 * time.Second, WaitDelay: 200 * time.Millisecond},
		script(t, "sleep 30 &\necho $! > '"+pidFile+"'\nexit 0\n"))
	defer cancel()
	var out strings.Builder
	cmd.Stdout = &out
	start := time.Now()
	if err := Run(cctx, cmd); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("wait delay not enforced")
	}
	assertGone(t, pidFile)
}

func assertGone(t *testing.T, pidFile string) {
	t.Helper()
	data, _ := os.ReadFile(pidFile) // #nosec G304 -- test file
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		t.Fatal("no descendant pid")
	}
	deadline := time.Now().Add(3 * time.Second)
	for exec.CommandContext(t.Context(), "/bin/kill", "-0", pid).Run() == nil { // #nosec G204 -- test pid
		if time.Now().After(deadline) {
			t.Fatal("descendant survived")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
