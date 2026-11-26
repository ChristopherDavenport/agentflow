package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
)

// drafting is a loop a person drives: a draft, a review, and a revision
// for as long as the person asks for changes. Its tree allows 4 steps.
func drafting() *Graph {
	return &Graph{Name: "drafting", MaxSteps: 4, Nodes: []Node{
		{ID: "draft", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"draft"}`), Next: map[string]string{OK: "review"}},
		{ID: "review", Kind: Person, Prompt: "Good?", Outcomes: []string{"change", "done"}, Next: map[string]string{"change": "revise"}, PersonPaced: true},
		{ID: "revise", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"revise"}`), Next: map[string]string{OK: "review"}, PersonPaced: true},
	}}
}

// changes answers the review with change n times, then done, and returns
// the state it came to.
func changes(t *testing.T, g *Graph, a *agentturn.Agent, end *agentturn.RunEnd, n int) (*agentturn.RunEnd, State) {
	t.Helper()
	for k := 0; k <= n; k++ {
		if q := question(t, end); q.Limit != "" || q.Step != "review" {
			t.Fatalf("answer %d: question = %+v", k, q)
		}
		outcome := "change"
		if k == n {
			outcome = "done"
		}
		end = reply(t, g, a, end, outcome)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil {
		t.Fatal(err)
	}
	return end, st
}

// A person can keep sending a draft back past the loop's default bound,
// and past the tree's steps, without being asked for more.
func TestALoopAPersonDrivesHasNoBound(t *testing.T) {
	g := drafting()
	var ran []string
	a := newAgent(t, g, &fakeModel{}, &ran)
	end, st := changes(t, g, a, start(t, a), 3*DefaultLoopVisits)
	if end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("end = %+v, state %+v", end, st)
	}
	if len(ran) != 1+3*DefaultLoopVisits {
		t.Errorf("ran %d scripts", len(ran))
	}
	// Only the first visit of each step counted: none after was ever
	// held to a bound.
	if st.Entries != 3 {
		t.Errorf("entries = %d, want the first visit of each step", st.Entries)
	}
	// What the summary says ran is every step.
	if got := len(st.Steps); got != 2+2*3*DefaultLoopVisits {
		t.Errorf("steps = %d", got)
	}
}

// The same loop answered by the host's rules is not paced by a person,
// so it comes to the bound and asks for more.
func TestALoopRulesDriveIsBound(t *testing.T) {
	g := drafting()
	g.MaxSteps = 0
	var ran []string
	rules := Reply(func(_ context.Context, q Question) (Result, bool) {
		return Result{Outcome: "change"}, q.Limit == ""
	})
	a := newAgent(t, g, &fakeModel{}, &ran, rules)
	end := start(t, a)
	q := question(t, end)
	if q.Limit != "visits" || q.Step != "review" {
		t.Fatalf("question = %+v", q)
	}
	st, _ := g.Replay(a.State().Transcript)
	if st.Visit != DefaultLoopVisits+1 || st.Paced != 0 {
		t.Errorf("visit %d, paced %d", st.Visit, st.Paced)
	}
	end = stopAt(t, g, a, end, "visits")
	if end == nil || !errors.Is(end.Err, ErrVisits) || !strings.Contains(end.Err.Error(), "for visits no person paced") {
		t.Errorf("end = %+v", end)
	}
}

// A judge that decides is not a person either: what it sends back counts.
func TestALoopAJudgeDrivesIsBound(t *testing.T) {
	g := drafting()
	g.MaxSteps = 0
	g.node("review").Judge = &Judge{Criteria: "Change it until it reads well.", Outcomes: []string{"change", "done"}, Returns: 99}
	var lines []says
	for range DefaultLoopVisits {
		lines = append(lines, says{judgeTool, `{"step":"review","outcome":"change","reason":"not yet"}`})
	}
	var ran []string
	cfg, err := kinApply(g, agentturn.Config{Model: &cast{script: lines}, ModelName: "worker", Tools: []agenttool.Tool{scriptTool(&ran)}}, Delegate(Decide))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end, st, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if q := question(t, end); q.Limit != "visits" || q.Step != "review" || st.Visit != DefaultLoopVisits+1 || st.Paced != 0 {
		t.Errorf("question = %+v at visit %d, paced %d", q, st.Visit, st.Paced)
	}
}

// Where rules answer some passes and a person others, only the person's
// go free: the bound is reached at the sixth visit no person paced.
func TestALoopPartlyPacedCountsTheRest(t *testing.T) {
	g := drafting()
	g.MaxSteps = 0
	asked := 0
	rules := Reply(func(_ context.Context, q Question) (Result, bool) {
		if q.Limit != "" {
			return Result{}, false
		}
		asked++
		return Result{Outcome: "change"}, asked%2 == 1
	})
	var ran []string
	a := newAgent(t, g, &fakeModel{}, &ran, rules)
	end := start(t, a)
	for {
		q := question(t, end)
		if q.Limit != "" {
			break
		}
		end = reply(t, g, a, end, "change")
	}
	st, _ := g.Replay(a.State().Transcript)
	// Unpaced: the first visit, then each after a rule's answer.
	if q := question(t, end); q.Limit != "visits" || q.Step != "review" || st.Visit-st.Paced != DefaultLoopVisits+1 || st.Paced == 0 {
		t.Errorf("question = %+v at visit %d, paced %d", q, st.Visit, st.Paced)
	}
}

