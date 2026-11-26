package agentflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// refusing opens no child, with err, as an opener that cannot do what the
// call asks says so.
func refusing(err error) Option {
	return Children(func(context.Context, Child) (context.Context, *agentturn.Agent, error) { return nil, nil, err })
}

func TestARefusedChildRoutesOnFailed(t *testing.T) {
	g := linked(t, &Graph{Name: "careful", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "fragile", Context: ContextFresh, Next: map[string]string{Failed: "report"}},
		{ID: "report", Kind: Run, Tool: "script", With: map[string]Value{"name": From("steps.call.outputs.reason")}},
	}}, fragile())
	fails := 0
	var ran []string
	tools := []agenttool.Tool{failing(&fails), scriptTool(&ran)}
	refused := fmt.Errorf("dir %q: %w", "trees/gone", ErrChildRefused)
	a := nestedAgent(t, g, &fakeModel{}, tools, Unattended(), refusing(refused))
	end, st, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone || !slices.Equal(steps(st), []string{"call:failed", "report:ok"}) {
		t.Fatalf("end = %s %v, path %v", end.Reason, err, steps(st))
	}
	if len(ran) != 1 || !strings.Contains(ran[0], `dir "trees/gone"`) {
		t.Errorf("reported %v", ran)
	}
}

func TestARefusedChildFailsItsStepUnattended(t *testing.T) {
	g := linked(t, &Graph{Name: "plain", Nodes: []Node{{ID: "call", Kind: Run, Flow: "fragile", Context: ContextFresh}}}, fragile())
	fails := 0
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{failing(&fails)}, Unattended(), refusing(fmt.Errorf("gone: %w", ErrChildRefused)))
	_, _, err := Drive(context.Background(), g, a, start(t, a))
	if !errors.Is(err, ErrStepFailed) || !strings.Contains(err.Error(), "gone") {
		t.Errorf("err = %v, want the step failed with the opener's words", err)
	}
}

func TestAnOpenersOwnErrorIsTheHosts(t *testing.T) {
	g := linked(t, &Graph{Name: "plain", Nodes: []Node{{ID: "call", Kind: Run, Flow: "fragile", Context: ContextFresh}}}, fragile())
	fails := 0
	down := errors.New("the store is down")
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{failing(&fails)}, refusing(down))
	_, _, err := Drive(context.Background(), g, a, start(t, a))
	if !errors.Is(err, down) || errors.Is(err, ErrStepFailed) {
		t.Fatalf("err = %v, want the opener's own error", err)
	}
	// The call still waits on its child: nothing was answered for it.
	if pending := a.State().Pending; len(pending) != 1 || pending[0].Call.Name != callTool {
		t.Errorf("pending = %+v", pending)
	}
}
