package agentflow

import (
	"context"
	"encoding/json"
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

const secret = "the judge's private view"

func unseen(outcome string) says {
	return says{judgeTool, `{"step":"triage","outcome":"` + outcome + `","reason":"` + secret + `"}`}
}

var fixDone = says{finishTool, `{"step":"fix","outcome":"ok"}`}

// answered runs to the question, checks nothing has judged it, and gives
// the person's answer.
func answered(t *testing.T, g *Graph, m *cast, a *agentturn.Agent, end *agentturn.RunEnd, res Result) *agentturn.RunEnd {
	t.Helper()
	if end.Reason != agentturn.ReasonInputRequired || len(m.requests) != 1 {
		t.Fatalf("reason = %s after %d requests, %v", end.Reason, len(m.requests), end.Err)
	}
	var q Question
	if err := decode([]byte(end.Pending[0].Call.Arguments), &q); err != nil || q.Mode != "shadow" || q.Advice != nil {
		t.Fatalf("question = %+v, %v", q, err)
	}
	ans, err := g.Answer(end.Pending[0], res)
	if err != nil {
		t.Fatal(err)
	}
	end, err = a.Resume(context.Background(), ans)
	if err != nil {
		t.Fatal(err)
	}
	return end
}

// everything is a request's whole content, to search.
func everything(t *testing.T, req openresponses.Request) string {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestShadowJudgesUnseen(t *testing.T) {
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
	look := says{"script", `{"name":"look"}`}
	// The person says fix; the judge, who looks first, would have
	// reported. Then the work goes round and triage is not reached.
	m := &cast{script: []says{found, look, unseen("report"), fixDone, {finishTool, `{"step":"review","outcome":"clean"}`}}}
	cfg, err := kinApply(g, agentturn.Config{Model: m, ModelName: "worker", Tools: []agenttool.Tool{scriptTool(&ran)}}, Delegate(Shadow))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	rec.Attach(a)
	end := answered(t, g, m, a, start(t, a), Result{Outcome: "fix", Reason: "the race is real, zzz-person"})
	if end, _, err = Drive(ctx, g, a, end); err != nil {
		t.Fatal(err)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("end = %s %v, %v", end.Reason, end.Err, err)
	}
	got := stepOf(st, "triage")
	if got.Result.Outcome != "fix" || got.Result.By != "" || got.Result.Reason != "the race is real, zzz-person" ||
		got.Advice == nil || got.Advice.Outcome != "report" || !got.Blind {
		t.Errorf("triage = %+v", got)
	}
	// The judge never saw the answer.
	for _, req := range m.requests[1:3] {
		if all := everything(t, req); strings.Contains(all, "zzz-person") || !strings.Contains(all, "You are judging") {
			t.Errorf("judge read %s", all)
		}
	}
	// The work never sees the judge: not its words, its calls or what it
	// looked at.
	for i, req := range m.requests[3:] {
		all := everything(t, req)
		for _, leak := range []string{secret, judgeTool, "look", beginning("triage")} {
			if strings.Contains(all, leak) {
				t.Errorf("request %d after the judge holds %q: %s", i, leak, all)
			}
		}
	}
	// It does see what the person said.
	if !strings.Contains(everything(t, m.requests[3]), "zzz-person") {
		t.Error("the person's answer was dropped with the judge")
	}
	got2, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, got2)
}

func TestShadowNeverHoldsUpThePerson(t *testing.T) {
	for name, lines := range map[string][]says{
		"a judge that only talks": {found, {}, {}},
		"a judge refused twice":   {found, unseen("nope"), unseen("nope")},
		"a judge that hands back": {found, unseen(HandBack)},
	} {
		g := judged()
		m := &cast{script: append(slices.Clone(lines), fixDone, says{finishTool, `{"step":"review","outcome":"clean"}`})}
		a, end, _ := play(t, g, m, Delegate(Shadow))
		end = answered(t, g, m, a, end, Result{Outcome: "fix"})
		end, st, err := Drive(context.Background(), g, a, end)
		if err != nil || !st.Done {
			t.Fatalf("%s: done = %v, %v, %s", name, st.Done, err, end.Reason)
		}
		got := stepOf(st, "triage")
		handed := got.Advice != nil && got.Advice.Outcome == HandBack
		if got.Result.Outcome != "fix" || !got.Blind || handed != (name == "a judge that hands back") || !handed && got.Advice != nil {
			t.Errorf("%s: triage = %+v", name, got)
		}
	}
}

func TestShadowJudgesOnlyPeople(t *testing.T) {
	for name, opt := range map[string]Option{
		"nobody there": Unattended(),
		"rules":        Reply(func(context.Context, Question) (Result, bool) { return Result{Outcome: "report"}, true }),
	} {
		m := &cast{script: []says{found}}
		_, end, st := play(t, judged(), m, Delegate(Shadow), opt)
		if got := stepOf(st, "triage"); end.Reason != agentturn.ReasonDone || len(m.requests) != 1 || got.Advice != nil || got.Blind {
			t.Errorf("%s: triage = %+v, %d requests, %s", name, got, len(m.requests), end.Reason)
		}
	}
}

func TestADecidingJudgesReasonIsKept(t *testing.T) {
	_, _, st := play(t, judged(), &cast{script: []says{found, verdict("report")}}, Delegate(Decide))
	if got := stepOf(st, "triage"); got.Result.Reason != "because" || got.Blind {
		t.Errorf("triage = %+v", got)
	}
}

func TestAJudgeIsDrivenAndNeverPrompted(t *testing.T) {
	g := judged()
	m := &cast{script: []says{found, unseen("report"), fixDone, {finishTool, `{"step":"review","outcome":"clean"}`}}}
	a, end, _ := play(t, g, m, Delegate(Shadow))
	end = answered(t, g, m, a, end, Result{Outcome: "fix"})
	// The program holds its call while the judge looks in its child, so
	// nothing the person says next reaches the judge or moves on past it.
	if _, err := a.Prompt(context.Background(), openresponses.UserText("go on")); !errors.Is(err, agentturn.ErrInputRequired) {
		t.Fatalf("prompt over a held judge: %v", err)
	}
	end, st, err := Drive(context.Background(), g, a, end)
	if got := stepOf(st, "triage"); err != nil || !st.Done || got.Result.Outcome != "fix" || got.Advice == nil || got.Advice.Outcome != "report" {
		t.Errorf("done = %v, triage = %+v, %s %v", st.Done, got, end.Reason, err)
	}
}
