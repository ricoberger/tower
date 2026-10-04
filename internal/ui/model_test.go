package ui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ricoberger/tower/internal/provider/alerts"
	"github.com/ricoberger/tower/internal/provider/tasks"
	"github.com/ricoberger/tower/internal/store"
)

// TestMain fixes the Markdown style of the details popup, so the user's
// $GLAMOUR_STYLE does not change the rendered output.
func TestMain(m *testing.M) {
	if err := os.Setenv("GLAMOUR_STYLE", "notty"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

type fakeBackend struct {
	items   []store.Item
	started []int64
	resumed []int64
	opened  []int64
	moved   map[int64]store.State
	polled  int
	err     error
}

func (f *fakeBackend) Items(context.Context) ([]store.Item, error) { return f.items, nil }
func (f *fakeBackend) Start(_ context.Context, it store.Item) error {
	f.started = append(f.started, it.ID)
	return f.err
}
func (f *fakeBackend) Resume(_ context.Context, it store.Item) error {
	f.resumed = append(f.resumed, it.ID)
	return nil
}
func (f *fakeBackend) Move(_ context.Context, id int64, to store.State) error {
	f.moved[id] = to
	return nil
}
func (f *fakeBackend) CreateTask(context.Context, string) error { return nil }
func (f *fakeBackend) PollNow()                                 { f.polled++ }
func (f *fakeBackend) LogPath(int64) string                     { return "/nonexistent" }
func (f *fakeBackend) Open(_ context.Context, it store.Item)    { f.opened = append(f.opened, it.ID) }

var now = time.Unix(1_700_000_000, 0)

func fixture() (*Model, *fakeBackend) {
	started := now.Add(-5 * time.Minute)
	f := &fakeBackend{moved: map[int64]store.State{}, items: []store.Item{
		{ID: 1, Kind: alerts.Kind, Source: "prod", Title: "Older", State: store.StateTodo, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)},
		{ID: 2, Kind: alerts.Kind, Source: "prod", Title: "Oldest", State: store.StateTodo, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour)},
		{ID: 3, Kind: tasks.Kind, Source: tasks.Kind, Title: "Task", State: store.StateTodo, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)},
		{ID: 4, Kind: alerts.Kind, Source: "dev", Title: "Running", State: store.StateInProgress, SessionID: "s4", UpdatedAt: started},
		{ID: 5, Kind: alerts.Kind, Source: "dev", Title: "Waits", State: store.StateWaiting, SessionID: "s5", UpdatedAt: now.Add(-7 * time.Minute)},
		{ID: 6, Kind: alerts.Kind, Source: "dev", Title: "Failed", State: store.StateWaiting, SessionID: "s6", Failed: true, UpdatedAt: now.Add(-8 * time.Minute)},
		{ID: 7, Kind: alerts.Kind, Source: "dev", Title: "Resolved", State: store.StateDone, ResolvedAt: &started, UpdatedAt: now.Add(-time.Hour)},
	}}
	for i, it := range f.items {
		if it.Kind == alerts.Kind {
			f.items[i].Description = it.Source + " · a long alert summary that needs to be wrapped over more than three lines of the column"
		}
	}
	m := New(context.Background(), f, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.now = func() time.Time { return now }
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.Update(itemsMsg{items: f.items})
	return m, f
}

func press(m *Model, key string) tea.Cmd {
	var msg tea.KeyPressMsg
	switch key {
	case "enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "ctrl+d", "ctrl+u":
		msg = tea.KeyPressMsg{Code: rune(key[len(key)-1]), Mod: tea.ModCtrl}
	default:
		msg = tea.KeyPressMsg{Code: rune(key[0]), Text: key}
	}
	_, cmd := m.Update(msg)
	return cmd
}

func run(m *Model, cmd tea.Cmd) {
	if cmd != nil {
		m.Update(cmd())
	}
}

func TestSortingByTimeInState(t *testing.T) {
	m, f := fixture()
	var items []store.Item
	for i, state := range store.States {
		base := int64(10 * (i + 1))
		// Creation order is the reverse of the order the items entered the state;
		// the last two share the same time and are ordered by ID.
		items = append(items,
			store.Item{ID: base + 1, Kind: tasks.Kind, State: state, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-3 * time.Hour)},
			store.Item{ID: base + 2, Kind: tasks.Kind, State: state, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Minute)},
			store.Item{ID: base + 3, Kind: tasks.Kind, State: state, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now.Add(-2 * time.Minute)},
		)
	}
	f.items = items
	m.Update(itemsMsg{items: f.items})
	for i, state := range store.States {
		var ids []int64
		for _, it := range m.sections[i] {
			ids = append(ids, it.ID)
		}
		base := int64(10 * (i + 1))
		if want := []int64{base + 3, base + 2, base + 1}; !slices.Equal(ids, want) {
			t.Errorf("%s order = %v, want %v", state, ids, want)
		}
	}
}

