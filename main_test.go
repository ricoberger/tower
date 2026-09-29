package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/config"
	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/reconcile"

	"gopkg.in/yaml.v3"
)

// TestMain lets subprocess tests re-execute the test binary as tower.
func TestMain(m *testing.M) {
	if os.Getenv("TOWER_TEST_RUN_MAIN") == "1" {
		os.Args = append([]string{"tower"}, strings.Fields(os.Getenv("TOWER_TEST_ARGS"))...)
		main()
		return
	}
	os.Exit(m.Run())
}

const credentialSentinel = "SENTINEL-token-7f3a9c"

// syncBuffer is a goroutine-safe bytes.Buffer.
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

func found(f string) (string, error) { return "/usr/bin/" + f, nil }

func missing(string) (string, error) { return "", errors.New("not found") }

// testEnv returns an environment rooted at dir.
func testEnv(dir string, vars map[string]string, now time.Time) env {
	return env{
		lookup: func(k string) (string, bool) {
			v, ok := vars[k]
			return v, ok
		},
		getwd:    func() (string, error) { return dir, nil },
		now:      func() time.Time { return now },
		lookPath: found,
	}
}

func runCLI(t *testing.T, e env, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), args, &stdout, &stderr, e)
	return code, stdout.String(), stderr.String()
}

const fixtureAlert = `{
  "fingerprint": "abc123",
  "labels": {"alertname": "KubePodCrashLooping", "namespace": "core", "pod": "api-1", "severity": "critical"},
  "annotations": {"runbook_url": "https://runbooks.example.com/crashloop"},
  "startsAt": "2020-01-01T00:00:00Z",
  "updatedAt": "2020-01-01T00:00:00Z",
  "endsAt": "2099-01-01T00:00:00Z",
  "generatorURL": "https://prometheus.example.com/graph",
  "status": {"state": "active", "silencedBy": [], "inhibitedBy": []},
  "receivers": [{"name": "team"}]
}`

// setup writes a config with a file source into a new temp directory.
func setup(t *testing.T, extra string) (dir, cfgPath, fixture, stateDir string) {
	t.Helper()
	dir = t.TempDir()
	cfgPath = filepath.Join(dir, "config.yaml")
	fixture = filepath.Join(dir, "alerts.json")
	stateDir = filepath.Join(dir, "state")
	content := `state_dir: ./state
sources:
  - name: dev
    type: file
    path: ./alerts.json
  - name: legacy
    type: alertmanager
    url: https://alertmanager.example.com
    auth:
      type: bearer
      token: $TOWER_TEST_TOKEN
runs:
  command: sh
` + extra
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, fixture, "["+fixtureAlert+"]")
	return
}

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestVersion(t *testing.T) {
	e := env{
		lookup:   func(string) (string, bool) { return "", false },
		getwd:    func() (string, error) { return "", errors.New("no cwd") },
		now:      time.Now,
		lookPath: missing,
	}
	for _, args := range [][]string{{"version"}, {"--config", "/does/not/exist.yaml", "version"}} {
		code, out, stderr := runCLI(t, e, args...)
		if code != 0 || out != "tower dev (commit unknown, built unknown)\n" || stderr != "" {
			t.Fatalf("%v: code=%d out=%q err=%q", args, code, out, stderr)
		}
	}
	if code, _, _ := runCLI(t, e, "version", "extra"); code != 1 {
		t.Fatal("extra arguments must fail")
	}
}

func TestUsageErrors(t *testing.T) {
	e := testEnv(t.TempDir(), nil, time.Now())
	for _, args := range [][]string{{"bogus"}, {"config"}, {"config", "bogus"}, {"--unknown"}, {"prune", "extra"}, {"config", "init", "extra"}} {
		if code, _, _ := runCLI(t, e, args...); code != 1 {
			t.Errorf("%v: code=%d", args, code)
		}
	}
}

