package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewTreatsStateDirAsFilesystemPath(t *testing.T) {
	for _, name := range []string{"plain", "spaces and ünicode", "state#fragment", "state?mode=memory", "state%2Fencoded"} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if err := os.Mkdir(name, 0o700); err != nil {
				t.Fatal(err)
			}
			s, err := New(name)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			id := sync(t, s, t0, "a")[0]
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(name, "tower.db")); err != nil {
				t.Fatal(err)
			}
			reopened, err := New(name)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			if got := items(t, reopened)[id]; got.Title != "title a" {
				t.Fatalf("reopened item = %+v", got)
			}
		})
	}
}