// A loop in a workflow called in the same context, and one called in a
// child, are paced the same way, by their own run's record.
func TestAPacedLoopAtDepth(t *testing.T) {
	inner := drafting()
	inner.MaxSteps = 0
	for _, ctxName := range []string{ContextSession, ContextFresh} {
		t.Run(ctxName, func(t *testing.T) {
			g := linked(t, &Graph{Name: "outer", MaxSteps: 5, Nodes: []Node{{ID: "call", Kind: Run, Flow: "drafting", Context: ctxName}}}, inner)
			var ran []string
			tools := []agenttool.Tool{scriptTool(&ran)}
			kids := newKin(&fakeModel{}, "fake", tools...)
			a := nestedAgent(t, g, &fakeModel{}, tools, kids.option())
			ctx := context.Background()
			end, _, err := Drive(ctx, g, a, start(t, a))
			for k := 0; ; k++ {
				if err != nil {
					t.Fatal(err)
				}
				if end.Reason == agentturn.ReasonDone {
					break
				}
				q := question(t, end)
				if q.Limit != "" || q.Workflow != "drafting" {
					t.Fatalf("answer %d: question = %+v", k, q)
				}
				outcome := "change"
				if k == 2*DefaultLoopVisits {
					outcome = "done"
				}
				ans, aerr := g.Answer(end.Pending[0], Result{Outcome: outcome})
				if aerr != nil {
					t.Fatal(aerr)
				}
				end, _, err = Resume(ctx, g, a, []agentturn.Answer{ans})
			}
			st, err := g.Replay(a.State().Transcript)
			if err != nil || !st.Done {
				t.Fatalf("state = %+v, %v", st, err)
			}
			// The call, and the first visit of each of the callee's steps.
			if st.Entries != 4 {
				t.Errorf("entries = %d", st.Entries)
			}
		})
	}
}

// A loop a person paces resumes in a new agent from the store with its
// count read back: what was paced stays paced.
func TestAPacedLoopResumesFromTheStore(t *testing.T) {
	ctx := context.Background()
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
	if err != nil {
		t.Fatal(err)
	}
	g := drafting()
	var ran []string
	a := newAgent(t, g, &fakeModel{}, &ran)
	rec.Attach(a)
	end := start(t, a)
	for range 2 * DefaultLoopVisits {
		end = reply(t, g, a, end, "change")
	}
	if err := store.Sync(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}

	rec, s, err = session.Resume(ctx, store, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	seed, err := session.AgentOptions(s, rec.ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := kinApply(g, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}})
	if err != nil {
		t.Fatal(err)
	}
	b := agentturn.New(cfg, seed...)
	rec.Attach(b)
	pending := b.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	st, err := g.Replay(b.State().Transcript)
	if err != nil || st.Visit != 2*DefaultLoopVisits+1 || st.Paced != 2*DefaultLoopVisits || st.Entries != 3 {
		t.Fatalf("resumed at visit %d, paced %d, entries %d, %v", st.Visit, st.Paced, st.Entries, err)
	}
	end = &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: pending}
	end, st = changes(t, g, b, end, 3)
	if end.Reason != agentturn.ReasonDone || st.Entries != 3 {
		t.Errorf("end = %s, entries %d", end.Reason, st.Entries)
	}
	got, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, got)
}

func TestPersonPacedIsCheckedAtLoad(t *testing.T) {
	script := func(id, next string) Node {
		return Node{ID: id, Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"x"}`), Next: map[string]string{OK: next}}
	}
	for want, g := range map[string]*Graph{
		"no path comes back": {Name: "w", Nodes: []Node{
			{ID: "once", Kind: Run, Tool: "script", PersonPaced: true},
		}},
		"fix → check → fix passes none": {Name: "w", Nodes: []Node{
			func() Node { n := script("fix", "check"); n.PersonPaced = true; return n }(),
			{ID: "check", Kind: Run, Tool: "script", Outcomes: []string{"pass", "fail"}, Next: map[string]string{"fail": "fix", "pass": "ask"}, MaxVisits: 9},
			{ID: "ask", Kind: Person, Prompt: "Again?", Outcomes: []string{"yes", "no"}, Next: map[string]string{"yes": "fix"}},
		}},
		"a number or person, not both": func() *Graph {
			g := drafting()
			g.node("review").MaxVisits = 3
			return g
		}(),
		`"revise": it is on a loop through "review"`: func() *Graph {
			g := drafting()
			g.node("revise").PersonPaced = false
			return g
		}(),
	} {
		t.Run(want, func(t *testing.T) {
			err := g.Validate()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want %q", err, want)
			}
		})
	}
	// A number on the step beside it says what it wants.
	g := drafting()
	g.node("revise").PersonPaced, g.node("revise").MaxVisits = false, 50
	if err := g.Validate(); err != nil {
		t.Error(err)
	}
	if err := drafting().Validate(); err != nil {
		t.Error(err)
	}
}
