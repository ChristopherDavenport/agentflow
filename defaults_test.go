package agentflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

var greeting = &agenttool.Schema{Type: "object", Required: []string{"who"}, Properties: []agenttool.Property{
	{Name: "who", Schema: &agenttool.Schema{Type: "string"}},
	{Name: "how", Schema: &agenttool.Schema{Type: "string"}},
}}

// greet calls script with an input that has a default.
func greet() *Graph {
	return &Graph{Name: "greet", Inputs: greeting, InputDefaults: map[string]any{"how": "hello"}, Nodes: []Node{
		{ID: "say", Kind: Run, Tool: "script", With: map[string]Value{"name": From("inputs.how")}},
	}}
}

// startInputs is what the start of run key recorded as its inputs.
func startInputs(t *testing.T, items agentturn.Transcript, workflow string) map[string]any {
	t.Helper()
	for _, item := range items {
		c, ok := item.(*openresponses.FunctionCall)
		if !ok || c.Name != startTool {
			continue
		}
		var p startPayload
		if err := json.Unmarshal([]byte(c.Arguments), &p); err != nil {
			t.Fatal(err)
		}
		if p.Workflow == workflow {
			m, _ := p.Inputs.(map[string]any)
			return m
		}
	}
	t.Fatalf("no start of %s", workflow)
	return nil
}

func TestAnInputLeftOutTakesItsDefault(t *testing.T) {
	g := greet()
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{scriptTool(&ran)}, Unattended())
	item, err := g.Invocation(map[string]any{"who": "ada"})
	if err != nil {
		t.Fatal(err)
	}
	end, err := a.Prompt(context.Background(), item)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(ran) != 1 || ran[0] != "hello" {
		t.Errorf("ran %v", ran)
	}
	// The start records what the run was given, the default filled in.
	if got := startInputs(t, whole(a), "greet"); got["how"] != "hello" || got["who"] != "ada" {
		t.Errorf("start recorded %v", got)
	}
}

func TestAnInputGivenOverridesItsDefault(t *testing.T) {
	g := greet()
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{scriptTool(&ran)}, Unattended())
	item, err := g.Invocation(map[string]any{"who": "ada", "how": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Prompt(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0] != "hi" {
		t.Errorf("ran %v", ran)
	}
}

func TestACallerMayLeaveOutADefaultedInput(t *testing.T) {
	caller := &Graph{Name: "caller", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "greet", With: map[string]Value{"who": Literal("bob")}},
	}}
	g := linked(t, caller, greet())
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{scriptTool(&ran)}, Unattended())
	end := start(t, a)
	if end.Reason != agentturn.ReasonDone || end.Err != nil {
		t.Fatalf("end = %s, %v", end.Reason, end.Err)
	}
	if len(ran) != 1 || ran[0] != "hello" {
		t.Errorf("ran %v", ran)
	}
	if got := startInputs(t, whole(a), "greet"); got["how"] != "hello" || got["who"] != "bob" {
		t.Errorf("the callee's start recorded %v", got)
	}
}

func TestInputDefaultsAreCheckedAtLoad(t *testing.T) {
	for _, c := range []struct {
		name     string
		defaults map[string]any
		want     string
	}{
		{"unknown", map[string]any{"hows": "x"}, `no such input; did you mean "how"?`},
		{"required", map[string]any{"who": "x"}, "is required and has a default"},
		{"type", map[string]any{"how": 3}, "default:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			g := greet()
			g.InputDefaults = c.defaults
			err := g.Validate()
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestAReferenceToADefaultedInputIsCertain(t *testing.T) {
	g := greet()
	g.InputDefaults = nil
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "nor one with a default") {
		t.Errorf("without the default: %v", err)
	}
	if err := greet().Validate(); err != nil {
		t.Errorf("with it: %v", err)
	}
}
