package jira

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// noDescription is shown in the details of tickets without a description.
const noDescription = "_No description provided._"

// unrenderable replaces a description that is neither text nor a readable
// Atlassian Document Format (ADF) document, so that it is not lost silently.
const unrenderable = "_(The Jira description could not be rendered; open the ticket to read it.)_"

// Description renders a ticket's raw description field as text: plain
// strings stay as they are, ADF documents (Jira Cloud) become Markdown.
// Missing, null and empty descriptions are empty. The renderer is built in,
// so polling needs no external converter.
func Description(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return unrenderable
		}
		return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	case '{':
		s, err := renderADF(raw)
		if err != nil {
			return unrenderable
		}
		return s
	default:
		return unrenderable
	}
}

// adfNode is a node of an ADF document.
type adfNode struct {
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Attrs   map[string]any `json:"attrs"`
	Marks   []adfNode      `json:"marks"`
	Content []adfNode      `json:"content"`
}

// renderADF renders an ADF document as Markdown. Unknown nodes are rendered
// through their content, so new node types degrade to their text.
func renderADF(raw json.RawMessage) (string, error) {
	var doc adfNode
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", err
	}
	if doc.Type != "doc" {
		return "", errors.New("not an ADF document")
	}
	return strings.TrimSpace(blocks(doc.Content)), nil
}

// blocks renders block nodes separated by blank lines.
func blocks(nodes []adfNode) string {
	return joinBlocks(nodes, "\n\n")
}

// joinBlocks renders block nodes separated by sep, skipping empty ones.
func joinBlocks(nodes []adfNode, sep string) string {
	var parts []string
	for _, n := range nodes {
		if s := strings.TrimRight(block(n), " \n"); strings.TrimSpace(s) != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, sep)
}

func block(n adfNode) string {
	switch n.Type {
	case "paragraph":
		return inline(n.Content)
	case "heading":
		level := min(max(attrInt(n, "level", 1), 1), 6)
		return strings.Repeat("#", level) + " " + inline(n.Content)
	case "bulletList":
		return list(n, func(int) string { return "- " })
	case "orderedList":
		start := attrInt(n, "order", 1)
		return list(n, func(i int) string { return strconv.Itoa(start+i) + ". " })
	case "codeBlock":
		lang, _ := n.Attrs["language"].(string)
		return "```" + lang + "\n" + plain(n.Content) + "\n```"
	case "blockquote", "panel":
		return prefix(blocks(n.Content), "> ", "> ")
	case "rule":
		return "---"
	case "table":
		return table(n)
	case "mediaSingle", "mediaGroup", "media":
		return "_(attachment)_"
	case "text", "hardBreak", "mention", "emoji", "inlineCard", "blockCard", "embedCard", "date", "status":
		return inline([]adfNode{n})
	default:
		return blocks(n.Content)
	}
}

// list renders list items with a marker and indented continuation lines.
// Item blocks are not separated by blank lines, so nested lists stay
// compact.
func list(n adfNode, marker func(int) string) string {
	var lines []string
	for i, item := range n.Content {
		m := marker(i)
		lines = append(lines, prefix(joinBlocks(item.Content, "\n"), m, strings.Repeat(" ", len(m))))
	}
	return strings.Join(lines, "\n")
}

// table renders a table as Markdown with the first row as header.
func table(n adfNode) string {
	var rows []string
	for i, row := range n.Content {
		var cells []string
		for _, cell := range row.Content {
			text := strings.Join(strings.Fields(blocks(cell.Content)), " ")
			cells = append(cells, strings.ReplaceAll(text, "|", `\|`))
		}
		rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
		if i == 0 {
			rows = append(rows, "|"+strings.Repeat(" --- |", len(cells)))
		}
	}
	return strings.Join(rows, "\n")
}

// inline renders inline nodes.
func inline(nodes []adfNode) string {
	var b strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case "text":
			b.WriteString(marks(n.Text, n.Marks))
		case "hardBreak":
			b.WriteString("\n")
		case "mention", "emoji", "status":
			text, _ := n.Attrs["text"].(string)
			if text == "" {
				text, _ = n.Attrs["shortName"].(string)
			}
			b.WriteString(text)
		case "inlineCard", "blockCard", "embedCard":
			url, _ := n.Attrs["url"].(string)
			b.WriteString(url)
		case "date":
			b.WriteString(adfDate(n))
		default:
			b.WriteString(inline(n.Content))
		}
	}
	return b.String()
}

// plain renders the text of nodes without marks (code blocks).
func plain(nodes []adfNode) string {
	var b strings.Builder
	for _, n := range nodes {
		switch n.Type {
		case "text":
			b.WriteString(n.Text)
		case "hardBreak":
			b.WriteString("\n")
		default:
			b.WriteString(plain(n.Content))
		}
	}
	return b.String()
}

// marks applies the text marks as Markdown.
func marks(text string, ms []adfNode) string {
	if text == "" {
		return ""
	}
	for _, m := range ms {
		if m.Type == "code" {
			text = "`" + text + "`"
		}
	}
	for _, m := range ms {
		switch m.Type {
		case "strong":
			text = "**" + text + "**"
		case "em":
			text = "*" + text + "*"
		case "strike":
			text = "~~" + text + "~~"
		case "link":
			if href, _ := m.Attrs["href"].(string); href != "" {
				text = "[" + text + "](" + href + ")"
			}
		}
	}
	return text
}

// adfDate renders a date node, whose timestamp is in milliseconds.
func adfDate(n adfNode) string {
	ts, _ := n.Attrs["timestamp"].(string)
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ts
	}
	return time.UnixMilli(ms).UTC().Format(time.DateOnly)
}

// attrInt returns a numeric attribute or def.
func attrInt(n adfNode, name string, def int) int {
	if v, ok := n.Attrs[name].(float64); ok {
		return int(v)
	}
	return def
}

// prefix prefixes the first line with first and the other lines with rest.
func prefix(s, first, rest string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		p := rest
		if i == 0 {
			p = first
		}
		if l == "" && i > 0 {
			lines[i] = strings.TrimRight(p, " ")
			continue
		}
		lines[i] = p + l
	}
	return strings.Join(lines, "\n")
}
