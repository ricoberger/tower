package ui

import (
	"strings"

	"charm.land/lipgloss/v2"
)

var markerStyle = lipgloss.NewStyle().Foreground(Mauve)

// Marker returns the two-column selection marker prefix.
func Marker(selected bool) string {
	if selected {
		return markerStyle.Render("▌ ")
	}
	return "  "
}

// List tracks a selection index.
type List struct {
	Selected int
}

// Clamp returns the selection clamped to a list of length n.
func (l *List) Clamp(n int) int {
	return max(min(l.Selected, n-1), 0)
}

// Handle processes list navigation keys for a list of length n. It returns
// moved=true when the key is a navigation key. Any other key is the caller's
// responsibility.
func (l *List) Handle(key string, n int) (moved bool) {
	switch key {
	case "j", "down", "k", "up", "g", "G":
	default:
		return false
	}
	if n == 0 {
		l.Selected = 0
		return true
	}
	i := l.Clamp(n)
	switch key {
	case "j", "down":
		l.Selected = min(i+1, n-1)
	case "k", "up":
		l.Selected = max(i-1, 0)
	case "g":
		l.Selected = 0
	case "G":
		l.Selected = n - 1
	}
	return true
}

// Window computes the first visible row so the selection stays centered
// while scrolling.
func Window(length, selected, visible int) int {
	if length <= visible {
		return 0
	}
	return min(max(selected-visible/2, 0), length-visible)
}

// ListView renders the visible window of rows for the given content height.
// reserve holds back lines used by the caller (e.g. a table header).
func ListView(rows []string, selected, height, reserve int) string {
	visible := max(1, height-reserve)
	start := Window(len(rows), selected, visible)
	end := min(start+visible, len(rows))
	if start >= end {
		return ""
	}
	return strings.Join(rows[start:end], "\n")
}
