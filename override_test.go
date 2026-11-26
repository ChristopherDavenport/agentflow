package agentflow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// overrides are the results of the program's flow_override calls.
func overrides(t agentturn.Transcript) []string {
	ids := map[string]bool{}
	var out []string
	for _, item := range t {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			ids[it.CallID] = it.Name == overrideTool
		case *openresponses.FunctionCallOutput:
			if ids[it.CallID] {
				out = append(out, it.Output.String())
			}
		}
	}
	return out
}

func TestAnOverrideRunsAStepAgainWithANote(t *testing.T) {
	g := release()
	var ran []string
	a := newAgent(t, g, &fakeModel{outcomes: []string{"findings"}}, &ran)
	end := start(t, a)
	q := question(t, end)
	end, _, err := OverrideRun(t.Context(), g, a, Override{Run: q.Run, Action: OverrideRetry, Note: "look at the tests too"})
	if err != nil {
		t.Fatal(err)
	}
	again := question(t, end)
	if again.Step != "triage" || again.Note != "look at the tests too" || !strings.HasSuffix(end.Pending[0].Call.CallID, "_triage_v1-a2") {
		t.Fatalf("question = %+v, id %s", again, end.Pending[0].Call.CallID)
	}
	if got := overrides(a.State().Transcript); len(got) != 1 || !strings.Contains(got[0], `"outcome":"ok"`) {
		t.Errorf("overrides = %q", got)
	}
}

func TestAJumpTakesItsNoteToTheStep(t *testing.T) {
	g := release()
	var ran []string
	m := &fakeModel{outcomes: []string{"findings", OK, "clean"}}
	a := newAgent(t, g, m, &ran)
	q := question(t, start(t, a))
	end, _, err := OverrideRun(t.Context(), g, a, Override{Run: q.Run, Action: OverrideJump, Step: "fix", Note: "the null check only"})
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	opening := ""
	for _, item := range m.requests[1].Input {
		if msg, ok := item.(*openresponses.Message); ok && strings.HasPrefix(msg.Text(), beginning("fix")) {
			opening = msg.Text()
		}
	}
	if !strings.Contains(opening, `Note from the operator: "the null check only".`) || !strings.HasSuffix(opening, visitTag(State{Instance: q.Run, Visit: 1, Attempt: 1})) {
		t.Errorf("opening = %q", opening)
	}
	// The jump skipped triage's answer: it is not on the path.
	if got := path(t, g, a); !slices.Equal(got, []string{"gather:ok", "review:findings", "fix:ok", "gather:ok", "review:clean", "report:ok"}) {
		t.Errorf("path = %v", got)
	}
}

func TestAJumpIsHeldToItsBounds(t *testing.T) {
	g := &Graph{Name: "once", Nodes: []Node{
		{ID: "ask", Kind: Person, Prompt: "Go?", Next: map[string]string{OK: "done"}},
		{ID: "done", Kind: Person, Prompt: "Done?"},
	}}
	var ran []string
	a := newAgent(t, g, &fakeModel{}, &ran)
	end := reply(t, g, a, start(t, a), OK)
	q := question(t, end)
	end, _, err := OverrideRun(t.Context(), g, a, Override{Run: q.Run, Action: OverrideJump, Step: "ask"})
	if err != nil {
		t.Fatal(err)
	}
	if limit := question(t, end); limit.Limit != "visits" || limit.Step != "ask" {
		t.Fatalf("question = %+v", limit)
	}
}

func TestARefusedOverrideIsRecorded(t *testing.T) {
	g := release()
	var ran []string
	a := newAgent(t, g, &fakeModel{outcomes: []string{"findings"}}, &ran)
	q := question(t, start(t, a))
	end, _, err := OverrideRun(t.Context(), g, a, Override{Run: q.Run, Action: OverrideJump, Step: "fx"})
	if err != nil {
		t.Fatal(err)
	}
	if got := overrides(a.State().Transcript); len(got) != 1 || !strings.Contains(got[0], `has no step "fx"; did you mean "fix"?`) {
		t.Errorf("overrides = %q", got)
	}
	// The question set aside for the override is put again.
	if again := question(t, end); again.Retry || again.Step != "triage" || !strings.HasSuffix(end.Pending[0].Call.CallID, "-a2") {
		t.Errorf("question = %+v, id %s", again, end.Pending[0].Call.CallID)
	}
	if _, _, err := OverrideRun(t.Context(), g, a, Override{Run: "0123456789", Action: OverrideRetry}); err == nil || !strings.Contains(err.Error(), "not under way") {
		t.Errorf("err = %v", err)
	}
}

