// Package ui renders the board: one column per state and a statusline with
// the keys. Errors are written to the log, not shown.
package ui

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/ricoberger/tower/internal/store"
)

// Backend is everything the UI does besides rendering.
type Backend interface {
	// Items returns all items. It also detects agents whose wrapper died.
	Items(ctx context.Context) ([]store.Item, error)
	Start(ctx context.Context, it store.Item) error
	Resume(ctx context.Context, it store.Item) error
	Move(ctx context.Context, id int64, to store.State) error
	// CreateTask adds a task from the edited text; empty text creates
	// nothing.
	CreateTask(ctx context.Context, text string) error
	PollNow()
	LogPath(id int64) string
	// Open opens an item's URL; it is called outside the UI loop.
	Open(ctx context.Context, it store.Item)
}

// Model is the Bubble Tea model.
type Model struct {
	ctx     context.Context
	backend Backend

	width, height int
	sections      [4][]store.Item
	focus         int
	cursor        [4]int
	offset        [4]int
	// visible is the number of cards shown per column in the last render.
	visible [4]int
	// details is the ID of the item shown in the details popup, 0 if it is
	// closed; detailsScroll is the popup's first line.
	details       int64
	detailsScroll int
	log           *slog.Logger
	now           func() time.Time
}

// New creates the model. Failed actions are written to log.
func New(ctx context.Context, b Backend, log *slog.Logger) *Model {
	return &Model{ctx: ctx, backend: b, log: log, now: time.Now}
}

type (
	tickMsg  time.Time
	itemsMsg struct {
		items []store.Item
		err   error
	}
	// resultMsg reloads the items after an action.
	resultMsg struct{}
)

// Init loads the items and starts the refresh loop.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(m.load(), m.tick())
}

func (m *Model) tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) load() tea.Cmd {
	return func() tea.Msg {
		items, err := m.backend.Items(m.ctx)
		return itemsMsg{items, err}
	}
}

// Update handles messages.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tickMsg:
		return m, tea.Batch(m.load(), m.tick())
	case itemsMsg:
		if msg.err != nil {
			m.log.Warn("load items", "err", msg.err)
			return m, nil
		}
		m.setItems(msg.items)
		if _, ok := m.detailsItem(); !ok {
			m.details = 0
		}
	case resultMsg:
		return m, m.load()
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// result logs a failed action and reloads the items.
func (m *Model) result(action string, it store.Item, err error) tea.Msg {
	if err != nil {
		m.log.Warn(action+" failed", "item", it.ID, "title", it.Title, "err", err)
	}
	return resultMsg{}
}

// setItems groups and sorts the items and keeps each section's selection on
// the same item.
func (m *Model) setItems(items []store.Item) {
	var selected [4]int64
	for i := range m.sections {
		if it, ok := m.selectedIn(i); ok {
			selected[i] = it.ID
		}
	}
	var sections [4][]store.Item
	for _, it := range items {
		// Older stored items and future providers may supply multiline titles.
		it.Title = strings.Join(strings.Fields(it.Title), " ")
		if i := slices.Index(store.States, it.State); i >= 0 {
			sections[i] = append(sections[i], it)
		}
	}
	for i := range sections {
		m.sort(sections[i])
		if selected[i] != 0 {
			if j := slices.IndexFunc(sections[i], func(it store.Item) bool { return it.ID == selected[i] }); j >= 0 {
				m.cursor[i] = j
			}
		}
		m.cursor[i] = max(0, min(m.cursor[i], len(sections[i])-1))
	}
	m.sections = sections
}

// sort orders items by the time they entered their state, most recent first,
// so the order matches the age shown on the cards. The source's creation time
// is not comparable across kinds and can predate the item's arrival on the
// board.
func (m *Model) sort(items []store.Item) {
	slices.SortStableFunc(items, func(a, b store.Item) int {
		return cmpInt64(b.UpdatedAt.Unix(), a.UpdatedAt.Unix(), b.ID, a.ID)
	})
}

func cmpInt64(a, b, tieA, tieB int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	case tieA < tieB:
		return -1
	case tieA > tieB:
		return 1
	}
	return 0
}

func (m *Model) selectedIn(section int) (store.Item, bool) {
	items := m.sections[section]
	c := m.cursor[section]
	if c < 0 || c >= len(items) {
		return store.Item{}, false
	}
	return items[c], true
}

func (m *Model) selected() (store.Item, bool) { return m.selectedIn(m.focus) }