func TestLogLevel(t *testing.T) {
	dir, cfgPath, _, _ := setup(t, "")
	e := testEnv(dir, map[string]string{"TOWER_TEST_TOKEN": credentialSentinel}, time.Now())
	for _, lvl := range []string{"debug", "info", "warn", "error"} {
		if code, _, stderr := runCLI(t, e, "--config", cfgPath, "--log-level", lvl, "config", "validate"); code != 0 {
			t.Errorf("%s: code=%d %s", lvl, code, stderr)
		}
	}
	code, _, stderr := runCLI(t, e, "--config", cfgPath, "--log-level", "verbose", "config", "validate")
	if code != 1 || !strings.Contains(stderr, "--log-level") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	// Global flags are accepted after the subcommand as well.
	code, _, stderr = runCLI(t, e, "config", "validate", "--config", cfgPath, "--log-level", "loud")
	if code != 1 || !strings.Contains(stderr, "--log-level") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := runCLI(t, e, "--config", cfgPath, "--log-level", "loud"); code != 1 {
		t.Fatal("engine must reject invalid log level")
	}
}

func TestConfigInitAndValidate(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	e := testEnv(cwd, map[string]string{"HOME": home}, time.Now())

	code, out, stderr := runCLI(t, e, "config", "init")
	want := filepath.Join(home, ".config", "tower", "config.yaml")
	if code != 0 || !strings.Contains(out, want) {
		t.Fatalf("code=%d out=%q err=%q", code, out, stderr)
	}
	data, err := os.ReadFile(want) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), home) || !strings.Contains(string(data), "$HOME/") {
		t.Fatal("config init must write literal $HOME references")
	}
	// Never overwrite.
	if err := os.WriteFile(want, []byte("# mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI(t, e, "config", "init"); code != 1 || !strings.Contains(stderr, "exists") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if got, _ := os.ReadFile(want); string(got) != "# mine\n" { // #nosec G304 -- test file
		t.Fatal("existing config modified")
	}

	// TOWER_CONFIG selects the path (relative to the CWD) and --config wins.
	e = testEnv(cwd, map[string]string{"HOME": home, "TOWER_CONFIG": "sel/env.yaml"}, time.Now())
	if code, _, _ := runCLI(t, e, "config", "init"); code != 0 {
		t.Fatal("init via TOWER_CONFIG failed")
	}
	if _, err := os.Stat(filepath.Join(cwd, "sel", "env.yaml")); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runCLI(t, e, "config", "init", "--config", "~/flag.yaml"); code != 0 {
		t.Fatal("init via --config failed")
	}
	if _, err := os.Stat(filepath.Join(home, "flag.yaml")); err != nil {
		t.Fatal(err)
	}

	// The generated config validates; missing executables are warnings only.
	e.lookPath = missing
	code, out, _ = runCLI(t, e, "config", "validate")
	if code != 0 || !strings.Contains(out, "valid (2 warning(s))") || !strings.Contains(out, "runs.command") || !strings.Contains(out, "ghostty.command") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	e.lookPath = found
	code, out, _ = runCLI(t, e, "config", "validate")
	if code != 0 || !strings.Contains(out, "valid (0 warning(s))") {
		t.Fatalf("code=%d out=%q", code, out)
	}

	// Missing config file.
	if code, _, stderr := runCLI(t, e, "--config", "/nope/c.yaml", "config", "validate"); code != 1 || !strings.Contains(stderr, "config init") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	// No HOME for the default location.
	e = testEnv(cwd, map[string]string{}, time.Now())
	if code, _, stderr := runCLI(t, e, "config", "validate"); code != 1 || !strings.Contains(stderr, "HOME") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}

