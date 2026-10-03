package ui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ricoberger/tower/internal/store"
)

var (
	accent = lipgloss.Color("#c6a0f6")
	faint  = lipgloss.NewStyle().Faint(true)
	bold   = lipgloss.NewStyle().Bold(true)
	red    = lipgloss.NewStyle().Foreground(lipgloss.Red)
	yellow = lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	green  = lipgloss.NewStyle().Foreground(lipgloss.Green)
	gray   = lipgloss.NewStyle().Foreground(lipgloss.BrightBlack)
)

func stateTitle(s store.State) string {
	switch s {
	case store.StateTodo:
		return "TO DO"
	case store.StateInProgress:
		return "IN PROGRESS"
	case store.StateWaiting:
		return "WAITING"
	case store.StateDone:
		return "DONE"
	}
	return string(s)
}

// View renders the board.
func (m *Model) View() tea.View {
	v := tea.NewView(m.Render())
	v.AltScreen = true
	return v
}

// Render returns the board as a string: one column per state, like a kanban
// board, and the statusline.
func (m *Model) Render() string {
	n := len(m.sections)
	if m.width < n*20 || m.height < 1+3 {
		return "terminal too small"
	}
	cols := make([][]string, n)
	for i := range m.sections {
		w := m.width / n
		if i < m.width%n {
			w++
		}
		cols[i] = strings.Split(strings.TrimSuffix(m.renderColumn(i, w, m.height-1), "\n"), "\n")
	}
	var b strings.Builder
	for row := range m.height - 1 {
		for _, c := range cols {
			b.WriteString(c[row])
		}
		b.WriteString("\n")
	}
	board := strings.TrimSuffix(b.String(), "\n")
	if popup, ok := m.renderDetails(); ok {
		w, h := lipgloss.Width(popup), lipgloss.Height(popup)
		c := lipgloss.NewCanvas(m.width, m.height-1)
		c.Compose(lipgloss.NewCompositor(
			lipgloss.NewLayer(board),
			lipgloss.NewLayer(popup).X((m.width-w)/2).Y((m.height-1-h)/2).Z(1),
		))
		board = c.Render()
	}
	return board + "\n" + m.statusline()
}

// detailsWidth and detailsHeight include the border. Shrink the margins
// before removing the last content row; Render rejects smaller terminals.
func (m *Model) detailsWidth() int {
	return max(min(m.width-10, 180), 6)
}

func (m *Model) detailsHeight() int {
	return max(min(m.height-10, 180), 3)
}

// detailsLines returns the wrapped content of the details popup: the item's
// details, or its title and description when it has none.
func (m *Model) detailsLines() []string {
	it, ok := m.detailsItem()
	if !ok {
		return nil
	}
	text := it.Details
	if strings.TrimSpace(text) == "" {
		text = "# " + it.Title + "\n\n" + it.Description
	}
	return strings.Split(ansi.Wrap(strings.TrimSpace(text), m.detailsWidth()-4, ""), "\n")
}

// renderDetails renders the details popup of the selected item; ok is false
// when it is closed or its item is gone.
func (m *Model) renderDetails() (string, bool) {
	it, ok := m.detailsItem()
	if m.details == 0 || !ok {
		return "", false
	}
	lines := m.detailsLines()
	rows := m.detailsHeight() - 2
	m.detailsScroll = max(min(m.detailsScroll, len(lines)-rows), 0)
	title := it.Title
	if len(lines) > rows {
		title += fmt.Sprintf(" (%d%%)", 100*(m.detailsScroll+rows)/len(lines))
	}
	return strings.TrimSuffix(box(title, lines[m.detailsScroll:], m.detailsWidth(), m.detailsHeight(), true), "\n"), true
}

