package agentflow

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// The RFC plans to read decisions out of sessions later. This holds the
// record to what that will need, read from the entries alone: neither the
// graph nor Replay is there to help with an old session.
func TestTheRecordHoldsWhatALaterReadingNeeds(t *testing.T) {
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
	m := &cast{script: []says{found, unseen("report"), fixDone, {finishTool, `{"step":"review","outcome":"clean"}`}}}
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "worker", Tools: []agenttool.Tool{scriptTool(&ran)}},
		Delegate(Shadow), JudgeModel("critic"))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	rec.Attach(a)
	answered(t, g, m, a, start(t, a), Result{Outcome: "fix", Reason: "it is real"})

	got, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	var (
		started, question, answer, verdict, judge string
		asked, decided                            bool
	)
	for _, e := range got.Path(got.Leaf()) {
		switch e := e.(type) {
		case *agentsession.ItemEntry:
			switch it := e.Item.(type) {
			case *openresponses.FunctionCall:
				switch it.Name {
				case startTool:
					started = it.Arguments
				case askTool:
					question, asked = it.Arguments, !e.Timestamp.IsZero()
				case judgeTool:
					verdict = it.Arguments
				}
			case *openresponses.FunctionCallOutput:
				if strings.HasSuffix(it.CallID, "_triage_v1") {
					answer = it.Output.String()
				}
			}
		case *agentsession.DecisionEntry:
			decided = decided || string(e.By) == "human" && !e.Timestamp.IsZero()
		case *agentsession.ResponseEntry:
			if e.Model == "critic" {
				judge = e.Model
			}
		}
	}
	for what, have := range map[string]bool{
		"the workflow and its digest": strings.Contains(started, `"workflow":"pr"`) && strings.Contains(started, `"digest":"sha256:`),
		"the question and its step":   strings.Contains(question, `"step":"triage"`) && strings.Contains(question, "Fix them"),
		"the step's version":          strings.Contains(question, `"version":"`+g.node("triage").version()),
		"the mode":                    strings.Contains(question, `"mode":"shadow"`),
		"the answer and why":          strings.Contains(answer, `"outcome":"fix"`) && strings.Contains(answer, "it is real"),
		"that a person gave it":       decided,
		"when it was asked":           asked,
		"the verdict and why":         strings.Contains(verdict, `"outcome":"report"`) && strings.Contains(verdict, secret),
		"the model that judged":       judge == "critic",
	} {
		if !have {
			t.Errorf("the session does not hold %s", what)
		}
	}
	// A reworded step is another version; an unrelated one is not.
	before := g.node("triage").version()
	g.node("report").Args = nil
	if g.node("triage").version() != before {
		t.Error("another step's change gave this one a new version")
	}
	g.node("triage").Judge.Criteria = "Fix everything."
	if g.node("triage").version() == before {
		t.Error("new criteria kept the old version")
	}
}