func TestConfigValidateErrorsAndSecrets(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "c.yaml")
	if err := os.WriteFile(cfgPath, []byte(`state_dir: /tmp/x
sources:
  - name: a
    type: alertmanager
    url: https://x
    auth:
      type: bearer
      token: $TOK
      token_file: $TOK
alerts:
  poll_interval: $TOK
`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := testEnv(dir, map[string]string{"TOK": credentialSentinel}, time.Now())
	code, out, stderr := runCLI(t, e, "--config", cfgPath, "config", "validate")
	if code != 1 || !strings.Contains(out, "error:") {
		t.Fatalf("code=%d out=%q", code, out)
	}
	if strings.Contains(out+stderr, credentialSentinel) {
		t.Fatal("validation output discloses a credential")
	}
	// Validation does not need the engine lock.
	dir2, cfg2, _, state2 := setup(t, "")
	st, err := item.Open(state2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	l, err := st.TryLockInstance()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	e = testEnv(dir2, map[string]string{"TOWER_TEST_TOKEN": "x"}, time.Now())
	if code, out, stderr := runCLI(t, e, "--config", cfg2, "config", "validate"); code != 0 {
		t.Fatalf("validate while locked: %d %s %s", code, out, stderr)
	}
}

func TestEngineRequiresRunsCommand(t *testing.T) {
	dir, cfgPath, _, stateDir := setup(t, "")
	e := testEnv(dir, map[string]string{"TOWER_TEST_TOKEN": "x"}, time.Now())
	e.lookPath = missing
	code, _, stderr := runCLI(t, e, "--config", cfgPath)
	if code != 1 || !strings.Contains(stderr, "runs.command") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "items")); !os.IsNotExist(err) {
		t.Fatal("engine must fail before touching state")
	}
	// A missing ghostty.command alone is only a warning (checked in the
	// integration test through a successful start).
}

func loadCfg(t *testing.T, path string, vars map[string]string) *config.Config {
	t.Helper()
	cfg, report, err := config.Load(path, func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	if err != nil || !report.OK() {
		t.Fatalf("load config: %v %v", err, report.Errors)
	}
	return cfg
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readItem(t *testing.T, stateDir, id string) *item.Item {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "items", id, "item.yaml")) // #nosec G304 -- test file
	if err != nil {
		return nil
	}
	var it item.Item
	if err := yaml.Unmarshal(data, &it); err != nil {
		return nil
	}
	return &it
}

func seedItem(t *testing.T, st *item.Store, fp string, state item.State, updated time.Time) string {
	t.Helper()
	k := item.Key{Source: "dev", Fingerprint: fp}
	id, _ := item.NewID(k, 1)
	it := &item.Item{
		Version: item.Version, ID: id, Type: item.TypeAlert, State: state, Title: "seed",
		CreatedAt: updated, UpdatedAt: updated,
		Source: item.SourceRef{Name: "dev", Fingerprint: fp},
		Alert:  item.AlertInfo{StartsAt: updated, LastSeenAt: updated, Status: item.AlertResolved, ResolvedAt: &updated, Occurrences: 1},
	}
	if err := st.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestApplyChangeKeepsTimestampsMonotonic verifies that applying a change
// computed before a concurrently persisted user action keeps the history
// chronological and never moves updated_at backwards (review round 1, B6).
func TestApplyChangeKeepsTimestampsMonotonic(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	change := func(t *testing.T, st *item.Store, id string) reconcile.Change {
		t.Helper()
		it, err := st.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		it.State = item.StateResolved
		it.UpdatedAt = now
		e := item.HistoryEntry{At: now, From: item.StateNew, To: item.StateResolved, Reason: "alert no longer firing"}
		it.History = item.AppendHistory(it.History, e)
		return reconcile.Change{ItemID: id, Item: *it, Appended: []item.HistoryEntry{e}}
	}
	chronological := func(t *testing.T, got *item.Item) {
		t.Helper()
		for i := 1; i < len(got.History); i++ {
			if got.History[i].At.Before(got.History[i-1].At) {
				t.Errorf("history out of order: %v then %v", got.History[i-1].At, got.History[i].At)
			}
		}
	}

	t.Run("concurrent newer action", func(t *testing.T) {
		st, err := item.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		id := seedItem(t, st, "fp1", item.StateNew, now.Add(-time.Hour))
		ch := change(t, st, id)
		actionAt := now.Add(time.Millisecond)
		if _, err := st.AppendAction(id, item.ActionOpenedReport, 1, actionAt); err != nil {
			t.Fatal(err)
		}
		if err := (&engine{store: st}).applyChange(ch); err != nil {
			t.Fatal(err)
		}
		got, err := st.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		chronological(t, got)
		if got.UpdatedAt.Before(actionAt) {
			t.Errorf("updated_at went backwards: %v < %v", got.UpdatedAt, actionAt)
		}
		n := len(got.History)
		if n < 2 || got.History[n-2].Action != item.ActionOpenedReport || got.History[n-1].To != item.StateResolved || !got.Seen {
			t.Fatalf("concurrent action lost or transition missing: %+v", got.History)
		}
		if got.State != item.StateResolved {
			t.Fatalf("state = %s", got.State)
		}
	})
	t.Run("no concurrent write keeps the computed time", func(t *testing.T) {
		st, err := item.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = st.Close() }()
		id := seedItem(t, st, "fp1", item.StateNew, now.Add(-time.Hour))
		if err := (&engine{store: st}).applyChange(change(t, st, id)); err != nil {
			t.Fatal(err)
		}
		got, err := st.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		chronological(t, got)
		if !got.UpdatedAt.Equal(now) || !got.History[len(got.History)-1].At.Equal(now) {
			t.Fatalf("updated_at = %v, history = %+v", got.UpdatedAt, got.History)
		}
	})
}

