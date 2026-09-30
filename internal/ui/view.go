package ui

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/ricoberger/tower/internal/item"
	"github.com/ricoberger/tower/internal/snapshot"
)

// View renders the header, the item list, the preview and the footer.
func (m *Model) View() tea.View {
	v := tea.NewView(m.Render())
	v.AltScreen = true
	return v
}

// Render returns the screen content.
func (m *Model) Render() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	header := padTo(" "+Truncate(m.headerText(), m.width-2), m.width)
	footer := padTo(" "+Truncate(m.footerText(), m.width-2), m.width)
	bodyH := m.height - 2
	if bodyH < 4 {
		return strings.Join([]string{header, footer}[:min(m.height, 2)], "\n")
	}
	listW := max(min(m.width*2/5, 60), min(m.width, 24))
	previewW := m.width - listW
	list := Frame(listW, bodyH, FrameState{Title: "Items", Status: m.listStatus()}, m.focus == focusList, m.listContent(listW, bodyH))
	var right string
	if previewW >= 4 {
		title, content := m.previewTitle(), ""
		if m.help {
			title = "Help"
		}
		cw, ch := FrameContentSize(previewW, bodyH)
		content = m.previewContent(cw, ch)
		right = Frame(previewW, bodyH, FrameState{Title: title}, m.focus == focusPreview && !m.help, content)
	}
	body := list
	if right != "" {
		body = lipgloss.JoinHorizontal(lipgloss.Top, list, right)
	}
	return header + "\n" + body + "\n" + footer
}

// headerText renders source health and the run counters.
func (m *Model) headerText() string {
	if !m.hasSnap {
		return Bold("tower") + " · " + Dim("starting…")
	}
	var parts []string
	// A failed notification comes first so a narrow terminal still shows it.
	if n := m.snap.Notify; n.LastErr != "" {
		parts = append(parts, "notification "+Colored("red", "✗ "+SanitizeLine(n.LastErr))+" ("+SanitizeLine(n.ItemTitle)+")")
	}
	for _, s := range m.snap.Sources {
		name := SanitizeLine(s.Name)
		switch {
		case s.LastErr != "":
			parts = append(parts, name+" "+Colored("red", "✗ "+SanitizeLine(s.LastErr)))
		case s.LastSuccess.IsZero():
			parts = append(parts, name+" "+Dim("… never polled"))
		default:
			parts = append(parts, name+" "+Colored("green", "✓")+" "+FormatAge(m.now.Sub(s.LastSuccess))+" ago")
		}
	}
	counters := fmt.Sprintf("running %d/%d · queued %d", m.snap.Running, m.snap.Concurrency, m.snap.Queued)
	if m.snap.Unreadable > 0 {
		counters += " · " + Colored("yellow", fmt.Sprintf("%d unreadable", m.snap.Unreadable))
	}
	return Bold("tower") + " · " + strings.Join(parts, " · ") + " │ " + counters
}

func (m *Model) listStatus() string {
	if m.showDone {
		return Dim("+done")
	}
	return ""
}

// footerText renders the pending confirmation, the latest hint or the key
// hints.
func (m *Model) footerText() string {
	if c := m.confirm; c != nil {
		title := SanitizeLine(c.title)
		switch c.kind {
		case confirmDismiss:
			return Colored("yellow", fmt.Sprintf("Dismiss %q? y to confirm, n/esc to cancel", title))
		case confirmResume:
			return Colored("yellow", fmt.Sprintf("Run %d of %q is still executing; resuming starts a second process on the same session. y to resume, n/esc to cancel", c.run, title))
		}
	}
	if m.help {
		return Dim("j/k ↓/↑ g/G scroll help · ? / esc close help · q quit")
	}
	if m.footer != "" {
		if m.footerErr {
			return Colored("red", SanitizeLine(m.footer))
		}
		return SanitizeLine(m.footer)
	}
	return Dim("o report · c resume · C resume in tab · p prep now · x dismiss · l logs · b source · a dir · d done · r poll · ? help · q quit")
}

// runLabel describes a run outcome.
func runLabel(r item.Run) string {
	switch r.Outcome {
	case item.OutcomeRunning:
		return "preparing"
	case item.OutcomeReady:
		return "ready"
	case item.OutcomeBlocked:
		return "blocked"
	case item.OutcomeFailed:
		return "failed"
	case item.OutcomeInterrupted:
		return "interrupted"
	case item.OutcomeCancelled:
		return "cancelled"
	}
	return string(r.Outcome)
}

