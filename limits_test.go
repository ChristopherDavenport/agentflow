package agentflow

import (
	"context"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
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
	next, err := carry(t, a, ans)
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
		h := hosted(t, dir, "", g(), &fakeModel{}, tools(), false)
		first, err := h.a.Prompt(ctx, openresponses.UserText("go"))
		if err != nil {
			t.Fatal(err)
		}
		end, _, err := Drive(ctx, h.g, h.a, first, h.hist)
		if err != nil {
			t.Fatal(err)
		}
		if q := question(t, end); q.Limit != "depth" {
			t.Fatalf("question = %+v", q)
		}
		syncAll(t, h.store)
		if err := os.WriteFile(dir+"/id", []byte(h.id), 0o600); err != nil {
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
	h := hosted(t, dir, string(id), g(), &fakeModel{}, tools(), false)
	// The question waits three sessions down, each a fork of the one above.
	end, _, err := Drive(ctx, h.g, h.a, nil, h.hist)
	if err != nil || end == nil || len(end.Pending) != 1 {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if q := question(t, end); q.Limit != "depth" || q.Workflow != "countdown" || len(q.Path) != 3 {
		t.Fatalf("question = %+v", q)
	}
	ans, err := h.g.Answer(end.Pending[0], Result{Outcome: grantOutcome, Data: map[string]any{"amount": 3}})
	if err != nil {
		t.Fatal(err)
	}
	end, _, err = Resume(ctx, h.g, h.a, []agentturn.Answer{ans}, h.hist)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("resume: %v, %+v", err, end)
	}
	// What ran before the question did not run again.
	if !slices.Equal(counts, []int{2, 1, 0}) {
		t.Errorf("counted %v", counts)
	}
	checkHashes(t, h.session(t, ""))
	if got := tree(h.runs(t)); len(got) != 6 || got[0] != "deep:done:zero" {
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
