package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
)

// asker asks whether to go on.
func asker() *Graph {
	return &Graph{Name: "asker", Nodes: []Node{
		{ID: "confirm", Kind: Person, Prompt: "Go on?", Outcomes: []string{"yes", "no"}},
	}, OutputsBy: map[string]map[string]Value{"yes": {"said": Literal("yes")}}}
}

// thinker thinks, then asks, in a child of its own.
func thinker() *Graph {
	return &Graph{Name: "thinker", Nodes: []Node{
		{ID: "think", Kind: Agent, Prompt: "Think.", Next: map[string]string{OK: "deep"}},
		{ID: "deep", Kind: Run, Flow: "asker", Context: ContextFresh},
	}, OutputsBy: map[string]map[string]Value{"yes": {"said": From("steps.deep.outputs.said")}}}
}

// outer calls thinker in a child, and reports what it said.
func outer() *Graph {
	return &Graph{Name: "outer", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "thinker", Context: ContextFresh, Next: map[string]string{"yes": "report"}},
		{ID: "report", Kind: Run, Tool: "script", With: map[string]Value{"name": From("steps.call.outputs.said")}},
	}}
}

const freshEnv = "AGENTFLOW_FRESH_STORE"

func TestAQuestionTwoChildrenDeepResumesInANewProcess(t *testing.T) {
	g := func() *Graph { return linked(t, outer(), thinker(), asker()) }
	var ran []string
	tools := func() []agenttool.Tool { return []agenttool.Tool{scriptTool(&ran)} }
	if dir := os.Getenv(freshEnv); dir != "" {
		ctx := context.Background()
		store, err := jsonl.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
		if err != nil {
			t.Fatal(err)
		}
		m := &fakeModel{outcomes: []string{"ok"}}
		kids := &recordedKin{store: store, rec: rec, model: m, name: "fake", tools: tools()}
		graph := g()
		a := nestedAgent(t, graph, m, tools(), kids.option())
		rec.Attach(a)
		end, _, err := Drive(ctx, graph, a, start(t, a))
		if err != nil || end.Reason != agentturn.ReasonInputRequired {
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
	child := exec.Command(os.Args[0], "-test.run=^TestAQuestionTwoChildrenDeepResumesInANewProcess$")
	child.Env = append(os.Environ(), freshEnv+"="+dir)
	out, err := child.CombinedOutput()
	id, readErr := os.ReadFile(dir + "/id")
	if err == nil || readErr != nil {
		t.Fatalf("child: err = %v, read = %v\n%s", err, readErr, out)
	}
	// A new process has the store and the definitions, and nothing else.
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
	m := &fakeModel{}
	kids := &recordedKin{store: store, rec: rec, model: m, name: "fake", tools: tools()}
	cfg, err := Apply(graph, agentturn.Config{Model: m, ModelName: "fake", Tools: tools()}, kids.option(), recordOf(store, string(id)))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg, seed...)
	rec.Attach(a)
	// The caller holds its call; driving it finds the question two
	// children down.
	end, st, err := Drive(ctx, graph, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, end)
	if q.Workflow != "asker" || q.Step != "confirm" || len(q.Path) != 3 || q.Path[0].Workflow != "outer" || q.Path[0].Step != "call" ||
		q.Path[1].Workflow != "thinker" || q.Path[1].Step != "deep" || st.Workflow != "asker" || !st.Fresh || st.Depth != 2 {
		t.Fatalf("question = %+v, state at %s depth %d", q, st.Workflow, st.Depth)
	}
	ans, err := graph.Answer(end.Pending[0], Result{Outcome: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	// The answer is the grandchild's; Resume takes it there.
	if _, err := a.Resume(ctx, ans); !errors.Is(err, agentturn.ErrNotPending) {
		t.Errorf("the caller took an answer to a question it does not hold: %v", err)
	}
	end, _, err = Resume(ctx, graph, a, []agentturn.Answer{ans})
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("resume: %v, %+v", err, end)
	}
	if !slices.Equal(ran, []string{"yes"}) || len(m.requests) != 0 {
		t.Errorf("ran %v, asked the model %d times", ran, len(m.requests))
	}
	// Each session's requests rebuild.
	for _, c := range kids.opened {
		got, err := store.Open(ctx, spawnedID(t, store, c.Spawn))
		if err != nil {
			t.Fatal(err)
		}
		checkHashes(t, got)
	}
	got, err := store.Open(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, got)
	// The record reads back as a tree, through the children's sessions.
	runs, err := Runs(pathItems(got), childSessions(t, store))
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(runs); !slices.Equal(got, []string{"outer:done:ok", " thinker:done:yes", "  asker:done:yes"}) {
		t.Errorf("tree = %v", got)
	}
	if kid := runs[0].Children[0]; !kid.Fresh || kid.Spawn == "" || kid.Depth != 1 || kid.Parent != runs[0].ID {
		t.Errorf("child = %+v", kid)
	}
	// Without the sessions, the call's result still says how it went.
	if runs, err := Runs(pathItems(got), nil); err != nil || !slices.Equal(tree(runs), []string{"outer:done:ok", " thinker:done:yes"}) {
		t.Errorf("tree without sessions = %v, %v", tree(runs), err)
	}
}

// flip says again the first time it is called, and done after.
func flip(calls *int) agenttool.Tool {
	return agenttool.New("flip", "Again?", func(context.Context, struct{}) (Result, error) {
		*calls++
		if *calls == 1 {
			return Result{Outcome: "again"}, nil
		}
		return Result{Outcome: "done"}, nil
	})
}

func keeper() *Graph {
	return &Graph{Name: "keeper", Nodes: []Node{
		{ID: "draft", Kind: Agent, Prompt: "Draft it.", Context: ContextFresh, Child: "writer", Dir: "docs", Model: "small",
			Instructions: "Write plainly.", Next: map[string]string{OK: "flip"}},
		{ID: "flip", Kind: Run, Tool: "flip", Outcomes: []string{"again", "done"}, Next: map[string]string{"again": "draft", "done": "polish"}},
		{ID: "polish", Kind: Agent, Prompt: "Polish it.", Context: ContextFresh, Child: "writer", Dir: "docs", Model: "small",
			Instructions: "Write plainly."},
	}}
}

func TestAKeptChildServesEveryVisitAndStep(t *testing.T) {
	ctx := context.Background()
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
	if err != nil {
		t.Fatal(err)
	}
	var flips int
	m := &fakeModel{outcomes: []string{OK, OK, OK}}
	kids := &recordedKin{store: store, rec: rec, model: m, name: "fake"}
	g := keeper()
	a := nestedAgent(t, g, m, []agenttool.Tool{flip(&flips)}, kids.option(), Models("fake", "small"))
	rec.Attach(a)
	end, st, err := Drive(ctx, g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if want := []string{"draft:ok", "flip:again", "draft:ok", "flip:done", "polish:ok"}; !slices.Equal(steps(st), want) {
		t.Errorf("path = %v", steps(st))
	}
	if len(kids.opened) != 3 {
		t.Fatalf("opened %d children", len(kids.opened))
	}
	for i, c := range kids.opened {
		if c.Spawn != kids.opened[0].Spawn || c.Kept != "writer" || c.Settings.Dir != "docs" || c.Settings.Model != "small" || c.Settings.Instructions != "Write plainly." {
			t.Errorf("child %d = %+v", i, c)
		}
	}
	if kids.opened[0].Run == kids.opened[1].Run {
		t.Error("two visits shared a run")
	}
	// The child's model is the step's, its instructions are added, and it
	// reads its earlier visits.
	last := m.requests[2]
	if last.Model != "small" || !strings.Contains(last.Instructions, "Write plainly.") || !strings.Contains(last.Instructions, `step "polish"`) {
		t.Errorf("polish was asked of %s with %s", last.Model, last.Instructions)
	}
	if n := strings.Count(strings.Join(texts(last.Input), "\n"), beginPrefix); n != 3 {
		t.Errorf("polish read %d openings, want its own and both drafts'", n)
	}
	// One session, a run for each visit, every request rebuilt.
	child, err := store.Open(ctx, spawnedID(t, store, kids.opened[0].Spawn))
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, child)
	runs, err := Runs(pathItems(child), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(runs); !slices.Equal(got, []string{"keeper:draft:done:ok", "keeper:draft:done:ok", "keeper:polish:done:ok"}) {
		t.Errorf("child's runs = %v", got)
	}
	parent, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, parent)
}

func TestAFailedChildRoutesOnFailed(t *testing.T) {
	g := linked(t, &Graph{Name: "careful", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "fragile", Context: ContextFresh, Next: map[string]string{Failed: "report"}},
		{ID: "report", Kind: Run, Tool: "script", With: map[string]Value{"name": From("steps.call.outputs.reason")}},
	}}, fragile())
	fails := 9
	var ran []string
	tools := []agenttool.Tool{failing(&fails), scriptTool(&ran)}
	kids := newKin(&fakeModel{}, "fake", tools...)
	a := nestedAgent(t, g, &fakeModel{}, tools, Unattended(), kids.option())
	end, st, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone || !slices.Equal(steps(st), []string{"call:failed", "report:ok"}) {
		t.Fatalf("end = %s %v, path %v", end.Reason, err, steps(st))
	}
	if len(ran) != 1 || !strings.Contains(ran[0], "not yet") {
		t.Errorf("reported %v", ran)
	}
	// Without an edge for it, the calling step fails, and unattended so
	// does its run.
	g = linked(t, &Graph{Name: "plain", Nodes: []Node{{ID: "call", Kind: Run, Flow: "fragile", Context: ContextFresh}}}, fragile())
	kids = newKin(&fakeModel{}, "fake", tools...)
	a = nestedAgent(t, g, &fakeModel{}, tools, Unattended(), kids.option())
	end, _, err = Drive(context.Background(), g, a, start(t, a))
	if !errors.Is(err, ErrStepFailed) || !strings.Contains(err.Error(), "fragile") {
		t.Errorf("end = %s, %v", end.Reason, err)
	}
	// The failed child's steps count toward its caller's tree: the call,
	// and the one step the child entered.
	st, _ = g.Replay(a.State().Transcript)
	if st.Entries != 2 {
		t.Errorf("entries = %d, want the call and the child's step", st.Entries)
	}
}

