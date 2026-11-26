package agentflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// tagger tags a version, and takes no other arguments.
func tagger() agenttool.Tool {
	params := json.RawMessage(`{"type":"object","required":["name"],"additionalProperties":false,"properties":{"name":{"type":"string"},"sign":{"type":"boolean"},"count":{"type":"integer"}}}`)
	return agenttool.NewFunc("tag", "Tag.", params, func(context.Context, agenttool.Call) (agenttool.Result, error) {
		return agenttool.Text(`{"outcome":"ok"}`), nil
	})
}

var versioned = &agenttool.Schema{Type: "object", Required: []string{"version", "n"}, Properties: []agenttool.Property{
	{Name: "version", Schema: &agenttool.Schema{Type: "string"}},
	{Name: "n", Schema: &agenttool.Schema{Type: "integer"}},
}}

// tagStep is a workflow whose tag step takes args and with.
func tagStep(args string, with map[string]Value) *Graph {
	n := Node{ID: "tag", Kind: Run, Tool: "tag", With: with}
	if args != "" {
		n.Args = json.RawMessage(args)
	}
	return &Graph{Name: "tags", Nodes: []Node{
		{ID: "ask", Kind: Person, Prompt: "Which version?", Data: map[string]*agenttool.Schema{OK: versioned}, Next: map[string]string{OK: "tag"}},
		n,
	}}
}

func TestAToolStepsArgumentsAreCheckedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    *Graph
		want string
	}{
		{"a required one missing", tagStep(`{"sign":true}`, nil), `parameter "name" is required`},
		{"a misspelt one", tagStep(`{"name":"v1","signn":true}`, nil), `parameter "signn" is not one the tool takes; did you mean "sign"?`},
		{"a literal of the wrong type", tagStep(`{"name":"v1","sign":"yes"}`, nil), `parameter "sign"`},
		{"a reference of the wrong type", tagStep("", map[string]Value{"name": From("steps.ask.outputs.n")}), `parameter "name" wants string, and steps.ask.outputs.n is integer`},
		{"an optional value", tagStep("", map[string]Value{"name": Optional("steps.ask.outputs.version")}), `parameter "name" is required`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Apply(tc.g, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: []agenttool.Tool{tagger()}})
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `node "tag": tool tag`) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// What fits passes: with over args, a reference of the right type.
	ok := tagStep(`{"name":"draft","sign":true}`, map[string]Value{"name": From("steps.ask.outputs.version"), "count": From("steps.ask.outputs.n")})
	if _, err := Apply(ok, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: []agenttool.Tool{tagger()}}); err != nil {
		t.Errorf("err = %v", err)
	}
	// A config with a ToolProvider names its tools only as it runs.
	bad := tagStep(`{"sign":true}`, nil)
	provided := func(context.Context) []agenttool.Tool { return []agenttool.Tool{tagger()} }
	if _, err := Apply(bad, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", ToolProvider: provided}); err != nil {
		t.Errorf("err = %v", err)
	}
}
