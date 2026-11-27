package agentflow

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

func TestSuggest(t *testing.T) {
	for _, c := range []struct {
		s     string
		names []string
		want  string
	}{
		{"reveiw", []string{"gather", "review", "report"}, `; did you mean "review"?`},
		{"triag", []string{"triage"}, `; did you mean "triage"?`},
		{"x", []string{"report"}, ""},
		{"completely", []string{"review"}, ""},
		// Ties go to the first in order, whatever order names come in.
		{"ab", []string{"ac", "aa"}, `; did you mean "aa"?`},
	} {
		if got := Suggest(c.s, c.names); got != c.want {
			t.Errorf("Suggest(%q, %v) = %q, want %q", c.s, c.names, got, c.want)
		}
	}
}

// numbered is release() with inputs and a schema on review's findings,
// so references to both can be misspelt.
func numbered() *Graph {
	g := release()
	g.Inputs = &agenttool.Schema{Type: "object", Required: []string{"pr"}, Properties: []agenttool.Property{{Name: "pr", Schema: &agenttool.Schema{Type: "integer"}}}}
	g.node("review").Data = map[string]*agenttool.Schema{"findings": {Type: "object", Required: []string{"findings"}, Properties: []agenttool.Property{{Name: "findings", Schema: &agenttool.Schema{Type: "array"}}}}}
	return g
}

func TestLoadErrorsSuggestTheNameMeant(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*Graph)
		want   string
	}{
		"an edge's node":         {func(g *Graph) { g.node("fix").Next[OK] = "gathr" }, `edge to unknown node "gathr"; did you mean "gather"?`},
		"an edge's outcome":      {func(g *Graph) { g.node("review").Next["clen"] = "report" }, `undeclared outcome "clen"; did you mean "clean"?`},
		"an end's outcome":       {func(g *Graph) { g.node("triage").Ends = map[string]string{"reprot": "x"} }, `end for undeclared outcome "reprot"; did you mean "report"?`},
		"a judge's outcome":      {func(g *Graph) { g.node("triage").Judge = &Judge{Criteria: "c", Outcomes: []string{"fx"}} }, `judge: undeclared outcome "fx"; did you mean "fix"?`},
		"the unattended outcome": {func(g *Graph) { g.node("triage").Unattended.Outcome = "reprot" }, `unattended must be an outcome of a person node; did you mean "report"?`},
		"a data outcome":         {func(g *Graph) { g.node("review").Data["finding"] = &agenttool.Schema{Type: "object"} }, `data for undeclared outcome "finding"; did you mean "findings"?`},
		"a step":                 {func(g *Graph) { g.node("fix").With = map[string]Value{"f": From("steps.reveiw.outputs.findings")} }, `no step "reveiw"; did you mean "review"?`},
		"an input":               {func(g *Graph) { g.node("gather").With = map[string]Value{"pr": From("inputs.p")} }, `not an input of the workflow; did you mean "pr"?`},
		"a member":               {func(g *Graph) { g.node("fix").With = map[string]Value{"f": Optional("steps.review.outputs.findigns")} }, `declares "findigns"; did you mean "findings"?`},
		"a workflow outcome": {func(g *Graph) {
			g.OutputsBy = map[string]map[string]Value{"okk": {}}
		}, `outputs for "okk", which is not an outcome of the workflow; did you mean "ok"?`},
		"a choices outcome": {func(g *Graph) {
			g.node("triage").Choices = &Choices{From: From("steps.gather.outputs.run"), Outcome: "fx"}
		}, `outcome "fx" is not declared; did you mean "fix"?`},
	} {
		t.Run(name, func(t *testing.T) {
			g := numbered()
			c.change(g)
			if err := g.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestApplySuggestsTheNameMeant(t *testing.T) {
	var ran []string
	tools := []agenttool.Tool{scriptTool(&ran)}
	for name, c := range map[string]struct {
		change func(*Graph)
		opts   []Option
		want   string
	}{
		"a model step's tool": {func(g *Graph) { g.node("review").Tools = []string{"scirpt"} }, nil, `names the tool scirpt, which the config does not offer; did you mean "script"?`},
		"a program step's tool": {func(g *Graph) {
			g.node("report").Tool = "scrip"
		}, nil, `names the tool scrip, which the config does not offer; did you mean "script"?`},
		"a policy's tool": {func(g *Graph) {
			g.node("review").Tools = []string{"script"}
			g.node("review").Policy = &Policy{Deny: []PolicyRule{{Rule: "scripts(gather)"}}}
		}, nil, `offers no tool scripts; did you mean "script"?`},
		"a delegated step": {func(g *Graph) {
			g.node("triage").Judge = &Judge{Criteria: "c", Outcomes: []string{"fix"}}
		}, []Option{Delegate(Decide, "triag")}, `step "triag" cannot be judged; did you mean "triage"?`},
	} {
		t.Run(name, func(t *testing.T) {
			g := release()
			c.change(g)
			_, err := kinApply(g, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: tools}, c.opts...)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}
