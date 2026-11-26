package agentflow

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
)

func one(name string, required bool) *agenttool.Schema {
	s := &agenttool.Schema{Type: "object", Properties: []agenttool.Property{{Name: name, Schema: &agenttool.Schema{Type: "string"}}}}
	if required {
		s.Required = []string{name}
	}
	return s
}

// forked goes from a to b or c, and both reach d.
func forked(with Value) *Graph {
	script := func(id string, next map[string]string, outcomes ...string) Node {
		return Node{ID: id, Kind: Run, Script: id, Outcomes: outcomes, Next: next}
	}
	g := &Graph{Name: "f", Nodes: []Node{
		script("a", map[string]string{"left": "b", "right": "c"}, "left", "right"),
		script("b", map[string]string{OK: "d"}),
		script("c", map[string]string{OK: "d"}),
		script("d", nil),
	}}
	g.Nodes[1].Data = map[string]*agenttool.Schema{OK: one("x", true)}
	g.Nodes[2].Data = map[string]*agenttool.Schema{OK: one("x", true)}
	g.Nodes[3].With = map[string]Value{"v": with}
	return g
}

func TestAlternativesAreCheckedAtLoad(t *testing.T) {
	for name, c := range map[string]struct {
		value Value
		want  string
	}{
		"one of two branches":   {From("steps.b.outputs.x"), "without running"},
		"both branches":         {Latest("steps.b.outputs.x", "steps.c.outputs.x"), ""},
		"optional":              {Optional("steps.b.outputs.x"), ""},
		"not enough":            {Latest("steps.b.outputs.x", "steps.d.outputs"), "none of"},
		"no such step":          {Optional("steps.z.outputs.x"), `no step "z"`},
		"no such member":        {Optional("steps.b.outputs.y"), `no outcome of "b" declares "y"`},
		"no such input":         {Optional("inputs.y"), "not an input"},
		"latest with a typo":    {Latest("steps.b.outputs.x", "steps.c.outputs.y"), "declares"},
		"too many alternatives": {Latest("steps.b.outputs.x", slicesOf("steps.c.outputs.x", 8)...), "alternatives"},
	} {
		err := forked(c.value).Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
	// An outcome that drops the member again leaves nothing on that path.
	g := forked(Latest("steps.b.outputs.x", "steps.c.outputs.x"))
	g.Nodes[2].Data[OK] = one("x", false)
	if err := g.Validate(); err == nil {
		t.Error("a path on which c need not produce x was accepted")
	}
	// So does a workflow output on some way out.
	g = forked(Literal("-"))
	g.Outputs = map[string]Value{"v": Latest("steps.b.outputs.x", "steps.c.outputs.x")}
	if err := g.Validate(); err != nil {
		t.Error(err)
	}
	g.Outputs = map[string]Value{"v": Latest("steps.b.outputs.x", "steps.b.outputs")}
	if err := g.Validate(); err == nil {
		t.Error("an output with no value after c was accepted")
	}
}

func slicesOf(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestTheNewestAlternativeWins(t *testing.T) {
	st := State{Inputs: map[string]any{"x": "given"}, Steps: []Step{
		{Node: "b", Result: Result{Data: map[string]any{"x": "from b"}}},
		{Node: "c", Result: Result{Data: map[string]any{"x": "from c"}}},
		{Node: "b", Result: Result{Data: map[string]any{"x": "from b again"}}},
		// The latest result of c no longer has x, so c has no value.
		{Node: "c", Result: Result{Data: map[string]any{}}},
	}}
	got, err := st.resolveAll(map[string]Value{
		"newest":  Latest("steps.c.outputs.x", "steps.b.outputs.x"),
		"input":   Latest("inputs.x", "steps.z.outputs.x"),
		"over":    Latest("inputs.x", "steps.b.outputs.x"),
		"absent":  Optional("steps.c.outputs.x", "steps.z.outputs"),
		"present": Optional("steps.b.outputs.x"),
	})
	want := map[string]any{"newest": "from b again", "input": "given", "over": "from b again", "present": "from b again"}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, %v; want %v", got, err, want)
	}
}