func TestActions(t *testing.T) {
	m, f := fixture()
	if cmd := press(m, "enter"); cmd != nil {
		t.Fatal("enter did something")
	}
	if cmd := press(m, "r"); cmd != nil {
		t.Fatal("r on an item without session did something")
	}
	run(m, press(m, "p"))
	if len(f.started) != 1 || f.started[0] != 3 {
		t.Fatalf("started = %v", f.started)
	}
	press(m, "j")
	run(m, press(m, "d"))
	if f.moved[1] != store.StateDone {
		t.Fatalf("moved = %v", f.moved)
	}

	press(m, "l")
	if cmd := press(m, "r"); cmd != nil {
		t.Fatal("r on IN PROGRESS did something")
	}
	if cmd := press(m, "p"); cmd != nil {
		t.Fatal("p on IN PROGRESS did something")
	}
	press(m, "l")
	press(m, "g") // item 5 entered WAITING most recently
	run(m, press(m, "r"))
	if len(f.resumed) != 1 || f.resumed[0] != 5 {
		t.Fatalf("resumed = %v", f.resumed)
	}
	run(m, press(m, "t"))
	if f.moved[5] != store.StateTodo {
		t.Fatalf("moved = %v", f.moved)
	}
	press(m, "h")
	press(m, "h")
	press(m, "h")
	if m.focus != 3 {
		t.Fatalf("focus = %d", m.focus)
	}
	press(m, "R")
	if f.polled != 1 {
		t.Fatal("not polled")
	}
	if _, ok := press(m, "q")().(tea.QuitMsg); !ok {
		t.Fatal("q does not quit")
	}
}

func TestResumeWithExistingSession(t *testing.T) {
	for i, state := range store.States {
		t.Run(string(state), func(t *testing.T) {
			m, f := fixture()
			it := store.Item{ID: 10, State: state, Title: "Task", SessionID: "existing"}
			m.Update(itemsMsg{items: []store.Item{it}})
			m.focus = i
			run(m, press(m, "r"))
			if state == store.StateInProgress {
				if len(f.resumed) != 0 {
					t.Fatal("resumed IN PROGRESS item")
				}
			} else if !reflect.DeepEqual(f.resumed, []int64{it.ID}) {
				t.Fatalf("resumed = %v", f.resumed)
			}
		})
	}
}

func TestSelectionFollowsItem(t *testing.T) {
	m, f := fixture()
	press(m, "j") // item 1
	f.items = append(f.items, store.Item{ID: 9, Kind: alerts.Kind, Title: "Oldest of all", State: store.StateTodo, CreatedAt: now.Add(-4 * time.Hour), UpdatedAt: now.Add(-4 * time.Hour)})
	m.Update(itemsMsg{items: f.items})
	if it, _ := m.selected(); it.ID != 1 {
		t.Fatalf("selected = %d", it.ID)
	}
}

func TestDetails(t *testing.T) {
	m, f := fixture()
	press(m, "j") // item 1
	var details []string
	for i := range 60 {
		details = append(details, fmt.Sprintf("line %d", i))
	}
	f.items[0].Details = "# Older\n\n" + strings.Join(details, "\n\n")
	m.Update(itemsMsg{items: f.items})

	press(m, "K")
	if m.details != 1 {
		t.Fatalf("details = %d", m.details)
	}
	plain := ansi.Strip(m.Render())
	if !strings.Contains(plain, "# Older") || !strings.Contains(plain, "line 0") || strings.Contains(plain, "line 59") {
		t.Fatalf("render:\n%s", plain)
	}
	if lines := strings.Split(plain, "\n"); len(lines) != 30 {
		t.Fatalf("render has %d lines", len(lines))
	}
	// Board keys are ignored while the popup is open.
	if cmd := press(m, "p"); cmd != nil || len(f.started) != 0 {
		t.Fatal("p started an agent")
	}
	press(m, "G")
	if plain := ansi.Strip(m.Render()); !strings.Contains(plain, "line 59") || strings.Contains(plain, "line 0 ") {
		t.Fatalf("render after G:\n%s", plain)
	}
	press(m, "g")
	if m.detailsScroll != 0 {
		t.Fatalf("scroll = %d", m.detailsScroll)
	}
	press(m, "esc")
	if m.details != 0 || strings.Contains(ansi.Strip(m.Render()), "line 0") {
		t.Fatal("popup not closed")
	}

	// Items without details show their title and description.
	press(m, "g") // item 3, a task
	press(m, "K")
	if it, _ := m.selected(); it.ID != 3 || !strings.Contains(ansi.Strip(m.Render()), "# Task") {
		t.Fatalf("render:\n%s", ansi.Strip(m.Render()))
	}

	// The popup closes when its item is gone.
	m.Update(itemsMsg{items: f.items[3:]})
	if m.details != 0 {
		t.Fatal("popup of a deleted item still open")
	}
}

