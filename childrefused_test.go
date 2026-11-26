package agentflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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

// careless calls fragile fresh, within timeout, and reports a failure.
func careless(t *testing.T, timeout time.Duration) *Graph {
	return linked(t, &Graph{Name: "careless", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "fragile", Context: ContextFresh, Timeout: timeout, Next: map[string]string{Failed: "report"}},
		{ID: "report", Kind: Run, Tool: "script", With: map[string]Value{"name": From("steps.call.outputs.reason")}},
	}}, fragile())
}

// A refused child under a call with a timeout still routes on failed, with
// the opener's words, before the time runs out.
func TestARefusedChildUnderATimeoutRoutesOnFailed(t *testing.T) {
	g := careless(t, time.Hour)
	fails := 0
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{failing(&fails), scriptTool(&ran)}, refusing(fmt.Errorf("dir %q: %w", "trees/gone", ErrChildRefused)))
	end, st, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone || !slices.Equal(steps(st), []string{"call:failed", "report:ok"}) {
		t.Fatalf("end = %s %v, path %v", end.Reason, err, steps(st))
	}
	if len(ran) != 1 || !strings.Contains(ran[0], `dir "trees/gone"`) {
		t.Errorf("reported %v", ran)
	}
}

// A call whose time runs out while its child cannot be opened times out:
// there is nothing in the child to abandon, and that does not fail.
func TestACallTimesOutOverAChildThatCannotBeOpened(t *testing.T) {
	g := careless(t, 50*time.Millisecond)
	fails := 0
	var ran []string
	down := errors.New("the store is down")
	opened := 0
	opener := Children(func(context.Context, Child) (context.Context, *agentturn.Agent, error) {
		opened++
		if opened == 1 {
			return nil, nil, down
		}
		return nil, nil, fmt.Errorf("gone: %w", ErrChildRefused)
	})
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{failing(&fails), scriptTool(&ran)}, opener)
	if _, _, err := Drive(context.Background(), g, a, start(t, a)); !errors.Is(err, down) {
		t.Fatalf("err = %v, want the host's", err)
	}
	time.Sleep(70 * time.Millisecond)
	end, st, err := Drive(context.Background(), g, a, nil)
	if err != nil || end.Reason != agentturn.ReasonDone || !slices.Equal(steps(st), []string{"call:failed", "report:ok"}) {
		t.Fatalf("end = %+v, %v, path %v", end, err, steps(st))
	}
	if len(ran) != 1 || !strings.Contains(ran[0], `call "call" of careless ran past its timeout of 50ms`) {
		t.Errorf("reported %v", ran)
	}
}
