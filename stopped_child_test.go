package agentflow

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
)

// A child whose agent stops for a reason of its own, here its turns
// running out, leaves its call held: the error says what stopped it.
func TestAChildThatStopsSaysWhy(t *testing.T) {
	g := &Graph{Name: "long", Nodes: []Node{
		{ID: "work", Kind: Agent, Prompt: "Work.", Context: ContextFresh},
	}}
	m := &cast{script: []says{{}, {}, {}}}
	short := Children(func(ctx context.Context, c Child) (context.Context, *agentturn.Agent, error) {
		cfg, err := c.Apply(agentturn.Config{Model: m, ModelName: "fake", MaxTurns: 1})
		if err != nil {
			return nil, nil, err
		}
		return ctx, agentturn.New(cfg), nil
	})
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake"}, short)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end, err := a.Prompt(context.Background(), mustInvocation(t, g))
	if end == nil {
		t.Fatal(err)
	}
	_, _, err = Drive(context.Background(), g, a, end, short)
	if err == nil || !strings.Contains(err.Error(), "stopped without ending its run: its agent's run ended stopped, max_turns") {
		t.Fatalf("err = %v", err)
	}
}
