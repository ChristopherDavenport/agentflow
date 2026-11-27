package agentflow

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// A grant of steps is the run's it was given to: the next run in the
// session starts with its own bound.
func TestAStepsGrantEndsWithItsRun(t *testing.T) {
	chain := &Graph{Name: "chain", MaxSteps: 2, Nodes: []Node{
		{ID: "a", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "b"}},
		{ID: "b", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "c"}},
		{ID: "c", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "d"}},
		{ID: "d", Kind: Run, Tool: "script", Args: named},
	}}
	var ran []string
	grants := 0
	rules := Reply(func(_ context.Context, q Question) (Result, bool) {
		grants++
		return Result{Outcome: grantOutcome, Data: map[string]any{"amount": 5}}, q.Limit == "steps" && grants == 1
	})
	a := nestedAgent(t, chain, &fakeModel{}, []agenttool.Tool{scriptTool(&ran)}, rules)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	inv, err := chain.Invocation(nil)
	if err != nil {
		t.Fatal(err)
	}
	end, err := a.Prompt(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if q := question(t, end); q.Limit != "steps" || q.Step != "c" {
		t.Fatalf("question = %+v", q)
	}
	st, err := chain.Replay(a.State().Transcript)
	if err != nil || st.Entries != 3 || st.Granted != 0 {
		t.Errorf("entries %d, granted %d, %v", st.Entries, st.Granted, err)
	}
}

// A fresh model step under a policy from the record works in its child,
// held to the rules its caller's record resolves.
func TestAFreshStepWorksUnderAPolicyFromTheRecord(t *testing.T) {
	var did []string
	m := &cast{script: []says{
		{"write", `{"path":"src/c.go"}`},
		{"write", `{"path":"src/a.go"}`},
		{finishTool, `{"step":"edit","outcome":"ok"}`},
	}}
	tools := append(touching(&did), planTool([]any{"src/a.go", "src/b.go"}))
	kids := newKin(m, "fake", tools...)
	g := toPlan(ContextFresh)
	a := nestedAgent(t, g, m, tools, matchers, kids.option())
	end, _, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, err = %v", end, err)
	}
	if want := []string{`write {"path":"src/a.go"}`}; !slices.Equal(did, want) {
		t.Errorf("did %v, want %v", did, want)
	}
}

// An override asked for and never applied does not outlive the runs it
// was asked of: an invocation after it starts its run.
func TestAnInvocationDropsAnOverrideNeverApplied(t *testing.T) {
	g := release()
	var ran []string
	cfg, err := kinApply(g, agentturn.Config{Model: &fakeModel{outcomes: []string{"findings", "findings"}}, ModelName: "fake", MaxTurns: 40,
		Tools: []agenttool.Tool{scriptTool(&ran)}})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end := start(t, a)
	q := question(t, end)
	req, err := OverrideRequest(Override{Run: q.Run, Action: OverrideRetry})
	if err != nil {
		t.Fatal(err)
	}
	inv, err := g.Invocation(nil)
	if err != nil {
		t.Fatal(err)
	}
	end, err = a.Prompt(t.Context(), openresponses.NewFunctionCallOutput(end.Pending[0].Call.CallID, overriddenText), req, inv)
	if err != nil {
		t.Fatal(err)
	}
	if again := question(t, end); again.Run == q.Run || again.Step != "triage" {
		t.Fatalf("question = %+v", again)
	}
	if got := overrides(a.State().Transcript); len(got) != 0 {
		t.Errorf("overrides = %q", got)
	}
}

// With nobody there, a cleanup's question that has no default fails its
// step, and the abandon goes on.
func TestAnUnattendedCleanupQuestionDoesNotHoldTheAbandon(t *testing.T) {
	g := &Graph{Name: "asking", OnAbort: "confirm", Nodes: []Node{
		{ID: "wait", Kind: Person, Prompt: "Go on?"},
		{ID: "confirm", Kind: Person, Prompt: "Clear up?"},
	}}
	a := nestedAgent(t, g, &fakeModel{}, nil, Unattended())
	end := start(t, a)
	first := question(t, end)
	end = abandoned(t, g, a, end)
	if q := question(t, end); q.Step != "wait" || q.Run == first.Run {
		t.Fatalf("question = %+v", q)
	}
	ends := endings(a.State().Transcript)
	if len(ends) != 1 || ends[0].Status != Abandoned || !strings.HasPrefix(ends[0].Cleanup, "failed at confirm: ") {
		t.Fatalf("ends = %+v", ends)
	}
}

