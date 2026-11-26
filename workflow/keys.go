package workflow

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/ChristopherDavenport/agentflow"
	"go.yaml.in/yaml/v3"
)

var (
	yamlNode        = reflect.TypeFor[yaml.Node]()
	yamlUnmarshaler = reflect.TypeFor[yaml.Unmarshaler]()
)

// knownKeys refuses a key of a mapping in n that t, the settings it is
// decoded into, has no field for, naming the key it was likely meant to
// be. It walks into nested settings; what a yaml.Node field or a type of
// its own reading holds is that reader's to check.
func knownKeys(n *yaml.Node, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n == nil || t == yamlNode || reflect.PointerTo(t).Implements(yamlUnmarshaler) {
		return nil
	}
	switch {
	case n.Kind == yaml.DocumentNode:
		for _, c := range n.Content {
			if err := knownKeys(c, t); err != nil {
				return err
			}
		}
	case t.Kind() == reflect.Slice && n.Kind == yaml.SequenceNode:
		for _, c := range n.Content {
			if err := knownKeys(c, t.Elem()); err != nil {
				return err
			}
		}
	case t.Kind() == reflect.Struct && n.Kind == yaml.MappingNode:
		fields := map[string]reflect.Type{}
		var keys []string
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "" || name == "-" {
				continue
			}
			fields[name] = f.Type
			keys = append(keys, name)
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			ft, ok := fields[k.Value]
			if !ok {
				return fmt.Errorf("line %d: unknown key %q%s", k.Line, k.Value, agentflow.Suggest(k.Value, keys))
			}
			if err := knownKeys(v, ft); err != nil {
				return err
			}
		}
	}
	return nil
}

// unknownKey is the error of a key a mapping read by hand does not have.
func unknownKey(what, key string, keys ...string) error {
	if what != "" {
		what += ": "
	}
	return fmt.Errorf("%sunknown key %q%s", what, key, agentflow.Suggest(key, keys))
}
