package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/ricoberger/tower/internal/store"
)

func TestNewItem(t *testing.T) {
	p := &Provider{}
	now := time.Unix(1, 0)
	it, ok := p.NewItem("\n\n  Fix the build  \n\nThe CI fails.\n# Logs\nSee logs.\n\n", now)
	if !ok {
		t.Fatal("ok = false")
	}
	if !strings.HasPrefix(it.Key, "task:") || it.Kind != Kind || it.Source != Kind || !it.CreatedAt.Equal(now) {
		t.Fatalf("item = %+v", it)
	}
	if it.Title != "Fix the build" {
		t.Fatalf("title = %q", it.Title)
	}
	if it.Description != "The CI fails.\n# Logs\nSee logs." {
		t.Fatalf("description = %q", it.Description)
	}
	if it.Details != "" || it.URL != "" {
		t.Fatalf("details = %q, url = %q", it.Details, it.URL)
	}
}

func TestNewItemNormalizesTitleWhitespace(t *testing.T) {
	it, ok := (&Provider{}).NewItem("Fix\tthe\rbuild\u2028now\nDescription\nsecond line", time.Now())
	if !ok || it.Title != "Fix the build now" || it.Description != "Description\nsecond line" {
		t.Fatalf("item = %+v, ok = %v", it, ok)
	}
}

func TestNewItemEmpty(t *testing.T) {
	for _, s := range []string{"", "\n\n  \n", "\r\n"} {
		if _, ok := (&Provider{}).NewItem(s, time.Now()); ok {
			t.Errorf("%q: ok = true", s)
		}
	}
}

func TestPrompt(t *testing.T) {
	p, err := New(Config{Prompt: "{{.ID}} {{.Kind}}/{{.Source}} {{.Title}}: {{.Description}}"})
	if err != nil {
		t.Fatal(err)
	}
	it, _ := p.NewItem("Fix the build\nThe CI fails.", time.Now())
	it.ID = 3
	if got, err := p.Prompt(it); err != nil || got != "3 task/task Fix the build: The CI fails." {
		t.Fatalf("prompt = %q, err = %v", got, err)
	}
}

func TestPromptUnknownField(t *testing.T) {
	p, err := New(Config{Prompt: "{{.Labels}}"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Prompt(store.Item{}); err == nil {
		t.Fatal("want error")
	}
}

func TestNewInvalidPrompt(t *testing.T) {
	if _, err := New(Config{Prompt: "{{.Title"}); err == nil {
		t.Fatal("want error")
	}
}