func TestEngineIntegration(t *testing.T) {
	dir, cfgPath, fixture, stateDir := setup(t, "ghostty:\n  command: /nonexistent/ghostty-new\nretention:\n  done_after: 24h\n")
	vars := map[string]string{"TOWER_TEST_TOKEN": credentialSentinel}
	cfg := loadCfg(t, cfgPath, vars)

	// Seed: an old done item (pruned at startup), a recent done item (kept)
	// and a corrupt item (skipped, untouched).
	st, err := item.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	oldDone := seedItem(t, st, "old", item.StateDone, time.Now().Add(-48*time.Hour))
	recentDone := seedItem(t, st, "recent", item.StateDone, time.Now().Add(-time.Hour))
	corruptDir := filepath.Join(stateDir, "items", "alert-dev-corrupt-1")
	if err := os.MkdirAll(corruptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("{{ definitely not yaml")
	if err := os.WriteFile(filepath.Join(corruptDir, "item.yaml"), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	stderr := &syncBuffer{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runEngine(ctx, engineOptions{
			cfg: cfg, level: slog.LevelDebug, stderr: stderr, now: time.Now, lookPath: func(f string) (string, error) {
				if strings.Contains(f, "ghostty") {
					return "", errors.New("not found")
				}
				return found(f)
			},
			tick: 20 * time.Millisecond, pollInterval: 50 * time.Millisecond,
		})
	}()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("engine: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("engine did not stop")
		}
	}
	defer stop()

	id := "alert-dev-abc123-1"
	// Created and queued immediately (first poll without waiting).
	eventually(t, "queued item", func() bool {
		it := readItem(t, stateDir, id)
		return it != nil && it.State == item.StateQueued
	})
	it := readItem(t, stateDir, id)
	if it.Runs.PendingReason == nil || *it.Runs.PendingReason != item.ReasonAuto || it.Runs.Current != 0 ||
		it.Title != "KubePodCrashLooping: core/api-1" || len(it.History) != 2 {
		t.Fatalf("item = %+v", it)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "items", id, "runs")); !os.IsNotExist(err) {
		t.Fatal("no run must be launched or fabricated")
	}
	raw, err := os.ReadFile(filepath.Join(stateDir, "items", id, "alert.json")) // #nosec G304 -- test file
	if err != nil || !json.Valid(raw) || !strings.Contains(string(raw), "abc123") {
		t.Fatalf("alert.json = %s %v", raw, err)
	}

	// Startup pruning removed the old done item only.
	eventually(t, "startup prune", func() bool {
		_, err := os.Stat(filepath.Join(stateDir, "items", oldDone))
		return os.IsNotExist(err)
	})
	if readItem(t, stateDir, recentDone) == nil {
		t.Fatal("recent done item pruned")
	}

	// Source failure is nonfatal and visible.
	writeFixture(t, fixture, "not json")
	eventually(t, "source failure log", func() bool { return strings.Contains(stderr.String(), "source poll failed") })
	if readItem(t, stateDir, id).State != item.StateQueued {
		t.Fatal("source failure changed the item")
	}

	// Recovery with an empty snapshot resolves the item.
	writeFixture(t, fixture, "[]")
	eventually(t, "resolved item", func() bool {
		it := readItem(t, stateDir, id)
		return it != nil && it.State == item.StateResolved
	})
	// The alert fires again: T9 reopens the same item.
	writeFixture(t, fixture, "["+fixtureAlert+"]")
	eventually(t, "reopened item", func() bool {
		it := readItem(t, stateDir, id)
		return it != nil && it.State == item.StateQueued && it.Alert.Occurrences == 2
	})

	// A user action written by another process while the engine runs is
	// preserved.
	other, err := item.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.AppendAction(id, item.ActionOpenedReport, 0, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_ = other.Close()
	writeFixture(t, fixture, "[]")
	eventually(t, "resolved again", func() bool {
		it := readItem(t, stateDir, id)
		return it != nil && it.State == item.StateResolved
	})
	it = readItem(t, stateDir, id)
	hasAction := false
	for _, h := range it.History {
		hasAction = hasAction || h.Action == item.ActionOpenedReport
	}
	if !hasAction || !it.Seen {
		t.Fatalf("concurrent user action lost: %+v", it.History)
	}

	stop()

	// The lock is released after shutdown.
	st, err = item.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	l, err := st.TryLockInstance()
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	_ = l.Release()

	// Corrupt item untouched and reported.
	if got, _ := os.ReadFile(filepath.Join(corruptDir, "item.yaml")); !bytes.Equal(got, corrupt) { // #nosec G304 -- test file
		t.Fatal("corrupt item modified")
	}
	errOut := stderr.String()
	for _, want := range []string{"level=INFO msg=transition", "to=queued", "unreadable_items=1", "skipping unreadable item", "alertmanager polling is not available", "ghostty.command", "pruned done item"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q", want)
		}
	}

	// tower.log is valid JSON with levels and context.
	f, err := os.Open(filepath.Join(stateDir, "tower.log")) // #nosec G304 -- test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	levels := map[string]bool{}
	var sawTransition, sawSource bool
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("invalid JSON log line %q: %v", sc.Text(), err)
		}
		levels[rec["level"].(string)] = true
		if rec["msg"] == "transition" && rec["item_id"] == id && rec["source"] == "dev" {
			sawTransition = true
		}
		if rec["msg"] == "source polled" && rec["source"] == "dev" && rec["level"] == "DEBUG" {
			sawSource = true
		}
		if rec["msg"] == "skipping unreadable item" && (rec["level"] != "ERROR" || rec["item_id"] != "alert-dev-corrupt-1") {
			t.Errorf("corrupt skip log = %v", rec)
		}
		if rec["msg"] == "source poll failed" && rec["level"] != "WARN" {
			t.Errorf("source error log = %v", rec)
		}
	}
	if !sawTransition || !sawSource || !levels["INFO"] || !levels["WARN"] || !levels["ERROR"] || !levels["DEBUG"] {
		t.Fatalf("log levels=%v transition=%v poll=%v", levels, sawTransition, sawSource)
	}

	// The credential sentinel never appears in logs or item/run files.
	assertNoSentinel(t, stateDir)
	if strings.Contains(errOut, credentialSentinel) {
		t.Fatal("sentinel on stderr")
	}

	// Permissions.
	for p, want := range map[string]os.FileMode{
		stateDir: 0o700, filepath.Join(stateDir, "tower.log"): 0o600, filepath.Join(stateDir, "tower.lock"): 0o600,
		filepath.Join(stateDir, "items", id): 0o700, filepath.Join(stateDir, "items", id, "item.yaml"): 0o600,
	} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v", p, fi.Mode().Perm(), err)
		}
	}
	_ = dir
}

