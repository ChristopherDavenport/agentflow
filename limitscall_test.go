package agentflow

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// planOf plans to touch what it is given, the next value each time it
// runs, with no schema to hold it to.
func planOf(values ...any) agenttool.Tool {
	return agenttool.New("plan", "Plan.", func(context.Context, struct{}) (Result, error) {
		v := values[0]
		if len(values) > 1 {
			values = values[1:]
		}
		return Result{Outcome: OK, Data: map[string]any{"files": v}}, nil
	})
}

// toPlanUntyped is toPlan with a plan of no schema, so only the record can
// say whether its files are paths. Each step may run twice, for a jump
// back to the plan.
func toPlanUntyped() *Graph {
	g := toPlan("")
	g.Nodes[0].Data, g.Nodes[0].MaxVisits, g.Nodes[1].MaxVisits = nil, 2, 2
	return g
}

// limitsIn is the flow_limits calls of items, and the outputs they had.
func limitsIn(t *testing.T, items agentturn.Transcript) (calls []*openresponses.FunctionCall, args []limitsArgs, outputs []string, after []openresponses.Item) {
	t.Helper()
	at := map[string]int{}
	for i, item := range items {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			if it.Name == limitsTool {
				var a limitsArgs
				if err := json.Unmarshal([]byte(it.Arguments), &a); err != nil {
					t.Fatal(err)
				}
				calls, args = append(calls, it), append(args, a)
				at[it.CallID] = len(calls) - 1
			}
		case *openresponses.FunctionCallOutput:
			if _, ok := at[it.CallID]; ok {
				outputs = append(outputs, it.Output.String())
				if i+1 < len(items) {
					after = append(after, items[i+1])
				}
			}
		}
	}
	return calls, args, outputs, after
}

// limitsRecorded holds what a run whose step's policy cannot be read must
// say: one flow_limits under its key for the step, failing it, then the
// step's retry question with the same words.
func limitsRecorded(t *testing.T, items agentturn.Transcript, end *agentturn.RunEnd, want string) Question {
	t.Helper()
	q := question(t, end)
	calls, args, outputs, after := limitsIn(t, items)
	if len(calls) != 1 || len(outputs) != 1 {
		t.Fatalf("%d flow_limits calls, %d outputs", len(calls), len(outputs))
	}
	if id := calls[0].CallID; id != reserved+q.Run+"_edit_v1-limits" {
		t.Errorf("call ID %s", id)
	}
	if a := args[0]; a.Run != q.Run || a.Step != "edit" || !strings.HasPrefix(a.Reason, `the policy of step "edit" of toplan: `) || !strings.Contains(a.Reason, want) {
		t.Errorf("args = %+v, want the reason %q", a, want)
	}
	if out := outputs[0]; !strings.Contains(out, `step "edit" cannot work under its policy: `+args[0].Reason) {
		t.Errorf("output = %q", out)
	}
	if c, ok := after[0].(*openresponses.FunctionCall); !ok || c.Name != askTool || c.CallID != end.Pending[0].Call.CallID {
		t.Errorf("after flow_limits: %+v, want the retry question", after[0])
	}
	if !q.Retry || q.Step != "edit" || !strings.Contains(q.Question, args[0].Reason) {
		t.Errorf("question = %+v", q)
	}
	return q
}