// A child whose run ended within its call's timeout is read as it ended,
// however late its caller comes back for it.
func TestAChildThatEndedInTimeIsNotExpired(t *testing.T) {
	g := linked(t, impatient("waitsOn", 500*time.Millisecond, ContextFresh, true), waitsOn())
	var ran []string
	m := &fakeModel{}
	kids := newKin(m, "fake", scriptTool(&ran))
	a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)}, kids.option())
	end, _, err := Drive(t.Context(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if q := question(t, end); q.Step != "ask" || len(kids.agents) != 1 {
		t.Fatalf("question = %+v, %d children", q, len(kids.agents))
	}
	// The child's run ends in its own session, and the process stops
	// before the caller reads it.
	ans, err := g.Answer(end.Pending[0], Result{Outcome: OK})
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range kids.agents {
		if done, err := child.Resume(t.Context(), ans); err != nil || done.Reason != agentturn.ReasonDone {
			t.Fatalf("child end = %+v, %v", done, err)
		}
	}
	time.Sleep(600 * time.Millisecond)
	end, st, err := Drive(t.Context(), g, a, &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: a.State().Pending})
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(st.Steps) != 1 || st.Steps[0].Result.Outcome != OK || len(ran) != 0 {
		t.Errorf("steps = %+v, ran %v", st.Steps, ran)
	}
}

