// Package ui contains tower's terminal user interface: the root Bubble Tea
// model with the item list and preview, and the shared rendering building
// blocks (frame, select list, styles).
package ui

import (
	"image/color"

	"charm.land/lipgloss/v2"
)

// Mauve is the accent color used for the focused frame and the selection
// marker.
var Mauve = lipgloss.Color("#c6a0f6")

// Named colors of the ANSI 16 palette.
var (
	Red     = lipgloss.Red
	Green   = lipgloss.Green
	Yellow  = lipgloss.Yellow
	Blue    = lipgloss.Blue
	Magenta = lipgloss.Magenta
	White   = lipgloss.White
	Gray    = lipgloss.BrightBlack
)

// NamedColor resolves a color name to a color; unknown names are white.
func NamedColor(name string) color.Color {
	switch name {
	case "red":
		return Red
	case "green":
		return Green
	case "yellow":
		return Yellow
	case "blue":
		return Blue
	case "magenta":
		return Magenta
	case "white":
		return White
	case "gray":
		return Gray
	default:
		return White
	}
}

// Dim renders s with the terminal's faint attribute.
func Dim(s string) string {
	return lipgloss.NewStyle().Faint(true).Render(s)
}

// Bold renders s bold.
func Bold(s string) string {
	return lipgloss.NewStyle().Bold(true).Render(s)
}

// Colored renders s in the given named color.
func Colored(name, s string) string {
	return lipgloss.NewStyle().Foreground(NamedColor(name)).Render(s)
}