func TestAPolicyTheRecordCannotResolveIsRecordedByFlowLimits(t *testing.T) {
	for name, c := range map[string]struct {
		value any
		want  string
	}{
		"a number":           {42.0, "is 42, not a path or a list of them"},
		"an unclean path":    {[]any{"src/../a.go"}, `path "src/../a.go": want a clean path below the workspace's root`},
		"a path out of root": {[]any{"../etc/passwd"}, `path "../etc/passwd": want a clean path below the workspace's root`},
	} {
		t.Run(name, func(t *testing.T) {
			g := toPlanUntyped()
			if err := g.Validate(); err != nil {
				t.Fatalf("an untyped plan should load: %v", err)
			}
			var did []string
			m := &cast{script: []says{
				{"write", `{"path":"src/a.go"}`},
				{finishTool, `{"step":"edit","outcome":"ok"}`},
			}}
			a := nestedAgent(t, g, m, append(touching(&did), planOf(c.value, []any{"src/a.go"})), matchers)
			end := start(t, a)
			q := limitsRecorded(t, a.State().Transcript, end, c.want)
			// No model worked under the policy it could not read.
			if len(m.requests) != 0 {
				t.Fatalf("%d model requests", len(m.requests))
			}
			// The plan made again, the step resolves its policy and goes on.
			end, _, err := OverrideRun(t.Context(), g, a, Override{Run: q.Run, Action: OverrideJump, Step: "plan", Note: "plan real paths"})
			if err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("end = %+v, %v", end, err)
			}
			if want := []string{`write {"path":"src/a.go"}`}; !slices.Equal(did, want) {
				t.Errorf("did %v, want %v", did, want)
			}
			// A model may read that the program could not hold the step to
			// its policy: a program's call, with its result, as any is.
			req := m.requests[0]
			if !strings.Contains(req.Instructions, "write(src/a.go) (the plan's files)") {
				t.Errorf("instructions = %q", req.Instructions)
			}
			var seen bool
			for _, item := range req.Input {
				if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == limitsTool {
					seen = true
				}
			}
			if !seen {
				t.Error("the model's request leaves flow_limits out")
			}
			if calls, _, _, _ := limitsIn(t, a.State().Transcript); len(calls) != 1 {
				t.Errorf("%d flow_limits calls after the jump", len(calls))
			}
		})
	}
}

// A run killed with its step failed by flow_limits resumes from the store
// with that failure pending, and goes on once the plan is made again.
// A jump back to a step no loop reaches meets its bound of one, and the
// question says where that bound comes from.
func TestAJumpToAStepOnNoLoopNamesItsBound(t *testing.T) {
	g := toPlan("")
	g.Nodes[0].MaxVisits = 2
	var did []string
	m := &cast{script: []says{{finishTool, `{"step":"edit","outcome":"ok"}`}}}
	a := nestedAgent(t, g, m, append(touching(&did), planTool([]any{"../x"}, []any{"src/a.go"})), matchers)
	q := question(t, start(t, a))
	end, _, err := OverrideRun(t.Context(), g, a, Override{Run: q.Run, Action: OverrideJump, Step: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if q := question(t, end); !strings.Contains(q.Question, `step "edit" of workflow "toplan" has run 1 times, the default for a step on no loop`) {
		t.Errorf("question = %q", q.Question)
	}
}

func TestAFlowLimitsFailurePendingSurvivesAKill(t *testing.T) {
	const want = `path "../etc/passwd": want a clean path below the workspace's root`
	g := toPlanUntyped()
	childCfg := func() agentturn.Config {
		var did []string
		return must(Apply(g, agentturn.Config{Model: &cast{}, ModelName: "fake", Tools: append(touching(&did), planOf([]any{"../etc/passwd"}))}, matchers))
	}
	var did []string
	m := &cast{script: []says{
		{"write", `{"path":"src/a.go"}`},
		{finishTool, `{"step":"edit","outcome":"ok"}`},
	}}
	b := killedRecord(t, childCfg, func(*jsonl.Store, string) agentturn.Config {
		return must(Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: append(touching(&did), planOf([]any{"src/a.go"}))}, matchers))
	}, func(t *testing.T, a *agentturn.Agent) {
		end := start(t, a)
		limitsRecorded(t, a.State().Transcript, end, want)
		die()
	})
	ctx := context.Background()
	end, _, err := Drive(ctx, g, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := limitsRecorded(t, b.State().Transcript, end, want)
	if len(m.requests) != 0 {
		t.Fatalf("%d model requests on resume", len(m.requests))
	}
	end, _, err = OverrideRun(ctx, g, b, Override{Run: q.Run, Action: OverrideJump, Step: "plan"})
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if !slices.Equal(did, []string{`write {"path":"src/a.go"}`}) {
		t.Errorf("did %v", did)
	}
}
