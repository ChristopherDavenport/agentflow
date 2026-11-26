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
// literal. There are no operators. A reference may name alternatives,
// for a step that several branches reach, and may be allowed to have no
// value.
type Value struct {
	// From is inputs.<name> or steps.<id>.outputs, either followed by
	// further .<name> members.
	From    string `json:"from,omitempty"`
	Literal any    `json:"literal,omitempty"`
	// Or are further references. Of those with a value, the one whose
	// step finished last wins; an input is older than any step.
	Or []string `json:"or,omitempty"`
	// Optional leaves the argument out when no reference has a value.
	Optional bool `json:"optional,omitempty"`
}

// maxAlternatives bounds the work of checking a reference at load.
const maxAlternatives = 8

// From refers to recorded data.
func From(path string) Value { return Value{From: path} }

// Latest takes whichever of the references was produced last. At least
// one must have a value on every path that reaches the step.
func Latest(first string, rest ...string) Value { return Value{From: first, Or: rest} }

// Optional is [Latest] of references that may all be without a value.
func Optional(first string, rest ...string) Value {
	return Value{From: first, Or: rest, Optional: true}
}

// refs are the references v names, none for a literal.
func (v Value) refs() []string {
	if v.From == "" {
		return nil
	}
	return append([]string{v.From}, v.Or...)
}

// Literal is a fixed value.
func Literal(v any) Value { return Value{Literal: v} }

type ref struct {
	// step is empty for a reference to the workflow's inputs, and doc
	// names a document for a reference to its text.
	step string
	doc  string
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
	case p[0] == "documents" && len(p) == 2:
		return ref{doc: p[1]}, nil
	}
	return ref{}, fmt.Errorf("invalid reference %q: want inputs.<name>, steps.<id>.outputs or documents.<name>", s)
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
	if err := checkData(n.schemas()[res.Outcome], res.Data); err != nil {
		return fmt.Errorf("data of outcome %q: %w", res.Outcome, err)
	}
	return nil
}

// resolve reads v from what the instance has recorded: its inputs and
// the latest result of each step. It reports false for an optional value
// that has none.
func (st State) resolve(v Value) (any, bool, error) {
	if v.From == "" {
		return v.Literal, true, nil
	}
	var best any
	var missing []error
	found, newest := false, -1
	for _, path := range v.refs() {
		got, at, err := st.lookup(path)
		switch {
		case err != nil:
			missing = append(missing, err)
		case !found || at > newest:
			best, newest, found = got, at, true
		}
	}
	if !found && !v.Optional {
		return nil, false, errors.Join(missing...)
	}
	return best, found, nil
}

// lookup reads one reference, and says which finished step it came from,
// -1 for an input.
func (st State) lookup(path string) (any, int, error) {
	r, err := parseRef(path)
	if err != nil {
		return nil, 0, err
	}
	if r.doc != "" {
		text, ok := st.Documents[r.doc]
		if !ok {
			return nil, 0, fmt.Errorf("%s: no document %q", path, r.doc)
		}
		return text, st.docAt[r.doc] - 1, nil
	}
	cur, at := st.Inputs, -1
	if r.step != "" {
		at = len(st.Steps) - 1
		for at >= 0 && st.Steps[at].Node != r.step {
			at--
		}
		if at < 0 {
			return nil, 0, fmt.Errorf("%s: step %q has not run", path, r.step)
		}
		cur = st.Steps[at].Result.Data
	}
	for _, name := range r.path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, 0, fmt.Errorf("%s: no member %q", path, name)
		}
		if cur, ok = m[name]; !ok {
			return nil, 0, fmt.Errorf("%s: no member %q", path, name)
		}
	}
	return cur, at, nil
}