// rowStatus returns the icon and right-aligned status of a list row.
func (m *Model) rowStatus(v snapshot.ItemView) (string, string) {
	it := v.Item
	r, hasRun := v.Latest()
	if hasRun && r.Outcome == item.OutcomeRunning {
		if r.StartedAt != nil {
			return "⟳", FormatElapsed(m.now.Sub(*r.StartedAt))
		}
		return "⟳", "starting"
	}
	switch it.State {
	case item.StateNeedsYou:
		if !hasRun {
			return "●", "needs you"
		}
		switch r.Outcome {
		case item.OutcomeReady:
			return "●", "ready"
		case item.OutcomeBlocked:
			return "◐", "blocked"
		default:
			return "✗", runLabel(r)
		}
	case item.StateQueued:
		return "…", "queued"
	case item.StatePreparing:
		return "⟳", "preparing"
	case item.StateNew:
		if it.Alert.Status == item.AlertSuppressed {
			return "○", "suppressed"
		}
		due := it.Alert.StartsAt.Add(m.opts.PrepareAfter)
		if due.After(m.now) {
			return "○", "prep in " + FormatElapsed(due.Sub(m.now))
		}
		return "○", "prep due"
	case item.StateSnoozed:
		return "☾", "snoozed"
	case item.StateResolved:
		if it.Alert.ResolvedAt != nil {
			return "✓", "resolved " + FormatAge(m.now.Sub(*it.Alert.ResolvedAt))
		}
		return "✓", "resolved"
	case item.StateDone:
		return "✔", "done " + FormatAge(m.now.Sub(it.UpdatedAt))
	}
	return " ", string(it.State)
}

// listContent renders the grouped list with the selection kept visible.
func (m *Model) listContent(width, height int) string {
	cw, ch := FrameContentSize(width, height)
	if len(m.visible) == 0 {
		if !m.hasSnap {
			return Dim("loading…")
		}
		return Dim("No items.")
	}
	var rows []string
	selRow := 0
	for _, g := range m.groups {
		rows = append(rows, Bold(strings.ToUpper(g.label)+fmt.Sprintf(" (%d)", len(g.items))))
		for _, v := range g.items {
			sel := v.Item.ID == m.selected
			if sel {
				selRow = len(rows)
			}
			rows = append(rows, m.row(v, sel, cw))
		}
	}
	return ListView(rows, selRow, ch, 0)
}

func (m *Model) row(v snapshot.ItemView, selected bool, width int) string {
	icon, status := m.rowStatus(v)
	style := lipgloss.NewStyle()
	if !v.Item.Seen {
		style = style.Bold(true)
	}
	if v.Item.State == item.StateResolved || v.Item.State == item.StateDone {
		style = style.Faint(true)
	}
	titleW := max(width-2-2-ansi.StringWidth(status)-1, 1)
	title := padTo(Truncate(icon+" "+SanitizeLine(v.Item.Title), titleW), titleW)
	return Marker(selected) + style.Render(title+" "+status)
}

func (m *Model) previewTitle() string {
	v, ok := m.selectedView()
	if !ok {
		return "Preview"
	}
	return SanitizeLine(v.Item.Title)
}

// previewContent wraps the preview (or help) lines to the width and applies
// the scroll offset.
func (m *Model) previewContent(width, height int) string {
	if width <= 0 || height <= 0 {
		return ""
	}
	var logical []string
	if m.help {
		logical = helpLines()
	} else {
		logical = m.previewLines(width)
	}
	var lines []string
	for _, l := range logical {
		lines = append(lines, strings.Split(ansi.Wrap(l, width, ""), "\n")...)
	}
	var scroll int
	if m.help {
		m.helpScroll = min(m.helpScroll, max(len(lines)-height, 0))
		scroll = m.helpScroll
	} else {
		m.scroll = min(m.scroll, max(len(lines)-height, 0))
		scroll = m.scroll
	}
	end := min(scroll+height, len(lines))
	if scroll >= end {
		return ""
	}
	return strings.Join(lines[scroll:end], "\n")
}

func section(title string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(Mauve).Render(title)
}

