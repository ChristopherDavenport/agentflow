package agentflow

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// texts are the messages and calls of a request, in order.
func texts(items openresponses.Items) (out []string) {
	for _, item := range items {
		switch it := item.(type) {
		case *openresponses.Message:
			out = append(out, it.Text())
		case *openresponses.FunctionCall:
			out = append(out, "call "+it.Name)
		case *openresponses.FunctionCallOutput:
			out = append(out, "output")
		}
	}
	return out
}

func TestEachVisitOpensOnce(t *testing.T) {
	g, m, ran := release(), &fakeModel{outcomes: []string{"findings", "ok", "clean"}}, []string{}
	a := newAgent(t, g, m, &ran, Reply(func(context.Context, Question) (Result, bool) {
		return Result{Outcome: "fix"}, true
	}))
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("reason = %s, err = %v", end.Reason, end.Err)
	}
	var opened []string
	for _, s := range texts(openresponses.Items(a.State().Transcript)) {
		if rest, ok := strings.CutPrefix(s, beginPrefix); ok {
			opened = append(opened, rest)
		}
	}
	const rest = " The steps before it are finished and their results accepted, the flow_finish calls included. Do this step now. (run "
	want := []string{`"review".` + rest, `"fix".` + rest, `"review".` + rest}
	if len(opened) != len(want) {
		t.Fatalf("opened = %q", opened)
	}
	seen := map[string]bool{}
	for i, got := range opened {
		// No two visits read alike, a step's second visit included.
		if !strings.HasPrefix(got, want[i]) || seen[got] {
			t.Errorf("opened[%d] = %q, want %q and a visit of its own", i, got, want[i])
		}
		seen[got] = true
	}
}

func TestFreshContextSeesOnlyItsStep(t *testing.T) {
	ctx := context.Background()
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
	if err != nil {
		t.Fatal(err)
	}
	g := release()
	g.node("fix").Context = ContextFresh
	// The model first only talks at the fresh step, so the step takes
	// two requests, the second after a reminder.
	m, ran := &fakeModel{outcomes: []string{"findings", "", "ok", "clean"}}, []string{}
	kids := &recordedKin{store: store, rec: rec, model: m, name: "fake"}
	a := newAgent(t, g, m, &ran, kids.option(), Reply(func(context.Context, Question) (Result, bool) {
		return Result{Outcome: "fix"}, true
	}))
	rec.Attach(a)
	end := start(t, a)
	if _, st, err := Drive(ctx, g, a, end); err != nil || !st.Done {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if len(m.requests) != 4 {
		t.Fatalf("the model took %d turns, want 4", len(m.requests))
	}
	// The child reads what its call passed in, and its own turns.
	fresh := texts(m.requests[1].Input)
	if len(fresh) != 2 || !strings.HasPrefix(fresh[0], invocationPrefix+"pr:fix") || !strings.HasPrefix(fresh[1], beginning("fix")) || strings.Contains(fresh[1], "before it") {
		t.Errorf("fresh step read %q, want only its invocation and opening message", fresh)
	}
	if again := texts(m.requests[2].Input); len(again) != 4 || again[2] != "some text" || !strings.HasPrefix(again[3], nudgePrefix) {
		t.Errorf("fresh step then read %q", again)
	}
	// The next step has the whole conversation again, with the call
	// that held the child and its result, and none of the child's turns.
	after := texts(m.requests[3].Input)
	if after[0] != "review this PR" || slices.Contains(after, "some text") || !slices.Contains(after, "call "+callTool) {
		t.Errorf("the step after read %q", after)
	}
	for _, id := range []string{s.ID()} {
		got, err := store.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		checkHashes(t, got)
	}
	// The child's session holds the step, linked from the parent's.
	if len(kids.opened) == 0 {
		t.Fatal("no child was opened")
	}
	child, err := childSessions(t, store)(kids.opened[0].Spawn)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := Runs(child, nil)
	if err != nil || len(runs) != 1 || runs[0].Workflow != "pr:fix" || runs[0].Status != Done || !runs[0].Fresh || runs[0].Caller == "" {
		t.Fatalf("child runs = %+v, %v", runs, err)
	}
	all, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	top, err := Runs(pathItems(all), childSessions(t, store))
	if err != nil || len(top) != 1 || len(top[0].Children) != 1 || top[0].Children[0].ID != runs[0].ID || top[0].Children[0].Spawn == "" {
		t.Fatalf("tree = %+v, %v", top, err)
	}
}

func TestContextIsAModelSetting(t *testing.T) {
	for node, want := range map[string]string{"triage": "only a model node or a call has a context", "review": `context "all"`} {
		g := release()
		g.node(node).Context = "all"
		if err := g.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", node, err, want)
		}
	}
}