// resolveAll resolves a set of named values; nil in, nil out.
func (st State) resolveAll(values map[string]Value) (map[string]any, error) {
	if values == nil {
		return nil, nil
	}
	out := make(map[string]any, len(values))
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(values)) {
		v, ok, err := st.resolve(values[name])
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			out[name] = v
		}
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
	if s := n.schemas()[outcome]; s != nil {
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
	// A cleanup path starts with nothing certain but the inputs: a run may
	// be abandoned anywhere.
	if c := g.node(g.OnAbort); c != nil && c != &g.Nodes[0] {
		in[c.ID] = facts{}
	}
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
	// use checks a plain reference against what is certain at a point.
	use := func(where string, v Value, have facts) {
		r, err := parseRef(v.From)
		if err != nil {
			errs = append(errs, fmt.Errorf("agentflow: %s: %w", where, err))
			return
		}
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("agentflow: %s: %s: "+format, append([]any{where, v.From}, args...)...))
		}
		if r.doc != "" {
			if _, ok := g.Documents[r.doc]; !ok {
				bad("no document %q%s", r.doc, suggest(r.doc, g.documentNames()))
			}
			return
		}
		if r.step == "" {
			switch {
			case g.Inputs == nil || !declares(g.Inputs, r.path[0]):
				bad("not an input of the workflow%s", suggest(r.path[0], memberNames(g.Inputs)))
			case !slices.Contains(g.Inputs.Required, r.path[0]):
				bad("not a required input of the workflow")
			}
			return
		}
		from := g.node(r.step)
		switch {
		case from == nil:
			bad("no step %q%s", r.step, suggest(r.step, g.nodeIDs()))
		case !have[r.step]:
			bad("some path gets here without running %q", r.step)
		case len(r.path) > 0 && len(from.schemas()) > 0 && !have[r.step+"."+r.path[0]]:
			bad("%q does not certainly produce %q on every path here", r.step, r.path[0])
		}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		at, ok := in[n.ID]
		if !ok {
			continue
		}
		if c := n.Choices; c != nil && plain(c.From) {
			use("node "+n.ID+": choices", c.From, at)
		}
		for _, name := range slices.Sorted(maps.Keys(n.With)) {
			if v := n.With[name]; plain(v) {
				use("node "+n.ID, v, at)
			}
		}
		for _, outcome := range n.outcomes() {
			if n.Next[outcome] != "" || g.cleans(n.ID) {
				continue
			}
			outputs := g.outputs(n.ending(outcome))
			for _, name := range slices.Sorted(maps.Keys(outputs)) {
				if v := outputs[name]; plain(v) {
					use("output "+name+" after "+n.ID+" "+outcome, v, at.after(n, outcome))
				}
			}
		}
	}
	return append(errs, g.checkAlternatives()...)
}

// plain says v is one reference that must have a value.
func plain(v Value) bool { return v.From != "" && len(v.Or) == 0 && !v.Optional }

// checkAlternatives checks the references [checkRefs] leaves: that each
// could ever have a value, and, where one of them must, that no path
// arrives with none.
func (g *Graph) checkAlternatives() []error {
	var errs []error
	check := func(where string, v Value, at func(refs []ref) bool) {
		if v.From == "" || plain(v) {
			return
		}
		bad := func(format string, args ...any) {
			errs = append(errs, fmt.Errorf("agentflow: %s: "+format, append([]any{where}, args...)...))
		}
		paths := v.refs()
		if len(paths) > maxAlternatives {
			bad("over %d alternatives", maxAlternatives)
			return
		}
		refs := make([]ref, len(paths))
		for i, path := range paths {
			r, err := parseRef(path)
			if err == nil {
				err = g.possible(r)
			}
			if err != nil {
				bad("%s: %v", path, err)
				return
			}
			refs[i] = r
		}
		if !v.Optional && !at(refs) {
			bad("some path gets here with none of %s", strings.Join(paths, ", "))
		}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		for _, name := range slices.Sorted(maps.Keys(n.With)) {
			check("node "+n.ID, n.With[name], func(refs []ref) bool {
				return !g.reached(refs)[n.ID][0]
			})
		}
		if c := n.Choices; c != nil {
			check("node "+n.ID+": choices", c.From, func(refs []ref) bool {
				return !g.reached(refs)[n.ID][0]
			})
		}
	}
	byOutcome := map[string]map[string]Value{"": g.Outputs}
	if g.OutputsBy != nil {
		byOutcome = g.OutputsBy
	}
	for _, end := range slices.Sorted(maps.Keys(byOutcome)) {
		outputs := byOutcome[end]
		for _, name := range slices.Sorted(maps.Keys(outputs)) {
			where := "output " + name
			if end != "" {
				where = "output " + name + " of " + end
			}
			check(where, outputs[name], func(refs []ref) bool {
				at := g.reached(refs)
				for i := range g.Nodes {
					n := &g.Nodes[i]
					for _, outcome := range n.outcomes() {
						if n.Next[outcome] != "" || end != "" && n.ending(outcome) != end || g.cleans(n.ID) {
							continue
						}
						for have := range at[n.ID] {
							if n.available(refs, have, outcome) == 0 {
								return false
							}
						}
					}
				}
				return true
			})
		}
	}
	return errs
}

