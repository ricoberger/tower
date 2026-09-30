package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/notify"
)

// fakeCopilotPath returns the absolute path of the fake Copilot fixture.
func fakeCopilotPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("testdata", "fake-copilot.sh"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeRuns returns the runs and notifications configuration of engine tests:
// runs use the fake Copilot fixture (hanging by default, see fixtureEnv) and
// desktop notifications are disabled. Fixture processes of runs below
// stateDir are killed when the test ends.
func fakeRuns(t *testing.T, stateDir string) string {
	t.Helper()
	fixtureEnv(t, stateDir, "hang")
	return "runs:\n  command: '" + fakeCopilotPath(t) + "'\nnotifications:\n  enabled: false\n"
}

// fixtureEnv sets the fixture controls inherited by runs (mode, no delay,
// no control directory) and registers the cleanup of the fixture processes
// of runs below stateDir.
func fixtureEnv(t *testing.T, stateDir, mode string) {
	t.Helper()
	t.Setenv("FAKE_COPILOT_MODE", mode)
	t.Setenv("FAKE_COPILOT_DELAY", "0")
	t.Setenv("FAKE_COPILOT_DESCENDANT", "")
	t.Setenv("FAKE_COPILOT_EARLY", "")
	t.Setenv("FAKE_COPILOT_DIR", "")
	t.Cleanup(func() { killFixtures(stateDir) })
}

// killFixtures kills the process groups of all runs below stateDir (from
// their metadata and the fixture's evidence) and recorded descendants.
func killFixtures(stateDir string) {
	var targets []int
	runs, _ := filepath.Glob(filepath.Join(stateDir, "items", "*", "runs", "*"))
	for _, dir := range runs {
		var pids []int
		if data, err := os.ReadFile(filepath.Join(dir, "meta.yaml")); err == nil { // #nosec G304 -- test state
			for line := range strings.SplitSeq(string(data), "\n") {
				if v, ok := strings.CutPrefix(line, "pid: "); ok {
					if pid, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
						pids = append(pids, -pid)
					}
				}
			}
		}
		for _, f := range []string{"fake-pgid", "fake-descendant"} {
			data, err := os.ReadFile(filepath.Join(dir, f)) // #nosec G304 -- test state
			if err != nil {
				continue
			}
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if f == "fake-pgid" {
					pid = -pid
				}
				pids = append(pids, pid)
			}
		}
		for _, pid := range pids {
			if pid > 1 || pid < -1 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
				targets = append(targets, pid)
			}
		}
	}
	// Wait until the processes are gone so that no fixture writes into a
	// directory that is being removed.
	deadline := time.Now().Add(10 * time.Second)
	for _, p := range targets {
		for time.Now().Before(deadline) && !errors.Is(syscall.Kill(p, 0), syscall.ESRCH) {
			_ = syscall.Kill(p, syscall.SIGKILL)
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// deliveries records notification deliveries instead of showing them.
type deliveries struct {
	mu   sync.Mutex
	list []notify.Notification
	// block, if set, blocks deliveries until the context ends.
	block bool
	ch    chan notify.Notification
}

func newDeliveries() *deliveries { return &deliveries{ch: make(chan notify.Notification, 64)} }

func (d *deliveries) deliver(ctx context.Context, n notify.Notification) error {
	d.mu.Lock()
	d.list = append(d.list, n)
	block := d.block
	d.mu.Unlock()
	d.ch <- n
	if block {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (d *deliveries) all() []notify.Notification {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]notify.Notification(nil), d.list...)
}
