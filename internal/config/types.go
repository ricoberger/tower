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
// of which one the decoder would let win. Explicit tags ("!!int '7'",
// "!!str 12", custom tags) are rejected, because they re-type values. A null
// mapping value is treated like an omitted setting. Unknown keys, including
// unknown keys in merged mappings, are left to the decoder's strict field
// check. Messages identify the field and line, never the value.
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
	var errs []error
	n = resolve(n)
	if isNull(n) && !tagged(n) {
		return nil
	}
	checkNode(n, t, "", &errs)
	return errors.Join(errs...)
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

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

func isMergeKey(k *yaml.Node) bool {
	return k.Kind == yaml.ScalarNode && k.ShortTag() == "!!merge"
}

func typeError(n *yaml.Node, field, want string) error {
	if field == "" {
		field = "configuration"
	}
	return fmt.Errorf("line %d: %s: must be %s", n.Line, field, want)
}

func checkNode(n *yaml.Node, t reflect.Type, field string, errs *[]error) {
	n = resolve(n)
	if tagged(n) {
		*errs = append(*errs, typeError(n, field, "written without an explicit YAML tag"))
		return
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
			*errs = append(*errs, typeError(n, field, "a string"))
		}
	case reflect.Int:
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!int" {
			*errs = append(*errs, typeError(n, field, "an integer"))
		}
	case reflect.Bool:
		if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!bool" {
			*errs = append(*errs, typeError(n, field, "a boolean (true or false)"))
		}
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			*errs = append(*errs, typeError(n, field, "a list"))
			return
		}
		for i, c := range n.Content {
			checkNode(c, t.Elem(), fmt.Sprintf("%s[%d]", field, i), errs)
		}
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			*errs = append(*errs, typeError(n, field, "a mapping"))
			return
		}
		checkMapping(n, structFields(t), field, errs)
	default:
		*errs = append(*errs, typeError(n, field, "a supported value"))
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

// checkMapping checks the key/value pairs of mapping n, including all pairs
// merged into it through "<<".
func checkMapping(n *yaml.Node, fields map[string]reflect.Type, field string, errs *[]error) {
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := resolve(n.Content[i]), n.Content[i+1]
		if tagged(k) {
			*errs = append(*errs, fmt.Errorf("line %d: %s: keys must be written without an explicit YAML tag", k.Line, orConfig(field)))
			continue
		}
		if isMergeKey(k) {
			checkMerge(v, fields, field, errs)
			continue
		}
		ft, ok := fields[k.Value]
		if !ok || k.Kind != yaml.ScalarNode {
			continue // unknown keys are reported by the strict decoder
		}
		name := k.Value
		if field != "" {
			name = field + "." + k.Value
		}
		if rv := resolve(v); isNull(rv) && !tagged(rv) {
			continue // null is treated as an omitted setting
		}
		checkNode(v, ft, name, errs)
	}
}

// checkMerge checks the value of a merge key: a mapping or a list of
// mappings (each possibly an alias).
func checkMerge(v *yaml.Node, fields map[string]reflect.Type, field string, errs *[]error) {
	v = resolve(v)
	if tagged(v) {
		*errs = append(*errs, typeError(v, field, "merged without an explicit YAML tag"))
		return
	}
	switch v.Kind {
	case yaml.MappingNode:
		checkMapping(v, fields, field, errs)
	case yaml.SequenceNode:
		for _, e := range v.Content {
			e = resolve(e)
			if e.Kind != yaml.MappingNode || tagged(e) {
				*errs = append(*errs, typeError(e, field, "merged from mappings only"))
				continue
			}
			checkMapping(e, fields, field, errs)
		}
	default:
		*errs = append(*errs, typeError(v, field, "merged from mappings only"))
	}
}

func orConfig(field string) string {
	if field == "" {
		return "configuration"
	}
	return field
}
