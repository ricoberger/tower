package item

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunFiles(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	run := Run{Number: 1, SessionID: "sid", Reason: ReasonAuto, Skill: "sre-analyze-alert", QueuedAt: t0, Outcome: OutcomeRunning}
	if err := s.CreateRun(it.ID, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runs, err := s.ReadRuns(it.ID)
	if err != nil || len(runs) != 1 || runs[0].SessionID != "sid" {
		t.Fatalf("runs = %+v %v", runs, err)
	}
	// An existing run directory is never reused, even when empty.
	if err := s.CreateRun(it.ID, run); !errors.Is(err, ErrRunExists) {
		t.Fatalf("second CreateRun = %v", err)
	}
	if err := os.Mkdir(filepath.Join(s.ItemDir(it.ID), "runs", "2"), 0o700); err != nil {
		t.Fatal(err)
	}
	run2 := run
	run2.Number = 2
	if err := s.CreateRun(it.ID, run2); !errors.Is(err, ErrRunExists) {
		t.Fatalf("CreateRun over empty dir = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.ItemDir(it.ID), "runs"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temporary run directory left behind: %s", e.Name())
		}
	}
	// Runs of unknown items are not created.
	other := newItem(t, key, 2)
	if err := s.CreateRun(other.ID, run); err == nil {
		t.Fatal("CreateRun of a missing item succeeded")
	}

	dir, err := s.RunPath(it.ID, 1)
	if err != nil || dir != filepath.Join(s.ItemDir(it.ID), "runs", "1") || !filepath.IsAbs(dir) {
		t.Fatalf("RunPath = %q %v", dir, err)
	}
	if _, err := s.RunPath(it.ID, 3); err == nil {
		t.Error("RunPath of a missing run succeeded")
	}
	if _, err := s.RunPath(it.ID, 0); err == nil {
		t.Error("RunPath of run 0 succeeded")
	}
	if _, err := s.RunPath("../x", 1); err == nil {
		t.Error("RunPath accepted an invalid ID")
	}

	if err := s.WriteRunFile(it.ID, 1, PromptFile, []byte("prompt")); err != nil {
		t.Fatal(err)
	}
	if m := mode(t, filepath.Join(dir, PromptFile)); m != 0o600 {
		t.Errorf("prompt.md mode = %v", m)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("run dir mode = %v", m)
	}
	data, err := s.ReadRunFile(it.ID, 1, PromptFile)
	if err != nil || string(data) != "prompt" {
		t.Fatalf("ReadRunFile = %q %v", data, err)
	}
	if ok, err := s.HasRunFile(it.ID, 1, PromptFile); !ok || err != nil {
		t.Errorf("HasRunFile = %v %v", ok, err)
	}
	if ok, err := s.HasRunFile(it.ID, 1, ReportFile); ok || err != nil {
		t.Errorf("HasRunFile(missing) = %v %v", ok, err)
	}
	if _, err := s.ReadRunFile(it.ID, 1, ReportFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadRunFile(missing) = %v", err)
	}
	for _, name := range []string{"", ".", "..", "../item.yaml", "a/b", "meta.yaml"} {
		if err := s.WriteRunFile(it.ID, 1, name, nil); err == nil {
			t.Errorf("WriteRunFile(%q) accepted", name)
		}
		if _, err := s.ReadRunFile(it.ID, 1, name); err == nil {
			t.Errorf("ReadRunFile(%q) accepted", name)
		}
	}
	if err := s.WriteRunFile(it.ID, 5, PromptFile, nil); err == nil {
		t.Error("WriteRunFile created a run directory")
	}

	// Symlinked artifacts are not reported as existing, and links cannot
	// escape the state directory.
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ReportFile)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.HasRunFile(it.ID, 1, ReportFile); ok {
		t.Error("symlinked report counted as existing")
	}
	if _, err := s.ReadRunFile(it.ID, 1, ReportFile); err == nil {
		t.Error("ReadRunFile followed a link out of the state directory")
	}
	// A directory is not a readable artifact.
	if err := os.Mkdir(filepath.Join(dir, ResultFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadRunFile(it.ID, 1, ResultFile); err == nil {
		t.Error("ReadRunFile read a directory")
	}
}

func TestRunPathContainment(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateRun(it.ID, Run{Number: 1, Reason: ReasonAuto, Outcome: OutcomeRunning}); err != nil {
		t.Fatal(err)
	}
	// A symlinked runs/<n> directory is rejected.
	target := t.TempDir()
	if err := os.Symlink(target, filepath.Join(s.ItemDir(it.ID), "runs", "7")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunPath(it.ID, 7); err == nil {
		t.Error("RunPath accepted a symlinked run directory")
	}
	// A symlinked item directory is rejected.
	linked := newItem(t, key, 3)
	if err := os.Symlink(s.ItemDir(it.ID), s.ItemDir(linked.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ItemPath(linked.ID); err == nil {
		t.Error("ItemPath accepted a symlinked item directory")
	}
	if p, err := s.ItemPath(it.ID); err != nil || p != s.ItemDir(it.ID) {
		t.Errorf("ItemPath = %q %v", p, err)
	}
	if p, err := s.AlertMarkdownPath(it.ID); err != nil || p != filepath.Join(s.ItemDir(it.ID), "alert.md") {
		t.Errorf("AlertMarkdownPath = %q %v", p, err)
	}
}

// A FIFO artifact without a writer is rejected instead of blocking the
// reader.
func TestReadRunFileFIFO(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	run := Run{Number: 1, SessionID: "sid", Reason: ReasonAuto, Skill: "sre-analyze-alert", QueuedAt: t0, Outcome: OutcomeRunning}
	if err := s.CreateRun(it.ID, run); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ReportFile, ResultFile} {
		if err := syscall.Mkfifo(filepath.Join(s.ItemDir(it.ID), "runs", "1", name), 0o600); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := s.ReadRunFile(it.ID, 1, name)
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("%s: err = %v", name, err)
			}
		case <-time.After(5 * time.Second):
			// Unblock the reader before failing.
			if f, err := os.OpenFile(filepath.Join(s.ItemDir(it.ID), "runs", "1", name), os.O_WRONLY, 0); err == nil {
				_ = f.Close()
			}
			t.Fatalf("%s: ReadRunFile blocked on a FIFO", name)
		}
	}
}
