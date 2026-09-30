package ui

import (
	"regexp"
	"strings"
	"unicode"

	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"github.com/charmbracelet/x/ansi"
)

// MaxRenderBytes bounds the Markdown that is rendered; larger documents are
// shown as plain text.
const MaxRenderBytes = 1 << 20

// markdownStyle is glamour's dark style without the document margin, so the
// preview width is used in full.
var markdownStyle = func() gansi.StyleConfig {
	s := styles.DarkStyleConfig
	zero := uint(0)
	s.Document.Margin = &zero
	return s
}()

// trailingPadding matches trailing spaces and SGR sequences of a line.
var trailingPadding = regexp.MustCompile(`(?:\x1b\[[0-9;:]*m| )+$`)

// mdCache holds the rendered lines of the latest rendered document.
type mdCache struct {
	src   string
	width int
	lines []string
}

// markdownLines returns src rendered as Markdown for the width. The result
// is cached, so rendering only happens when the document or the width
// changed.
func (m *Model) markdownLines(src string, width int) []string {
	if m.md.lines != nil && m.md.width == width && m.md.src == src {
		return m.md.lines
	}
	m.md = mdCache{src: src, width: width, lines: RenderMarkdown(src, width)}
	return m.md.lines
}

// RenderMarkdown renders Markdown wrapped to the width. The input is
// sanitized before rendering, and the output keeps only text, newlines, SGR
// styling and hyperlinks, so the document cannot inject other terminal
// sequences (for example through character references). Documents larger
// than MaxRenderBytes, or ones glamour cannot render, are returned as
// sanitized plain text.
func RenderMarkdown(src string, width int) []string {
	src = Sanitize(src)
	plain := func() []string {
		return strings.Split(strings.TrimRight(src, "\n"), "\n")
	}
	if len(src) > MaxRenderBytes || width < 1 {
		return plain()
	}
	r, err := glamour.NewTermRenderer(glamour.WithStyles(markdownStyle), glamour.WithWordWrap(width))
	if err != nil {
		return plain()
	}
	out, err := r.Render(src)
	if err != nil {
		return plain()
	}
	lines := strings.Split(sanitizeRendered(out), "\n")
	for i, l := range lines {
		// glamour pads every line to the wrap width with styled spaces.
		if t := trailingPadding.ReplaceAllString(l, ""); t != l {
			if strings.Contains(t, "\x1b") {
				t += "\x1b[m"
			}
			lines[i] = t
		}
	}
	blank := func(l string) bool { return strings.TrimSpace(ansi.Strip(l)) == "" }
	for len(lines) > 0 && blank(lines[0]) {
		lines = lines[1:]
	}
	for len(lines) > 0 && blank(lines[len(lines)-1]) {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// sanitizeRendered keeps printable text, newlines, SGR sequences and OSC 8
// hyperlinks of renderer output and drops every other control character or
// escape sequence.
func sanitizeRendered(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var state byte
	for len(s) > 0 {
		seq, _, n, newState := ansi.DecodeSequence(s, state, nil)
		state = newState
		if n <= 0 {
			break
		}
		s = s[n:]
		switch {
		case seq == "\n":
			b.WriteString(seq)
		case strings.HasPrefix(seq, "\x1b["):
			if isSGR(seq[2:]) {
				b.WriteString(seq)
			}
		case strings.HasPrefix(seq, "\x1b]8;"):
			if safeOSC(seq[4:]) {
				b.WriteString(seq)
			}
		case seq[0] == '\x1b':
		default:
			b.WriteString(strings.Map(func(r rune) rune {
				if unicode.IsControl(r) {
					return -1
				}
				return r
			}, seq))
		}
	}
	return b.String()
}

// isSGR reports whether the CSI body (after "ESC [") is a plain SGR
// sequence: parameters and a final "m".
func isSGR(body string) bool {
	if !strings.HasSuffix(body, "m") {
		return false
	}
	for _, c := range body[:len(body)-1] {
		if (c < '0' || c > '9') && c != ';' && c != ':' {
			return false
		}
	}
	return true
}

// safeOSC reports whether the OSC 8 body (after "ESC ] 8 ;") contains no
// control characters other than its terminator.
func safeOSC(body string) bool {
	body = strings.TrimSuffix(strings.TrimSuffix(body, "\x07"), "\x1b\\")
	return !strings.ContainsFunc(body, unicode.IsControl)
}
