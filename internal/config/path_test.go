package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolvesStateDir(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(cwd)
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, tt := range []struct{ input, want string }{
		{input: "~", want: home},
		{input: "~/state", want: filepath.Join(home, "state")},
		{input: "$HOME/state", want: filepath.Join(home, "state")},
		{input: "${HOME}/state", want: filepath.Join(home, "state")},
		{input: "relative/../state", want: filepath.Join(cwd, "state")},
		{input: "state #?%", want: filepath.Join(cwd, "state #?%")},
		{input: filepath.Join(home, "absolute"), want: filepath.Join(home, "absolute")},
	} {
		t.Run(tt.input, func(t *testing.T) {
			if err := os.WriteFile(path, fmt.Appendf(nil, "state_dir: %q\n", tt.input), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.StateDir != tt.want {
				t.Fatalf("state_dir = %q, want %q", cfg.StateDir, tt.want)
			}
		})
	}
}

func TestLoadRejectsEmptyStateDir(t *testing.T) {
	t.Setenv("TOWER_TEST_STATE_DIR", "")
	for _, text := range []string{"", "state_dir: ''", "state_dir: $TOWER_TEST_STATE_DIR"} {
		if _, err := load(t, text); err == nil {
			t.Errorf("config %q: empty state_dir accepted", text)
		}
	}
}