func assertNoSentinel(t *testing.T, stateDir string) {
	t.Helper()
	err := filepath.WalkDir(stateDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p) // #nosec G304 G122 -- test-owned temp directory
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte(credentialSentinel)) {
			return fmt.Errorf("%s contains the credential sentinel", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLogRotation(t *testing.T) {
	st, err := item.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	w, err := newRotatingWriter(st, logFile, 1000, logFiles)
	if err != nil {
		t.Fatal(err)
	}
	logger := newLogger(w, &bytes.Buffer{}, slog.LevelDebug)
	for i := range 500 {
		logger.Info("line", "i", i, "item_id", "alert-dev-a-1")
	}
	_ = w.Close()
	entries, _ := os.ReadDir(st.Dir())
	var logs []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tower.log") {
			logs = append(logs, e.Name())
			fi, _ := e.Info()
			if fi.Size() > 1000 || fi.Mode().Perm() != 0o600 {
				t.Errorf("%s: size %d mode %v", e.Name(), fi.Size(), fi.Mode().Perm())
			}
			data, _ := os.ReadFile(filepath.Join(st.Dir(), e.Name())) // #nosec G304 -- test file
			for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("invalid JSON line in %s", e.Name())
				}
			}
		}
	}
	if strings.Join(logs, ",") != "tower.log,tower.log.1,tower.log.2" {
		t.Fatalf("log files = %v", logs)
	}
	// The newest entry is in tower.log.
	data, _ := os.ReadFile(filepath.Join(st.Dir(), "tower.log")) // #nosec G304 -- test file
	if !strings.Contains(string(data), `"i":499`) {
		t.Fatal("latest line not in tower.log")
	}
}

