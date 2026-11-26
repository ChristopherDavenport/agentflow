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
)

var built = &agenttool.Schema{Type: "object", Required: []string{"version", "files"}, Properties: []agenttool.Property{
	{Name: "version", Schema: &agenttool.Schema{Type: "string"}},
	{Name: "files", Schema: &agenttool.Schema{Type: "array"}},
}}

// builder makes a release whose question names the version it built.
func builder(version string) agenttool.Tool {
	return agenttool.New("build", "Build.", func(context.Context, struct{}) (Result, error) {
		return Result{Outcome: OK, Data: map[string]any{"version": version, "files": []any{"a", "b"}}}, nil
	})
}

func shipper() *Graph {
	return &Graph{Name: "shipper", Nodes: []Node{
		{ID: "build", Kind: Run, Tool: "build", Data: map[string]*agenttool.Schema{OK: built}, Next: map[string]string{OK: "ship"}},
		{ID: "ship", Kind: Person, Prompt: "Ship ${{ steps.build.outputs.version }} with ${{ steps.build.outputs.files }}?", Outcomes: []string{"yes", "no"}},
	}}
}

func TestAQuestionIsFilledInAndKeepsItsTemplate(t *testing.T) {
	g := shipper()
	first := question(t, start(t, nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{builder("1.2.0")})))
	if first.Question != `Ship 1.2.0 with ["a","b"]?` || first.Template != g.Nodes[1].Prompt {
		t.Fatalf("question = %q, template %q", first.Question, first.Template)
	}
	second := question(t, start(t, nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{builder("1.3.0")})))
	if second.Question != `Ship 1.3.0 with ["a","b"]?` {
		t.Errorf("question = %q", second.Question)
	}
	// Answers to both are answers to one question: its version is read
	// from what the step says, not from what it was filled in with.
	if first.Version == "" || first.Version != second.Version || first.Template != second.Template {
		t.Errorf("versions %s, %s", first.Version, second.Version)
	}
}

func TestAQuestionWithNoValuesHasNoTemplate(t *testing.T) {
	g := shipper()
	g.Nodes[1].Prompt = "Ship it?"
	q := question(t, start(t, nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{builder("1.2.0")})))
	if q.Question != "Ship it?" || q.Template != "" {
		t.Errorf("question = %+v", q)
	}
}

func TestQuestionTextIsCheckedAtLoad(t *testing.T) {
	for prompt, want := range map[string]string{
		"Ship ${{ steps.buld.outputs.version }}?":           `no step "buld"; did you mean "build"?`,
		"Ship ${{ steps.build.outputs.versoin }}?":          `"build" does not certainly produce "versoin"`,
		"Ship ${{ steps.build.outputs.version ?":            "never closed",
		"Ship ${{ steps.build.outputs.version + 1 }}?":      "want a reference",
		"Ship ${{ latest(steps.build.outputs.version) }}?":  "one reference needs no latest",
		"Ship ${{ optional(steps.ship.outputs.why) }} now?": "",
	} {
		g := shipper()
		g.Nodes[1].Prompt = prompt
		err := g.Validate()
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", prompt, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: err = %v, want %q", prompt, err, want)
		}
	}
	g := shipper()
	g.Nodes[1] = Node{ID: "ship", Kind: Agent, Prompt: "Ship ${{ steps.build.outputs.version }}."}
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "only a person step's question takes ${{ }}") {
		t.Errorf("a model step's prompt: %v", err)
	}
}

func TestAJudgeReadsTheQuestionAsItIsAsked(t *testing.T) {
	g := shipper()
	g.Nodes[1].Judge = &Judge{Criteria: "Ship only a release.", Outcomes: []string{"yes", "no"}}
	m := &cast{script: []says{{judgeTool, `{"step":"ship","outcome":"yes","reason":"fine"}`}}}
	k := newKin(m, "fake")
	a := nestedAgent(t, g, m, []agenttool.Tool{builder("2.0.0")}, k.option(), Delegate(Advise, "ship"))
	end, _, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if q := question(t, end); q.Advice == nil || q.Advice.Outcome != "yes" {
		t.Fatalf("question = %+v", q)
	}
	if len(m.requests) != 1 || !strings.Contains(m.requests[0].Instructions, `Ship 2.0.0 with ["a","b"]?`) {
		t.Errorf("the judge read %q", m.requests[0].Instructions)
	}
}

// A question pending in a recorded session reads the same from the store:
// what it was filled in with is in the record, not worked out again.
func TestAFilledInQuestionResumesFromTheStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	g := shipper()
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{builder("3.1.0")})
	rec.Attach(a)
	asked := question(t, start(t, a))
	if err := store.Sync(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	rec2, s2, err := session.Resume(ctx, store, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	seed, err := session.AgentOptions(s2, rec2.ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	// The new process's build would say another version; nothing runs it.
	b := agentturn.New(must(Apply(g, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: []agenttool.Tool{builder("9.9.9")}})), seed...)
	rec2.Attach(b)
	pending := b.State().Pending
	again := question(t, &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: pending})
	if again.Question != asked.Question || again.Version != asked.Version {
		t.Fatalf("asked %q, then %q", asked.Question, again.Question)
	}
	ans, err := g.Answer(pending[0], Result{Outcome: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	end, err := b.Resume(ctx, ans)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	got, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, got)
}
