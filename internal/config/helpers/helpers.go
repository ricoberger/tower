// Package helpers contains configuration types and functions shared by the
// config package and the configuration of the other packages.
package helpers

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
)

// Duration is a time.Duration written in Go syntax (e.g. "90s", "48h").
type Duration struct{ time.Duration }

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(n ast.Node) error {
	var s string
	if err := yaml.NodeToValue(n, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q (use Go syntax such as 90s, 30m, 48h)", n.GetToken().Position.Line, s)
	}
	d.Duration = v
	return nil
}

// ExpandPath expands $VAR, ${VAR}, ~ and a leading ~/ in path and makes it
// absolute. Relative paths are resolved against the current working
// directory. An empty result is an error.
func ExpandPath(path string) (string, error) {
	path = os.ExpandEnv(path)
	if path == "" {
		return "", errors.New("must not be empty")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	return filepath.Abs(path)
}
