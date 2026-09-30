package item

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p) // #nosec G304 -- test path
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestAlertMarkdown(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, json.RawMessage(`{"fingerprint":"abc"}`)); err != nil {
		t.Fatal(err)
	}
	dir := s.ItemDir(it.ID)
	md := filepath.Join(dir, "alert.md")
	itemBefore := readFile(t, filepath.Join(dir, "item.yaml"))

	wrote, err := s.InitAlertMarkdown(it.ID, []byte("# first\n"))
	if err != nil || !wrote {
		t.Fatalf("InitAlertMarkdown = %v, %v", wrote, err)
	}
	if got := readFile(t, md); got != "# first\n" {
		t.Fatalf("alert.md = %q", got)
	}
	if m := mode(t, md); m != 0o600 {
		t.Errorf("alert.md mode %v", m)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Errorf("item dir mode %v", m)
	}

	// Initialization never replaces an existing file.
	stamp, _ := os.Stat(md)
	wrote, err = s.InitAlertMarkdown(it.ID, []byte("# second\n"))
	if err != nil || wrote {
		t.Fatalf("InitAlertMarkdown over existing = %v, %v", wrote, err)
	}
	if got := readFile(t, md); got != "# first\n" {
		t.Fatalf("alert.md replaced: %q", got)
	}
	if fi, _ := os.Stat(md); !os.SameFile(stamp, fi) || !fi.ModTime().Equal(stamp.ModTime()) {
		t.Error("alert.md identity changed")
	}

	if err := s.WriteAlertMarkdown(it.ID, []byte("# third\n")); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, md); got != "# third\n" {
		t.Fatalf("alert.md = %q", got)
	}
	if m := mode(t, md); m != 0o600 {
		t.Errorf("alert.md mode %v", m)
	}
	// Markdown writes change neither item.yaml nor alert.json.
	if got := readFile(t, filepath.Join(dir, "item.yaml")); got != itemBefore {
		t.Error("item.yaml changed")
	}
	if got := readFile(t, filepath.Join(dir, "alert.json")); got != `{"fingerprint":"abc"}` {
		t.Errorf("alert.json = %q", got)
	}
	// No temporary files are left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch e.Name() {
		case "item.yaml", "alert.json", "alert.md", ".lock":
		default:
			t.Errorf("unexpected file %s", e.Name())
		}
	}

	if err := s.RemoveAlertMarkdown(it.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(md); !os.IsNotExist(err) {
		t.Fatal("alert.md not removed")
	}
	if err := s.RemoveAlertMarkdown(it.ID); err != nil {
		t.Fatalf("removing a missing alert.md: %v", err)
	}
}

func TestAlertMarkdownRequiresReadableItem(t *testing.T) {
	s := openStore(t)
	// Missing items: no directory is created.
	for _, err := range []error{
		s.WriteAlertMarkdown("alert-dev-zzz-1", []byte("x")),
		func() error { _, err := s.InitAlertMarkdown("alert-dev-zzz-1", []byte("x")); return err }(),
	} {
		if err == nil {
			t.Fatal("want error for missing item")
		}
	}
	if _, err := os.Stat(s.ItemDir("alert-dev-zzz-1")); !os.IsNotExist(err) {
		t.Fatal("item directory was created")
	}
	// Corrupt items are never touched.
	id := "alert-dev-bad-1"
	writeCorrupt(t, s, id, "{not yaml")
	if _, err := s.InitAlertMarkdown(id, []byte("x")); err == nil {
		t.Fatal("want error for corrupt item")
	}
	if err := s.WriteAlertMarkdown(id, []byte("x")); err == nil {
		t.Fatal("want error for corrupt item")
	}
	if _, err := os.Stat(filepath.Join(s.ItemDir(id), "alert.md")); !os.IsNotExist(err) {
		t.Fatal("alert.md written for a corrupt item")
	}
	// Unsafe IDs are rejected.
	for _, bad := range []string{"../x", "alert-dev-../../x-1", ""} {
		if err := s.WriteAlertMarkdown(bad, []byte("x")); err == nil {
			t.Errorf("WriteAlertMarkdown(%q) succeeded", bad)
		}
		if err := s.RemoveAlertMarkdown(bad); err == nil {
			t.Errorf("RemoveAlertMarkdown(%q) succeeded", bad)
		}
	}
}

func TestAlertMarkdownWaitsForItemLock(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, nil); err != nil {
		t.Fatal(err)
	}
	unlock, err := s.lockItem(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = s.WriteAlertMarkdown(it.ID, []byte("x"))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("write did not wait for the item lock")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("write did not proceed after unlock")
	}
}

func TestOpenAlertMarkdown(t *testing.T) {
	s := openStore(t)
	it := newItem(t, key, 1)
	if err := s.Create(it, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAlertMarkdown(it.ID); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing alert.md: err = %v", err)
	}
	if err := s.WriteAlertMarkdown(it.ID, []byte("# alert\n")); err != nil {
		t.Fatal(err)
	}
	f, err := s.OpenAlertMarkdown(it.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || string(data) != "# alert\n" {
		t.Fatalf("alert.md = %q, %v", data, err)
	}
	if _, err := s.OpenAlertMarkdown("../x"); err == nil {
		t.Fatal("invalid ID accepted")
	}

	// A symlinked alert.md is rejected, even when it points inside the store.
	md := filepath.Join(s.ItemDir(it.ID), "alert.md")
	if err := os.Remove(md); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("item.yaml", md); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAlertMarkdown(it.ID); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlinked alert.md: err = %v", err)
	}

	// A FIFO is rejected without blocking.
	if err := os.Remove(md); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(md, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAlertMarkdown(it.ID); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("FIFO alert.md: err = %v", err)
	}

	// A symlinked item directory is rejected.
	other := newItem(t, key, 2)
	if err := os.Symlink(s.ItemDir(it.ID), s.ItemDir(other.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenAlertMarkdown(other.ID); err == nil {
		t.Fatal("symlinked item directory accepted")
	}
}
