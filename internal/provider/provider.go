// Package provider defines the interface of the item modules (alerts, tasks
// and later pull requests or Jira tickets). Every provider handles one item
// kind.
package provider

import (
	"bytes"
	"context"
	"fmt"
	"text/template"

	"github.com/ricoberger/tower/internal/store"
)

// Provider is one item module. Providers fill the title, description,
// details and URL of their items; everything else works on these fields.
type Provider interface {
	// Kind is the item kind the provider handles, e.g. alerts.Kind.
	Kind() string
	// Poll polls every source immediately, then periodically and whenever
	// trigger receives, and sends one store.Update per source and poll to
	// out. It returns when ctx is done; providers that do not poll return
	// immediately.
	Poll(ctx context.Context, trigger <-chan struct{}, out chan<- store.Update)
	// Prompt renders the agent prompt of an item (see RenderPrompt).
	Prompt(it store.Item) (string, error)
}

// PromptData is the data of every provider's prompt template.
type PromptData struct {
	ID          int64
	Kind        string
	Source      string
	Title       string
	Description string
	// Details is the item as Markdown.
	Details string
	URL     string
}

// RenderPrompt renders a provider's prompt template for an item.
func RenderPrompt(t *template.Template, it store.Item) (string, error) {
	var b bytes.Buffer
	err := t.Execute(&b, PromptData{
		ID:          it.ID,
		Kind:        it.Kind,
		Source:      it.Source,
		Title:       it.Title,
		Description: it.Description,
		Details:     it.Details,
		URL:         it.URL,
	})
	if err != nil {
		return "", fmt.Errorf("render prompt: %w", err)
	}
	return b.String(), nil
}

// Set is the list of registered providers.
type Set []Provider

// Get returns the provider of a kind.
func (s Set) Get(kind string) (Provider, error) {
	for _, p := range s {
		if p.Kind() == kind {
			return p, nil
		}
	}
	return nil, fmt.Errorf("no provider for item kind %q", kind)
}
