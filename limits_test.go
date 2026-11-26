package agentflow

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
)

// stopAt answers the question a bound raised with stop, holding it to be
// about limit.
func stopAt(t *testing.T, g *Graph, a *agentturn.Agent, end *agentturn.RunEnd, limit string) *agentturn.RunEnd {
	t.Helper()
	if q := question(t, end); q.Limit != limit {
		t.Fatalf("question = %+v, want one about %s", q, limit)
	}
	ans, err := g.Answer(end.Pending[0], Result{Outcome: stopOutcome})
	if err != nil {
		t.Fatal(err)
	}
	next, err := a.Resume(context.Background(), ans)
	if next == nil {
		t.Fatal(err)
	}
	return next
}

const boundEnv = "AGENTFLOW_BOUND_STORE"

// A question a bound raised, two calls deep, is pending when the process
// dies; a new one grants more from the store and the definitions alone.
func TestABoundQuestionResumesInANewProcess(t *testing.T) {
	g := func() *Graph {
		return linked(t, &Graph{Name: "deep", MaxDepth: 2, Nodes: []Node{
			{ID: "call", Kind: Run, Flow: "countdown", With: map[string]Value{"n": Literal(4)}},
		}}, countdown())
	}
	var counts []int
	tools := func() []agenttool.Tool { return []agenttool.Tool{countTool(&counts)} }
	if dir := os.Getenv(boundEnv); dir != "" {
		ctx := context.Background()
		store, err := jsonl.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
		if err != nil {
			t.Fatal(err)
		}
		a := nestedAgent(t, g(), &fakeModel{}, tools())
		rec.Attach(a)
		if q := question(t, run(a)); q.Limit != "depth" {
			t.Fatalf("question = %+v", q)
		}
		if err := store.Sync(ctx, s.ID()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/id", []byte(s.ID()), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	ctx := context.Background()
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestABoundQuestionResumesInANewProcess$")
	child.Env = append(os.Environ(), boundEnv+"="+dir)
	out, err := child.CombinedOutput()
	id, readErr := os.ReadFile(dir + "/id")
	if err == nil || readErr != nil {
		t.Fatalf("child: err = %v, read = %v\n%s", err, readErr, out)
	}
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Resume(ctx, store, string(id))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := session.AgentOptions(s, rec.ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	graph := g()
	a := agentturn.New(must(Apply(graph, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: tools()})), seed...)
	rec.Attach(a)
	pending := a.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	var q Question
	if err := json.Unmarshal([]byte(pending[0].Call.Arguments), &q); err != nil || q.Limit != "depth" || q.Workflow != "countdown" || len(q.Path) != 3 {
		t.Fatalf("question = %+v, %v", q, err)
	}
	ans, err := graph.Answer(pending[0], Result{Outcome: grantOutcome, Data: map[string]any{"amount": 3}})
	if err != nil {
		t.Fatal(err)
	}
	end, err := a.Resume(ctx, ans)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("resume: %v, %+v", err, end)
	}
	// What ran before the question did not run again.
	if !slices.Equal(counts, []int{2, 1, 0}) {
		t.Errorf("counted %v", counts)
	}
	got, err := store.Open(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, got)
	runs, err := Runs(pathItems(got), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(runs); len(got) != 6 || got[0] != "deep:done:zero" {
		t.Errorf("tree = %v", got)
	}
}

func must(cfg agentturn.Config, err error) agentturn.Config {
	if err != nil {
		panic(err)
	}
	return cfg
}

// A grant is an amount, and answers only a bound's question.
func TestAGrantIsHeldToItsQuestion(t *testing.T) {
	n := &Node{ID: "x", Kind: Run}
	q := Question{Limit: "visits"}
	for _, bad := range []Result{{Outcome: grantOutcome}, {Outcome: grantOutcome, Data: map[string]any{"amount": 0}},
		{Outcome: grantOutcome, Data: map[string]any{"amount": 1.5}}, {Outcome: grantOutcome, Data: map[string]any{"amount": 1000}}, {Outcome: retryOutcome}} {
		if validAnswer(n, q, bad) == nil {
			t.Errorf("%+v was taken", bad)
		}
	}
	if err := validAnswer(n, q, Result{Outcome: grantOutcome, Data: map[string]any{"amount": 2}}); err != nil {
		t.Error(err)
	}
	if err := validAnswer(n, q, Result{Outcome: stopOutcome}); err != nil {
		t.Error(err)
	}
}
