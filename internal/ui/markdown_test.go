package ui

import (
	"regexp"
	"strings"
	"testing"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// allowedSequences matches the sequences rendered Markdown may contain: SGR
// styling and OSC 8 hyperlinks.
var allowedSequences = regexp.MustCompile("\x1b\\[[0-9;:]*m|\x1b\\]8;[^\x07\x1b]*(\x07|\x1b\\\\)")

// assertOnlySafeSequences fails when s contains a control character outside
// the allowed sequences (newlines excepted).
func assertOnlySafeSequences(t *testing.T, s string) {
	t.Helper()
	rest := allowedSequences.ReplaceAllString(s, "")
	if i := strings.IndexFunc(rest, func(r rune) bool { return r != '\n' && unicode.IsControl(r) }); i >= 0 {
		t.Fatalf("unsafe control character %q in %q", rest[i:min(i+8, len(rest))], s)
	}
}

func TestSanitizeRendered(t *testing.T) {
	in := "x\x1b[2Jy\x1b[31mz\x1b[m\x1b]0;title\x07w\x1b]8;;http://e\x07l\x1b]8;;\x07\u009b2J\x1b]52;c;aGk=\x07\x07v\n\tq"
	want := "xy\x1b[31mz\x1b[mw\x1b]8;;http://e\x07l\x1b]8;;\x072Jvq"
	got := sanitizeRendered(in)
	if strings.ReplaceAll(got, "\n", "") != want {
		t.Fatalf("sanitizeRendered = %q, want %q", got, want)
	}
	assertOnlySafeSequences(t, got)
}

func TestRenderMarkdown(t *testing.T) {
	src := "# Heading\n\n" + strings.Repeat("word ", 40) + "\n\n- item **bold**\n\n`code`\n\n[link](https://example.com)\n"
	lines := RenderMarkdown(src, 30)
	text := ansi.Strip(strings.Join(lines, "\n"))
	for _, s := range []string{"Heading", "• item bold", "code", "link"} {
		if !strings.Contains(text, s) {
			t.Errorf("rendered Markdown lacks %q:\n%s", s, text)
		}
	}
	for _, s := range []string{"# Heading", "**", "`"} {
		if strings.Contains(text, s) {
			t.Errorf("rendered Markdown contains raw %q:\n%s", s, text)
		}
	}
	for _, l := range lines {
		if w := ansi.StringWidth(l); w > 30 {
			t.Errorf("line %q is %d cells wide", ansi.Strip(l), w)
		}
		if l != strings.TrimRight(l, " ") || strings.HasSuffix(ansi.Strip(l), " ") {
			t.Errorf("line %q keeps trailing padding", l)
		}
	}
	if len(lines) == 0 || strings.TrimSpace(ansi.Strip(lines[0])) == "" || strings.TrimSpace(ansi.Strip(lines[len(lines)-1])) == "" {
		t.Errorf("leading or trailing blank lines: %q", lines)
	}

	// Raw escape sequences, C1 controls and character references that
	// decode to control characters never reach the terminal.
	hostile := "# T\x1b]0;x\x07itle\n\nclear \x1b[2J screen \u009b2J &#27;[2J &#x1b;]52;c;aGk=&#7; &#155;2J\n\n```\n\x1b[31mred\x1b]52;c;aGk=\x07\n```\n\n[l](http://e.com/\x1b]52;c;x\x07)\n"
	out := strings.Join(RenderMarkdown(hostile, 60), "\n")
	assertOnlySafeSequences(t, out)
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b]52") || strings.Contains(out, "\x1b]0;") {
		t.Fatalf("unsafe sequence survived: %q", out)
	}

	// Oversized documents are shown as sanitized plain text.
	big := "# big\x1b[2J\n" + strings.Repeat("x", MaxRenderBytes)
	lines = RenderMarkdown(big, 40)
	if lines[0] != "# big[2J" || len(lines) != 2 {
		t.Fatalf("oversized document: first line %q, %d lines", lines[0], len(lines))
	}
}