// possible says r could have a value at all, which catches a misspelt
// name that an optional reference would otherwise swallow.
func (g *Graph) possible(r ref) error {
	if r.doc != "" {
		if _, ok := g.Documents[r.doc]; !ok {
			return fmt.Errorf("no document %q%s", r.doc, suggest(r.doc, g.documentNames()))
		}
		return nil
	}
	if r.step == "" {
		if g.Inputs == nil || !declares(g.Inputs, r.path[0]) {
			return fmt.Errorf("not an input of the workflow%s", suggest(r.path[0], memberNames(g.Inputs)))
		}
		return nil
	}
	n := g.node(r.step)
	if n == nil {
		return fmt.Errorf("no step %q%s", r.step, suggest(r.step, g.nodeIDs()))
	}
	if len(r.path) == 0 {
		return nil
	}
	for _, outcome := range n.outcomes() {
		if s := n.schemas()[outcome]; s == nil && len(n.schemas()) == 0 || s != nil && declares(s, r.path[0]) {
			return nil
		}
	}
	var members []string
	for _, s := range n.schemas() {
		members = append(members, memberNames(s)...)
	}
	return fmt.Errorf("no outcome of %q declares %q%s", r.step, r.path[0], suggest(r.path[0], members))
}

func declares(s *agenttool.Schema, name string) bool {
	return slices.ContainsFunc(s.Properties, func(p agenttool.Property) bool { return p.Name == name })
}

// available is which of refs have a value after n ends with outcome,
// one bit each, given which had one before.
func (n *Node) available(refs []ref, have uint, outcome string) uint {
	for i, r := range refs {
		if r.step != n.ID {
			continue
		}
		have &^= 1 << i
		s := n.schemas()[outcome]
		if len(r.path) == 0 || len(n.schemas()) == 0 || s != nil && slices.Contains(s.Required, r.path[0]) {
			have |= 1 << i
		}
	}
	return have
}

// reached is, for each node, every combination of refs that some path
// arrives with.
func (g *Graph) reached(refs []ref) map[string]map[uint]bool {
	var first uint
	for i, r := range refs {
		// A document always has a text, empty at first.
		if r.doc != "" || r.step == "" && g.Inputs != nil && slices.Contains(g.Inputs.Required, r.path[0]) {
			first |= 1 << i
		}
	}
	at := map[string]map[uint]bool{g.Nodes[0].ID: {first: true}}
	if c := g.node(g.OnAbort); c != nil {
		at[c.ID] = map[uint]bool{first: true}
	}
	for changed := true; changed; {
		changed = false
		for i := range g.Nodes {
			n := &g.Nodes[i]
			for have := range at[n.ID] {
				for outcome, next := range n.Next {
					out := n.available(refs, have, outcome)
					if at[next] == nil {
						at[next] = map[uint]bool{}
					}
					if !at[next][out] {
						at[next][out], changed = true, true
					}
				}
			}
		}
	}
	return at
}
