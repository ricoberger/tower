package ui

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/ricoberger/tower/internal/store"
)

// raws runs cmd and returns the sequences of all tea.Raw commands in it.
func raws(cmd tea.Cmd) []string {
	if cmd == nil {
		return nil
	}
	switch msg := cmd().(type) {
	case tea.RawMsg:
		return []string{msg.Msg.(string)}
	case tea.BatchMsg:
		var out []string
		for _, c := range msg {
			out = append(out, raws(c)...)
		}
		return out
	}
	return nil
}

func TestNotifications(t *testing.T) {
	m := New(context.Background(), &fakeBackend{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	load := func(items ...store.Item) []string {
		_, cmd := m.Update(itemsMsg{items: items})
		return raws(cmd)
	}
	item := func(id int64, state store.State, session string, failed bool, title string) store.Item {
		return store.Item{ID: id, State: state, SessionID: session, Failed: failed, Title: title}
	}

	// Runs that finished before the first load are not reported.
	if got := load(
		item(1, store.StateInProgress, "s1", false, "Done ok"),
		item(2, store.StateInProgress, "s2", false, "Broken\x1b]0;evil\a;\nrun"),
		item(3, store.StateWaiting, "s3", false, "Old"),
		item(4, store.StateInProgress, "s4", false, "Moved by user"),
		item(5, store.StateInProgress, "s5", false, "Still running"),
	); got != nil {
		t.Fatalf("first load notified %q", got)
	}

	want := []string{
		"\x1b]777;notify;tower;Agent finished: Done ok\a",
		"\x1b]777;notify;tower;Agent failed: Broken ]0;evil ; run\a",
	}
	got := load(
		item(1, store.StateWaiting, "s1", false, "Done ok"),
		item(2, store.StateWaiting, "s2", true, "Broken\x1b]0;evil\a;\nrun"),
		item(3, store.StateWaiting, "s3", false, "Old"),
		item(4, store.StateTodo, "s4", false, "Moved by user"),
		item(5, store.StateInProgress, "s5", false, "Still running"),
	)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("notifications = %q, want %q", got, want)
	}

	// A stale load that still shows the run IN PROGRESS must not report the
	// same session again.
	load(item(1, store.StateInProgress, "s1", false, "Done ok"))
	if got := load(item(1, store.StateWaiting, "s1", false, "Done ok")); got != nil {
		t.Fatalf("session notified twice: %q", got)
	}

	// A new session of the same item is reported again.
	load(item(1, store.StateInProgress, "s1b", false, "Done ok"))
	if got := load(item(1, store.StateWaiting, "s1b", false, "Done ok")); len(got) != 1 {
		t.Fatalf("new session notifications = %q", got)
	}
}