func TestAnOverrideAbandonsARunAndWhatItCalled(t *testing.T) {
	g := linked(t, tidyOut(), tidyIn())
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{undoTool(&ran, nil)})
	inner := question(t, start(t, a))
	outer := inner.Path[0].Run
	// Abandoning the inner run leaves its caller, whose step has failed.
	end, _, err := OverrideRun(t.Context(), g, a, Override{Run: inner.Run, Action: OverrideAbandon})
	if err != nil {
		t.Fatal(err)
	}
	if q := question(t, end); !q.Retry || q.Run != outer || !strings.Contains(q.Question, "abandoned") {
		t.Fatalf("question = %+v", q)
	}
	if !slices.Equal(ran, []string{"inner"}) {
		t.Errorf("ran %v", ran)
	}
	// Abandoning the outer one runs its cleanup, and nothing starts.
	end, _, err = OverrideRun(t.Context(), g, a, Override{Run: outer, Action: OverrideAbandon})
	if err != nil {
		t.Fatal(err)
	}
	end = reply(t, g, a, end, OK)
	if end.Reason != agentturn.ReasonDone || !slices.Equal(ran, []string{"inner", "outer"}) {
		t.Fatalf("end = %+v, ran %v", end, ran)
	}
	runs, err := Runs(a.State().Transcript, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(runs); !slices.Equal(got, []string{"tidyout:abandoned:", " tidyin:abandoned:"}) {
		t.Errorf("tree = %v", got)
	}
}

func TestAnOverrideReachesARunInAChild(t *testing.T) {
	g := linked(t, outer(), thinker(), asker())
	var ran []string
	m := &fakeModel{outcomes: []string{OK}}
	kids := newKin(m, "fake")
	a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)}, kids.option())
	end, _, err := Drive(t.Context(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, end)
	if q.Workflow != "asker" {
		t.Fatalf("question = %+v", q)
	}
	end, _, err = OverrideRun(t.Context(), g, a, Override{Run: q.Run, Action: OverrideRetry, Note: "ask again"})
	if err != nil {
		t.Fatal(err)
	}
	if again := question(t, end); again.Run != q.Run || again.Note != "ask again" {
		t.Fatalf("question = %+v", again)
	}
}

func TestAnOverridePendingAtAKillIsApplied(t *testing.T) {
	g := release()
	var ran []string
	childCfg := func() agentturn.Config {
		cfg, err := kinApply(g, agentturn.Config{Model: &fakeModel{outcomes: []string{"findings"}}, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}})
		if err != nil {
			t.Fatal(err)
		}
		hook := cfg.BeforeToolCall
		cfg.BeforeToolCall = func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
			if info.Call.Name == overrideTool {
				die()
			}
			return hook(ctx, info)
		}
		return cfg
	}
	m := &fakeModel{outcomes: []string{OK, "clean"}}
	a := killedRecord(t, childCfg, func(store *jsonl.Store, id string) agentturn.Config {
		cfg, err := kinApply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}}, recordOf(store, id))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}, func(t *testing.T, a *agentturn.Agent) {
		q := question(t, start(t, a))
		_, _, _ = OverrideRun(context.Background(), g, a, Override{Run: q.Run, Action: OverrideJump, Step: "fix"})
	})
	// The request is on the record and its call never ran: it runs, and
	// the jump is applied.
	p := a.State().Pending
	if len(p) != 1 || p[0].Call.Name != overrideTool {
		t.Fatalf("pending = %+v", p)
	}
	end, err := a.Resume(t.Context(), agentturn.Approve(p[0].Call.CallID))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if got := path(t, g, a); !slices.Equal(got, []string{"gather:ok", "review:findings", "fix:ok", "gather:ok", "review:clean", "report:ok"}) {
		t.Errorf("path = %v", got)
	}
}

func TestOverridesAreCheckedAsAsked(t *testing.T) {
	for _, tc := range []struct {
		o    Override
		want string
	}{
		{Override{Run: "nope", Action: OverrideRetry}, "not a run's key"},
		{Override{Run: "0123456789", Action: "rety"}, `did you mean "retry"?`},
		{Override{Run: "0123456789", Action: OverrideJump}, "names the step"},
		{Override{Run: "0123456789", Action: OverrideRetry, Step: "x"}, "only a jump"},
	} {
		if _, err := OverrideRequest(tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: err = %v, want %q", tc.o, err, tc.want)
		}
	}
}

