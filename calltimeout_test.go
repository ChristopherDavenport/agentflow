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
)

// slow waits until it is cut off, and counts the times it ran.
func slow(ran *int) agenttool.Tool {
	return agenttool.New("slow", "Take a while.", func(ctx context.Context, _ struct{}) (Result, error) {
		*ran++
		<-ctx.Done()
		return Result{}, ctx.Err()
	})
}

func TestAWorkflowsTimeoutHoldsItsSteps(t *testing.T) {
	g := &Graph{Name: "patient", Timeout: 60 * time.Millisecond, Nodes: []Node{
		{ID: "wait", Kind: Run, Tool: "slow"},
	}}
	var ran int
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{slow(&ran)})
	q := question(t, start(t, a))
	if !q.Retry || !strings.Contains(q.Question, `step "wait" timed out after 60ms`) {
		t.Fatalf("question = %+v", q)
	}
	// A step's own timeout wins, and a model step that sets none is told
	// its workflow's.
	g.Nodes[0].Timeout = time.Hour
	if got := (&flow{}).timeout(State{g: g, Node: &g.Nodes[0]}); got != time.Hour {
		t.Errorf("own timeout = %s", got)
	}
	m := &Node{ID: "think", Kind: Agent, Prompt: "Think."}
	unattended := &flow{unattended: true}
	if got := unattended.timeout(State{g: g, Node: m}); got != 60*time.Millisecond {
		t.Errorf("a model step unattended = %s, want the workflow's", got)
	}
	if got := unattended.timeout(State{g: &Graph{}, Node: m}); got != DefaultUnattendedTimeout {
		t.Errorf("with no workflow timeout = %s", got)
	}
	person := &Node{ID: "ask", Kind: Person, Prompt: "Go?"}
	if got := unattended.timeout(State{g: g, Node: person}); got != 0 {
		t.Errorf("a question = %s, want its own only", got)
	}
}

// lingerer waits on a slow step, and on abandon tidies up.
func lingerer() *Graph {
	return &Graph{Name: "lingerer", OnAbort: "tidy", Nodes: []Node{
		{ID: "wait", Kind: Run, Tool: "slow"},
		{ID: "tidy", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"tidied"}`)},
	}}
}

// waitsOn waits on a person, and on abandon tidies up.
func waitsOn() *Graph {
	return &Graph{Name: "waitsOn", OnAbort: "tidy", Nodes: []Node{
		{ID: "ask", Kind: Person, Prompt: "Go on?"},
		{ID: "tidy", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"tidied"}`)},
	}}
}

// impatient calls a workflow for at most timeout, and on failure reports.
func impatient(callee string, timeout time.Duration, context string, routes bool) *Graph {
	call := Node{ID: "call", Kind: Run, Flow: callee, Timeout: timeout, Context: context}
	if routes {
		call.Next = map[string]string{Failed: "report"}
	}
	return &Graph{Name: "impatient", Nodes: []Node{
		call,
		{ID: "report", Kind: Run, Tool: "script", With: map[string]Value{"name": From("steps.call.outputs.reason")}},
	}}
}

func TestACallInContextThatRunsPastItsTimeoutFails(t *testing.T) {
	g := linked(t, impatient("lingerer", 80*time.Millisecond, "", true), lingerer())
	var ran []string
	var waited int
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{slow(&waited), scriptTool(&ran)})
	end, st, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	want := `timeout: call "call" of impatient ran past its timeout of 80ms`
	if !slices.Equal(ran, []string{"tidied", want}) || waited != 1 {
		t.Errorf("ran %v, waited %d", ran, waited)
	}
	if len(st.Steps) != 2 || st.Steps[0].Result.Outcome != Failed {
		t.Errorf("steps = %+v", st.Steps)
	}
	runs, err := Runs(a.State().Transcript, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(runs); !slices.Equal(got, []string{"impatient:done:ok", " lingerer:abandoned:"}) {
		t.Errorf("tree = %v", got)
	}
	if callee := runs[0].Children[0]; callee.Reason != want || callee.State.Cleanup != "done" {
		t.Errorf("callee = %q, cleanup %q", callee.Reason, callee.State.Cleanup)
	}
}

func TestACallThatDoesNotRouteATimeoutAsksToRetry(t *testing.T) {
	g := linked(t, impatient("lingerer", 60*time.Millisecond, "", false), lingerer())
	var ran []string
	var waited int
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{slow(&waited), scriptTool(&ran)})
	end, _, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, end)
	if !q.Retry || q.Step != "call" || !strings.Contains(q.Question, "ran past its timeout of 60ms") {
		t.Fatalf("question = %+v", q)
	}
}

