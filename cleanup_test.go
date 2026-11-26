package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// undoTool undoes what a run did, saying for whom; fail makes it fail.
func undoTool(ran *[]string, fail func(string) bool) agenttool.Tool {
	return agenttool.New("undo", "Undo.", func(_ context.Context, a struct {
		Who string `json:"who"`
	}) (Result, error) {
		*ran = append(*ran, a.Who)
		if fail != nil && fail(a.Who) {
			return Result{}, errors.New("cannot undo")
		}
		return Result{Outcome: OK}, nil
	})
}

// tidyIn waits on a person, and undoes its work when abandoned.
func tidyIn() *Graph {
	return &Graph{Name: "tidyin", OnAbort: "undo", Nodes: []Node{
		{ID: "wait", Kind: Person, Prompt: "Go on?"},
		{ID: "undo", Kind: Run, Tool: "undo", With: map[string]Value{"who": Literal("inner")}},
	}}
}

// tidyOut calls tidyIn, and when abandoned undoes its own work and asks
// a person to confirm.
func tidyOut() *Graph {
	return &Graph{Name: "tidyout", OnAbort: "undo", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "tidyin"},
		{ID: "undo", Kind: Run, Tool: "undo", With: map[string]Value{"who": Literal("outer")}, Next: map[string]string{OK: "confirm"}},
		{ID: "confirm", Kind: Person, Prompt: "Undone. Anything else to clear up?", Unattended: OK},
	}}
}

// abandoned answers the pending question with a refusal, as a host does
// before it prompts, and prompts with an invocation of g.
func abandoned(t *testing.T, g *Graph, a *agentturn.Agent, end *agentturn.RunEnd) *agentturn.RunEnd {
	t.Helper()
	var outs openresponses.Items
	for _, p := range end.Pending {
		outs = append(outs, openresponses.NewFunctionCallOutput(p.Call.CallID, abandonedText))
	}
	inv, err := g.Invocation(nil)
	if err != nil {
		t.Fatal(err)
	}
	next, err := a.Prompt(context.Background(), append(outs, inv)...)
	if next == nil {
		t.Fatal(err)
	}
	return next
}

// endings are the endings written in t, in order.
func endings(t agentturn.Transcript) []Ending {
	var out []Ending
	for _, item := range t {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == endTool {
			var e Ending
			_ = json.Unmarshal([]byte(c.Arguments), &e)
			out = append(out, e)
		}
	}
	return out
}

func TestAnAbandonedStackCleansUpInnermostFirst(t *testing.T) {
	g := linked(t, tidyOut(), tidyIn())
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{undoTool(&ran, nil)})
	end := start(t, a)
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	end = abandoned(t, g, a, end)
	// The outer cleanup asks its person before the run ends.
	if q := question(t, end); q.Workflow != "tidyout" || q.Step != "confirm" {
		t.Fatalf("question = %+v", q)
	}
	if !slices.Equal(ran, []string{"inner", "outer"}) {
		t.Errorf("ran %v", ran)
	}
	end = reply(t, g, a, end, OK)
	// The new run starts once both have ended.
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	ends := endings(a.State().Transcript)
	if len(ends) != 2 || ends[0].Status != Abandoned || ends[0].Cleanup != "done" || ends[1].Status != Abandoned || ends[1].Cleanup != "done" {
		t.Fatalf("ends = %+v", ends)
	}
	runs, err := Runs(a.State().Transcript, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tidyout:abandoned:", " tidyin:abandoned:", "tidyout:running:", " tidyin:running:"}
	if got := tree(runs); !slices.Equal(got, want) {
		t.Errorf("tree =\n%s", strings.Join(got, "\n"))
	}
	if steps := runs[0].State.Steps; len(steps) != 2 || steps[1].Node != "confirm" {
		t.Errorf("steps = %+v", steps)
	}
}

func TestACleanupThatFailsStillEnds(t *testing.T) {
	g := linked(t, tidyOut(), tidyIn())
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{undoTool(&ran, func(who string) bool { return who == "inner" })}, Unattended())
	// Unattended, the inner question waits; it has no default.
	end := start(t, a)
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	end = abandoned(t, g, a, end)
	// The outer confirm takes its default, and nothing asks again.
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	ends := endings(a.State().Transcript)
	if len(ends) != 2 || !strings.HasPrefix(ends[0].Cleanup, "failed at undo: ") || !strings.Contains(ends[0].Cleanup, "cannot undo") || ends[1].Cleanup != "done" {
		t.Fatalf("ends = %+v", ends)
	}
}

