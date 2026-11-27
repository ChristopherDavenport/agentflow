package agentflow

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// DefaultMaxDepth bounds how deeply calls nest when neither the workflow
// nor the host sets a bound. A run's root has depth 0.
const DefaultMaxDepth = 8

// failure is the data of the outcome [Failed] a call ends with when the
// workflow it called failed.
var failure = &agenttool.Schema{Type: "object", Required: []string{"reason", "workflow"}, Properties: []agenttool.Property{
	{Name: "reason", Schema: &agenttool.Schema{Type: "string"}},
	{Name: "workflow", Schema: &agenttool.Schema{Type: "string"}},
	{Name: "step", Schema: &agenttool.Schema{Type: "string"}},
}}

// calleeName is the name of the workflow a Flow node of g calls: a
// workflow's own name, or for ./<path>.md one of the caller's skill,
// named <skill>/<path>.
func calleeName(g *Graph, flow string) string {
	rel, ok := strings.CutPrefix(flow, "./")
	if !ok {
		return flow
	}
	skill, _, _ := strings.Cut(g.Name, "/")
	return skill + "/" + strings.TrimSuffix(rel, ".md")
}

// Link resolves the workflows g calls, and those they call, among flows,
// and checks each call against what it calls: the inputs it passes, the
// outcomes it routes on and what it reads of the result. g may call
// itself. A graph that calls nothing needs no linking; one that does is
// refused by [Apply] until it is linked.
func (g *Graph) Link(flows ...*Graph) error {
	pool := map[string]*Graph{g.Name: g}
	for _, f := range flows {
		if old := pool[f.Name]; old != nil && old != f {
			return fmt.Errorf("agentflow: two workflows are named %q", f.Name)
		}
		pool[f.Name] = f
	}
	// Resolve breadth first, so an error names the shortest chain.
	chain := map[string][]string{g.Name: {g.Name}}
	queue := []*Graph{g}
	var errs []error
	for len(queue) > 0 {
		x := queue[0]
		queue = queue[1:]
		x.Callees = nil
		for i := range x.Nodes {
			n := &x.Nodes[i]
			if n.Flow == "" {
				continue
			}
			name := calleeName(x, n.Flow)
			c := pool[name]
			if c == nil {
				errs = append(errs, fmt.Errorf("agentflow: %s: node %q calls %s: no such workflow%s", strings.Join(chain[x.Name], " → "), n.ID, name, Suggest(name, slices.Collect(maps.Keys(pool)))))
				continue
			}
			if x.Callees == nil {
				x.Callees = map[string]*Graph{}
			}
			x.Callees[n.Flow] = c
			if chain[c.Name] == nil {
				chain[c.Name] = append(slices.Clone(chain[x.Name]), c.Name)
				queue = append(queue, c)
			}
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	closure := g.closure()
	names := slices.Sorted(maps.Keys(closure))
	calls := func(yield func(*Graph, *Node) bool) {
		for _, name := range names {
			x := closure[name]
			for i := range x.Nodes {
				if n := &x.Nodes[i]; n.Flow != "" && !yield(x, n) {
					return
				}
			}
		}
	}
	for x, n := range calls {
		c := x.Callees[n.Flow]
		n.call = &callee{g: c, other: c.Model != "" && c.Model != x.Model}
	}
	// A workflow's outcomes are its ends, and a call that ends a
	// workflow ends it with what the call ended with, so they are found
	// together, growing until nothing changes.
	for changed := true; changed; {
		changed = false
		for _, n := range calls {
			outcomes := append(n.call.g.Outcomes(), Failed)
			slices.Sort(outcomes)
			outcomes = slices.Compact(outcomes)
			if !slices.Equal(outcomes, n.call.outcomes) {
				n.call.outcomes, changed = outcomes, true
			}
		}
	}
	// What a call gives back can be what another call gave back, so its
	// schema is read once per workflow that could lie between.
	for range len(closure) + 1 {
		for _, n := range calls {
			n.call.data = n.call.g.results()
		}
	}
	for _, name := range names {
		if err := closure[name].Validate(); err != nil {
			errs = append(errs, err)
		}
	}
	for x, n := range calls {
		errs = append(errs, x.contract(n)...)
	}
	return errors.Join(errs...)
}

// results is the data the workflow gives back, by outcome, as a caller
// receives it.
func (g *Graph) results() map[string]*agenttool.Schema {
	out := map[string]*agenttool.Schema{Failed: failure}
	for _, outcome := range g.Outcomes() {
		if outcome == Failed {
			continue
		}
		values := g.outputs(outcome)
		if values == nil {
			continue
		}
		s := &agenttool.Schema{Type: "object", Properties: []agenttool.Property{}}
		for _, name := range slices.Sorted(maps.Keys(values)) {
			v := values[name]
			s.Properties = append(s.Properties, agenttool.Property{Name: name, Schema: cmp.Or(g.valueSchema(v), &agenttool.Schema{})})
			if !v.Optional {
				s.Required = append(s.Required, name)
			}
		}
		out[outcome] = s
	}
	return out
}

// valueSchema is the schema of what v holds, nil when it cannot be known:
// a literal, or references that do not agree.
func (g *Graph) valueSchema(v Value) *agenttool.Schema {
	if v.From == "" {
		return nil
	}
	var found *agenttool.Schema
	for _, path := range v.refs() {
		s := g.refSchema(path)
		if s == nil || found != nil && !sameSchema(found, s) {
			return nil
		}
		found = s
	}
	return found
}

// refSchema is the schema of what a reference reads, as every outcome
// that declares it agrees, nil when unknown.
func (g *Graph) refSchema(path string) *agenttool.Schema {
	r, err := parseRef(path)
	if err != nil {
		return nil
	}
	switch {
	case r.doc != "":
		return &agenttool.Schema{Type: "string"}
	case r.step == "":
		return member(g.Inputs, r.path)
	}
	n := g.node(r.step)
	if n == nil {
		return nil
	}
	var found *agenttool.Schema
	for _, outcome := range n.outcomes() {
		s := n.schemas()[outcome]
		if s == nil {
			continue
		}
		m := member(s, r.path)
		if m == nil {
			continue
		}
		if found != nil && !sameSchema(found, m) {
			return nil
		}
		found = m
	}
	return found
}

// member walks an object schema down path.
func member(s *agenttool.Schema, path []string) *agenttool.Schema {
	for _, name := range path {
		if s == nil {
			return nil
		}
		i := slices.IndexFunc(s.Properties, func(p agenttool.Property) bool { return p.Name == name })
		if i < 0 {
			return nil
		}
		s = s.Properties[i].Schema
	}
	return s
}

func sameSchema(a, b *agenttool.Schema) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

// compatible says a value of schema have may be given where want is
// asked for, as far as their types tell.
func compatible(have, want *agenttool.Schema) bool {
	switch {
	case have == nil || want == nil || have.Type == "" || want.Type == "":
		return true
	case have.Type == want.Type:
		return true
	}
	return have.Type == "integer" && want.Type == "number"
}

// contract checks a call node of g against the workflow it calls.
func (g *Graph) contract(n *Node) []error {
	var errs []error
	c := n.call.g
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agentflow: %s: node %q calls %s: "+format, append([]any{g.Name, n.ID, c.Name}, args...)...))
	}
	if c.Context == ContextFresh && n.Context == ContextSession {
		bad("it says it runs in a child of its own, context: %s, and the call says %s", ContextFresh, ContextSession)
	}
	if n.call.other && n.Context == ContextSession {
		bad("it names its own model, %q, which is not this workflow's%s, and the call says context: %s, which keeps the model of the session it reads: say context: %s",
			c.Model, workflowModel(g), ContextSession, ContextFresh)
	}
	if len(n.Outcomes) > 0 {
		own := slices.Sorted(slices.Values(n.Outcomes))
		without := slices.DeleteFunc(slices.Clone(n.call.outcomes), func(o string) bool { return o == Failed })
		if !slices.Equal(own, n.call.outcomes) && !slices.Equal(own, without) {
			bad("outcomes %s: want %s, with or without %s", strings.Join(own, ", "), strings.Join(without, ", "), Failed)
		}
	}
	var props []string
	if c.Inputs != nil {
		for _, p := range c.Inputs.Properties {
			props = append(props, p.Name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(n.With)) {
		v := n.With[name]
		want := member(c.Inputs, []string{name})
		switch {
		case !slices.Contains(props, name):
			bad("%s is not an input of it%s", name, Suggest(name, props))
		case v.From == "":
			if err := checkData(want, v.Literal); err != nil {
				bad("%s: %v", name, err)
			}
		case !compatible(g.valueSchema(v), want):
			bad("%s: %s is %s, and it wants %s", name, v.From, g.valueSchema(v).Type, want.Type)
		}
	}
	if c.Inputs != nil {
		for _, name := range c.Inputs.Required {
			if v, ok := n.With[name]; !ok || v.Optional {
				bad("it requires the input %s, which is not certainly given", name)
			}
		}
	}
	return errs
}
