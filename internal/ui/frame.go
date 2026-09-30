package ui

import (
	"fmt"
	"image/color"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// FrameState is the metadata shown in a frame header.
type FrameState struct {
	Title string
	// Status is shown right-aligned in the header (may be styled).
	Status string
}

// FormatAge renders a duration compactly: 42s, 5m, 3h, 2d.
func FormatAge(d time.Duration) string {
	s := max(int(d.Seconds()), 0)
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	m := s / 60
	if m < 60 {
		return fmt.Sprintf("%dm", m)
	}
	h := m / 60
	if h < 24 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dd", h/24)
}

// FormatElapsed renders a duration with minute and second precision
// (4s, 2m05s, 1h02m).
func FormatElapsed(d time.Duration) string {
	s := max(int(d.Seconds()), 0)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	default:
		return fmt.Sprintf("%dh%02dm", s/3600, s%3600/60)
	}
}

// Truncate cuts s to width display columns, appending … when truncated.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	return ansi.Truncate(s, width, "…")
}

// padTo pads or hard-clips a styled line to exactly width columns.
func padTo(s string, width int) string {
	w := ansi.StringWidth(s)
	if w > width {
		return ansi.Truncate(s, width, "")
	}
	return s + strings.Repeat(" ", width-w)
}

// Frame renders a frame: rounded border, one space of horizontal padding, a
// header line (title left, status right) with a single-line bottom border,
// and the content clipped below it. Frames smaller than 4x4 render empty.
func Frame(width, height int, st FrameState, focused bool, content string) string {
	if width < 4 || height < 4 {
		return ""
	}
	innerW := width - 4 // border + padding on both sides
	var borderColor color.Color = Gray
	if focused {
		borderColor = Mauve
	}
	border := lipgloss.NewStyle().Foreground(borderColor)

	titleStyle := lipgloss.NewStyle().Bold(true)
	if focused {
		titleStyle = titleStyle.Foreground(Mauve)
	}
	title := titleStyle.Render(Truncate(st.Title, innerW))
	titleW := ansi.StringWidth(title)
	statusStr := st.Status
	statusMax := innerW - titleW - 1
	if ansi.StringWidth(statusStr) > statusMax {
		statusStr = Truncate(statusStr, statusMax)
	}
	header := title
	if gap := innerW - titleW - ansi.StringWidth(statusStr); gap > 0 && statusStr != "" {
		header += strings.Repeat(" ", gap) + statusStr
	}

	contentH := height - 4 // borders, header, header underline
	contentLines := strings.Split(content, "\n")
	if len(contentLines) > contentH {
		contentLines = contentLines[:contentH]
	}

	pad := " "
	var b strings.Builder
	b.WriteString(border.Render("╭" + strings.Repeat("─", width-2) + "╮"))
	writeRow := func(line string) {
		b.WriteString("\n")
		b.WriteString(border.Render("│"))
		b.WriteString(pad)
		b.WriteString(padTo(line, innerW))
		b.WriteString(pad)
		b.WriteString(border.Render("│"))
	}
	writeRow(header)
	writeRow(border.Render(strings.Repeat("─", innerW)))
	for i := range contentH {
		line := ""
		if i < len(contentLines) {
			line = contentLines[i]
		}
		writeRow(line)
	}
	b.WriteString("\n")
	b.WriteString(border.Render("╰" + strings.Repeat("─", width-2) + "╯"))
	return b.String()
}

// FrameContentSize returns the content area of a frame of the given size.
func FrameContentSize(width, height int) (int, int) {
	return max(width-4, 0), max(height-4, 0)
}