func (m *Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.String()
	if m.details != 0 {
		return m.handleDetailsKey(key)
	}
	switch key {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "l", "right":
		m.focus = (m.focus + 1) % len(m.sections)
	case "h", "left":
		m.focus = (m.focus + len(m.sections) - 1) % len(m.sections)
	case "j", "down":
		m.cursor[m.focus] = min(m.cursor[m.focus]+1, max(len(m.sections[m.focus])-1, 0))
	case "k", "up":
		m.cursor[m.focus] = max(m.cursor[m.focus]-1, 0)
	case "g", "home":
		m.cursor[m.focus] = 0
	case "G", "end":
		m.cursor[m.focus] = max(len(m.sections[m.focus])-1, 0)
	case "ctrl+d", "pgdown":
		m.cursor[m.focus] = min(m.cursor[m.focus]+m.halfPage(), max(len(m.sections[m.focus])-1, 0))
	case "ctrl+u", "pgup":
		m.cursor[m.focus] = max(m.cursor[m.focus]-m.halfPage(), 0)
	case "R":
		m.backend.PollNow()
	case "n":
		return m, m.newTask()
	case "p":
		return m, m.start()
	case "r":
		return m, m.resume()
	case "d":
		return m, m.move(store.StateDone)
	case "t":
		return m, m.move(store.StateTodo)
	case "o":
		return m, m.open()
	case "L":
		return m, m.openLog()
	case "K":
		if it, ok := m.selected(); ok {
			m.details, m.detailsScroll = it.ID, 0
		}
	}
	return m, nil
}

// halfPage is half the number of cards visible in the focused column, at
// least 1.
func (m *Model) halfPage() int {
	return max(m.visible[m.focus]/2, 1)
}

// handleDetailsKey handles the keys while the details popup is open.
func (m *Model) handleDetailsKey(key string) (tea.Model, tea.Cmd) {
	page := max(m.detailsHeight()-2, 1)
	switch key {
	case "ctrl+c":
		return m, tea.Quit
	case "K", "q", "esc":
		m.details = 0
	case "j", "down":
		m.detailsScroll++
	case "k", "up":
		m.detailsScroll--
	case "ctrl+d", "pgdown":
		m.detailsScroll += max(page/2, 1)
	case "ctrl+u", "pgup":
		m.detailsScroll -= max(page/2, 1)
	case "g", "home":
		m.detailsScroll = 0
	case "G", "end":
		m.detailsScroll = len(m.detailsLines())
	}
	// Clamp; the view clamps again when the content or size changes.
	m.detailsScroll = max(min(m.detailsScroll, len(m.detailsLines())-page), 0)
	return m, nil
}

// detailsItem returns the item shown in the details popup.
func (m *Model) detailsItem() (store.Item, bool) {
	for _, items := range m.sections {
		for _, it := range items {
			if it.ID == m.details {
				return it, true
			}
		}
	}
	return store.Item{}, false
}

func (m *Model) start() tea.Cmd {
	it, ok := m.selected()
	if !ok || it.State != store.StateTodo {
		return nil
	}
	return func() tea.Msg {
		return m.result("start", it, m.backend.Start(m.ctx, it))
	}
}

func (m *Model) resume() tea.Cmd {
	it, ok := m.selected()
	if !ok || it.State == store.StateInProgress || it.SessionID == "" {
		return nil
	}
	return func() tea.Msg {
		return m.result("resume", it, m.backend.Resume(m.ctx, it))
	}
}

func (m *Model) move(to store.State) tea.Cmd {
	it, ok := m.selected()
	if !ok || it.State == to {
		return nil
	}
	return func() tea.Msg {
		return m.result("move", it, m.backend.Move(m.ctx, it.ID, to))
	}
}

func (m *Model) open() tea.Cmd {
	it, ok := m.selected()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		m.backend.Open(m.ctx, it)
		return nil
	}
}

func (m *Model) openLog() tea.Cmd {
	it, ok := m.selected()
	if !ok {
		return nil
	}
	path := m.backend.LogPath(it.ID)
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return tea.ExecProcess(editor(path), func(err error) tea.Msg {
		return m.result("open log", it, err)
	})
}

func (m *Model) newTask() tea.Cmd {
	f, err := os.CreateTemp("", "tower-task-*.md")
	if err != nil {
		m.log.Warn("new task failed", "err", err)
		return nil
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		m.log.Warn("new task failed", "err", err)
		return nil
	}
	return tea.ExecProcess(editor(path), func(err error) tea.Msg {
		defer func() { _ = os.Remove(path) }()
		if err == nil {
			var data []byte
			if data, err = os.ReadFile(path); err == nil { // #nosec G304 -- our temp file
				err = m.backend.CreateTask(m.ctx, string(data))
			}
		}
		if err != nil {
			m.log.Warn("new task failed", "err", err)
		}
		return resultMsg{}
	})
}

// editor returns the command that opens path in $EDITOR (default vi).
func editor(path string) *exec.Cmd {
	e := strings.TrimSpace(os.Getenv("EDITOR"))
	if e == "" {
		e = "vi"
	}
	// $EDITOR may contain arguments (e.g. "code --wait").
	return exec.Command("sh", "-c", e+` "$1"`, "sh", path) //nolint:gosec,noctx // the user's editor; tea.ExecProcess owns its lifetime
}