// previewLines builds the preview of the selected item. Markdown documents
// are rendered for the width; other lines are wrapped by the caller.
func (m *Model) previewLines(width int) []string {
	v, ok := m.selectedView()
	if !ok {
		if !m.hasSnap {
			return []string{Dim("Waiting for the engine…")}
		}
		return []string{Dim("No item selected.")}
	}
	it := v.Item
	r, hasRun := v.Latest()
	// The title is shown in the frame border.
	lines := []string{m.metaLine(v), ""}
	if !hasRun {
		switch it.State {
		case item.StateNew:
			_, status := m.rowStatus(v)
			lines = append(lines, "No preparation run yet ("+status+").")
		case item.StateQueued:
			lines = append(lines, "Queued for preparation.")
		default:
			lines = append(lines, "No preparation run yet. Press p to prepare now.")
		}
		return append(append(lines, ""), m.alertLines(it.ID, width)...)
	}
	switch r.Outcome {
	case item.OutcomeRunning:
		elapsed := "starting"
		if r.StartedAt != nil {
			elapsed = FormatElapsed(m.now.Sub(*r.StartedAt))
		}
		lines = append(lines, fmt.Sprintf("Preparing run %d · elapsed %s", r.Number, elapsed), "")
		p := m.prog
		switch {
		case p.id != it.ID || p.run != r.Number:
			lines = append(lines, Dim("Reading progress…"))
		case p.message != "":
			lines = append(lines, "› "+p.message)
		default:
			lines = append(lines, Dim("No assistant message yet."))
		}
		if p.id == it.ID && p.run == r.Number && p.err != nil {
			lines = append(lines, Colored("red", "output.jsonl: "+SanitizeLine(p.err.Error())))
		}
		return lines
	case item.OutcomeReady, item.OutcomeBlocked:
		return append(lines, m.resultLines(it.ID, r, width)...)
	default:
		return append(lines, m.failureLines(it.ID, r)...)
	}
}

// metaLine renders severity, source, alert status, run and confidence.
func (m *Model) metaLine(v snapshot.ItemView) string {
	it := v.Item
	parts := []string{}
	if it.Severity != "" {
		parts = append(parts, SanitizeLine(it.Severity))
	} else {
		parts = append(parts, "no severity")
	}
	parts = append(parts, SanitizeLine(it.Source.Name))
	a := it.Alert
	switch a.Status {
	case item.AlertResolved:
		if a.ResolvedAt != nil {
			parts = append(parts, "resolved "+FormatAge(m.now.Sub(*a.ResolvedAt))+" ago")
		} else {
			parts = append(parts, "resolved")
		}
	case item.AlertSuppressed:
		parts = append(parts, "suppressed, started "+FormatAge(m.now.Sub(a.StartsAt))+" ago")
	default:
		parts = append(parts, "firing "+FormatAge(m.now.Sub(a.StartsAt)))
	}
	parts = append(parts, string(it.State))
	if r, ok := v.Latest(); ok {
		parts = append(parts, fmt.Sprintf("run %d %s", r.Number, runLabel(r)))
		if m.art.key.id == it.ID && m.art.key.run == r.Number && m.art.result != nil && m.art.result.Confidence != "" {
			parts = append(parts, SanitizeLine(m.art.result.Confidence)+" confidence")
		}
	} else {
		parts = append(parts, "no run")
	}
	return Dim(strings.Join(parts, " · "))
}

func (m *Model) loadedFor(id string, r item.Run) bool {
	return m.art.key == previewKey{id: id, run: r.Number, outcome: r.Outcome}
}

// alertLines renders the alert.md of an item without a run.
func (m *Model) alertLines(id string, width int) []string {
	d := m.alert
	switch {
	case d.id != id:
		return []string{Dim("Loading alert.md…")}
	case errors.Is(d.err, fs.ErrNotExist):
		return []string{Dim("No alert.md yet.")}
	case d.err != nil:
		return []string{Dim("alert.md: " + SanitizeLine(d.err.Error()))}
	}
	return m.markdownLines(d.text, width)
}

