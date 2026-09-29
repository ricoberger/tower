package config

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// checkTypes verifies that the YAML node tree has the value types required by
// the raw configuration type t, before the lenient conversions of the YAML
// decoder apply (e.g. 1.9 into an integer, "yes" into a boolean, or numbers
// and booleans into strings).
//
// Every way YAML can supply a value to the decoder is covered: aliases
// (values and keys) are resolved to their anchors, and all values contributed
// through merge keys ("<<") are checked as well as explicit keys, regardless
// of which one the decoder would let win. Explicit standard tags are allowed
// when they match the expected type (e.g. "!!str vi", "!!int 2", "!!map");
// mismatching standard tags ("!!float 1" for an integer, "!!binary" for a
// string) and custom tags are rejected. A tagged scalar must also be a valid
// plain representation of its tag, so the decoder never reports (and thereby
// discloses) an unconvertible value. Cyclic aliases are reported instead of
// being followed. A null mapping value is treated like an omitted setting.
// Unknown keys, including unknown keys in merged mappings, are left to the
// decoder's strict field check. Messages identify the field and line, never
// the value.
func checkTypes(doc *yaml.Node, t reflect.Type) error {
	if doc == nil || doc.Kind == 0 {
		return nil // empty file
	}
	n := doc
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil
		}
		n = n.Content[0]
	}
	c := &checker{active: map[*yaml.Node]bool{}, done: map[visit]bool{}}
	if !omitted(n) {
		c.node(n, t, "")
	}
	return errors.Join(c.errs...)
}

// visit identifies a composite node checked against a Go type for a field.
// Each visit is performed once, which bounds the work for documents that
// reference the same anchor many times (e.g. repeated merges), while an
// anchor used for different fields is still reported for each of them.
type visit struct {
	n     *yaml.Node
	t     reflect.Type
	field string
}

type checker struct {
	active map[*yaml.Node]bool // composite nodes on the current traversal path
	done   map[visit]bool
	errs   []error
}

// resolve follows aliases to the anchored node.
func resolve(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

func tagged(n *yaml.Node) bool {
	return n.Style&yaml.TaggedStyle != 0
}

// tagOf returns the effective YAML tag of the resolved node n, or "" if n has
// a custom (non-standard) tag or is a tagged scalar whose text is not a valid
// plain representation of its tag (e.g. "!!int 'x'", "!!bool yes"). Plain
// scalars resolve implicitly; "!!str" accepts any text.
func tagOf(n *yaml.Node) string {
	tag := n.ShortTag()
	if !strings.HasPrefix(tag, "!!") {
		return ""
	}
	if n.Kind == yaml.ScalarNode && tagged(n) && tag != "!!str" {
		plain := yaml.Node{Kind: yaml.ScalarNode, Value: n.Value}
		if plain.ShortTag() != tag {
			return ""
		}
	}
	return tag
}

// omitted reports whether the value of a mapping entry is null, which is
// treated like an omitted setting.
func omitted(v *yaml.Node) bool {
	v = resolve(v)
	return v.Kind == yaml.ScalarNode && tagOf(v) == "!!null"
}

// isMergeKey mirrors the decoder's merge key detection.
func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.Value == "<<" && (k.Tag == "" || k.Tag == "!" || k.ShortTag() == "!!merge")
}

func orConfig(field string) string {
	if field == "" {
		return "configuration"
	}
	return field
}

func (c *checker) fail(line int, field, msg string) {
	c.errs = append(c.errs, fmt.Errorf("line %d: %s: %s", line, orConfig(field), msg))
}

// enter marks composite node n as being checked against t for field. It
// reports false if n was already checked against t for field, or if n is on the current path, i.e.
// the document references n from inside itself (line is the reference).
func (c *checker) enter(n *yaml.Node, t reflect.Type, line int, field string) bool {
	if c.active[n] {
		c.fail(line, field, "must not contain itself (cyclic YAML alias)")
		return false
	}
	if c.done[visit{n, t, field}] {
		return false
	}
	c.done[visit{n, t, field}] = true
	c.active[n] = true
	return true
}

func (c *checker) leave(n *yaml.Node) { delete(c.active, n) }

func (c *checker) node(orig *yaml.Node, t reflect.Type, field string) {
	n := resolve(orig)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	tag := tagOf(n)
	switch t.Kind() {
	case reflect.String:
		if n.Kind != yaml.ScalarNode || tag != "!!str" {
			c.fail(n.Line, field, "must be a string")
		}
	case reflect.Int:
		if n.Kind != yaml.ScalarNode || tag != "!!int" {
			c.fail(n.Line, field, "must be an integer")
		}
	case reflect.Bool:
		if n.Kind != yaml.ScalarNode || tag != "!!bool" {
			c.fail(n.Line, field, "must be a boolean (true or false)")
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode || tag != "!!seq" {
			c.fail(n.Line, field, "must be a list")
			return
		}
		if !c.enter(n, t, orig.Line, field) {
			return
		}
		defer c.leave(n)
		for i, e := range n.Content {
			c.node(e, t.Elem(), fmt.Sprintf("%s[%d]", field, i))
		}
	case reflect.Struct:
		if n.Kind != yaml.MappingNode || tag != "!!map" {
			c.fail(n.Line, field, "must be a mapping")
			return
		}
		c.mapping(orig, t, field)
	default:
		c.fail(n.Line, field, "must be a supported value")
	}
}

// mapping checks the key/value pairs of the mapping orig resolves to,
// including all pairs merged into it through "<<", against struct type t.
func (c *checker) mapping(orig *yaml.Node, t reflect.Type, field string) {
	n := resolve(orig)
	if !c.enter(n, t, orig.Line, field) {
		return
	}
	defer c.leave(n)
	fields := structFields(t)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := resolve(n.Content[i]), n.Content[i+1]
		if isMergeKey(k) {
			c.merge(v, t, field)
			continue
		}
		if k.Kind != yaml.ScalarNode || tagOf(k) != "!!str" {
			if tagged(k) {
				c.fail(k.Line, field, "keys must be strings")
			}
			continue // other unknown keys are reported by the strict decoder
		}
		ft, ok := fields[k.Value]
		if !ok {
			continue
		}
		name := k.Value
		if field != "" {
			name = field + "." + k.Value
		}
		if omitted(v) {
			continue
		}
		c.node(v, ft, name)
	}
}

// merge checks the value of a merge key: a mapping or a list of mappings
// (each possibly an alias).
func (c *checker) merge(orig *yaml.Node, t reflect.Type, field string) {
	v := resolve(orig)
	switch {
	case v.Kind == yaml.MappingNode && tagOf(v) == "!!map":
		c.mapping(orig, t, field)
	case v.Kind == yaml.SequenceNode && tagOf(v) == "!!seq":
		if !c.enter(v, t, orig.Line, field) {
			return
		}
		defer c.leave(v)
		for _, e := range v.Content {
			if r := resolve(e); r.Kind != yaml.MappingNode || tagOf(r) != "!!map" {
				c.fail(r.Line, field, "must be merged from mappings only")
				continue
			}
			c.mapping(e, t, field)
		}
	default:
		c.fail(v.Line, field, "must be merged from mappings only")
	}
}

func structFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
		if name != "" && name != "-" {
			fields[name] = f.Type
		}
	}
	return fields
}
