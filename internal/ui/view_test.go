package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestDetailsSurvivesSmallTerminalResizes(t *testing.T) {
	m, f := fixture()
	f.items[0].Details = strings.Repeat("word ", 1000)
	m.Update(itemsMsg{items: f.items})
	press(m, "j")
	press(m, "K")
	press(m, "G")
	m.Render()

	for _, size := range []tea.WindowSizeMsg{
		{Width: 300, Height: 8},
		{Width: 80, Height: 4},
		{Width: 80, Height: 12},
		{Width: 100, Height: 13},
		{Width: 100, Height: 14},
		{Width: 0, Height: 0},
		{Width: 79, Height: 3},
		{Width: 100, Height: 30},
	} {
		m.Update(size)
		for _, key := range []string{"", "G", "g", "ctrl+d", "ctrl+u", "j", "k"} {
			if key != "" {
				press(m, key)
			}
			out := m.Render()
			if size.Width < 80 || size.Height < 4 {
				if out != "terminal too small" {
					t.Fatalf("render at %+v = %q", size, out)
				}
				continue
			}
			lines := strings.Split(out, "\n")
			if len(lines) != size.Height {
				t.Fatalf("render at %+v has %d lines", size, len(lines))
			}
			for _, line := range lines {
				if width := ansi.StringWidth(line); width != size.Width {
					t.Fatalf("render at %+v has line width %d", size, width)
				}
			}
			if m.detailsScroll < 0 || m.detailsScroll >= len(m.detailsLines()) {
				t.Fatalf("invalid scroll offset at %+v: %d", size, m.detailsScroll)
			}
			if key == "ctrl+d" && m.detailsHeight() == 3 && m.detailsScroll != 1 {
				t.Fatalf("half-page did not scroll the one-row popup: %d", m.detailsScroll)
			}
		}
	}
}

func TestRenderNormalizesMultilineTitles(t *testing.T) {
	m, f := fixture()
	const title = "Alert\non\r\nseveral\rlines\t\u2028"
	f.items[0].Title = title
	m.Update(itemsMsg{items: f.items})
	press(m, "j")
	if it, _ := m.selected(); it.Title != "Alert on several lines" {
		t.Fatalf("title = %q", it.Title)
	}
	if f.items[0].Title != title {
		t.Fatal("normalization mutated the backend's item")
	}
	for range 2 {
		lines := strings.Split(m.Render(), "\n")
		if len(lines) != m.height {
			t.Fatalf("render has %d lines", len(lines))
		}
		for _, line := range lines {
			if width := ansi.StringWidth(line); width != m.width {
				t.Fatalf("line width = %d, want %d", width, m.width)
			}
		}
		press(m, "K")
	}
}
