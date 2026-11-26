package agentflow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// says is what the model does next: a call, or text when tool is empty.
type says struct{ tool, args string }

type cast struct {
	script   []says
	requests []openresponses.Request
}

func (m *cast) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.requests = append(m.requests, req)
	if len(m.script) == 0 {
		return errors.New("cast: out of lines")
	}
	next := m.script[0]
	m.script = m.script[1:]
	if next.tool == "" {
		return say(sink, req.Model, "hmm")
	}
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	call, err := e.FunctionCall("", next.tool)
	if err != nil {
		return err
	}
	if err := call.Arguments(withRun(req, next.tool, next.args)); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

var found = says{finishTool, `{"step":"review","outcome":"findings"}`}

func verdict(outcome string) says {
	return says{judgeTool, `{"step":"triage","outcome":"` + outcome + `","reason":"because"}`}
}

func judged() *Graph {
	g := release()
	g.node("triage").Judge = &Judge{Criteria: "Fix real bugs.", Outcomes: []string{"fix", "report"}, Tools: []string{"script"}}
	return g
}

// play runs the release graph with the model's lines, driven through
// its children, and returns the run's end and the workflow's state.
func play(t *testing.T, g *Graph, m *cast, opts ...Option) (*agentturn.Agent, *agentturn.RunEnd, State) {
	t.Helper()
	var ran []string
	cfg, err := kinApply(g, agentturn.Config{Model: m, ModelName: "worker", Tools: []agenttool.Tool{scriptTool(&ran)}}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end, _, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil && !errors.Is(err, end.Err) {
		t.Fatal(err)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil {
		t.Fatal(err)
	}
	return a, end, st
}

func stepOf(st State, node string) Step {
	for _, s := range st.Steps {
		if s.Node == node {
			return s
		}
	}
	return Step{}
}

func TestJudgeDecides(t *testing.T) {
	ctx := context.Background()
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
	if err != nil {
		t.Fatal(err)
	}
	g, ran := judged(), []string{}
	m := &cast{script: []says{found, verdict("report")}}
	cfg, err := kinApply(g, agentturn.Config{Model: m, ModelName: "worker", Tools: []agenttool.Tool{scriptTool(&ran)}},
		Delegate(Decide), JudgeModel("critic"))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	rec.Attach(a)
	if end, _, err := Drive(ctx, g, a, start(t, a)); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("reason = %s, err = %v", end.Reason, err)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if got := stepOf(st, "triage"); got.Result.Outcome != "report" || got.Result.By != ByJudge || got.Advice.Reason != "because" {
		t.Errorf("triage = %+v", got)
	}
	// The judge's look is a step of the tree.
	if st.Entries != len(st.Steps)+1 {
		t.Errorf("entries = %d, steps %d", st.Entries, len(st.Steps))
	}
	// The judge works in a child: its model, none of the work.
	req := m.requests[1]
	if in := texts(req.Input); req.Model != "critic" || len(in) != 2 || !strings.HasPrefix(in[0], invocationPrefix+"pr:triage:judge") || !strings.HasPrefix(in[1], beginning("triage")) {
		t.Errorf("judge asked %s with %q", req.Model, in)
	}
	var offered []string
	for _, tool := range req.Tools {
		offered = append(offered, tool.(*openresponses.FunctionTool).Name)
	}
	slices.Sort(offered)
	if want := []string{judgeTool, "script"}; !slices.Equal(offered, want) {
		t.Errorf("judge offered %v", offered)
	}
	if !strings.Contains(req.Instructions, "Fix real bugs.") {
		t.Errorf("judge instructions: %s", req.Instructions)
	}
	// Nothing in the record says a person did this: the program held its
	// call while the judge looked, let it go, and the judge decided.
	if got := verdicts(t, store, s.ID()); !slices.Equal(got, []string{"hold by policy", "proceed by policy", "proceed by policy"}) {
		t.Errorf("decisions = %v", got)
	}
	got, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, got)
}

func TestJudgeAdvisesAndThePersonDecides(t *testing.T) {
	g := judged()
	m := &cast{script: []says{found, verdict("fix")}}
	a, end, _ := play(t, g, m, Delegate(Advise, "triage"))
	if end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("reason = %s, err = %v", end.Reason, end.Err)
	}
	var q Question
	if err := decode([]byte(end.Pending[0].Call.Arguments), &q); err != nil || q.Advice == nil || q.Advice.Outcome != "fix" {
		t.Fatalf("question = %+v, %v", q, err)
	}
	ans, err := g.Answer(end.Pending[0], Result{Outcome: "report"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Resume(context.Background(), ans); err != nil {
		t.Fatal(err)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil {
		t.Fatal(err)
	}
	// Both are there to be compared.
	if got := stepOf(st, "triage"); got.Result.Outcome != "report" || got.Result.By != "" || got.Advice == nil || got.Advice.Outcome != "fix" {
		t.Errorf("triage = %+v", got)
	}
}

func TestJudgeHandsBack(t *testing.T) {
	_, end, st := play(t, judged(), &cast{script: []says{found, verdict(HandBack)}}, Delegate(Decide))
	if end.Reason != agentturn.ReasonInputRequired || st.Advice == nil || st.Advice.Outcome != HandBack {
		t.Fatalf("reason = %s, advice = %+v", end.Reason, st.Advice)
	}
	// With nobody there, the node's default is taken, not the judge's.
	_, end, st = play(t, judged(), &cast{script: []says{found, verdict(HandBack)}}, Delegate(Decide), Unattended())
	if got := stepOf(st, "triage"); end.Reason != agentturn.ReasonDone || got.Result.By != "unattended default" {
		t.Errorf("triage = %+v, %s", got, end.Reason)
	}
	// Advice never decides, there or not.
	_, _, st = play(t, judged(), &cast{script: []says{found, verdict("fix")}}, Delegate(Advise), Unattended())
	if got := stepOf(st, "triage"); got.Result.Outcome != "report" || got.Result.By != "unattended default" {
		t.Errorf("advised unattended: %+v", got)
	}
}

func TestJudgeKeepsToItsOutcomes(t *testing.T) {
	g := judged()
	g.node("triage").Judge.Outcomes = []string{"report"}
	// It may not choose fix, and is told so.
	m := &cast{script: []says{found, verdict("fix"), verdict(HandBack)}}
	_, end, st := play(t, g, m, Delegate(Decide))
	if end.Reason != agentturn.ReasonInputRequired || st.Advice == nil || st.Advice.Outcome != HandBack || len(m.requests) != 3 {
		t.Errorf("reason = %s, advice = %+v, %d requests", end.Reason, st.Advice, len(m.requests))
	}
}

func TestAJudgeIsOnlyWhatTheHostDelegates(t *testing.T) {
	// A file that permits a judge does not get one by itself.
	m := &cast{script: []says{found}}
	_, end, _ := play(t, judged(), m)
	if end.Reason != agentturn.ReasonInputRequired || len(m.requests) != 1 {
		t.Errorf("reason = %s, %d requests", end.Reason, len(m.requests))
	}
	for name, opt := range map[string]Option{
		"a step with no judge": Delegate(Decide, "review"),
		"no such step":         Delegate(Decide, "nope"),
		"no mode":              Delegate(0),
	} {
		if _, err := Apply(judged(), agentturn.Config{Model: m, ModelName: "w", Tools: []agenttool.Tool{scriptTool(new([]string))}}, opt); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, err := Apply(release(), agentturn.Config{Model: m, ModelName: "w", Tools: []agenttool.Tool{scriptTool(new([]string))}}, Delegate(Decide)); err == nil {
		t.Error("delegating a graph with no judge was accepted")
	}
	// The host's rules still come first.
	_, _, st := play(t, judged(), &cast{script: []says{found, verdict("fix")}}, Delegate(Decide),
		Reply(func(context.Context, Question) (Result, bool) { return Result{Outcome: "report"}, true }))
	if got := stepOf(st, "triage"); got.Result.By != "rules" {
		t.Errorf("triage = %+v", got)
	}
}

func TestTheWorkingModelCannotJudge(t *testing.T) {
	// At its own step it answers the question that comes next. The call
	// is refused, and the judge is still asked.
	m := &cast{script: []says{verdict("fix"), found, verdict("report")}}
	_, end, st := play(t, judged(), m, Delegate(Decide))
	if got := stepOf(st, "triage"); end.Reason != agentturn.ReasonDone || got.Result.Outcome != "report" || len(m.requests) != 3 {
		t.Errorf("triage = %+v after %d requests, %s %v", got, len(m.requests), end.Reason, end.Err)
	}
}

func TestDriveRemindsAJudge(t *testing.T) {
	g := judged()
	m := &cast{script: []says{found, {}, {}}}
	_, end, st := play(t, g, m, Delegate(Decide))
	// A judge that stops twice has given no verdict, and the person is
	// asked.
	if end.Reason != agentturn.ReasonInputRequired || st.Node.ID != "triage" || st.Advice == nil || !st.Advice.None || st.Advice.Outcome != HandBack {
		t.Fatalf("end = %s, state = %+v", end.Reason, st)
	}
	var nudged bool
	for _, s := range texts(m.requests[2].Input) {
		nudged = nudged || strings.HasPrefix(s, nudgePrefix) && strings.Contains(s, judgeTool)
	}
	if !nudged {
		t.Error("the judge was not told to call " + judgeTool)
	}
}

func TestEachVisitIsJudgedAfresh(t *testing.T) {
	fixed := says{finishTool, `{"step":"fix","outcome":"ok"}`}
	_, end, st := play(t, judged(), &cast{script: []says{found, verdict("fix"), fixed, found, verdict("report")}}, Delegate(Decide))
	var got []string
	for _, s := range st.Steps {
		if s.Node == "triage" {
			got = append(got, s.Result.Outcome)
		}
	}
	if end.Reason != agentturn.ReasonDone || !slices.Equal(got, []string{"fix", "report"}) {
		t.Errorf("triage = %v, %s %v", got, end.Reason, end.Err)
	}
}

func TestJudgeIsValidated(t *testing.T) {
	for want, change := range map[string]func(*Node){
		"needs criteria":          func(n *Node) { n.Judge.Criteria = " " },
		"outcomes it may choose":  func(n *Node) { n.Judge.Outcomes = nil },
		`undeclared outcome "no"`: func(n *Node) { n.Judge.Outcomes = []string{"no"} },
		`outcome "ask"`:           func(n *Node) { n.Outcomes = []string{"fix", "report", "ask"} },
		"cannot call":             func(n *Node) { n.Judge.Tools = []string{ScriptTool} },
	} {
		g := judged()
		change(g.node("triage"))
		if err := g.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	g := judged()
	g.node("review").Judge = &Judge{Criteria: "x", Outcomes: []string{"clean"}}
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "only a person node") {
		t.Errorf("err = %v", err)
	}
}

// The work goes round: report is the judge sending it back.
func TestAJudgeCannotSendWorkBackForever(t *testing.T) {
	fixed := says{finishTool, `{"step":"fix","outcome":"ok"}`}
	round := []says{found, verdict("fix"), fixed}
	lines := slices.Concat(round, round, []says{found, verdict("fix")})
	g := judged()
	for i := range g.Nodes {
		g.Nodes[i].MaxVisits = 9
	}
	_, end, st := play(t, g, &cast{script: lines}, Delegate(Decide))
	if end.Reason != agentturn.ReasonInputRequired || st.Node.ID != "triage" || st.Advice.Outcome != "fix" {
		t.Fatalf("the third send-back was not held: %s %v, %+v", end.Reason, end.Err, st)
	}
	var q Question
	if err := decode([]byte(end.Pending[0].Call.Arguments), &q); err != nil || q.SentBack != DefaultReturns {
		t.Errorf("question = %+v, %v", q, err)
	}
	// A person's answer starts the count again.
	if n := g.sentBack(st, g.node("triage")); n != 2 {
		t.Errorf("sent back %d times", n)
	}
	st.Steps = append(st.Steps, Step{Node: "triage", Result: Result{Outcome: "fix"}})
	if n := g.sentBack(st, g.node("triage")); n != 0 {
		t.Errorf("after a person, sent back %d times", n)
	}

	// A verdict that moves on still stands after the limit.
	lines = slices.Concat(round, round, []says{found, verdict("report")})
	_, end, st = play(t, g, &cast{script: lines}, Delegate(Decide))
	if got := st.Steps[len(st.Steps)-2]; end.Reason != agentturn.ReasonDone || got.Result.Outcome != "report" || got.Result.By != ByJudge {
		t.Errorf("triage = %+v, %s %v", got, end.Reason, end.Err)
	}

	// Nobody there: the default, and the node says how often.
	g.node("triage").Judge.Returns = 1
	lines = slices.Concat(round, []says{found, verdict("fix")})
	_, end, st = play(t, g, &cast{script: lines}, Delegate(Decide), Unattended())
	if got := st.Steps[len(st.Steps)-2]; end.Reason != agentturn.ReasonDone || got.Result.By != "unattended default" {
		t.Errorf("triage = %+v, %s %v", got, end.Reason, end.Err)
	}
}
