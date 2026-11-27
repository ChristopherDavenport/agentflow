package agentflow

import (
	"context"
	"errors"
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

// spentIn is what run spent in a child's session, as its record says.
func spentIn(t *testing.T, child agentturn.Transcript, run string) int {
	t.Helper()
	runs, err := Runs(child, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range runs {
		if in.ID == run {
			if in.Status != Abandoned {
				t.Fatalf("run %s is %s, want abandoned", run, in.Status)
			}
			return in.State.Entries
		}
	}
	t.Fatalf("no run %s in the child's session", run)
	return 0
}

// careful calls tidyin in a child, then asks once more.
func careful() *Graph {
	return &Graph{Name: "careful", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "tidyin", Context: ContextFresh, Next: map[string]string{OK: "after"}},
		{ID: "after", Kind: Person, Prompt: "After?"},
	}}
}

func TestAnOverrideCountsTheStepsOfTheChildItThrewAway(t *testing.T) {
	for _, tc := range []struct {
		o Override
		// own is the steps the caller entered itself: the call, and the
		// retry or the step jumped to.
		own int
	}{
		{Override{Action: OverrideRetry}, 2},
		{Override{Action: OverrideJump, Step: "after"}, 2},
		{Override{Action: OverrideAbandon}, 1},
	} {
		t.Run(tc.o.Action, func(t *testing.T) {
			g := linked(t, careful(), tidyIn())
			var ran []string
			m := &fakeModel{}
			kids := newKin(m, "fake", undoTool(&ran, nil))
			a := nestedAgent(t, g, m, nil, kids.option())
			end, _, err := Drive(t.Context(), g, a, start(t, a))
			if err != nil {
				t.Fatal(err)
			}
			q := question(t, end)
			o := tc.o
			o.Run = q.Path[0].Run
			if _, _, err := OverrideRun(t.Context(), g, a, o); err != nil {
				t.Fatal(err)
			}
			// The child waited on its question and ran its cleanup: both
			// are steps of the tree, which the override did not take back.
			spent := spentIn(t, kids.agents[q.Run].State().Transcript, q.Run)
			if spent != 2 {
				t.Fatalf("the child spent %d, want its question and its cleanup", spent)
			}
			runs, err := Runs(a.State().Transcript, nil)
			if err != nil {
				t.Fatal(err)
			}
			root := runs[0]
			if root.State.Entries != tc.own+spent {
				t.Errorf("entries = %d, want %d of its own and %d of the child's", root.State.Entries, tc.own, spent)
			}
			if tc.o.Action != OverrideAbandon {
				return
			}
			// The child is listed as abandoned, with what its call's result
			// says it spent, though its session is not read here.
			if len(root.Children) != 1 || root.Children[0].Status != Abandoned || root.Children[0].State.Entries != spent || root.Children[0].Start != -1 {
				t.Errorf("child = %+v", root.Children)
			}
		})
	}
}

func TestAnOverrideCountsWhatAChildsOwnChildrenSpent(t *testing.T) {
	g := linked(t, outer(), thinker(), asker())
	var ran []string
	m := &fakeModel{outcomes: []string{OK, OK}}
	kids := newKin(m, "fake")
	a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)}, kids.option())
	end, _, err := Drive(t.Context(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, end)
	if q.Workflow != "asker" || len(q.Path) != 3 {
		t.Fatalf("question = %+v", q)
	}
	end, _, err = OverrideRun(t.Context(), g, a, Override{Run: q.Path[0].Run, Action: OverrideRetry})
	if err != nil || end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("end = %+v, %v", end, err)
	}
	// The thinker thought and called the asker, which asked: three steps,
	// the grandchild's rolled up into the child's.
	thinker := q.Path[1].Run
	if spent := spentIn(t, kids.agents[thinker].State().Transcript, thinker); spent != 3 {
		t.Fatalf("the child spent %d, want 3", spent)
	}
	st, _ := g.Replay(a.State().Transcript)
	if st.Entries != 2+3 {
		t.Errorf("entries = %d, want the call, its retry and the 3 the child spent", st.Entries)
	}
}

