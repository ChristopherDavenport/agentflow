package agentflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// checkArgs holds each tool step's arguments, its args with its with
// merged over them, to the parameters of the tool the config offers: a
// required parameter given on every path, no name the tool does not take
// where it takes no others, a literal of its parameter's schema, and a
// reference whose schema is known of its parameter's type. What a config
// with a ToolProvider offers is known only as it runs, and a reference
// with no schema only when it is resolved; both are checked when the
// call is made, as before.
func checkArgs(g *Graph, cfg agentturn.Config) error {
	if cfg.ToolProvider != nil {
		return nil
	}
	tools := map[string]agenttool.Tool{}
	for _, t := range cfg.Tools {
		tools[t.Name()] = t
	}
	var errs []error
	for _, x := range []*Graph{g} {
		for i := range x.Nodes {
			n := &x.Nodes[i]
			t := tools[n.Tool]
			if n.Kind != Run || n.Tool == "" || t == nil {
				continue
			}
			params, err := decodeSchema(t.Parameters())
			if err != nil || params == nil {
				continue
			}
			for _, err := range x.argsFit(n, params) {
				errs = append(errs, fmt.Errorf("agentflow: %s: node %q: tool %s: %w", x.Name, n.ID, n.Tool, err))
			}
		}
	}
	return errors.Join(errs...)
}

// argsFit is every way n's arguments do not fit the tool's parameters.
func (g *Graph) argsFit(n *Node, params *agenttool.Schema) []error {
	var errs []error
	var names []string
	for _, p := range params.Properties {
		names = append(names, p.Name)
	}
	param := func(name string) *agenttool.Schema {
		for _, p := range params.Properties {
			if p.Name == name {
				return p.Schema
			}
		}
		return nil
	}
	var args map[string]any
	if n.Args != nil {
		_ = decode(n.Args, &args)
	}
	given := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(args)) {
		given[name] = true
		if _, ok := n.With[name]; ok {
			// with is merged over args, so with's value is what is sent.
			continue
		}
		errs = append(errs, fit(name, param(name), params, names, Literal(args[name]), g)...)
	}
	for _, name := range slices.Sorted(maps.Keys(n.With)) {
		v := n.With[name]
		// An optional value may be left out, so it gives nothing for sure.
		if !v.Optional {
			given[name] = true
		}
		errs = append(errs, fit(name, param(name), params, names, v, g)...)
	}
	for _, name := range params.Required {
		if !given[name] {
			errs = append(errs, fmt.Errorf("parameter %q is required, and neither args nor with gives it on every path", name))
		}
	}
	return errs
}

// fit is how the value v, given as the argument name, does not fit its
// parameter p of params.
func fit(name string, p, params *agenttool.Schema, names []string, v Value, g *Graph) []error {
	if p == nil {
		if params.NoAdditional {
			return []error{fmt.Errorf("parameter %q is not one the tool takes%s", name, suggest(name, names))}
		}
		return nil
	}
	if v.From == "" {
		b, err := json.Marshal(v.Literal)
		if err != nil {
			return []error{fmt.Errorf("parameter %q: %w", name, err)}
		}
		if err := p.ValidateJSON(b); err != nil {
			return []error{fmt.Errorf("parameter %q: %w", name, err)}
		}
		return nil
	}
	for _, path := range v.refs() {
		s := g.refSchema(path)
		if s == nil || s.Type == "" || p.Type == "" || s.Type == p.Type || s.Type == "integer" && p.Type == "number" {
			continue
		}
		return []error{fmt.Errorf("parameter %q wants %s, and %s is %s", name, p.Type, path, s.Type)}
	}
	return nil
}