// An operator's jump to a step a person paces is the operator's visit:
// it counts toward the tree's steps as any other.
func TestAJumpIsNotAPacedVisit(t *testing.T) {
	g := &Graph{Name: "paced", Nodes: []Node{
		{ID: "work", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "check"}, PersonPaced: true},
		{ID: "check", Kind: Person, Prompt: "Good?", Outcomes: []string{"again", "done"}, Next: map[string]string{"again": "work", "done": "ship"}, MaxVisits: 9},
		{ID: "ship", Kind: Person, Prompt: "Ship?"},
	}}
	var ran []string
	a := newAgent(t, g, &fakeModel{}, &ran)
	end := reply(t, g, a, start(t, a), "done")
	q := question(t, end)
	end, st, err := OverrideRun(t.Context(), g, a, Override{Run: q.Run, Action: OverrideJump, Step: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if again := question(t, end); again.Step != "check" {
		t.Fatalf("question = %+v", again)
	}
	// work, check, ship, and the jump's work and check.
	if st.Entries != 5 {
		t.Errorf("entries = %d, want 5", st.Entries)
	}
}

// abandonedBelow drives a call into a child as far as the child's
// question, and then abandons the child's run in its own session only, as
// an abandon cut off before the caller's session heard of it leaves it.
func abandonedBelow(t *testing.T) (*Graph, *agentturn.Agent, *agentturn.Agent) {
	t.Helper()
	inner := &Graph{Name: "inner", Nodes: []Node{
		{ID: "a", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "ask"}},
		{ID: "ask", Kind: Person, Prompt: "Go on?"},
	}}
	g := linked(t, &Graph{Name: "outer", Nodes: []Node{{ID: "call", Kind: Run, Flow: "inner", Context: ContextFresh}}}, inner)
	var ran []string
	m := &fakeModel{}
	kids := newKin(m, "fake", scriptTool(&ran))
	a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)}, kids.option())
	end, _, err := Drive(t.Context(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if q := question(t, end); q.Step != "ask" || len(kids.agents) != 1 {
		t.Fatalf("question = %+v, %d children", q, len(kids.agents))
	}
	var child *agentturn.Agent
	for _, c := range kids.agents {
		child = c
	}
	refusal := openresponses.NewFunctionCallOutput(end.Pending[0].Call.CallID, abandonedText)
	if done, err := child.Prompt(t.Context(), refusal, abandonMessage("")); err != nil || done.Reason != agentturn.ReasonDone {
		t.Fatalf("child end = %+v, %v", done, err)
	}
	return g, a, child
}

// A child abandoned before its caller's session heard of it still counts
// what it spent, when the caller's runs are abandoned again.
func TestAnAbandonCutOffStillCountsItsChild(t *testing.T) {
	g, a, _ := abandonedBelow(t)
	if _, _, err := Invoke(t.Context(), g, a, nil); err != nil {
		t.Fatal(err)
	}
	runs, err := Runs(a.State().Transcript, nil)
	if err != nil || len(runs) != 2 {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	// The call, and the child's two steps.
	if got := runs[0].State.Entries; got != 3 {
		t.Errorf("entries = %d, want 3", got)
	}
}

// Nor is its run started a second time, when the caller is driven on
// instead: its call is let go as the child's run ended.
func TestAChildAbandonedBelowIsNotStartedAgain(t *testing.T) {
	g, a, child := abandonedBelow(t)
	end, _, err := Drive(t.Context(), g, a, &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: a.State().Pending})
	if err != nil {
		t.Fatal(err)
	}
	if q := question(t, end); !q.Retry || q.Step != "call" || !strings.Contains(q.Question, Abandoned) {
		t.Fatalf("question = %+v", q)
	}
	starts := 0
	for _, item := range child.State().Transcript {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == startTool {
			starts++
		}
	}
	if starts != 1 {
		t.Errorf("the child's session holds %d starts", starts)
	}
}

// What the program says on its own is recorded as the workflow's, not as
// the person's who drove it: a call's timeout said to the runs it waits
// on, and an abandon said to a child.
func TestTheProgramsPromptsAreRecordedAsTheWorkflows(t *testing.T) {
	for _, c := range []struct{ context, said string }{{"", timedOutText}, {ContextFresh, abandonText}} {
		t.Run("context "+c.context, func(t *testing.T) {
			ctx := t.Context()
			store, err := jsonl.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			rec, _, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
			if err != nil {
				t.Fatal(err)
			}
			g := linked(t, impatient("waitsOn", 500*time.Millisecond, c.context, true), waitsOn())
			var ran []string
			m := &fakeModel{}
			kids := &recordedKin{store: store, rec: rec, model: m, name: "fake", tools: []agenttool.Tool{scriptTool(&ran)}}
			a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)}, kids.option())
			rec.Attach(a)
			typed := agentturn.ContextWithTrigger(ctx, agentturn.Trigger{Kind: agentturn.TriggerHuman})
			end, err := a.Prompt(typed, openresponses.UserText("go"))
			if err != nil {
				t.Fatal(err)
			}
			if end, _, err = Drive(typed, g, a, end); err != nil {
				t.Fatal(err)
			}
			question(t, end)
			time.Sleep(600 * time.Millisecond)
			waiting := &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: a.State().Pending}
			if end, _, err = Drive(typed, g, a, waiting); err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("end = %+v, %v", end, err)
			}
			found := 0
			for sum, err := range store.List(ctx, agentsession.ListFilter{}) {
				if err != nil {
					t.Fatal(err)
				}
				s, err := store.Open(ctx, sum.Header.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range s.Path(s.Leaf()) {
					line, err := json.Marshal(e)
					if err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(line), c.said) || !strings.Contains(string(line), `"type":"item"`) || strings.Contains(string(line), `"type":"function_call"`) {
						continue
					}
					found++
					if !strings.Contains(string(line), `"kind":"`+TriggerKind+`"`) {
						t.Errorf("recorded as another's: %s", line)
					}
				}
			}
			if found == 0 {
				t.Errorf("no entry holds %q", c.said)
			}
		})
	}
}

// finishThenWrite finishes its step and writes a document in one
// response, the finish first, and says nothing after.
type finishThenWrite struct{ asked int }

func (m *finishThenWrite) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.asked++
	if m.asked > 1 {
		return say(sink, req.Model, "done")
	}
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	for _, s := range []says{{finishTool, `{"step":"draft","outcome":"ok"}`}, write("draft", "plan", "too late")} {
		call, err := e.FunctionCall("", s.tool)
		if err != nil {
			return err
		}
		if err := call.Arguments(withRun(req, s.tool, s.args)); err != nil {
			return err
		}
		if err := call.Close(); err != nil {
			return err
		}
	}
	return e.Complete()
}

