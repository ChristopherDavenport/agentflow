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

// helper reports once, and says it always runs alone and only when called.
func helper() *Graph {
	return &Graph{Name: "helper", Context: ContextFresh, Hidden: true, Nodes: []Node{
		{ID: "say", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"helped"}`)},
	}}
}

func helped() *Graph {
	return &Graph{Name: "helped", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "helper"},
	}}
}

// callArgsOf are the arguments of the flow_call items makes.
func callArgsOf(t *testing.T, items agentturn.Transcript) []callArgs {
	t.Helper()
	var out []callArgs
	for _, item := range items {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == callTool {
			var a callArgs
			if err := json.Unmarshal([]byte(c.Arguments), &a); err != nil {
				t.Fatal(err)
			}
			out = append(out, a)
		}
	}
	return out
}

func TestACalleeThatSaysFreshRunsInAChild(t *testing.T) {
	g := linked(t, helped(), helper())
	if !g.Nodes[0].fresh() {
		t.Fatal("the call does not run in a child")
	}
	var ran []string
	m := &fakeModel{}
	k := newKin(m, "fake", scriptTool(&ran))
	a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)}, k.option(), Unattended())
	end, _, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(ran) != 1 || len(k.opened) != 1 || k.opened[0].Workflow != "helper" {
		t.Errorf("ran %v, opened %+v", ran, k.opened)
	}
	calls := callArgsOf(t, a.State().Transcript)
	if len(calls) != 1 || calls[0].Context != ContextFresh {
		t.Errorf("calls = %+v", calls)
	}
}

func TestACallMayNotRunAFreshCalleeInItsContext(t *testing.T) {
	caller := helped()
	caller.Nodes[0].Context = ContextSession
	err := caller.Link(helper())
	if err == nil || !strings.Contains(err.Error(), "it says it runs in a child of its own") {
		t.Errorf("err = %v", err)
	}
}

func TestAFreshCalleeTakesAChildsSettingsOnACallThatDoesNotSayFresh(t *testing.T) {
	caller := helped()
	caller.Nodes[0].Dir = Literal("services")
	if err := caller.Validate(); err != nil {
		t.Errorf("before it is linked: %v", err)
	}
	if err := caller.Link(helper()); err != nil {
		t.Errorf("linked: %v", err)
	}
	alone := &Graph{Name: "alone", Nodes: []Node{{ID: "call", Kind: Run, Flow: "plain", Dir: Literal("services")}}}
	plain := &Graph{Name: "plain", Nodes: []Node{{ID: "say", Kind: Run, Tool: "script"}}}
	// Every call runs in a child, so one that opens with its caller's
	// context takes a child's settings too.
	if err := alone.Link(plain); err != nil {
		t.Errorf("a call to a workflow that does not say fresh: %v", err)
	}
}

func TestAHiddenWorkflowRunsOnlyWhenCalled(t *testing.T) {
	h := helper()
	if _, err := h.Invocation(nil); err == nil || !strings.Contains(err.Error(), "helper is hidden") {
		t.Errorf("invocation: %v", err)
	}
	var ran []string
	if _, err := Apply(h, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}}); err == nil || !strings.Contains(err.Error(), "helper is hidden") {
		t.Errorf("applied on its own: %v", err)
	}
	// Called in the caller's context, it runs.
	h.Context = ""
	g := linked(t, helped(), h)
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{scriptTool(&ran)}, Unattended())
	if end := start(t, a); end.Reason != agentturn.ReasonDone || end.Err != nil || len(ran) != 1 {
		t.Errorf("end = %+v, ran %v", end, ran)
	}
}

func TestAWorkflowSaysOnlyContextFresh(t *testing.T) {
	h := helper()
	h.Context = "frsh"
	if err := h.Validate(); err == nil || !strings.Contains(err.Error(), `context "frsh"`) || !strings.Contains(err.Error(), `did you mean "fresh"?`) {
		t.Errorf("err = %v", err)
	}
}