// resultLines renders a ready/blocked run: the rendered report, then the
// summary, gate question, proposed actions and assumptions of result.json.
func (m *Model) resultLines(id string, r item.Run, width int) []string {
	if !m.loadedFor(id, r) {
		return []string{Dim("Loading report…")}
	}
	a := m.art
	var lines []string
	if r.Outcome == item.OutcomeBlocked && a.result != nil {
		lines = append(lines, Colored("yellow", "Blocked: the preparation needs input before it can continue."), "")
	}
	if a.reportErr != nil {
		lines = append(lines, Colored("red", "report.md: "+SanitizeLine(a.reportErr.Error())))
	} else {
		lines = append(lines, m.markdownLines(a.report, width)...)
	}
	lines = append(lines, "")
	if a.resultErr != nil {
		lines = append(lines, Colored("red", "result.json: "+SanitizeLine(a.resultErr.Error())), "")
	}
	if res := a.result; res != nil {
		lines = append(lines, section("Summary"))
		lines = append(lines, strings.Split(Sanitize(res.Summary), "\n")...)
		if res.RootCause != "" {
			lines = append(lines, "", section("Root cause"))
			lines = append(lines, strings.Split(Sanitize(res.RootCause), "\n")...)
		}
		lines = append(lines, "", section("Question"))
		lines = append(lines, strings.Split(Sanitize(res.Gate.Question), "\n")...)
		for i, o := range res.Gate.Options {
			lines = append(lines, fmt.Sprintf("  %d) %s", i+1, SanitizeLine(o)))
		}
		lines = append(lines, "", section("Proposed actions"))
		if len(res.ProposedActions) == 0 {
			lines = append(lines, Dim("none"))
		}
		for _, pa := range res.ProposedActions {
			lines = append(lines, fmt.Sprintf("%d. %s  %s", pa.ID, SanitizeLine(pa.Type), Bold(SanitizeLine(pa.Title))))
			for l := range strings.SplitSeq(Sanitize(pa.Description), "\n") {
				lines = append(lines, "   "+l)
			}
			if pa.Preview != "" {
				lines = append(lines, "   "+Dim("preview:"))
				for l := range strings.SplitSeq(Sanitize(pa.Preview), "\n") {
					lines = append(lines, "     "+l)
				}
			}
		}
		lines = append(lines, "", section("Assumptions"))
		if len(res.Assumptions) == 0 {
			lines = append(lines, Dim("none"))
		}
		for _, as := range res.Assumptions {
			lines = append(lines, "• "+SanitizeLine(as))
		}
	}
	return lines
}

// failureLines renders a failed, interrupted or cancelled run: its error
// and the tail of stderr.log.
func (m *Model) failureLines(id string, r item.Run) []string {
	msg := r.Error
	if msg == "" {
		msg = "no error recorded"
	}
	lines := []string{Colored("red", fmt.Sprintf("Run %d %s: %s", r.Number, runLabel(r), SanitizeLine(msg))), ""}
	if !m.loadedFor(id, r) {
		return append(lines, Dim("Loading stderr.log…"))
	}
	lines = append(lines, section(fmt.Sprintf("stderr.log (last %d lines)", StderrLines)))
	switch {
	case m.art.stderrErr != nil:
		lines = append(lines, Dim("stderr.log: "+SanitizeLine(m.art.stderrErr.Error())))
	case len(m.art.stderr) == 0:
		lines = append(lines, Dim("empty"))
	default:
		for _, l := range m.art.stderr {
			lines = append(lines, SanitizeLine(l))
		}
	}
	return lines
}

func helpLines() []string {
	return []string{
		section("Navigation"),
		"j / k, ↓ / ↑   move the selection (scroll when the preview is focused)",
		"g / G         first / last item",
		"tab           switch focus between list and preview",
		"",
		section("Item actions"),
		"enter / o     open the latest report in the editor (marks seen)",
		"c             resume the latest session in Ghostty (configured placement)",
		"C             resume the latest session in a new Ghostty tab",
		"p             prepare now / re-run",
		"x             dismiss (asks for y)",
		"l             open output.jsonl and stderr.log in the editor",
		"b             open the runbook, else the source URL, in the browser",
		"a             open the item directory in the editor",
		"",
		section("View"),
		"d             show / hide done items of the last 7 days",
		"r             poll all sources now",
		"?             show / hide this help",
		"q / ctrl+c    quit (running preparations continue)",
		"",
		section("Confirmations"),
		"Dismissing and resuming a still executing run ask in the footer.",
		"Only y confirms; n or esc cancels. Other keys are ignored while a",
		"confirmation or this help is open.",
		"",
		section("This help"),
		"j / k, ↓ / ↑, g / G scroll this help; ? or esc closes it.",
	}
}
