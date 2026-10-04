package agent

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/provider"
	"github.com/ricoberger/tower/internal/provider/alerts"
	"github.com/ricoberger/tower/internal/provider/tasks"
	"github.com/ricoberger/tower/internal/store"
)

// TestMain lets the test binary act as the `tower exec` wrapper.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "exec" {
		fs := flag.NewFlagSet("exec", flag.ExitOnError)
		stateDir := fs.String("state-dir", "", "")
		item := fs.String("item", "", "")
		session := fs.String("session", "", "")
		title := fs.String("title", "", "")
		_ = fs.Parse(os.Args[2:])
		id, _ := strconv.ParseInt(*item, 10, 64)
		s, err := store.New(*stateDir)
		if err != nil {
			os.Exit(1)
		}
		code := Exec(fakeNotifier{}, s, id, *session, *title, fs.Args())
		_ = s.Close()
		os.Exit(code)
	}
	os.Exit(m.Run())
}

type fakeNotifier struct{}

func (fakeNotifier) Send(string, string, string) error {
	return nil
}

func setup(t *testing.T, run, resume string) (*Agent, *store.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state #?%")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(t.TempDir(), "work dir")
	if err := os.Mkdir(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRAFANA_INSTANCES", `{}`)
	ac := alerts.Config{Prompt: "alert {{.Title}}"}
	ac.PollInterval.Duration = time.Minute
	ap, err := alerts.New(ac)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := tasks.New(tasks.Config{Prompt: "task {{.Title}}: {{.Description}}"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	a, err := New(Config{RunCommand: run, ResumeCommand: resume, WorkingDir: workDir}, dir, os.Args[0], s, provider.Set{ap, mp})
	if err != nil {
		t.Fatal(err)
	}
	return a, s, dir
}

func createTask(t *testing.T, s *store.Store) store.Item {
	t.Helper()
	ctx := context.Background()
	id, err := s.Create(ctx, store.Item{Key: "task:t", Kind: tasks.Kind, Source: tasks.Kind, CreatedAt: time.Now(),
		Title: "Fix it", Description: "it's broken; really"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	it, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func waitState(t *testing.T, s *store.Store, id int64, want store.State) store.Item {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		it, err := s.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if it.State == want {
			return it
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %s, want %s", it.State, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestStartToWaiting(t *testing.T) {
	a, s, _ := setup(t, `sh -c 'printf "%s|%s\n" "$0" "$1"; exit 3' {{.Prompt}} {{.SessionID}}`, "true {{.SessionID}}")
	it := createTask(t, s)
	if err := a.Start(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	got := waitState(t, s, it.ID, store.StateWaiting)
	if !got.Failed || got.SessionID == "" || got.PID != 0 {
		t.Fatalf("item = %+v", got)
	}
	log, err := os.ReadFile(a.LogPath(it.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "task Fix it: it's broken; really|"+got.SessionID) || !strings.Contains(string(log), "exit code 3") {
		t.Fatalf("log = %s", log)
	}
}

func TestStartCommandNotFound(t *testing.T) {
	a, s, _ := setup(t, "/nonexistent/agent {{.Prompt}} {{.SessionID}}", "true {{.SessionID}}")
	it := createTask(t, s)
	if err := a.Start(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	got := waitState(t, s, it.ID, store.StateWaiting)
	log, _ := os.ReadFile(a.LogPath(it.ID))
	if !got.Failed || !strings.Contains(string(log), "exit code 127") {
		t.Fatalf("item = %+v, log = %s", got, log)
	}
}

func TestCommandsRunInWorkingDir(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	a, s, _ := setup(t, `sh -c 'printf "run %s %s\n" "$(pwd -P)" "$PWD" >> "$0"' `+shellQuote(out)+` {{.Prompt}} {{.SessionID}}`,
		`sh -c 'printf "resume %s %s\n" "$(pwd -P)" "$PWD" >> "$0"' `+shellQuote(out)+` {{.SessionID}}`)
	t.Chdir(t.TempDir())
	it := createTask(t, s)
	if err := a.Start(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	it = waitState(t, s, it.ID, store.StateWaiting)
	if err := a.Resume(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(a.workDir)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	want := "run " + physical + " " + a.workDir + "\nresume " + physical + " " + a.workDir + "\n"
	if string(data) != want {
		t.Fatalf("output = %q, want %q", data, want)
	}
}

func TestNewRejectsInvalidWorkingDir(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"", filepath.Join(t.TempDir(), "missing"), file} {
		cfg := Config{RunCommand: "true {{.Prompt}} {{.SessionID}}", ResumeCommand: "true {{.SessionID}}", WorkingDir: dir}
		if _, err := New(cfg, t.TempDir(), "tower", nil, nil); err == nil {
			t.Errorf("working_dir %q accepted", dir)
		}
	}
}

func TestResume(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	a, s, _ := setup(t, "true {{.Prompt}} {{.SessionID}}", "sh -c 'echo \"$0 $1\" > "+out+"' {{.SessionID}} {{.Title}}")
	it := createTask(t, s)
	it.SessionID = "abc"
	if err := a.Resume(context.Background(), it); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "abc Fix it\n" {
		t.Fatalf("out = %q", data)
	}

	a, _, _ = setup(t, "true {{.Prompt}} {{.SessionID}}", "sh -c 'echo boom; exit 1' {{.SessionID}}")
	if err := a.Resume(context.Background(), it); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

func TestAlive(t *testing.T) {
	a := &Agent{}
	if !a.Alive(os.Getpid()) || a.Alive(0) || a.Alive(-1) {
		t.Fatal("Alive is wrong")
	}
}