func TestASecondAbandonEndsACleanup(t *testing.T) {
	g := linked(t, tidyOut(), tidyIn())
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{undoTool(&ran, nil)})
	end := abandoned(t, g, a, start(t, a))
	if q := question(t, end); q.Step != "confirm" {
		t.Fatalf("question = %+v", q)
	}
	end = abandoned(t, g, a, end)
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	ends := endings(a.State().Transcript)
	// The question the cleanup waited on was refused as the host abandoned
	// the run again, which cuts the cleanup short there: it did not fail.
	if len(ends) != 2 || ends[1].Cleanup != cutShort || ends[0].Cleanup != "done" || !slices.Equal(ran, []string{"inner", "outer"}) {
		t.Fatalf("ends = %+v, ran %v", ends, ran)
	}
}

func TestACleanupPathIsCheckedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    *Graph
		want string
	}{
		{"no such step", &Graph{Name: "c", OnAbort: "undoo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"}, {ID: "undo", Kind: Run, Tool: "t"}}},
			`on_abort names no step "undoo"; did you mean "undo"?`},
		{"shared with the workflow", &Graph{Name: "c", OnAbort: "undo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t", Next: map[string]string{OK: "undo"}}, {ID: "undo", Kind: Run, Tool: "t"}}},
			"on both the workflow's path and its cleanup"},
		{"a model step", &Graph{Name: "c", OnAbort: "undo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"}, {ID: "undo", Kind: Agent, Prompt: "x"}}},
			"no model steps"},
		{"a loop", &Graph{Name: "c", OnAbort: "undo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"},
			{ID: "undo", Kind: Run, Tool: "t", Next: map[string]string{OK: "again"}}, {ID: "again", Kind: Run, Tool: "t", Next: map[string]string{OK: "undo"}}}},
			"no loop"},
		{"a step that may not have run", &Graph{Name: "c", OnAbort: "undo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"},
			{ID: "undo", Kind: Run, Tool: "t", With: map[string]Value{"x": From("steps.a.outputs")}}}},
			`some path gets here without running "a"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.g.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// A cleanup reads what may not have run as optional, and its ends are
	// no outcomes of the workflow.
	g := &Graph{Name: "c", OnAbort: "undo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"},
		{ID: "undo", Kind: Run, Tool: "t", With: map[string]Value{"x": Optional("steps.a.outputs")}, Outcomes: []string{"undone"}}}}
	if err := g.Validate(); err != nil || !slices.Equal(g.Outcomes(), []string{OK}) {
		t.Errorf("err = %v, outcomes %v", err, g.Outcomes())
	}
}

func TestACleanupCutOffByAKillGoesOn(t *testing.T) {
	g := func() *Graph { return linked(t, tidyOut(), tidyIn()) }
	var ran []string
	childCfg := func() agentturn.Config {
		var mine []string
		cfg, err := Apply(g(), agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: []agenttool.Tool{
			undoTool(&mine, func(who string) bool {
				if who == "outer" {
					die()
				}
				return false
			})}})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	a := killedRecord(t, childCfg, func(store *jsonl.Store, id string) agentturn.Config {
		cfg, err := Apply(g(), agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: []agenttool.Tool{undoTool(&ran, nil)}}, recordOf(store, id))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}, func(t *testing.T, a *agentturn.Agent) {
		// Abandoned while it waits, it dies in the outer cleanup.
		abandoned(t, g(), a, start(t, a))
	})
	p := a.State().Pending
	if len(p) != 1 || p[0].Call.Name != "undo" {
		t.Fatalf("pending = %+v", p)
	}
	// The undo may have run: the person says it did not.
	end, err := a.Resume(context.Background(), agentturn.OutcomeUnknown(p[0].Call.CallID))
	if end == nil {
		t.Fatal(err)
	}
	// The cleanup step failed, so the cleanup is over and the run ends.
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	ends := endings(a.State().Transcript)
	if len(ends) != 2 || ends[0].Cleanup != "done" || !strings.HasPrefix(ends[1].Cleanup, "failed at undo") {
		t.Fatalf("ends = %+v", ends)
	}
}
