// Package config loads tower's configuration file and resolves state_dir.
// Unknown keys are errors; missing package-owned settings stay at zero.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/ricoberger/tower/internal/agent"
	"github.com/ricoberger/tower/internal/app"
	"github.com/ricoberger/tower/internal/provider/alerts"
	"github.com/ricoberger/tower/internal/provider/pullrequests"
	"github.com/ricoberger/tower/internal/provider/tasks"
)

// Config is the complete configuration.
type Config struct {
	// StateDir contains the SQLite database, the lock file and run logs.
	// $VAR, ${VAR}, ~ and a leading ~/ are expanded; the result is absolute.
	StateDir string       `yaml:"state_dir"`
	App      app.Config   `yaml:"app"`
	Agent    agent.Config `yaml:"agent"`
	// Providers holds the settings of the item providers.
	Providers struct {
		Alerts       alerts.Config       `yaml:"alerts"`
		PullRequests pullrequests.Config `yaml:"pullrequests"`
		Tasks        tasks.Config        `yaml:"tasks"`
	} `yaml:"providers"`
}

// Load reads the configuration file at path and resolves state_dir once for
// both the TUI and its wrappers. Relative state paths use the current working
// directory. The package-owned configs are parsed by their constructors.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is chosen by the user
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.UnmarshalWithOptions(data, &cfg, yaml.Strict()); err != nil {
		return nil, fmt.Errorf("failed to parse configuration file %s: %w", path, err)
	}

	cfg.StateDir = os.ExpandEnv(cfg.StateDir)
	if cfg.StateDir == "" {
		return nil, errors.New("state_dir must not be empty")
	}
	if cfg.StateDir == "~" || strings.HasPrefix(cfg.StateDir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve state_dir: %w", err)
		}
		cfg.StateDir = filepath.Join(home, strings.TrimPrefix(cfg.StateDir, "~"))
	}
	cfg.StateDir, err = filepath.Abs(cfg.StateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state_dir: %w", err)
	}
	return &cfg, nil
}
