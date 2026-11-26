package agentflow

import (
	"fmt"
	"maps"
	"slices"
)

// checkDefaults holds each input's default to the input: one the workflow
// declares, not required, and of its schema.
func (g *Graph) checkDefaults() []error {
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(g.InputDefaults)) {
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("agentflow: input %s: "+format, append([]any{name}, args...)...))
		}
		switch {
		case g.Inputs == nil || !declares(g.Inputs, name):
			bad("has a default, and the workflow has no such input%s", suggest(name, memberNames(g.Inputs)))
		case slices.Contains(g.Inputs.Required, name):
			bad("is required and has a default: an input with a default may be left out, so it is not required")
		default:
			if err := checkData(member(g.Inputs, []string{name}), g.InputDefaults[name]); err != nil {
				bad("default: %v", err)
			}
		}
	}
	return errs
}

// defaulted says the input name has a default, so a run always has it.
func (g *Graph) defaulted(name string) bool {
	_, ok := g.InputDefaults[name]
	return ok
}

// certainInput says every run of g has the input name: it is required, or
// has a default.
func (g *Graph) certainInput(name string) bool {
	return g.Inputs != nil && slices.Contains(g.Inputs.Required, name) || g.defaulted(name)
}

// withDefaults is inputs with every default it leaves out filled in: what a
// run's start records. Inputs that are not an object are left as they are,
// for the start to refuse.
func (g *Graph) withDefaults(inputs any) any {
	if len(g.InputDefaults) == 0 {
		return inputs
	}
	var out map[string]any
	switch in := inputs.(type) {
	case nil:
		out = map[string]any{}
	case map[string]any:
		out = maps.Clone(in)
	default:
		return inputs
	}
	if out == nil {
		out = map[string]any{}
	}
	for _, name := range slices.Sorted(maps.Keys(g.InputDefaults)) {
		if _, given := out[name]; !given {
			out[name] = g.InputDefaults[name]
		}
	}
	return out
}