func TestAnInvocationAbandonsChildren(t *testing.T) {
	ctx := context.Background()
	g := linked(t, &Graph{Name: "outer", Nodes: []Node{{ID: "call", Kind: Run, Flow: "asker", Context: ContextFresh}}}, asker())
	kids := newKin(&fakeModel{}, "fake")
	a := nestedAgent(t, g, &fakeModel{}, nil, kids.option())
	end, _, err := Drive(ctx, g, a, start(t, a))
	if q := question(t, end); err != nil || q.Workflow != "asker" {
		t.Fatalf("question = %+v, %v", q, err)
	}
	first := kids.agents[kids.opened[0].Spawn]
	end, _, err = Invoke(ctx, g, a, nil)
	if q := question(t, end); err != nil || q.Workflow != "asker" || len(kids.agents) != 2 {
		t.Fatalf("after: question = %+v, %d children, %v", q, len(kids.agents), err)
	}
	// The child's run was ended in its own session first, and the call
	// that held it refused; then the caller's run.
	runs, err := Runs(first.State().Transcript, nil)
	if err != nil || !slices.Equal(tree(runs), []string{"asker:abandoned:"}) {
		t.Errorf("child = %v, %v", tree(runs), err)
	}
	runs, err = Runs(a.State().Transcript, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tree(runs); !slices.Equal(got, []string{"outer:abandoned:", " asker:failed:", "outer:running:", " asker:running:"}) {
		t.Errorf("tree = %v", got)
	}
}

func TestChildSettingsAreCheckedAtLoad(t *testing.T) {
	for want, change := range map[string]func(g *Graph){
		"dir is a setting of a child":   func(g *Graph) { g.node("draft").Context = ContextSession },
		`dir "../up"`:                   func(g *Graph) { g.node("draft").Dir = "../up" },
		`dir "/abs"`:                    func(g *Graph) { g.node("draft").Dir = "/abs" },
		"only a model step keeps":       func(g *Graph) { g.node("flip").Child, g.node("flip").Context = "w", ContextFresh },
		"differ in its dir":             func(g *Graph) { g.node("polish").Dir = "other" },
		`child "no good"`:               func(g *Graph) { g.node("draft").Child, g.node("polish").Child = "no good", "no good" },
		"only a model node or a call":   func(g *Graph) { g.node("flip").Context = ContextFresh },
		"only a model step gives a ver": func(g *Graph) { g.node("flip").Verdict = true },
	} {
		g := keeper()
		change(g)
		if err := g.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	base := agentturn.Config{Model: &fakeModel{}, ModelName: "fake"}
	if _, err := Apply(keeper(), base); err == nil || !strings.Contains(err.Error(), "no Children") {
		t.Errorf("no opener: %v", err)
	}
	kids := newKin(&fakeModel{}, "fake")
	if _, err := Apply(keeper(), base, kids.option(), Models("fake")); err == nil || !strings.Contains(err.Error(), `model "small"`) {
		t.Errorf("an unserved model: %v", err)
	}
	if _, err := Apply(judged(), base, Delegate(Decide)); err == nil || !strings.Contains(err.Error(), "no Children") {
		t.Errorf("a judge with no opener: %v", err)
	}
}

func TestAChildIsTheProgramsToMake(t *testing.T) {
	g := keeper()
	m := &fakeModel{outcomes: []string{OK}}
	kids := newKin(m, "fake")
	a := nestedAgent(t, g, m, []agenttool.Tool{flip(new(int))}, kids.option())
	end := start(t, a)
	// The call holding the child cannot be let go with a result of
	// someone else's making before its child has run.
	var args callArgs
	if err := json.Unmarshal([]byte(end.Pending[0].Call.Arguments), &args); err != nil || args.Context != ContextFresh || args.Child != "writer" || args.Spawn != args.Run {
		t.Fatalf("call = %+v, %v", args, err)
	}
	if _, err := (Child{}).Apply(agentturn.Config{}); err == nil {
		t.Error("a child the program did not make was applied")
	}
}
