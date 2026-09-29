package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// LookupEnv looks up an environment variable, like os.LookupEnv.
type LookupEnv func(key string) (string, bool)

// expand performs one pass of environment interpolation on value. It supports
// $VAR, ${VAR} and the $$ escape. Variable names must match
// [A-Za-z_][A-Za-z0-9_]*. Dollar signs that do not start a supported
// reference or escape are kept literally. Substituted values are never
// interpreted again. The returned error names the field and the variable, but
// never the value.
func expand(field, value string, env LookupEnv) (string, error) {
	if !strings.Contains(value, "$") {
		return value, nil
	}

	var b strings.Builder
	for i := 0; i < len(value); {
		c := value[i]
		if c != '$' || i+1 >= len(value) {
			b.WriteByte(c)
			i++
			continue
		}

		next := value[i+1]
		switch {
		case next == '$':
			b.WriteByte('$')
			i += 2
		case next == '{':
			end := strings.IndexByte(value[i+2:], '}')
			if end < 0 || !isName(value[i+2:i+2+end]) {
				b.WriteByte(c)
				i++
				continue
			}
			name := value[i+2 : i+2+end]
			v, ok := env(name)
			if !ok {
				return "", unsetError(field, name)
			}
			b.WriteString(v)
			i += 2 + end + 1
		case isNameStart(next):
			j := i + 2
			for j < len(value) && isNameChar(value[j]) {
				j++
			}
			name := value[i+1 : j]
			v, ok := env(name)
			if !ok {
				return "", unsetError(field, name)
			}
			b.WriteString(v)
			i = j
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

func unsetError(field, name string) error {
	return fmt.Errorf("%s: environment variable %q is not set", field, name)
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool {
	return isNameStart(c) || (c >= '0' && c <= '9')
}

func isName(s string) bool {
	if s == "" || !isNameStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isNameChar(s[i]) {
			return false
		}
	}
	return true
}

// expandHome replaces a leading "~/" with the value of HOME followed by a path
// separator. Bare "~", "~user/" and embedded tildes are left unchanged.
func expandHome(field, value string, env LookupEnv) (string, error) {
	if !strings.HasPrefix(value, "~/") {
		return value, nil
	}
	home, ok := env("HOME")
	if !ok || home == "" {
		return "", fmt.Errorf("%s: a leading \"~/\" requires HOME to be set", field)
	}
	return strings.TrimRight(home, string(filepath.Separator)) + string(filepath.Separator) + value[2:], nil
}

// normalizePath expands a leading "~/" and resolves a remaining relative path
// against base. An empty value stays empty. The value must already be
// interpolated.
func normalizePath(field, value, base string, env LookupEnv) (string, error) {
	if value == "" {
		return "", nil
	}
	v, err := expandHome(field, value, env)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(v) {
		return filepath.Clean(v), nil
	}
	return filepath.Join(base, v), nil
}

// normalizeExecutable applies path normalization to executable settings that
// contain a path separator (or start with "~/"). Bare executable names are
// returned unchanged so they are resolved through PATH.
func normalizeExecutable(field, value, base string, env LookupEnv) (string, error) {
	if !strings.ContainsRune(value, filepath.Separator) {
		return value, nil
	}
	return normalizePath(field, value, base, env)
}
