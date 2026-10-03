package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	tests := []struct {
		in   string
		want []string
		err  string
	}{
		{in: "a b  c", want: []string{"a", "b", "c"}},
		{in: `a 'b c' "d \"e\" \\ f"`, want: []string{"a", "b c", `d "e" \ f`}},
		{in: `a'b'"c" x\ y`, want: []string{"abc", "x y"}},
		{in: `a '' b`, want: []string{"a", "", "b"}},
		{in: `echo '$HOME; ls'`, want: []string{"echo", "$HOME; ls"}},
		{in: "cost $5", want: []string{"cost", "$5"}},
		{in: "a 'b", err: "unterminated"},
		{in: `a "b`, err: "unterminated"},
		{in: `a \`, err: "trailing backslash"},
		{in: "a && b", err: "without a shell"},
		{in: "a x>y", want: []string{"a", "x>y"}},
		{in: `a {{ .Prompt }} --t={{printf "%s b" .Title}}`, want: []string{"a", "{{ .Prompt }}", `--t={{printf "%s b" .Title}}`}},
		{in: `a "x {{ "y" }}" '{{ z }}'`, want: []string{"a", `x {{ "y" }}`, "{{ z }}"}},
		{in: `a {{printf "a }} b"}}`, want: []string{"a", `{{printf "a }} b"}}`}},
		{in: `a "x {{printf "a }} b"}}"`, want: []string{"a", `x {{printf "a }} b"}}`}},
		{in: "a {{printf `a }} b`}}", want: []string{"a", "{{printf `a }} b`}}"}},
		{in: `a {{printf "a \" }} b"}}`, want: []string{"a", `{{printf "a \" }} b"}}`}},
		{in: `a {{printf "%c }}" '}'}}`, want: []string{"a", `{{printf "%c }}" '}'}}`}},
		{in: `a {{/* }} " ' ignored */}}b`, want: []string{"a", `{{/* }} " ' ignored */}}b`}},
		{in: "a {{ .Prompt", err: "unterminated {{"},
		{in: `a {{printf "a }} b"}`, err: "unterminated {{"},
		{in: `a {{printf "a }} b`, err: "unterminated {{"},
		{in: `a {{/* }} ignored`, err: "unterminated template comment"},
		{in: "a | b", err: "without a shell"},
		{in: "  ", err: "empty"},
	}
	for _, tt := range tests {
		got, err := splitCommand(tt.in)
		if tt.err != "" {
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("%q: err = %v, want %q", tt.in, err, tt.err)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%q: got %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestCommandRendersTemplateDelimitersInsideLiterals(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{input: `echo {{printf "a }} b"}}`, want: "a }} b"},
		{input: `echo "x {{printf "a }} b"}}"`, want: "x a }} b"},
		{input: "echo {{printf `a }} b`}}", want: "a }} b"},
		{input: `echo {{printf "a \" }} b"}}`, want: `a " }} b`},
		{input: `echo {{printf "%c }}" '}'}}`, want: "} }}"},
		{input: `echo {{- /* }} " ignored */ -}}done`, want: "done"},
		{input: `echo '{{printf "a }} b"}}'`, want: "a }} b"},
	} {
		t.Run(tt.input, func(t *testing.T) {
			c, err := parseCommand(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			got, err := c.render(commandData{})
			if err != nil || !reflect.DeepEqual(got, []string{"echo", tt.want}) {
				t.Fatalf("argv = %q, %v", got, err)
			}
		})
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"": "''", "a b": "'a b'", "it's": `'it'\''s'`, "$(x)": "'$(x)'"} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
