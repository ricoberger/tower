package ui

import (
	"os"
	"path/filepath"
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

func TestDetailsRenderMarkdownWithGlamourStyle(t *testing.T) {
	style := filepath.Join(t.TempDir(), "style.json")
	if err := os.WriteFile(style, []byte(`{"h1": {"prefix": "H1> "}, "strong": {"block_prefix": "<", "block_suffix": ">"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GLAMOUR_STYLE", style)
	m, f := fixture()
	f.items[0].Details = "# Alert\n\nsome **bold** text"
	m.Update(itemsMsg{items: f.items})
	press(m, "j")
	press(m, "K")
	plain := ansi.Strip(m.Render())
	if !strings.Contains(plain, "H1> Alert") || !strings.Contains(plain, "some <bold> text") || strings.Contains(plain, "**") {
		t.Fatalf("render:\n%s", plain)
	}
}

func TestDetailsFallBackToPlainTextForInvalidGlamourStyle(t *testing.T) {
	t.Setenv("GLAMOUR_STYLE", filepath.Join(t.TempDir(), "missing.json"))
	m, f := fixture()
	f.items[0].Details = "# Alert\n\nsome **bold** text"
	m.Update(itemsMsg{items: f.items})
	press(m, "j")
	press(m, "K")
	if plain := ansi.Strip(m.Render()); !strings.Contains(plain, "# Alert") || !strings.Contains(plain, "some **bold** text") {
		t.Fatalf("render:\n%s", plain)
	}
}

func TestDetailsRerenderWhenItemOrWidthChanges(t *testing.T) {
	m, f := fixture()
	f.items[0].Details = "first"
	m.Update(itemsMsg{items: f.items})
	press(m, "j")
	press(m, "K")
	if plain := ansi.Strip(m.Render()); !strings.Contains(plain, "first") {
		t.Fatalf("render:\n%s", plain)
	}
	f.items[0].Details = "second " + strings.Repeat("word ", 30)
	m.Update(itemsMsg{items: f.items})
	if plain := ansi.Strip(m.Render()); !strings.Contains(plain, "second") || strings.Contains(plain, "first") {
		t.Fatalf("render after update:\n%s", plain)
	}
	n := len(m.detailsLines())
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	m.Render()
	if len(m.detailsLines()) <= n {
		t.Fatalf("details not rewrapped for the narrower popup: %d lines, before %d", len(m.detailsLines()), n)
	}
}

func TestDetailsHavePaddingAboveAndBelowTheContent(t *testing.T) {
	m, f := fixture()
	var details []string
	for i := range 60 {
		details = append(details, "line "+strings.Repeat("x", i%3))
	}
	f.items[0].Details = strings.Join(details, "\n\n")
	m.Update(itemsMsg{items: f.items})
	press(m, "j")
	press(m, "K")
	for _, key := range []string{"", "ctrl+d", "G"} {
		if key != "" {
			press(m, key)
		}
		popup, _ := m.renderDetails()
		lines := strings.Split(ansi.Strip(popup), "\n")
		inner := func(i int) string { return strings.Trim(lines[i], "│ ") }
		last := len(lines) - 2
		if inner(1) != "" || inner(last) != "" {
			t.Fatalf("no padding after %q:\n%s", key, strings.Join(lines, "\n"))
		}
		if !strings.Contains(inner(2), "line") && !strings.Contains(inner(3), "line") {
			t.Fatalf("content does not start below the padding after %q:\n%s", key, strings.Join(lines, "\n"))
		}
		if !strings.Contains(inner(last-1), "line") && !strings.Contains(inner(last-2), "line") {
			t.Fatalf("content does not end above the padding after %q:\n%s", key, strings.Join(lines, "\n"))
		}
	}
	if got := m.detailsScroll + m.detailsRows(); got != len(m.detailsLines()) {
		t.Fatalf("G does not show the last line: %d of %d", got, len(m.detailsLines()))
	}
}