func TestACallsTimeoutReachesAQuestionItWaitsOn(t *testing.T) {
	g := linked(t, impatient("waitsOn", 100*time.Millisecond, "", true), waitsOn())
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{scriptTool(&ran)})
	end, _, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if q := question(t, end); q.Workflow != "waitsOn" {
		t.Fatalf("question = %+v", q)
	}
	time.Sleep(120 * time.Millisecond)
	end, st, err := Drive(context.Background(), g, a, nil)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(ran) != 2 || ran[0] != "tidied" || !strings.Contains(ran[1], "ran past its timeout") || st.Steps[0].Result.Outcome != Failed {
		t.Errorf("ran %v, steps %+v", ran, st.Steps)
	}
}

func TestAFreshCallThatRunsPastItsTimeoutFails(t *testing.T) {
	for name, callee := range map[string]*Graph{"working": lingerer(), "waiting": waitsOn()} {
		t.Run(name, func(t *testing.T) {
			g := linked(t, impatient(callee.Name, 100*time.Millisecond, ContextFresh, true), callee)
			var ran []string
			var waited int
			tools := []agenttool.Tool{slow(&waited), scriptTool(&ran)}
			m := &fakeModel{}
			kids := newKin(m, "fake", tools...)
			a := nestedAgent(t, g, m, tools, kids.option())
			end, _, err := Drive(context.Background(), g, a, start(t, a))
			if err != nil {
				t.Fatal(err)
			}
			if end.Reason == agentturn.ReasonInputRequired {
				// The child waits on its person; the deadline passes.
				time.Sleep(120 * time.Millisecond)
				if end, _, err = Drive(context.Background(), g, a, nil); err != nil {
					t.Fatal(err)
				}
			}
			if end.Reason != agentturn.ReasonDone {
				t.Fatalf("end = %+v", end)
			}
			if len(ran) != 2 || ran[0] != "tidied" || !strings.Contains(ran[1], `call "call" of impatient ran past its timeout of 100ms`) {
				t.Errorf("ran %v", ran)
			}
			for _, child := range kids.agents {
				runs, err := Runs(child.State().Transcript, nil)
				if err != nil || len(runs) != 1 || runs[0].Status != Abandoned || !strings.HasPrefix(runs[0].Reason, timedOutPrefix) {
					t.Errorf("child runs = %+v, %v", runs, err)
				}
			}
		})
	}
}

// A call's deadline is in the record: a new process, with the store and the
// definitions, holds the run to it.
func TestACallsDeadlineHoldsAfterAResume(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	g := func() *Graph { return linked(t, impatient("waitsOn", 150*time.Millisecond, "", true), waitsOn()) }
	var ran []string
	a := nestedAgent(t, g(), &fakeModel{}, []agenttool.Tool{scriptTool(&ran)})
	rec.Attach(a)
	if q := question(t, start(t, a)); q.Workflow != "waitsOn" {
		t.Fatalf("question = %+v", q)
	}
	if err := store.Sync(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(170 * time.Millisecond)
	rec2, s2, err := session.Resume(ctx, store, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	seed, err := session.AgentOptions(s2, rec2.ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	graph := g()
	b := agentturn.New(must(Apply(graph, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}})), seed...)
	rec2.Attach(b)
	end, st, err := Drive(ctx, graph, b, nil)
	if err != nil || end.Reason != agentturn.ReasonDone || st.Steps[0].Result.Outcome != Failed {
		t.Fatalf("end = %+v, %v, steps %+v", end, err, st.Steps)
	}
	got, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, got)
	runs, err := Runs(pathItems(got), nil)
	if err != nil || len(runs) != 1 || runs[0].Problem != "" || len(runs[0].Children) != 1 || runs[0].Children[0].Status != Abandoned {
		t.Errorf("runs = %+v, %v", runs, err)
	}
}

func TestTimeoutsAreCheckedAtLoad(t *testing.T) {
	g := impatient("lingerer", 80*time.Millisecond, "", true)
	if err := g.Validate(); err != nil {
		t.Errorf("a call's timeout: %v", err)
	}
	g.Timeout = 48 * time.Hour
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "timeout must be above 0") {
		t.Errorf("a workflow's past the bound: %v", err)
	}
}
