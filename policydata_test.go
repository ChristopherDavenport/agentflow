package agentflow

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

var planned = &agenttool.Schema{Type: "object", Required: []string{"files"}, Properties: []agenttool.Property{
	{Name: "files", Schema: &agenttool.Schema{Type: "array", Items: &agenttool.Schema{Type: "string"}}},
}}

// planTool plans to touch files, the next list each time it runs.
func planTool(lists ...[]any) agenttool.Tool {
	return agenttool.New("plan", "Plan.", func(context.Context, struct{}) (Result, error) {
		files := lists[0]
		if len(lists) > 1 {
			lists = lists[1:]
		}
		return Result{Outcome: OK, Data: map[string]any{"files": files}}, nil
	})
}

// toPlan writes only the files its plan names.
func toPlan(context string) *Graph {
	return &Graph{Name: "toplan", Nodes: []Node{
		{ID: "plan", Kind: Run, Tool: "plan", Data: map[string]*agenttool.Schema{OK: planned}, Next: map[string]string{OK: "edit"}},
		{ID: "edit", Kind: Agent, Prompt: "Edit.", Tools: []string{"write"}, Context: context,
			Policy: &Policy{Allow: []PolicyRule{{Rule: "write", Path: From("steps.plan.outputs.files"), Note: "the plan's files"}}}},
	}}
}

func TestAPolicyTakesItsPathsFromTheRecord(t *testing.T) {
	var did []string
	m := &cast{script: []says{
		{"write", `{"path":"src/c.go"}`},
		{"write", `{"path":"src/a.go"}`},
		{finishTool, `{"step":"edit","outcome":"ok"}`},
	}}
	tools := append(touching(&did), planTool([]any{"src/a.go", "src/b.go"}))
	a := nestedAgent(t, toPlan(""), m, tools, matchers)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	if want := []string{`write {"path":"src/a.go"}`}; !slices.Equal(did, want) {
		t.Errorf("did %v, want %v", did, want)
	}
	if got := refusals(a.State().Transcript); len(got) != 1 || !strings.Contains(got[0], "no rule allows write") {
		t.Errorf("refusals = %q", got)
	}
	// The request records the limit as it stood, each path a rule.
	in := m.requests[0].Instructions
	if !strings.Contains(in, "write(src/a.go) (the plan's files), write(src/b.go) (the plan's files)") {
		t.Errorf("instructions = %q", in)
	}
}

func TestAPathFromTheRecordThatLeavesTheWorkspaceFailsTheStep(t *testing.T) {
	for _, c := range []string{"", ContextFresh} {
		t.Run("context "+c, func(t *testing.T) {
			var did []string
			m := &cast{script: []says{{finishTool, `{"step":"edit","outcome":"ok"}`}}}
			tools := append(touching(&did), planTool([]any{"../etc/passwd"}, []any{"src/a.go"}))
			kids := newKin(m, "fake", tools...)
			a := nestedAgent(t, toPlan(c), m, tools, matchers, kids.option())
			end, _, err := Drive(context.Background(), toPlan(c), a, start(t, a))
			if err != nil {
				t.Fatal(err)
			}
			q := question(t, end)
			if !q.Retry || !strings.Contains(q.Question, `path "../etc/passwd": want a clean path below the workspace's root`) {
				t.Fatalf("question = %+v", q)
			}
			// No model was asked to work under it.
			if len(m.requests) != 0 {
				t.Errorf("%d requests", len(m.requests))
			}
		})
	}
}

func TestACallsPolicyFromTheRecordHoldsWhatItCalls(t *testing.T) {
	inner := &Graph{Name: "inner", Nodes: []Node{{ID: "edit", Kind: Agent, Prompt: "Edit.", Tools: []string{"write"}}}}
	outer := &Graph{Name: "outer", Nodes: []Node{
		{ID: "plan", Kind: Run, Tool: "plan", Data: map[string]*agenttool.Schema{OK: planned}, Next: map[string]string{OK: "call"}},
		{ID: "call", Kind: Run, Flow: "inner", Policy: &Policy{Deny: []PolicyRule{{Rule: "write", Path: From("steps.plan.outputs.files")}}}},
	}}
	g := linked(t, outer, inner)
	var did []string
	m := &cast{script: []says{
		{"write", `{"path":"locked/a"}`},
		{"write", `{"path":"free/a"}`},
		{finishTool, `{"step":"edit","outcome":"ok"}`},
	}}
	a := nestedAgent(t, g, m, append(touching(&did), planTool([]any{"locked/a"})), matchers)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	if want := []string{`write {"path":"free/a"}`}; !slices.Equal(did, want) {
		t.Errorf("did %v, want %v", did, want)
	}
	if got := refusals(a.State().Transcript); len(got) != 1 || !strings.Contains(got[0], `the policy of step "call" of outer: denied by write(locked/a)`) {
		t.Errorf("refusals = %q", got)
	}
}

func TestPolicyRulesFromTheRecordAreCheckedAtLoad(t *testing.T) {
	for name, c := range map[string]struct {
		rule PolicyRule
		want string
	}{
		"literal":      {PolicyRule{Rule: "write", Path: Literal("src/a.go")}, "write a fixed one as write(...)"},
		"spec":         {PolicyRule{Rule: "write(src/*)", Path: From("steps.plan.outputs.files")}, "names its tool alone"},
		"both":         {PolicyRule{Rule: "write", Path: From("steps.plan.outputs.files"), Command: From("steps.plan.outputs.files")}, "not both"},
		"misspelt":     {PolicyRule{Rule: "write", Path: From("steps.pla.outputs.files")}, `no step "pla"; did you mean "plan"?`},
		"not a string": {PolicyRule{Rule: "write", Path: From("steps.plan.outputs")}, "is not a string, nor a list of them"},
	} {
		t.Run(name, func(t *testing.T) {
			g := toPlan("")
			g.Nodes[1].Policy = &Policy{Allow: []PolicyRule{c.rule}}
			if err := g.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	g := shipper()
	g.Nodes[1].Judge = &Judge{Criteria: "Ship.", Outcomes: []string{"yes"}, Tools: []string{"read"},
		Policy: &Policy{Deny: []PolicyRule{{Rule: "read", Path: From("steps.build.outputs.version")}}}}
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "a judge's rules are fixed") {
		t.Errorf("a judge's: %v", err)
	}
}
