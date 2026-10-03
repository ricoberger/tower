// Package tasks creates tasks that the user writes in their editor.
package tasks

import (
	"context"
	"fmt"
	"strings"
	"text/template"
	"time"
	"uuid"

	"github.com/ricoberger/tower/internal/provider"
	"github.com/ricoberger/tower/internal/store"
)

// Kind is the item kind of tasks.
const Kind = "task"

var _ provider.Provider = (*Provider)(nil)

// Config configures tasks.
type Config struct {
	Prompt string `yaml:"prompt"`
}

// Provider is the tasks provider. Tasks are created in the UI, so it
// does not poll.
type Provider struct {
	tmpl *template.Template
}

// New creates the tasks provider.
func New(cfg Config) (*Provider, error) {
	tmpl, err := template.New("prompt").Parse(cfg.Prompt)
	if err != nil {
		return nil, fmt.Errorf("parse prompt: %w", err)
	}

	return &Provider{
		tmpl: tmpl,
	}, nil
}

// Kind implements provider.Provider.
func (p *Provider) Kind() string {
	return Kind
}

// Poll returns immediately.
func (p *Provider) Poll(context.Context, <-chan struct{}, chan<- store.Update) {}

// Prompt renders providers.tasks.prompt for an item.
func (p *Provider) Prompt(it store.Item) (string, error) {
	return provider.RenderPrompt(p.tmpl, it)
}

// NewItem turns the text the user wrote in the editor into a new task: the
// first non-empty line is the title, the rest the description. ok is false if
// the text is empty, i.e. the user cancelled.
func (p *Provider) NewItem(text string, now time.Time) (it store.Item, ok bool) {
	title, description, _ := strings.Cut(strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n")), "\n")
	if title == "" {
		return store.Item{}, false
	}
	return store.Item{
		Key:         Kind + ":" + uuid.NewV4().String(),
		Kind:        Kind,
		Source:      Kind,
		CreatedAt:   now,
		Title:       strings.Join(strings.Fields(title), " "),
		Description: strings.TrimSpace(description),
	}, true
}
