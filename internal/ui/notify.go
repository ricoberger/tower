package ui

import (
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/ricoberger/tower/internal/store"
)

// notifications returns a terminal notification for every session that moved
// from IN PROGRESS to WAITING since the last load. The TUI sends them, not the
// detached `tower exec` wrapper, because the escape sequence must reach the
// terminal tab running tower, which is then focused on click. The first load
// notifies nothing, so runs that finished while tower was not running are not
// reported. Loads may complete out of order; notified keeps a session from
// being reported twice.
func (m *Model) notifications(items []store.Item) tea.Cmd {
	prev := m.running
	m.running = make(map[int64]string)
	notified := make(map[int64]string)
	var cmds []tea.Cmd
	for _, it := range items {
		if sid, ok := m.notified[it.ID]; ok {
			notified[it.ID] = sid
		}
		switch it.State {
		case store.StateInProgress:
			m.running[it.ID] = it.SessionID
		case store.StateWaiting:
			sid, ok := prev[it.ID]
			if !ok || sid != it.SessionID || notified[it.ID] == sid {
				continue
			}
			notified[it.ID] = sid
			body := "Agent finished: "
			if it.Failed {
				body = "Agent failed: "
			}
			cmds = append(cmds, tea.Raw(osc777("tower", body+it.Title)))
		}
	}
	m.notified = notified
	return tea.Batch(cmds...)
}

// osc777 returns the OSC 777 notification sequence (Ghostty, iTerm2, WezTerm).
// Control characters are removed because item titles come from external
// sources and must not end the sequence or inject others.
func osc777(title, body string) string {
	return "\x1b]777;notify;" + sanitize(strings.ReplaceAll(title, ";", " ")) + ";" + sanitize(body) + "\a"
}

func sanitize(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