func TestOpen(t *testing.T) {
	m, f := fixture()
	cmd := press(m, "o")
	if cmd == nil {
		t.Fatal("no command")
	}
	cmd()
	if it, _ := m.selected(); !reflect.DeepEqual(f.opened, []int64{it.ID}) {
		t.Fatalf("opened = %v, want %d", f.opened, it.ID)
	}
}

func TestRender(t *testing.T) {
	m, _ := fixture()
	out := m.Render()
	lines := strings.Split(out, "\n")
	if len(lines) != 30 {
		t.Fatalf("lines = %d", len(lines))
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w != 100 {
			t.Errorf("line %d width = %d: %q", i, w, ansi.Strip(l))
		}
	}
	plain := ansi.Strip(out)
	for _, want := range []string{
		"TO DO (3)", "IN PROGRESS (1)", "WAITING (2)", "DONE (1)",
		"Oldest", "3h", "1m", "5m", "7m", "8m", "1h", "prod · a long alert", "to be wrapped over…",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("render lacks %q:\n%s", want, plain)
		}
	}
}

func TestActionErrorsAreLogged(t *testing.T) {
	m, f := fixture()
	var log bytes.Buffer
	m.log = slog.New(slog.NewTextHandler(&log, nil))
	f.err = errors.New("boom")
	run(m, press(m, "p"))
	if !strings.Contains(log.String(), `msg="start failed"`) || !strings.Contains(log.String(), "err=boom") {
		t.Fatalf("log = %q", log.String())
	}
}

func TestScrolling(t *testing.T) {
	m, f := fixture()
	for i := range 40 {
		f.items = append(f.items, store.Item{ID: int64(100 + i), Kind: tasks.Kind, Title: "t", State: store.StateTodo, CreatedAt: now.Add(time.Duration(i)), UpdatedAt: now.Add(time.Duration(i))})
	}
	m.Update(itemsMsg{items: f.items})
	press(m, "G")
	out := ansi.Strip(m.Render())
	if !strings.Contains(out, "▌") {
		t.Fatal("selected card not visible")
	}
}

func TestHalfPage(t *testing.T) {
	m, f := fixture()
	for i := range 40 {
		f.items = append(f.items, store.Item{ID: int64(100 + i), Kind: tasks.Kind, Title: "t", State: store.StateTodo, CreatedAt: now.Add(time.Duration(i)), UpdatedAt: now.Add(time.Duration(i))})
	}
	m.Update(itemsMsg{items: f.items})
	press(m, "g")
	m.Render()
	// 27 rows of one-line cards separated by empty lines show 14 cards.
	if m.visible[0] != 14 {
		t.Fatalf("visible = %d", m.visible[0])
	}
	press(m, "ctrl+d")
	if m.cursor[0] != 7 {
		t.Fatalf("cursor after ctrl+d = %d", m.cursor[0])
	}
	press(m, "ctrl+u")
	press(m, "ctrl+u")
	if m.cursor[0] != 0 {
		t.Fatalf("cursor after ctrl+u = %d", m.cursor[0])
	}
	for range 10 {
		press(m, "ctrl+d")
		m.Render()
	}
	if m.cursor[0] != len(m.sections[0])-1 {
		t.Fatalf("cursor = %d, want last %d", m.cursor[0], len(m.sections[0])-1)
	}
}

func TestWrap(t *testing.T) {
	if got := wrap("", 10, 3); len(got) != 0 {
		t.Errorf("empty = %q", got)
	}
	if got := wrap("one two\n\nthree", 10, 3); !reflect.DeepEqual(got, []string{"one two", "three"}) {
		t.Errorf("short = %q", got)
	}
	got := wrap("aaaa bbbb cccc dddd eeee", 9, 2)
	if len(got) != 2 || got[0] != "aaaa bbbb" || !strings.HasSuffix(got[1], "…") {
		t.Errorf("long = %q", got)
	}
	for _, l := range got {
		if ansi.StringWidth(l) > 9 {
			t.Errorf("line %q wider than 9", l)
		}
	}
}