// A document written after the step's finish, in the same response, is
// refused where the model reads it, not answered as written and dropped.
func TestAWriteAfterTheFinishIsRefused(t *testing.T) {
	g := planner()
	var told []string
	cfg, err := Apply(g, agentturn.Config{Model: &finishThenWrite{}, ModelName: "fake", Tools: []agenttool.Tool{noter(&told)}})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	if !slices.Equal(told, []string{"# Plan\n"}) {
		t.Errorf("told %q", told)
	}
	calls := map[string]string{}
	for _, item := range a.State().Transcript {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			calls[it.CallID] = it.Name
		case *openresponses.FunctionCallOutput:
			if calls[it.CallID] == docWriteTool && !strings.Contains(it.Output.String(), "after the step's finish") {
				t.Errorf("the write was answered %q", it.Output.String())
			}
		}
	}
}

// An agent is driven with the options it was applied with by the graph it
// was applied with, or by the same workflow loaded again; another
// workflow is refused, not driven with no options at all.
func TestDriveKnowsItsWorkflowByDigest(t *testing.T) {
	build := func() *Graph {
		inner := &Graph{Name: "inner", Nodes: []Node{{ID: "a", Kind: Run, Tool: "script", Args: named}}}
		return linked(t, &Graph{Name: "outer", Nodes: []Node{{ID: "call", Kind: Run, Flow: "inner", Context: ContextFresh}}}, inner)
	}
	var ran []string
	m := &fakeModel{}
	kids := newKin(m, "fake", scriptTool(&ran))
	a := nestedAgent(t, build(), m, []agenttool.Tool{scriptTool(&ran)}, kids.option())
	end, _, err := Drive(t.Context(), build(), a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone || len(ran) != 1 {
		t.Fatalf("end = %+v, %v, ran %v", end, err, ran)
	}
	if _, _, err := Drive(t.Context(), release(), a, end); err == nil || !strings.Contains(err.Error(), "follows workflow outer") {
		t.Errorf("err = %v", err)
	}
}

// A question the host's rules answer costs no judge's look.
func TestAQuestionTheRulesAnswerIsNotJudged(t *testing.T) {
	m := &cast{script: []says{found}}
	a, _, st := play(t, judged(), m, Delegate(Decide),
		Reply(func(context.Context, Question) (Result, bool) { return Result{Outcome: "report"}, true }))
	if got := stepOf(st, "triage"); got.Result.By != "rules" {
		t.Fatalf("triage = %+v", got)
	}
	for _, item := range a.State().Transcript {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == judgeTool {
			t.Errorf("a judge looked at a question the rules answer: %s", c.CallID)
		}
	}
}

// Rules that answer when the judge would look and not when the question
// is put leave it to the person, unjudged: the step is not stuck.
func TestRulesThatChangeTheirMindLeaveTheQuestionToThePerson(t *testing.T) {
	asked := 0
	m := &cast{script: []says{found}}
	_, end, st := play(t, judged(), m, Delegate(Decide),
		Reply(func(context.Context, Question) (Result, bool) {
			asked++
			return Result{Outcome: "report"}, asked == 1
		}))
	if end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	if q := question(t, end); q.Step != "triage" || q.Advice != nil {
		t.Errorf("question = %+v", q)
	}
	if st.Node == nil || st.Node.ID != "triage" {
		t.Errorf("at %+v", st.Node)
	}
}

// A file's text is its own, whatever it begins with: a read that failed
// comes back as an error, not as text.
func TestAFileThatBeginsWithErrorIsGiven(t *testing.T) {
	g := &Graph{Name: "given", Nodes: []Node{{ID: "work", Kind: Agent, Prompt: "Work.", Files: []Value{Literal("log.txt")}}}}
	read := agenttool.New("read", "Read a file.", func(context.Context, struct {
		Path string `json:"path"`
	}) (string, error) {
		return "Error: disk full\nat 10:04", nil
	})
	a := nestedAgent(t, g, &fakeModel{outcomes: []string{OK}}, []agenttool.Tool{read})
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	if got := filesGiven(a.State().Transcript); !slices.Equal(got, []string{"log.txt=Error: disk full\nat 10:04"}) {
		t.Errorf("files = %q", got)
	}
}

// A path a rule takes from the record names one file: a plan that names
// a pattern does not widen the rule to it.
func TestAPathFromTheRecordIsNoPattern(t *testing.T) {
	for _, path := range []string{"**", "src/*", "!src/a.go"} {
		t.Run(path, func(t *testing.T) {
			var did []string
			m := &cast{script: []says{{"write", `{"path":"anywhere/else.go"}`}, {finishTool, `{"step":"edit","outcome":"ok"}`}}}
			tools := append(touching(&did), planTool([]any{path}))
			a := nestedAgent(t, toPlan(""), m, tools, matchers)
			q := question(t, start(t, a))
			if !q.Retry || !strings.Contains(q.Question, "want one path, not a pattern") {
				t.Fatalf("question = %+v", q)
			}
			if len(did) != 0 || len(m.requests) != 0 {
				t.Errorf("did %v in %d requests", did, len(m.requests))
			}
		})
	}
}

// A tool a step offers does not carry the model past the step's policy:
// the calls it makes are held to it too.
func TestAToolsOwnCallsAreHeldToTheStepsPolicy(t *testing.T) {
	g := &Graph{Name: "held", Nodes: []Node{{ID: "edit", Kind: Agent, Prompt: "Edit.", Tools: []string{"patch", "write"},
		Policy: &Policy{Deny: ruleList("write(locked/*)")}}}}
	var did []string
	patch := agenttool.New("patch", "Patch a file.", func(ctx context.Context, a struct {
		Path string `json:"path"`
	}) (string, error) {
		args, _ := json.Marshal(a)
		res, err := agentturn.Invoke(ctx, "write", args)
		if err != nil {
			return "", err
		}
		return res.Output.String(), nil
	})
	m := &cast{script: []says{
		{"patch", `{"path":"locked/a"}`},
		{"patch", `{"path":"free/a"}`},
		{finishTool, `{"step":"edit","outcome":"ok"}`},
	}}
	a := nestedAgent(t, g, m, append(touching(&did), patch), matchers)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	if want := []string{`write {"path":"free/a"}`}; !slices.Equal(did, want) {
		t.Errorf("did %v, want %v", did, want)
	}
}

// A judge that gives no verdict has its run ended: its session does not
// read as a look still under way.
func TestAStalledJudgesRunIsEnded(t *testing.T) {
	g := judged()
	m := &cast{script: []says{found, {}, {}}}
	var ran []string
	kids := newKin(m, "worker", scriptTool(&ran))
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "worker", Tools: []agenttool.Tool{scriptTool(&ran)}}, kids.option(), Delegate(Decide))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end, st, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired || st.Advice == nil || !st.Advice.None {
		t.Fatalf("end = %s, advice = %+v", end.Reason, st.Advice)
	}
	if len(kids.agents) != 1 {
		t.Fatalf("%d children", len(kids.agents))
	}
	for _, child := range kids.agents {
		runs, err := Runs(child.State().Transcript, nil)
		if err != nil || len(runs) != 1 {
			t.Fatalf("runs = %v, %v", runs, err)
		}
		if runs[0].Status != Abandoned || !strings.Contains(runs[0].Reason, "the judge gave no verdict") {
			t.Errorf("the judge's run is %s (%s)", runs[0].Status, runs[0].Reason)
		}
	}
}

