package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/glamour/v2"
	gansi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"

	"github.com/ricoberger/tower/internal/item"
)

// MaxRenderBytes bounds the Markdown that is rendered; larger documents are
// shown as plain text.
const MaxRenderBytes = 1 << 20

// MaxStyleBytes bounds the size of a JSON style file.
const MaxStyleBytes = 1 << 20

// markdownStyle is glamour's dark style without the document margin.
var markdownStyle = withoutMargin(styles.DarkStyleConfig)

// withoutMargin removes the document margin, so the preview width is used
// in full.
func withoutMargin(s gansi.StyleConfig) gansi.StyleConfig {
	zero := uint(0)
	s.Document.Margin = &zero
	return s
}

// MarkdownStyle resolves a glamour style the way GLAMOUR_STYLE is
// interpreted: empty selects the dark style, a standard style name (for
// example "light" or "dracula") selects that style, and anything else is
// read as a JSON style file of at most MaxStyleBytes. The file is opened
// without blocking, so a FIFO or device is rejected instead of waited on.
// The document margin is always removed.
func MarkdownStyle(name string) (gansi.StyleConfig, error) {
	if name == "" {
		return markdownStyle, nil
	}
	if st, ok := styles.DefaultStyles[name]; ok {
		return withoutMargin(*st), nil
	}
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 -- the user's own style file
	if err != nil {
		return gansi.StyleConfig{}, fmt.Errorf("markdown style %q: not a standard style and not readable: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return gansi.StyleConfig{}, fmt.Errorf("markdown style %q: not a regular file", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxStyleBytes+1))
	if err != nil {
		return gansi.StyleConfig{}, fmt.Errorf("markdown style %q: %w", name, err)
	}
	if len(data) > MaxStyleBytes {
		return gansi.StyleConfig{}, fmt.Errorf("markdown style %q: larger than %d bytes", name, MaxStyleBytes)
	}
	var st gansi.StyleConfig
	if err := json.Unmarshal(data, &st); err != nil {
		return gansi.StyleConfig{}, fmt.Errorf("markdown style %q: %w", name, err)
	}
	return withoutMargin(st), nil
}

// controlReferences matches numeric character references, which glamour
// decodes into characters after the input was sanitized (outside code
// blocks). Like the decoder, it accepts any number of digits and a missing
// semicolon.
var controlReferences = regexp.MustCompile(`&#(?:[0-9]+|[xX][0-9a-fA-F]+);?`)

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

// neutralizeReferences replaces numeric character references to control
// characters outside code blocks. Code blocks are shown literally, so their
// references are kept; glamour decodes references in code spans, so those
// are replaced too.
func neutralizeReferences(src string) string {
	locs := controlReferences.FindAllStringIndex(src, -1)
	if len(locs) == 0 {
		return src
	}
	code := codeRanges(src)
	var b strings.Builder
	last, c := 0, 0
	for _, loc := range locs {
		for c < len(code) && code[c][1] <= loc[0] {
			c++
		}
		inCode := c < len(code) && code[c][0] <= loc[0] && loc[1] <= code[c][1]
		ref := src[loc[0]:loc[1]]
		if repl := neutralizeReference(ref); repl != ref && !inCode {
			b.WriteString(src[last:loc[0]])
			b.WriteString(repl)
			last = loc[1]
		}
	}
	b.WriteString(src[last:])
	return b.String()
}

// codeRanges returns the byte ranges of the code block contents of the
// Markdown source, parsed like glamour parses it, in document order.
func codeRanges(src string) [][2]int {
	source := []byte(src)
	doc := goldmark.New(goldmark.WithExtensions(extension.GFM, extension.DefinitionList)).
		Parser().Parse(text.NewReader(source))
	var ranges [][2]int
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.CodeBlock, *ast.FencedCodeBlock:
			lines := n.Lines()
			for i := range lines.Len() {
				seg := lines.At(i)
				ranges = append(ranges, [2]int{seg.Start, seg.Stop})
			}
			return ast.WalkSkipChildren, nil
		}
		return ast.WalkContinue, nil
	})
	return ranges
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
	if !ok || m.mdPending || m.mdRender == nil || (m.md.lines != nil && m.md.key == key) {
		return nil
	}
	m.mdPending = true
	render := m.mdRender
	return func() tea.Msg { return markdownMsg{key: key, lines: render(key.src, key.width)} }
}

// styleMsg delivers the loaded Markdown style.
type styleMsg struct {
	style gansi.StyleConfig
	err   error
}

// loadStyle loads the Markdown style off the update loop, so a slow or
// blocking style file never delays the UI or its shutdown.
func (m *Model) loadStyle() tea.Cmd {
	if m.mdRender != nil {
		return nil
	}
	name := m.opts.MarkdownStyle
	return func() tea.Msg {
		st, err := MarkdownStyle(name)
		return styleMsg{style: st, err: err}
	}
}

// applyStyle starts rendering with the loaded style, or with the default
// style and an error hint when the style is unusable.
func (m *Model) applyStyle(msg styleMsg) {
	if msg.err != nil {
		m.setHint("GLAMOUR_STYLE ignored: "+msg.err.Error(), true)
		m.mdRender = RenderMarkdown
		return
	}
	m.mdRender = MarkdownRenderer(msg.style)
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

// RenderMarkdown renders Markdown with the default style; see
// MarkdownRenderer.
func RenderMarkdown(src string, width int) []string {
	return renderWithStyle(src, width, markdownStyle)
}

// MarkdownRenderer returns a function rendering Markdown with the style.
func MarkdownRenderer(style gansi.StyleConfig) func(src string, width int) []string {
	return func(src string, width int) []string { return renderWithStyle(src, width, style) }
}

// renderWithStyle renders Markdown wrapped to the width. The input is
// sanitized before rendering, and the output keeps only text, newlines, SGR
// styling and hyperlinks, so the document cannot inject other terminal
// sequences (for example through character references). Documents larger
// than MaxRenderBytes, or ones glamour cannot render, are returned as
// sanitized plain text.
func renderWithStyle(src string, width int, style gansi.StyleConfig) []string {
	src = Sanitize(src)
	plain := func() []string { return plainLines(src) }
	if len(src) > MaxRenderBytes || width < 1 {
		return plain()
	}
	src = neutralizeReferences(src)
	r, err := glamour.NewTermRenderer(glamour.WithStyles(style), glamour.WithWordWrap(width))
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
