package jira

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDescription(t *testing.T) {
	tests := []struct{ name, raw, want string }{
		{name: "absent", raw: "", want: ""},
		{name: "null", raw: "null", want: ""},
		{name: "empty string", raw: `" \n"`, want: ""},
		{name: "plain text", raw: `"Line one\r\nLine two\n\nParagraph"`, want: "Line one\nLine two\n\nParagraph"},
		{name: "empty document", raw: `{"type": "doc", "version": 1, "content": []}`, want: ""},
		{name: "not a document", raw: `{"type": "paragraph"}`, want: unrenderable},
		{name: "malformed document", raw: `{"type": "doc", "content": "x"}`, want: unrenderable},
		{name: "unexpected type", raw: `42`, want: unrenderable},
		{
			name: "paragraphs and marks",
			raw: `{"type": "doc", "content": [
				{"type": "paragraph", "content": [
					{"type": "text", "text": "bold", "marks": [{"type": "strong"}]}, {"type": "text", "text": " "},
					{"type": "text", "text": "em", "marks": [{"type": "em"}]}, {"type": "text", "text": " "},
					{"type": "text", "text": "x()", "marks": [{"type": "code"}]}, {"type": "text", "text": " "},
					{"type": "text", "text": "link", "marks": [{"type": "link", "attrs": {"href": "https://example.com"}}]},
					{"type": "hardBreak"},
					{"type": "mention", "attrs": {"id": "5b10ac8d", "text": "@Ada"}}, {"type": "text", "text": " "},
					{"type": "emoji", "attrs": {"shortName": ":smile:"}}, {"type": "text", "text": " "},
					{"type": "inlineCard", "attrs": {"url": "https://example.com/card"}}, {"type": "text", "text": " "},
					{"type": "date", "attrs": {"timestamp": "1767225600000"}}
				]},
				{"type": "paragraph", "content": [{"type": "text", "text": "second"}]}
			]}`,
			want: "**bold** *em* `x()` [link](https://example.com)\n@Ada :smile: https://example.com/card 2026-01-01\n\nsecond",
		},
		{
			name: "headings, lists, code, quote and rule",
			raw: `{"type": "doc", "content": [
				{"type": "heading", "attrs": {"level": 2}, "content": [{"type": "text", "text": "Steps"}]},
				{"type": "orderedList", "attrs": {"order": 1}, "content": [
					{"type": "listItem", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "one"}]}]},
					{"type": "listItem", "content": [
						{"type": "paragraph", "content": [{"type": "text", "text": "two"}]},
						{"type": "bulletList", "content": [
							{"type": "listItem", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "nested"}]}]}
						]}
					]}
				]},
				{"type": "codeBlock", "attrs": {"language": "sh"}, "content": [{"type": "text", "text": "kubectl get pods\nexit"}]},
				{"type": "blockquote", "content": [
					{"type": "paragraph", "content": [{"type": "text", "text": "quoted"}]},
					{"type": "paragraph", "content": [{"type": "text", "text": "more"}]}
				]},
				{"type": "rule"},
				{"type": "mediaSingle", "content": [{"type": "media", "attrs": {"id": "x"}}]},
				{"type": "unknownBlock", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "fallback"}]}]}
			]}`,
			want: "## Steps\n\n1. one\n2. two\n   - nested\n\n```sh\nkubectl get pods\nexit\n```\n\n> quoted\n>\n> more\n\n---\n\n_(attachment)_\n\nfallback",
		},
		{
			name: "standalone block card",
			raw:  `{"type": "doc", "content": [{"type": "blockCard", "attrs": {"url": "https://example.com/block"}}]}`,
			want: "https://example.com/block",
		},
		{
			name: "standalone embed card",
			raw:  `{"type": "doc", "content": [{"type": "embedCard", "attrs": {"url": "https://example.com/embed", "layout": "center"}}]}`,
			want: "https://example.com/embed",
		},
		{
			name: "cards mixed with paragraphs",
			raw: `{"type": "doc", "content": [
				{"type": "paragraph", "content": [{"type": "text", "text": "before"}]},
				{"type": "blockCard", "attrs": {"url": "https://example.com/block"}},
				{"type": "embedCard", "attrs": {"url": "https://example.com/embed"}},
				{"type": "paragraph", "content": [{"type": "text", "text": "after"}]}
			]}`,
			want: "before\n\nhttps://example.com/block\n\nhttps://example.com/embed\n\nafter",
		},
		{
			name: "table",
			raw: `{"type": "doc", "content": [{"type": "table", "content": [
				{"type": "tableRow", "content": [
					{"type": "tableHeader", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "Name"}]}]},
					{"type": "tableHeader", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "Value"}]}]}
				]},
				{"type": "tableRow", "content": [
					{"type": "tableCell", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "a|b"}]}]},
					{"type": "tableCell", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "1"}]}]}
				]}
			]}]}`,
			want: "| Name | Value |\n| --- | --- |\n| a\\|b | 1 |",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Description(json.RawMessage(tt.raw)); got != tt.want {
				t.Errorf("Description = %q\nwant          %q", got, tt.want)
			}
		})
	}
}

func TestDetailsDescriptionFallbacks(t *testing.T) {
	for raw, want := range map[string]string{
		"":       noDescription,
		"null":   noDescription,
		`"text"`: "text",
		`{"type": "doc", "content": [{"type": "blockCard", "attrs": {"url": "https://example.com/card"}}]}`: "https://example.com/card",
		`{"type": "nonsense"}`: unrenderable,
		`{"type": "doc", "content": [{"type": "paragraph", "content": [{"type": "text", "text": "adf"}]}]}`: "adf",
	} {
		it := Item(assigned, Issue{Key: "DEMO-1", Status: "Open", StatusCategory: "new", Description: json.RawMessage(raw)}, "")
		if !strings.HasSuffix(it.Details, "## Description\n\n"+want+"\n") {
			t.Errorf("description %q:\n%s", raw, it.Details)
		}
	}
}
