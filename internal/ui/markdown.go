package ui

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"github.com/charmbracelet/x/ansi"

	"github.com/ricoberger/tower/internal/item"
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

// controlReferences matches numeric character references, which Markdown
// decodes into characters after the input was sanitized.
var controlReferences = regexp.MustCompile(`&#(?:[0-9]{1,8}|[xX][0-9a-fA-F]{1,7});`)

// neutralizeReference replaces a numeric character reference to a control
// character with U+FFFD and keeps every other reference.
func neutralizeReference(ref string) string {
	num := strings.TrimSuffix(strings.TrimPrefix(ref, "&#"), ";")
	base := 10
	if num[0] == 'x' || num[0] == 'X' {
		num, base = num[1:], 16
	}
	n, err := strconv.ParseUint(num, base, 32)
	if err != nil || n > unicode.MaxRune || unicode.IsControl(rune(n)) {
		return "\uFFFD"
	}
	return ref
}

// trailingPadding matches trailing spaces and SGR sequences of a line.
var trailingPadding = regexp.MustCompile(`(?:\x1b\[[0-9;:]*m| )+$`)

// mdKey identifies a rendered document: its source and the wrap width.
type mdKey struct {
	src   string
	width int
}

// mdCache holds the rendered lines of the latest rendered document.
type mdCache struct {
	key   mdKey
	lines []string
}

// markdownMsg delivers a document rendered in the background.
type markdownMsg struct {
	key   mdKey
	lines []string
}

// markdownLines returns src rendered for the width when that rendering is
// available. Rendering happens in the background (see renderMarkdown);
// until it is done, the sanitized source is shown as plain text.
func (m *Model) markdownLines(src string, width int) []string {
	if m.md.lines != nil && m.md.key == (mdKey{src: src, width: width}) {
		return m.md.lines
	}
	if m.mdPlain.lines == nil || m.mdPlain.key.src != src {
		m.mdPlain = mdCache{key: mdKey{src: src}, lines: plainLines(Sanitize(src))}
	}
	return m.mdPlain.lines
}

// wantMarkdown returns the document the preview of the selection shows
// rendered: the report of a loaded ready/blocked run or the alert.md of an
// item without a run, at the current preview width.
func (m *Model) wantMarkdown() (mdKey, bool) {
	width := m.previewContentWidth()
	v, ok := m.selectedView()
	if !ok || width <= 0 {
		return mdKey{}, false
	}
	r, hasRun := v.Latest()
	switch {
	case !hasRun:
		if m.alert.id != v.Item.ID || m.alert.err != nil {
			return mdKey{}, false
		}
		return mdKey{src: m.alert.text, width: width}, true
	case r.Outcome == item.OutcomeReady || r.Outcome == item.OutcomeBlocked:
		if !m.loadedFor(v.Item.ID, r) || m.art.reportErr != nil {
			return mdKey{}, false
		}
		return mdKey{src: m.art.report, width: width}, true
	}
	return mdKey{}, false
}

// renderMarkdown starts rendering the document the preview needs, off the
// update loop, unless it is rendered already. At most one render is in
// flight; a result is only applied while it is still the needed document,
// and the next needed document is started when a render finishes.
func (m *Model) renderMarkdown() tea.Cmd {
	key, ok := m.wantMarkdown()
	if !ok || m.mdPending || (m.md.lines != nil && m.md.key == key) {
		return nil
	}
	m.mdPending = true
	render := m.opts.RenderMarkdown
	return func() tea.Msg { return markdownMsg{key: key, lines: render(key.src, key.width)} }
}

// applyMarkdown stores a background rendering if it is still needed.
func (m *Model) applyMarkdown(msg markdownMsg) {
	m.mdPending = false
	if key, ok := m.wantMarkdown(); ok && key == msg.key {
		m.md = mdCache(msg)
	}
}

// plainLines splits sanitized text into lines without the final newline.
func plainLines(s string) []string {
	return strings.Split(strings.TrimRight(s, "\n"), "\n")
}

// RenderMarkdown renders Markdown wrapped to the width. The input is
// sanitized before rendering, and the output keeps only text, newlines, SGR
// styling and hyperlinks, so the document cannot inject other terminal
// sequences (for example through character references). Documents larger
// than MaxRenderBytes, or ones glamour cannot render, are returned as
// sanitized plain text.
func RenderMarkdown(src string, width int) []string {
	src = Sanitize(src)
	plain := func() []string { return plainLines(src) }
	src = controlReferences.ReplaceAllStringFunc(src, neutralizeReference)
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

// safeOSC reports whether the OSC 8 body (after "ESC ] 8 ;") is a complete
// hyperlink sequence: parameters, ";", a URI and a BEL or ST terminator,
// with no other control characters. Unterminated or aborted fragments are
// rejected.
func safeOSC(body string) bool {
	switch {
	case strings.HasSuffix(body, "\x07"):
		body = strings.TrimSuffix(body, "\x07")
	case strings.HasSuffix(body, "\x1b\\"):
		body = strings.TrimSuffix(body, "\x1b\\")
	default:
		return false
	}
	return strings.Contains(body, ";") && !strings.ContainsFunc(body, unicode.IsControl)
}
