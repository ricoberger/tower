package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode"

	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"github.com/charmbracelet/x/ansi"
)

// allowedSequences matches the sequences rendered Markdown may contain: SGR
// styling and OSC 8 hyperlinks.
var allowedSequences = regexp.MustCompile("\x1b\\[[0-9;:]*m|\x1b\\]8;[^;\x07\x1b]*;[^\x07\x1b]*(\x07|\x1b\\\\)")

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

	// Hyperlinks must be complete and terminated; aborted or unterminated
	// fragments are dropped.
	for _, in := range []string{
		"a\x1b]8;;https://example.com\x1b]52;c;aGk=\x07b",
		"a\x1b]8;;https://example.comb",
		"a\x1b]8;https://example.com\x07b",
		"a\x1b]8;;https://example.com\x1bb",
	} {
		got := sanitizeRendered(in)
		if strings.Contains(got, "\x1b]8") || !strings.HasPrefix(got, "a") {
			t.Errorf("sanitizeRendered(%q) = %q", in, got)
		}
		assertOnlySafeSequences(t, got)
	}
	for _, in := range []string{"\x1b]8;;http://e\x07", "\x1b]8;id=1;http://e\x1b\\", "\x1b]8;;\x1b\\"} {
		if got := sanitizeRendered(in); got != in {
			t.Errorf("sanitizeRendered(%q) = %q, want it kept", in, got)
		}
	}
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

	// A character-reference hyperlink opener cut short by another sequence
	// does not survive rendering.
	for _, src := range []string{
		"x &#27;]8;;https://example.com&#27;]52;c;aGk=&#7; y\n",
		"x &#x1B;]8;;https://example.com y\n",
	} {
		out := strings.Join(RenderMarkdown(src, 60), "\n")
		assertOnlySafeSequences(t, out)
		if strings.Contains(out, "\x1b]8;;https://example.com") {
			t.Fatalf("unterminated hyperlink survived: %q", out)
		}
	}

	// Oversized documents are shown as sanitized plain text.
	big := "# big\x1b[2J\n" + strings.Repeat("x", MaxRenderBytes)
	lines = RenderMarkdown(big, 40)
	if lines[0] != "# big[2J" || len(lines) != 2 {
		t.Fatalf("oversized document: first line %q, %d lines", lines[0], len(lines))
	}
}

func TestMarkdownStyle(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	margin := func(s gansi.StyleConfig) uint {
		if s.Document.Margin == nil {
			return 99
		}
		return *s.Document.Margin
	}
	def, err := MarkdownStyle("")
	if err != nil || margin(def) != 0 || def.H1.Color == nil || *def.H1.Color != *styles.DarkStyleConfig.H1.Color {
		t.Fatalf("default style: %v", err)
	}
	light, err := MarkdownStyle("light")
	if err != nil || margin(light) != 0 || *light.Document.Color != *styles.LightStyleConfig.Document.Color {
		t.Fatalf("light style: %v", err)
	}
	if *styles.LightStyleConfig.Document.Margin == 0 {
		t.Fatal("the standard style was modified")
	}

	// A JSON style file, like the Catppuccin themes, is used as is apart
	// from the document margin.
	custom, err := MarkdownStyle(write("theme.json", `{"document": {"margin": 2}, "strong": {"color": "#ff0000", "bold": true}}`))
	if err != nil || margin(custom) != 0 || custom.Strong.Color == nil || *custom.Strong.Color != "#ff0000" {
		t.Fatalf("style file: %v %+v", err, custom.Strong)
	}
	out := strings.Join(MarkdownRenderer(custom)("a **b** c\n", 40), "\n")
	if !strings.Contains(out, "38;2;255;0;0") && !strings.Contains(out, "91") && !strings.Contains(out, "38;5;196") {
		t.Errorf("custom color not used: %q", out)
	}
	if strings.Join(RenderMarkdown("a **b** c\n", 40), "\n") == out {
		t.Error("the style file made no difference")
	}

	// Styles cannot bring terminal sequences past the output filter.
	hostile, err := MarkdownStyle(write("hostile.json", `{
		"document": {"block_prefix": "\u001b]52;c;aGk=\u0007\u001b[2J", "prefix": "\u001b]0;t\u0007"},
		"heading": {"block_suffix": "\u001b]8;;http://e", "prefix": "\u009b2J"},
		"link": {"format": "\u001b[?1049h{{.text}}"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	out = strings.Join(MarkdownRenderer(hostile)("# T\n\ntext [l](http://x)\n", 40), "\n")
	assertOnlySafeSequences(t, out)
	if !strings.Contains(ansi.Strip(out), "T") {
		t.Fatalf("hostile style output: %q", out)
	}

	for name, arg := range map[string]string{
		"missing":   filepath.Join(dir, "missing.json"),
		"directory": dir,
		"invalid":   write("bad.json", "{"),
		"oversized": write("big.json", `{"document":{"color":"`+strings.Repeat("x", MaxStyleBytes)+`"}}`),
	} {
		if _, err := MarkdownStyle(arg); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestReferencesInCodeBlocksStayLiteral(t *testing.T) {
	// Code blocks are shown literally; text and code spans are decoded, so
	// control references there are replaced (also without a semicolon or
	// with leading zeros).
	src := "Text &#27;[2J and `code &#10; &#27` here &#0000000027;x.\n\n```\nblock &#10; &#x1b;\n```\n\n    indented &#7;\n"
	out := strings.Join(RenderMarkdown(src, 60), "\n")
	assertOnlySafeSequences(t, out)
	text := ansi.Strip(out)
	for _, want := range []string{"code \uFFFD \uFFFD", "here \uFFFDx", "block &#10; &#x1b;", "indented &#7;", "Text \uFFFD[2J"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered text lacks %q:\n%s", want, text)
		}
	}
}