func TestARetryAbandonsItsChildForTheOverride(t *testing.T) {
	g := linked(t, &Graph{Name: "careful", Nodes: []Node{{ID: "call", Kind: Run, Flow: "tidyin", Context: ContextFresh}}}, tidyIn())
	var ran []string
	m := &fakeModel{}
	kids := newKin(m, "fake", undoTool(&ran, nil))
	a := nestedAgent(t, g, m, nil, kids.option())
	end, _, err := Drive(t.Context(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, end)
	if q.Workflow != "tidyin" || len(q.Path) != 2 {
		t.Fatalf("question = %+v", q)
	}
	end, _, err = OverrideRun(t.Context(), g, a, Override{Run: q.Path[0].Run, Action: OverrideRetry})
	if err != nil {
		t.Fatal(err)
	}
	// The step runs again, in a new child, whose question is asked.
	again := question(t, end)
	if again.Workflow != "tidyin" || again.Run == q.Run {
		t.Fatalf("question = %+v", again)
	}
	// The child set aside cleaned up, and its end says why it ended.
	ends := endings(kids.agents[q.Run].State().Transcript)
	if len(ends) != 1 || ends[0].Status != Abandoned || ends[0].Reason != "overridden: retry" || ends[0].Cleanup != "done" || !slices.Equal(ran, []string{"inner"}) {
		t.Errorf("ends = %+v, ran %v", ends, ran)
	}
}

func TestARetryKeepsAKeptChildsSession(t *testing.T) {
	g := &Graph{Name: "kept", Nodes: []Node{{ID: "draft", Kind: Agent, Prompt: "Draft it.", Context: ContextFresh, Child: "writer"}}}
	// The first visit only talks, and stalls; the retry finishes.
	m := &fakeModel{outcomes: []string{"", "", OK}}
	kids := newKin(m, "fake")
	a := nestedAgent(t, g, m, nil, kids.option())
	_, st, err := Drive(t.Context(), g, a, start(t, a))
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("err = %v", err)
	}
	end, st, err := OverrideRun(t.Context(), g, a, Override{Run: st.Path[0].Run, Action: OverrideRetry})
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("end = %+v, %v", end, err)
	}
	// One child served both attempts: the first's run ended for the
	// override, the second's in the same session, whose model read the
	// first visit.
	if len(kids.agents) != 1 || len(kids.opened) < 2 || kids.opened[0].Spawn != kids.opened[len(kids.opened)-1].Spawn {
		t.Fatalf("opened %+v", kids.opened)
	}
	var child *agentturn.Agent
	for _, c := range kids.agents {
		child = c
	}
	ends := endings(child.State().Transcript)
	if len(ends) != 2 || ends[0].Status != Abandoned || ends[0].Reason != "overridden: retry" || ends[1].Status != Done {
		t.Fatalf("ends = %+v", ends)
	}
	last := m.requests[len(m.requests)-1]
	if n := strings.Count(strings.Join(texts(last.Input), "\n"), beginPrefix); n != 2 {
		t.Errorf("the retry read %d openings, want its own and the first visit's", n)
	}
}

func TestAfterAnOperatorAbandonsTheConversationIsTheModels(t *testing.T) {
	g := release()
	var ran []string
	m := &fakeModel{outcomes: []string{"findings", ""}}
	a := newAgent(t, g, m, &ran)
	q := question(t, start(t, a))
	end, st, err := OverrideRun(t.Context(), g, a, Override{Run: q.Run, Action: OverrideAbandon})
	if err != nil || end.Reason != agentturn.ReasonDone || st.Started {
		t.Fatalf("end = %+v, %v, %+v", end, err, st)
	}
	if ends := endings(a.State().Transcript); len(ends) != 1 || ends[0].Status != Abandoned || ends[0].Reason != "overridden: abandon" {
		t.Fatalf("ends = %+v", ends)
	}
	// The program said so, and the person's next words go to the model,
	// with its tools and none of the workflow's.
	said := texts(a.State().Transcript)
	if !strings.Contains(said[len(said)-1], "an operator abandoned run "+q.Run) {
		t.Errorf("last said %q", said[len(said)-1])
	}
	end, err = a.Prompt(t.Context(), openresponses.UserText("what now?"))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	last := m.requests[len(m.requests)-1]
	if last.Model != "fake" || len(m.requests) != 2 {
		t.Fatalf("asked %s, %d requests", last.Model, len(m.requests))
	}
	for _, tool := range last.Tools {
		if fn, ok := tool.(*openresponses.FunctionTool); ok && (strings.HasPrefix(fn.Name, reserved) || fn.Name == ScriptTool) {
			t.Errorf("offered %s", fn.Name)
		}
	}
	// An invocation starts a new run.
	inv, err := g.Invocation(nil)
	if err != nil {
		t.Fatal(err)
	}
	m.outcomes = []string{"clean"}
	if end, err := a.Prompt(t.Context(), inv); err != nil || end.Reason != agentturn.ReasonDone || !slices.Contains(ran, "report") {
		t.Errorf("end = %+v, %v, ran %v", end, err, ran)
	}
}