func TestAnOverrideCountsOnlyTheKeptChildsVisitItEnded(t *testing.T) {
	g := &Graph{Name: "kept", Nodes: []Node{{ID: "draft", Kind: Agent, Prompt: "Draft it.", Context: ContextFresh, Child: "writer"}}}
	m := &fakeModel{outcomes: []string{"", "", OK}}
	kids := newKin(m, "fake")
	a := nestedAgent(t, g, m, nil, kids.option())
	_, st, err := Drive(t.Context(), g, a, start(t, a))
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("err = %v", err)
	}
	visit := st.Instance
	if _, _, err := OverrideRun(t.Context(), g, a, Override{Run: st.Path[0].Run, Action: OverrideRetry}); err != nil {
		t.Fatal(err)
	}
	var child *agentturn.Agent
	for _, c := range kids.agents {
		child = c
	}
	if spent := spentIn(t, child.State().Transcript, visit); spent != 1 {
		t.Fatalf("the first visit spent %d, want its one step", spent)
	}
	// The step, its retry, and one step in each of the child's two visits:
	// the second visit's count is its own, not the session's.
	st, _ = g.Replay(a.State().Transcript)
	if !st.Done || st.Entries != 4 {
		t.Errorf("done %v, entries = %d, want 4", st.Done, st.Entries)
	}
}

const overrideCountEnv = "AGENTFLOW_OVERRIDE_COUNT_STORE"

func TestAnOverrideBeforeAKillIsCountedOnResume(t *testing.T) {
	g := func() *Graph { return linked(t, outer(), thinker(), asker()) }
	var ran []string
	tools := func() []agenttool.Tool { return []agenttool.Tool{scriptTool(&ran)} }
	if dir := os.Getenv(overrideCountEnv); dir != "" {
		ctx := context.Background()
		store, err := jsonl.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
		if err != nil {
			t.Fatal(err)
		}
		m := &fakeModel{outcomes: []string{OK, OK}}
		kids := &recordedKin{store: store, rec: rec, model: m, name: "fake", tools: tools()}
		graph := g()
		a := nestedAgent(t, graph, m, tools(), kids.option())
		rec.Attach(a)
		end, _, err := Drive(ctx, graph, a, start(t, a))
		if err != nil {
			t.Fatal(err)
		}
		q := question(t, end)
		// The retry throws the first child away and asks again in a new one.
		if end, _, err := OverrideRun(ctx, graph, a, Override{Run: q.Path[0].Run, Action: OverrideRetry}); err != nil || end.Reason != agentturn.ReasonInputRequired {
			t.Fatalf("end = %+v, %v", end, err)
		}
		if err := os.WriteFile(dir+"/id", []byte(s.ID()), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	ctx := context.Background()
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestAnOverrideBeforeAKillIsCountedOnResume$")
	child.Env = append(os.Environ(), overrideCountEnv+"="+dir)
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
	// From the store alone: the call, its retry, and the 3 steps the child
	// thrown away spent.
	graph := g()
	if st, _ := graph.Replay(pathItems(s)); st.Entries != 2+3 {
		t.Fatalf("entries = %d after the resume, want 5", st.Entries)
	}
	seed, err := session.AgentOptions(s, rec.ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	m := &fakeModel{}
	kids := &recordedKin{store: store, rec: rec, model: m, name: "fake", tools: tools()}
	cfg, err := Apply(graph, agentturn.Config{Model: m, ModelName: "fake", Tools: tools()}, kids.option(), recordOf(store, string(id)))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg, seed...)
	rec.Attach(a)
	end, _, err := Drive(ctx, graph, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	ans, err := graph.Answer(end.Pending[0], Result{Outcome: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	end, st, err := Resume(ctx, graph, a, []agentturn.Answer{ans})
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("resume: %v, %+v", err, end)
	}
	// Both children's steps and the report: the summary counts the same.
	got, err := store.Open(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := Runs(pathItems(got), nil)
	if err != nil {
		t.Fatal(err)
	}
	root := runs[0]
	if root.State.Entries != 2+3+3+1 {
		t.Fatalf("entries %d", root.State.Entries)
	}
	statuses := []string{}
	for _, c := range root.Children {
		statuses = append(statuses, c.Status)
	}
	if !slices.Equal(statuses, []string{Abandoned, Done}) {
		t.Errorf("the calls' statuses are %v", statuses)
	}
}