func TestPrune(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	dir, cfgPath, _, stateDir := setup(t, "retention:\n  done_after: 240h\n")
	e := testEnv(dir, map[string]string{"TOWER_TEST_TOKEN": "x"}, now)
	st, err := item.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	d := 240 * time.Hour
	oldDone := seedItem(t, st, "old", item.StateDone, now.Add(-d-time.Second))
	boundary := seedItem(t, st, "boundary", item.StateDone, now.Add(-d))
	recent := seedItem(t, st, "recent", item.StateDone, now.Add(-time.Hour))
	oldOpen := seedItem(t, st, "open", item.StateResolved, now.Add(-1000*time.Hour))
	midDone := seedItem(t, st, "mid", item.StateDone, now.Add(-100*time.Hour))
	corruptDir := filepath.Join(stateDir, "items", "alert-dev-corrupt-1")
	_ = os.MkdirAll(corruptDir, 0o700)
	_ = os.WriteFile(filepath.Join(corruptDir, "item.yaml"), []byte("state: done\nupdated_at: 2000-01-01T00:00:00Z\n"), 0o600)
	exists := func(id string) bool {
		_, err := os.Stat(filepath.Join(stateDir, "items", id))
		return err == nil
	}

	// Invalid durations.
	for _, v := range []string{"0s", "-1h", "abc", "10"} {
		if code, _, stderr := runCLI(t, e, "--config", cfgPath, "prune", "--older-than", v); code != 1 || !strings.Contains(stderr, "--older-than") {
			t.Errorf("%s: code=%d stderr=%q", v, code, stderr)
		}
	}

	// Dry run with the default duration.
	code, out, _ := runCLI(t, e, "--config", cfgPath, "prune", "--dry-run")
	if code != 0 || !strings.Contains(out, "would delete "+oldDone) || strings.Contains(out, boundary) || strings.Contains(out, recent) ||
		strings.Contains(out, oldOpen) || strings.Contains(out, midDone) || strings.Contains(out, "corrupt") || !strings.Contains(out, "1 done item(s)") {
		t.Fatalf("dry run: code=%d out=%q", code, out)
	}
	if !exists(oldDone) {
		t.Fatal("dry run deleted an item")
	}

	// Refusal while the engine lock is held; dry run still works.
	other, err := item.Open(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := other.TryLockInstance()
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCLI(t, e, "--config", cfgPath, "prune")
	if code != 1 || !strings.Contains(stderr, "tower.lock") || !exists(oldDone) {
		t.Fatalf("locked prune: code=%d stderr=%q", code, stderr)
	}
	if code, out, _ := runCLI(t, e, "--config", cfgPath, "prune", "--dry-run", "--older-than", "50h"); code != 0 || !strings.Contains(out, midDone) {
		t.Fatalf("dry run while locked: %d %q", code, out)
	}
	_ = l.Release()
	_ = other.Close()

	// Actual deletion with the default duration.
	code, out, _ = runCLI(t, e, "--config", cfgPath, "prune")
	if code != 0 || !strings.Contains(out, "deleted "+oldDone) || !strings.Contains(out, "1 done item(s)") {
		t.Fatalf("prune: code=%d out=%q", code, out)
	}
	if exists(oldDone) || !exists(boundary) || !exists(recent) || !exists(oldOpen) || !exists(midDone) || !exists("alert-dev-corrupt-1") {
		t.Fatal("wrong items deleted")
	}
	// Explicit duration.
	code, out, _ = runCLI(t, e, "--config", cfgPath, "prune", "--older-than", "50h")
	if code != 0 || !strings.Contains(out, "deleted "+midDone) || !strings.Contains(out, "deleted "+boundary) || exists(midDone) || !exists(recent) {
		t.Fatalf("prune --older-than: code=%d out=%q", code, out)
	}
	// Counters are preserved.
	counters, err := st.Counters()
	if err != nil {
		t.Fatal(err)
	}
	for _, fp := range []string{"old", "mid", "boundary"} {
		if counters[item.Key{Source: "dev", Fingerprint: fp}] != 1 {
			t.Errorf("counter for %s lost: %v", fp, counters)
		}
	}
}

// Subprocess tests: the real binary entry point with signals.

func startTower(t *testing.T, cfgPath string, stderr *syncBuffer, extraEnv ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0]) // #nosec G204 G702 -- re-executes the test binary
	cmd.Env = append(os.Environ(), "TOWER_TEST_RUN_MAIN=1", "TOWER_TEST_ARGS=--config "+cfgPath, "TOWER_TEST_TOKEN="+credentialSentinel)
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stderr = stderr
	cmd.Stdout = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func waitExit(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	ch := make(chan error, 1)
	go func() { ch <- cmd.Wait() }()
	select {
	case err := <-ch:
		if err == nil {
			return 0
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		t.Fatal(err)
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process did not exit")
	}
	return -1
}

func TestProcessLockAndSignals(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess test")
	}
	_, cfgPath, _, stateDir := setup(t, "")
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			var first syncBuffer
			a := startTower(t, cfgPath, &first)
			eventually(t, "first engine ready", func() bool {
				it := readItem(t, stateDir, "alert-dev-abc123-1")
				return it != nil && it.State == item.StateQueued && strings.Contains(first.String(), "items loaded")
			})

			var second syncBuffer
			b := startTower(t, cfgPath, &second)
			if code := waitExit(t, b); code != 1 || !strings.Contains(second.String(), filepath.Join(stateDir, "tower.lock")) {
				t.Fatalf("second instance: code=%d out=%q", code, second.String())
			}

			if err := a.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if code := waitExit(t, a); code != 0 {
				t.Fatalf("first instance exit code %d: %s", code, first.String())
			}
			if !strings.Contains(first.String(), "tower engine stopping") {
				t.Fatalf("no clean shutdown: %s", first.String())
			}

			// A subsequent process can start once the first exited.
			var third syncBuffer
			c := startTower(t, cfgPath, &third)
			eventually(t, "third engine ready", func() bool { return strings.Contains(third.String(), "items loaded") })
			if err := c.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			if code := waitExit(t, c); code != 0 {
				t.Fatalf("third instance exit code %d: %s", code, third.String())
			}
			assertNoSentinel(t, stateDir)
		})
	}
}
