// Package helpers contains configuration types and functions shared by the
// config package and the configuration of the other packages.
package helpers

import (
	"fmt"
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
