package agentflow

import (
	"context"
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
	a := newAgent(t, g, m, &ran, Reply(func(context.Context, Question) (Result, bool) {
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
	fresh := texts(m.requests[1].Input)
	if len(fresh) != 1 || !strings.HasPrefix(fresh[0], beginning("fix")) || strings.Contains(fresh[0], "before it") {
		t.Errorf("fresh step read %q, want only its opening message", fresh)
	}
	// What the step itself did stays, and nothing from before it.
	if again := texts(m.requests[2].Input); len(again) != 3 || again[1] != "some text" || !strings.HasPrefix(again[2], nudgePrefix) {
		t.Errorf("fresh step then read %q", again)
	}
	// The next step has the whole conversation again.
	if after := texts(m.requests[3].Input); after[0] != "review this PR" {
		t.Errorf("the step after read %q", after)
	}
	got, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, got)
}

func TestFreshContextIsNotSilentlyWidened(t *testing.T) {
	g := release()
	if _, err := since(openresponses.Items{openresponses.UserText("a secret")}, State{Node: g.node("fix"), Instance: "k"}); err == nil {
		t.Error("a missing opening message sent the whole input")
	}
}

func TestContextIsAModelSetting(t *testing.T) {
	for node, want := range map[string]string{"triage": "only a model node has a context", "review": `context "all"`} {
		g := release()
		g.node(node).Context = "all"
		if err := g.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", node, err, want)
		}
	}
}
