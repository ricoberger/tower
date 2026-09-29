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
// and booleans into strings). A null mapping value is treated like an omitted
// setting. Unknown keys are left to the decoder's strict field check.
// Messages identify the field and line, never the value.
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
	if isNull(n) {
		return nil
	}
	var errs []error
	checkNode(n, t, "", &errs)
	return errors.Join(errs...)
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

func typeError(n *yaml.Node, field, want string) error {
	if field == "" {
		field = "configuration"
	}
	return fmt.Errorf("line %d: %s: must be %s", n.Line, field, want)
}

func checkNode(n *yaml.Node, t reflect.Type, field string, errs *[]error) {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
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
		fields := map[string]reflect.Type{}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name != "" && name != "-" {
				fields[name] = f.Type
			}
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			ft, ok := fields[k.Value]
			if !ok || k.Kind != yaml.ScalarNode {
				continue // unknown keys are reported by the strict decoder
			}
			name := k.Value
			if field != "" {
				name = field + "." + k.Value
			}
			if isNull(v) {
				continue // null is treated as an omitted setting
			}
			checkNode(v, ft, name, errs)
		}
	}
}
