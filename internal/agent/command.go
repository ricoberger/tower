package agent

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"text/template"
)

// commandData is the data of the run_command and resume_command templates.
type commandData struct {
	ID        int64
	Title     string
	SessionID string
	// Prompt is the rendered prompt; empty in resume_command.
	Prompt string
}

// command is a command line whose arguments are Go templates.
type command struct {
	args []*template.Template
}

// parseCommand splits s into arguments (see splitCommand) and parses every
// argument as a Go template.
func parseCommand(s string) (*command, error) {
	raw, err := splitCommand(s)
	if err != nil {
		return nil, err
	}
	c := &command{}
	for i, a := range raw {
		t, err := template.New(fmt.Sprintf("arg%d", i)).Funcs(template.FuncMap{"shquote": shellQuote}).Parse(a)
		if err != nil {
			return nil, err
		}
		c.args = append(c.args, t)
	}
	return c, nil
}

// render returns the argv. Every argument stays one argument, whatever the
// data contains.
func (c *command) render(d commandData) ([]string, error) {
	argv := make([]string, len(c.args))
	for i, t := range c.args {
		var b bytes.Buffer
		if err := t.Execute(&b, d); err != nil {
			return nil, err
		}
		argv[i] = b.String()
	}
	if argv[0] == "" {
		return nil, errors.New("the command name is empty")
	}
	return argv, nil
}

// shellQuote quotes s as one POSIX shell word. Use it for values that end up
// in a command line interpreted by a shell, e.g. ghostty-new --command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// splitCommand splits a command line into arguments with shell-like quoting:
// whitespace separates arguments, single quotes keep their content literally,
// double quotes allow \" and \\ escapes, and a backslash outside quotes
// escapes the next character. Template actions ({{ … }}) are kept verbatim,
// including their spaces and quotes, except inside single quotes. The result
// is executed directly, without a shell, so unquoted shell operators are
// rejected instead of being passed on as literal arguments.
func splitCommand(s string) ([]string, error) {
	var (
		args    []string
		cur     strings.Builder
		inWord  bool
		errOpen = errors.New("unterminated quote")
	)
	// action copies a template action starting at i and returns the index
	// of its last character.
	action := func(i int) (int, error) {
		var quote byte
		for j := i + 2; j < len(s); j++ {
			switch {
			case quote != 0:
				if s[j] == '\\' && quote != '`' {
					j++
				} else if s[j] == quote {
					quote = 0
				}
			case s[j] == '"' || s[j] == '\'' || s[j] == '`':
				quote = s[j]
			case strings.HasPrefix(s[j:], "/*"):
				end := strings.Index(s[j+2:], "*/")
				if end < 0 {
					return 0, errors.New("unterminated template comment")
				}
				j += end + 3
			case strings.HasPrefix(s[j:], "}}"):
				cur.WriteString(s[i : j+2])
				return j + 1, nil
			}
		}
		return 0, errors.New("unterminated {{")
	}
	for i := 0; i < len(s); i++ {
		r := s[i]
		switch {
		case strings.HasPrefix(s[i:], "{{"):
			inWord = true
			j, err := action(i)
			if err != nil {
				return nil, err
			}
			i = j
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		case r == '\'':
			inWord = true
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errOpen
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
		case r == '"':
			inWord = true
			j := i + 1
			for ; j < len(s) && s[j] != '"'; j++ {
				switch {
				case strings.HasPrefix(s[j:], "{{"):
					k, err := action(j)
					if err != nil {
						return nil, err
					}
					j = k
					continue
				case s[j] == '\\' && j+1 < len(s) && (s[j+1] == '"' || s[j+1] == '\\'):
					j++
				}
				cur.WriteByte(s[j])
			}
			if j == len(s) {
				return nil, errOpen
			}
			i = j
		case r == '\\':
			if i+1 == len(s) {
				return nil, errors.New("trailing backslash")
			}
			inWord = true
			i++
			cur.WriteByte(s[i])
		case strings.IndexByte(";|&()`", r) >= 0:
			return nil, fmt.Errorf("unquoted %q: commands are run without a shell; quote it or wrap the command in a script", r)
		default:
			inWord = true
			cur.WriteByte(r)
		}
	}
	if inWord {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("empty command")
	}
	return args, nil
}
