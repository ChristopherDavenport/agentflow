package agentflow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A fresh step whose model says it is stuck, under rules that stop every
// failed step: the run fails saying why, wherever the rules are asked.
func TestAStuckFreshStepStoppedByRulesFailsSayingWhy(t *testing.T) {
	g := &Graph{Name: "stuck", Nodes: []Node{
		{ID: "prep", Kind: Agent, Prompt: "Prepare.", Next: map[string]string{OK: "work"}},
		{ID: "work", Kind: Agent, Prompt: "Work.", Context: ContextFresh},
	}}
	rules := Reply(func(_ context.Context, q Question) (Result, bool) {
		if q.Retry {
			return Result{Outcome: stopOutcome}, true
		}
		return Result{}, false
	})
	m := &cast{script: []says{
		{finishTool, `{"step":"prep","outcome":"ok"}`},
		{stuckTool, `{"step":"work","reason":"no shell to run the checks"}`},
	}}
	a := nestedAgent(t, g, m, nil, rules)
	ctx := context.Background()
	end, err := a.Prompt(ctx, mustInvocation(t, g))
	if end == nil {
		t.Fatal(err)
	}
	end, _, err = Drive(ctx, g, a, end, rules)
	if !errors.Is(err, ErrStepFailed) || !strings.Contains(err.Error(), "no shell to run the checks") {
		t.Fatalf("end %v; err %v", end.Reason, err)
	}
}
