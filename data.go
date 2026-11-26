package agentflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// Value is one argument of a step: a reference to recorded data, or a
// literal. There are no operators and no functions.
type Value struct {
	// From is inputs.<name> or steps.<id>.outputs, either followed by
	// further .<name> members.
	From    string `json:"from,omitempty"`
	Literal any    `json:"literal,omitempty"`
}

// From refers to recorded data.
func From(path string) Value { return Value{From: path} }

// Literal is a fixed value.
func Literal(v any) Value { return Value{Literal: v} }

type ref struct {
	// step is empty for a reference to the workflow's inputs.
	step string
	path []string
}

func parseRef(s string) (ref, error) {
	p := strings.Split(s, ".")
	switch {
	case slices.Contains(p, ""):
	case p[0] == "inputs" && len(p) > 1:
		return ref{path: p[1:]}, nil
	case p[0] == "steps" && len(p) > 2 && p[2] == "outputs":
		return ref{step: p[1], path: p[3:]}, nil
	}
	return ref{}, fmt.Errorf("invalid reference %q: want inputs.<name> or steps.<id>.outputs", s)
}

// decode reads JSON keeping numbers as written.
func decode(raw []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	return d.Decode(v)
}

// exact reads the call's arguments again, keeping numbers as written: a
// typed tool decodes them into floating point, which would round a large
// ID on its way to the next step.
func exact[T any](ctx context.Context, v *T) error {
	call, ok := agenttool.CallFrom(ctx)
	if !ok {
		return nil
	}
	var fresh T
	if err := decode(call.Args, &fresh); err != nil {
		return fmt.Errorf("agentflow: arguments: %w", err)
	}
	*v = fresh
	return nil
}

// checkData holds v to a schema; nil accepts anything.
func checkData(s *agenttool.Schema, v any) error {
	if s == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.ValidateJSON(b)
}

// check holds a result to what its node declares.
func (n *Node) check(res Result) error {
	if !slices.Contains(n.outcomes(), res.Outcome) {
		return fmt.Errorf("outcome %q: want one of %s", res.Outcome, strings.Join(n.outcomes(), ", "))
	}
	if err := checkData(n.Data[res.Outcome], res.Data); err != nil {
		return fmt.Errorf("data of outcome %q: %w", res.Outcome, err)
	}
	return nil
}

// resolve reads v from what the instance has recorded: its inputs and
// the latest result of each step.
func (st State) resolve(v Value) (any, error) {
	if v.From == "" {
		return v.Literal, nil
	}
	r, err := parseRef(v.From)
	if err != nil {
		return nil, err
	}
	cur := st.Inputs
	if r.step != "" {
		i := len(st.Steps) - 1
		for i >= 0 && st.Steps[i].Node != r.step {
			i--
		}
		if i < 0 {
			return nil, fmt.Errorf("%s: step %q has not run", v.From, r.step)
		}
		cur = st.Steps[i].Result.Data
	}
	for _, name := range r.path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: no member %q", v.From, name)
		}
		if cur, ok = m[name]; !ok {
			return nil, fmt.Errorf("%s: no member %q", v.From, name)
		}
	}
	return cur, nil
}

// resolveAll resolves a set of named values; nil in, nil out.
func (st State) resolveAll(values map[string]Value) (map[string]any, error) {
	if values == nil {
		return nil, nil
	}
	out := make(map[string]any, len(values))
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(values)) {
		v, err := st.resolve(values[name])
		if err != nil {
			errs = append(errs, err)
		}
		out[name] = v
	}
	return out, errors.Join(errs...)
}

// facts is what is certainly recorded at a point in the graph: "step"
// for a step that has run, "step.field" for a field its latest result
// certainly has.
type facts map[string]bool

// after is what holds once n has ended with outcome.
func (f facts) after(n *Node, outcome string) facts {
	out := facts{n.ID: true}
	for k := range f {
		if k != n.ID && !strings.HasPrefix(k, n.ID+".") {
			out[k] = true
		}
	}
	if s := n.Data[outcome]; s != nil {
		for _, field := range s.Required {
			out[n.ID+"."+field] = true
		}
	}
	return out
}

// checkRefs reports every reference some path through the graph would
// leave without a value, so it fails at load and not halfway through.
func (g *Graph) checkRefs() []error {
	// A node with no entry has not been reached by the analysis.
	in := map[string]facts{g.Nodes[0].ID: {}}
	for changed := true; changed; {
		changed = false
		for i := range g.Nodes {
			n := &g.Nodes[i]
			at, ok := in[n.ID]
			if !ok {
				continue
			}
			for outcome, next := range n.Next {
				// Nothing is recorded on the way into the start.
				if next == g.Nodes[0].ID || g.node(next) == nil {
					continue
				}
				out := at.after(n, outcome)
				old, seen := in[next]
				if seen {
					maps.DeleteFunc(out, func(k string, _ bool) bool { return !old[k] })
					if len(out) == len(old) {
						continue
					}
				}
				in[next], changed = out, true
			}
		}
	}

	var errs []error
	use := func(where string, v Value, have facts) {
		if v.From == "" {
			return
		}
		r, err := parseRef(v.From)
		if err != nil {
			errs = append(errs, fmt.Errorf("agentflow: %s: %w", where, err))
			return
		}
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("agentflow: %s: %s: "+format, append([]any{where, v.From}, args...)...))
		}
		if r.step == "" {
			if g.Inputs == nil || !slices.Contains(g.Inputs.Required, r.path[0]) {
				bad("not a required input of the workflow")
			}
			return
		}
		from := g.node(r.step)
		switch {
		case from == nil:
			bad("no step %q", r.step)
		case !have[r.step]:
			bad("some path gets here without running %q", r.step)
		case len(r.path) > 0 && len(from.Data) > 0 && !have[r.step+"."+r.path[0]]:
			bad("%q does not certainly produce %q on every path here", r.step, r.path[0])
		}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		at, ok := in[n.ID]
		if !ok {
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(n.With)) {
			use("node "+n.ID, n.With[name], at)
		}
		for _, outcome := range n.outcomes() {
			if n.Next[outcome] != "" {
				continue
			}
			for _, name := range slices.Sorted(maps.Keys(g.Outputs)) {
				use("output "+name+" after "+n.ID+" "+outcome, g.Outputs[name], at.after(n, outcome))
			}
		}
	}
	return errs
}