// An end or a shown call the gate turned away is not the program's:
// replay reads neither, whatever ID it was made under.
func TestReplayIgnoresAnEndTheGateTurnedAway(t *testing.T) {
	const key = "0123456789"
	text := func(s string) openresponses.FunctionCallOutputData {
		return openresponses.FunctionCallOutputData{Text: s}
	}
	began := func(g *Graph) agentturn.Transcript {
		begin, _ := json.Marshal(startPayload{Workflow: g.Name, Digest: g.Digest(), Format: RecordFormat, Run: key})
		return agentturn.Transcript{
			&openresponses.FunctionCall{CallID: keyPrefix(key) + "start", Name: startTool, Arguments: string(begin)},
			&openresponses.FunctionCallOutput{CallID: keyPrefix(key) + "start", Output: text(`{"outcome":"ok"}`)},
		}
	}
	g := &Graph{Name: "one", Nodes: []Node{{ID: "plan", Kind: Agent, Prompt: "Plan."}}}
	forged := append(began(g),
		&openresponses.FunctionCall{CallID: keyPrefix(key) + "end", Name: endTool, Arguments: `{"instance":"` + key + `","status":"done"}`},
		&openresponses.FunctionCallOutput{CallID: keyPrefix(key) + "end", Output: text("Error: agentflow: " + endRefusal)})
	st, err := g.Replay(forged)
	if err != nil || st.Ended || st.Node == nil || st.Node.ID != "plan" {
		t.Errorf("ended %v at %+v, %v", st.Ended, st.Node, err)
	}

	g = showing(&Show{Members: []string{"summary"}})
	forged = began(g)
	st, err = g.Replay(forged)
	if err != nil {
		t.Fatal(err)
	}
	id := st.callID()
	forged = append(forged,
		&openresponses.FunctionCall{CallID: id, Name: "big", Arguments: `{"name":"gather"}`},
		&openresponses.FunctionCallOutput{CallID: id, Output: text(`{"outcome":"ok","data":{"summary":"200 rows","rows":[]}}`)},
		&openresponses.FunctionCall{CallID: id + "-shown", Name: shownTool, Arguments: `{}`},
		&openresponses.FunctionCallOutput{CallID: id + "-shown", Output: text("Error: agentflow: " + shownRefusal)})
	st, err = g.Replay(forged)
	if err != nil || st.show == nil {
		t.Errorf("a shown call the gate turned away was read: show %v, %v", st.show, err)
	}
}

