package agentflow

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentturn"
)

// described is the release graph with a description on the workflow and
// every step, a judge that decides and a fix done in a child.
func described() *Graph {
	g := judged()
	g.Description = "DESC-workflow"
	for i := range g.Nodes {
		g.Nodes[i].Description = "DESC-" + g.Nodes[i].ID
	}
	g.node("fix").Context = ContextFresh
	return g
}

func TestNoModelIsSentADescription(t *testing.T) {
	g := described()
	m := &cast{script: []says{found, verdict("fix"), {finishTool, `{"step":"fix","outcome":"ok"}`}, {finishTool, `{"step":"review","outcome":"clean"}`}}}
	a, end, st := play(t, g, m, Delegate(Decide))
	if end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("reason = %s, path %v", end.Reason, st.Steps)
	}
	// The working model, the child doing the fix and the judge were each
	// asked; none was sent a description.
	if len(m.requests) < 4 {
		t.Fatalf("%d requests", len(m.requests))
	}
	for i, req := range m.requests {
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "DESC-") {
			t.Errorf("request %d (%s) holds a description", i, req.Model)
		}
	}
	if b, _ := json.Marshal(SummaryFilter(a.State().Transcript)); strings.Contains(string(b), "DESC-") {
		t.Error("a summarizer reads a description")
	}
	// The record holds them, in the definition its start pins.
	runs, err := Runs(a.State().Transcript, under(a))
	if err != nil {
		t.Fatal(err)
	}
	if got := runs[0].Graph.Describe("triage"); got != "DESC-triage" {
		t.Errorf("triage reads back as %q", got)
	}
	if got := runs[0].Graph.Describe(""); got != "DESC-workflow" {
		t.Errorf("the workflow reads back as %q", got)
	}
}

func TestAFrontReadsAQuestionsDescription(t *testing.T) {
	g := described()
	g.node("triage").Judge = nil
	var ran []string
	a := newAgent(t, g, &fakeModel{outcomes: []string{"findings"}}, &ran)
	end := start(t, a)
	if end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("reason = %s", end.Reason)
	}
	p := end.Pending[0]
	if strings.Contains(p.Call.Arguments, "DESC-") {
		t.Errorf("the question's arguments, which a model reads, hold the description: %s", p.Call.Arguments)
	}
	q, err := g.Question(p)
	if err != nil {
		t.Fatal(err)
	}
	if q.Step != "triage" || q.Description != "DESC-triage" {
		t.Errorf("question = %+v", q)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if st.Node.Description != "DESC-triage" {
		t.Errorf("state's step = %q", st.Node.Description)
	}
	// A description is part of the step's definition, so its question's
	// version names it.
	other := described()
	other.node("triage").Description = "reworded"
	if fmt.Sprint(other.node("triage").version()) == q.Version {
		t.Error("a new description keeps the version")
	}
}

func TestAStepsSkillsAreInItsInstructions(t *testing.T) {
	for _, context := range []string{ContextSession, ContextFresh} {
		t.Run(context, func(t *testing.T) {
			g := release()
			review := g.node("review")
			review.Context = context
			review.Skills = []StepSkill{{Name: "go-style", Instructions: "Wrap errors with %w."}}
			var ran []string
			m := &fakeModel{outcomes: []string{"clean"}}
			a := newAgent(t, g, m, &ran)
			end, _, err := Drive(t.Context(), g, a, start(t, a))
			if err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("reason = %s, err = %v", end.Reason, err)
			}
			var told bool
			for _, req := range m.requests {
				told = told || strings.Contains(req.Instructions, "<skill name=\"go-style\">\nWrap errors with %w.\n</skill>")
			}
			if !told {
				t.Errorf("no request was told the skill: %q", m.requests[0].Instructions)
			}
		})
	}
}

func TestASkillMustBeRead(t *testing.T) {
	g := release()
	g.node("review").Skills = []StepSkill{{Name: "go-style"}}
	_, err := kinApply(g, agentturn.Config{Model: &fakeModel{}, ModelName: "fake"})
	if err == nil || !strings.Contains(err.Error(), "whose instructions were never read") {
		t.Errorf("err = %v", err)
	}
	g.node("review").Skills = nil
	g.node("triage").Skills = []StepSkill{{Name: "go-style", Instructions: "x"}}
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "skills are a model step's") {
		t.Errorf("a person's skills: %v", err)
	}
}
