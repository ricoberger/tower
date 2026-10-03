package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"

	"github.com/ricoberger/tower/internal/config"
)

func TestExecUsesResolvedStateDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Chdir(t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	for _, input := range []string{"~/state", "$HOME/state", "state #?%"} {
		t.Run(input, func(t *testing.T) {
			if err := os.WriteFile(path, fmt.Appendf(nil, "state_dir: %q\n", input), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(path)
			if err != nil {
				t.Fatal(err)
			}
			var cli CLI
			parser, err := kong.New(&cli)
			if err != nil {
				t.Fatal(err)
			}
			_, err = parser.Parse([]string{"exec", "--state-dir", cfg.StateDir, "--item", "1", "--session", "test", "--", "true"})
			if err != nil {
				t.Fatal(err)
			}
			if !filepath.IsAbs(cfg.StateDir) || cli.Exec.StateDir != cfg.StateDir {
				t.Fatalf("TUI state_dir = %q, wrapper state_dir = %q", cfg.StateDir, cli.Exec.StateDir)
			}
		})
	}
}
