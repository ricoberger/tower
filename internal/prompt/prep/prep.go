// Package prep renders the prompt of unattended preparation runs. The
// embedded default template and user overrides (prompts.alert) receive the
// same Data.
package prep

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"text/template"

	"github.com/ricoberger/tower/internal/result"
)

// MaxSameItemReports is the maximum number of earlier reports of the same
// item referenced by the prompt.
const MaxSameItemReports = 3

//go:embed alert.tmpl
var defaultText string

// DefaultText is the embedded default preparation prompt template.
var DefaultText = strings.TrimSuffix(defaultText, "\n")

// Data is the data contract of preparation prompt templates.
type Data struct {
	// RunDir is the absolute directory of the new run.
	RunDir string
	// AlertFile is the absolute path of the item's alert.md.
	AlertFile string
	// AlertMarkdown is the content of alert.md.
	AlertMarkdown string
	// SourceName is the item's source name.
	SourceName string
	// Fingerprint is the item's alert fingerprint.
	Fingerprint string
	// PreviousReports are absolute paths of earlier reports: up to three of
	// the same item (newest run first), then the latest report of the
	// previous item, if any.
	PreviousReports []string
	// ResultSchema is the pretty-printed result.json JSON schema.
	ResultSchema string
}

// Parse parses a preparation prompt template. Only the standard text/template
// functions are available.
func Parse(text string) (*template.Template, error) {
	return template.New("prompt").Option("missingkey=error").Parse(text)
}

// Default returns the parsed embedded template.
func Default() *template.Template {
	return template.Must(Parse(DefaultText))
}

// Validate parses text and executes it with sample data, both without and
// with previous reports, so errors in conditional sections are detected.
// Execution only renders into memory; templates cannot run external actions.
func Validate(text string) error {
	t, err := Parse(text)
	if err != nil {
		return err
	}
	sample := Data{
		RunDir:        "/state/items/src-0123456789abcdef/runs/4",
		AlertFile:     "/state/items/src-0123456789abcdef/alert.md",
		AlertMarkdown: "# Alert\n",
		SourceName:    "src",
		Fingerprint:   "0123456789abcdef",
		ResultSchema:  result.Schema,
	}
	if err := t.Execute(io.Discard, sample); err != nil {
		return err
	}
	sample.PreviousReports = []string{
		"/state/items/src-0123456789abcdef/runs/3/report.md",
		"/state/items/src-0123456789abcdef/runs/2/report.md",
		"/state/items/src-0123456789abcdef/runs/1/report.md",
		"/state/items/src-fedcba9876543210/runs/1/report.md",
	}
	return t.Execute(io.Discard, sample)
}

// Render executes t with data.
func Render(t *template.Template, data Data) ([]byte, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render prompt: %w", err)
	}
	return buf.Bytes(), nil
}