// renderColumn renders the cards of a state. Cards are separated by an empty
// line; the column scrolls so that the selected card is visible.
func (m *Model) renderColumn(i, width, height int) string {
	items := m.sections[i]
	focused := i == m.focus
	inner := width - 4
	rows := height - 2
	cards := make([][]string, len(items))
	for j, it := range items {
		cards[j] = m.renderCard(it, inner, focused && j == m.cursor[i])
	}

	// fits reports whether the cards from..to (inclusive) fit into rows.
	fits := func(from, to int) bool {
		used := 0
		for j := from; j <= to; j++ {
			used += len(cards[j])
			if j > from {
				used++
			}
		}
		return used <= rows
	}
	c := m.cursor[i]
	off := min(m.offset[i], c)
	for off < c && !fits(off, c) {
		off++
	}
	// Scroll back up when the cards above fit as well, e.g. after deletions.
	for off > 0 && fits(off-1, len(items)-1) {
		off--
	}
	m.offset[i] = max(off, 0)

	var lines []string
	m.visible[i] = 0
	for j := m.offset[i]; j < len(items); j++ {
		if j > m.offset[i] {
			lines = append(lines, "")
		}
		lines = append(lines, cards[j]...)
		if len(lines) > rows {
			break
		}
		m.visible[i]++
	}
	title := fmt.Sprintf("%s (%d)", stateTitle(store.States[i]), len(items))
	return box(title, lines, width, height, focused)
}

// renderCard renders an item as its title with the time since it entered its
// state on the right, followed by up to three lines of its description.
func (m *Model) renderCard(it store.Item, width int, selected bool) []string {
	marker := "  "
	if selected {
		marker = lipgloss.NewStyle().Foreground(accent).Render("▌ ")
	}
	w := max(width-2, 1)
	age := m.age(it)
	titleW := max(w-ansi.StringWidth(age)-1, 1)
	title := ansi.Truncate(it.Title, titleW, "…")
	if selected {
		title = bold.Render(title)
	}
	lines := []string{marker + pad(title, titleW) + " " + age}
	for _, l := range wrap(it.Description, w, 3) {
		lines = append(lines, marker+faint.Render(l))
	}
	return lines
}

// age is the time since the item entered its state. It is red for failed
// agents and green for resolved alerts that are still to do.
func (m *Model) age(it store.Item) string {
	text := ago(m.now().Sub(it.UpdatedAt))
	switch {
	case it.State == store.StateWaiting && it.Failed:
		return red.Render(text)
	case it.State == store.StateTodo && it.ResolvedAt != nil:
		return green.Render(text)
	case it.State == store.StateInProgress:
		return yellow.Render(text)
	}
	return faint.Render(text)
}

// wrap word-wraps s to width and returns at most n non-empty lines; the last
// line ends with "…" when text was cut.
func wrap(s string, width, n int) []string {
	var lines []string
	for l := range strings.SplitSeq(ansi.Wrap(strings.TrimSpace(s), width, ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[:n]
		lines[n-1] = ansi.Truncate(lines[n-1]+" …", width, "…")
	}
	return lines
}

func (m *Model) statusline() string {
	keys := "p progress · r resume · d done · t todo · n new · o open · K details · L log · R refresh · q quit"
	return pad(ansi.Truncate(faint.Render(keys), m.width, "…"), m.width)
}

// box draws a rounded border with the title in the top edge. height includes
// the border; missing lines are left empty.
func box(title string, lines []string, width, height int, focused bool) string {
	border := gray
	titleStyle := bold
	if focused {
		border = lipgloss.NewStyle().Foreground(accent)
		titleStyle = bold.Foreground(accent)
	}
	inner := width - 2
	t := " " + ansi.Truncate(title, inner-3, "…") + " "
	var b strings.Builder
	b.WriteString(border.Render("╭─") + titleStyle.Render(t) + border.Render(strings.Repeat("─", max(inner-1-ansi.StringWidth(t), 0))+"╮") + "\n")
	for i := range height - 2 {
		l := ""
		if i < len(lines) {
			l = ansi.Truncate(lines[i], inner-2, "…")
		}
		b.WriteString(border.Render("│") + " " + pad(l, inner-2) + " " + border.Render("│") + "\n")
	}
	b.WriteString(border.Render("╰"+strings.Repeat("─", inner)+"╯") + "\n")
	return b.String()
}

func pad(s string, w int) string {
	if d := w - ansi.StringWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// ago formats a duration compactly: 45s, 12m, 3h, 2d.
func ago(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