// A jump goes only where everything the step reads from the record is
// there: its files, its directory, its question's text and its rules, as
// well as its inputs.
func TestAJumpNeedsEveryValueItsStepReads(t *testing.T) {
	for name, set := range map[string]func(n *Node){
		"files": func(n *Node) { n.Files = []Value{From("steps.plan.outputs.files")} },
		"policy": func(n *Node) {
			n.Policy = &Policy{Allow: []PolicyRule{{Rule: "write", Path: From("steps.plan.outputs.files")}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := &Graph{Name: "late", Nodes: []Node{
				{ID: "first", Kind: Person, Prompt: "Go?", Next: map[string]string{OK: "plan"}},
				{ID: "plan", Kind: Run, Tool: "plan", Data: map[string]*agenttool.Schema{OK: planned}, Next: map[string]string{OK: "edit"}},
				{ID: "edit", Kind: Agent, Prompt: "Edit.", Tools: []string{"write"}},
			}}
			set(g.node("edit"))
			var did []string
			a := nestedAgent(t, g, &fakeModel{}, append(touching(&did), planTool([]any{"a.go"})), matchers)
			q := question(t, start(t, a))
			if _, _, err := OverrideRun(t.Context(), g, a, Override{Run: q.Run, Action: OverrideJump, Step: "edit"}); err != nil {
				t.Fatal(err)
			}
			if got := overrides(a.State().Transcript); len(got) != 1 || !strings.Contains(got[0], `step "edit" cannot run from here`) {
				t.Errorf("overrides = %q", got)
			}
		})
	}
}

// A graph built in Go is held to what each kind of node may have, as a
// file is.
func TestANodeHasOnlyWhatItsKindMay(t *testing.T) {
	for name, c := range map[string]struct {
		node Node
		want string
	}{
		"a model with a script": {Node{ID: "a", Kind: Agent, Prompt: "Work.", Script: "run.sh"}, "only a program node has a script, a tool, args or a flow"},
		"a model with a tool":   {Node{ID: "a", Kind: Agent, Prompt: "Work.", Tool: "script"}, "only a program node has a script, a tool, args or a flow"},
		"a person with a flow":  {Node{ID: "a", Kind: Person, Prompt: "Go?", Flow: "other"}, "only a program node has a script, a tool, args or a flow"},
		"a script with tools":   {Node{ID: "a", Kind: Run, Script: "run.sh", Tools: []string{"read"}}, "tools and exclude_tools are a model step's"},
		"a person with tools":   {Node{ID: "a", Kind: Person, Prompt: "Go?", Tools: []string{"read"}}, "tools and exclude_tools are a model step's"},
	} {
		t.Run(name, func(t *testing.T) {
			g := &Graph{Name: "kinds", Nodes: []Node{c.node}}
			if err := g.Validate(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}
