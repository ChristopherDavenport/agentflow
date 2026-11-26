package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// release is gather (program), review (model), then triage (person)
// when the review found something, a fix (model) and gather again.
func release() *Graph {
	return &Graph{Name: "pr", Nodes: []Node{
		{ID: "gather", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"gather"}`),
			Next: map[string]string{OK: "review"}, MaxVisits: 2},
		{ID: "review", Kind: Agent, Prompt: "Review the diff.", Outcomes: []string{"clean", "findings"},
			Next: map[string]string{"clean": "report", "findings": "triage"}, MaxVisits: 2},
		{ID: "triage", Kind: Person, Prompt: "Fix them, or report?", Outcomes: []string{"fix", "report"},
			Next: map[string]string{"fix": "fix", "report": "report"}, Unattended: "report"},
		{ID: "fix", Kind: Agent, Prompt: "Fix the findings.", Next: map[string]string{OK: "gather"}},
		{ID: "report", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"report"}`)},
	}}
}

type scriptArgs struct {
	Name string `json:"name"`
}

func scriptTool(ran *[]string) agenttool.Tool {
	return agenttool.New("script", "Run a script.", func(_ context.Context, a scriptArgs) (Result, error) {
		*ran = append(*ran, a.Name)
		return Result{Outcome: OK}, nil
	})
}

// fakeModel finishes whatever step it is asked about with the next of
// its outcomes. It reads the step from the finish call it is told to
// make, as a model would from the instructions.
type fakeModel struct {
	outcomes []string
	requests []openresponses.Request
}

func (m *fakeModel) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.requests = append(m.requests, req)
	if len(m.outcomes) == 0 {
		return errors.New("fake model: out of outcomes")
	}
	_, rest, _ := strings.Cut(req.Instructions, `with step "`)
	step, _, _ := strings.Cut(rest, `"`)
	args, err := json.Marshal(finishArgs{Step: step, Outcome: m.outcomes[0]})
	if err != nil {
		return err
	}
	m.outcomes = m.outcomes[1:]
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	call, err := e.FunctionCall("", finishTool)
	if err != nil {
		return err
	}
	if err := call.Arguments(string(args)); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

func newAgent(t *testing.T, g *Graph, m *fakeModel, ran *[]string, opts ...Option) *agentturn.Agent {
	t.Helper()
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(ran)}}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return agentturn.New(cfg)
}

func path(t *testing.T, g *Graph, a *agentturn.Agent) []string {
	t.Helper()
	st, err := g.Replay(a.State().Transcript)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range st.Steps {
		out = append(out, s.Node+":"+s.Result.Outcome)
	}
	return out
}

func start(t *testing.T, a *agentturn.Agent) *agentturn.RunEnd {
	t.Helper()
	end, err := a.Prompt(context.Background(), openresponses.UserText("review this PR"))
	if err != nil {
		t.Fatal(err)
	}
	return end
}

func TestNoQuestionWhenClean(t *testing.T) {
	g, m, ran := release(), &fakeModel{outcomes: []string{"clean"}}, []string{}
	a := newAgent(t, g, m, &ran)
	end := start(t, a)
	if end.Reason != agentturn.ReasonDone {
		t.Fatalf("reason = %s, err = %v", end.Reason, end.Err)
	}
	if want := []string{"gather:ok", "review:clean", "report:ok"}; !slices.Equal(path(t, g, a), want) {
		t.Errorf("path = %v, want %v", path(t, g, a), want)
	}
	if len(m.requests) != 1 {
		t.Fatalf("the model took %d turns, want 1", len(m.requests))
	}
	var offered []string
	for _, tool := range m.requests[0].Tools {
		offered = append(offered, tool.(*openresponses.FunctionTool).Name)
	}
	if want := []string{finishTool}; !slices.Equal(offered, want) {
		t.Errorf("offered = %v, want %v", offered, want)
	}
}

func TestPersonAnswersAndLoops(t *testing.T) {
	g, m, ran := release(), &fakeModel{outcomes: []string{"findings", "ok", "clean"}}, []string{}
	a := newAgent(t, g, m, &ran)
	end := start(t, a)
	if end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 {
		t.Fatalf("reason = %s, pending = %d, err = %v", end.Reason, len(end.Pending), end.Err)
	}
	ans, err := Answer(end.Pending[0], Result{Outcome: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	if end, err = a.Resume(context.Background(), ans); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("resume: %v, %+v", err, end)
	}
	want := []string{"gather:ok", "review:findings", "triage:fix", "fix:ok", "gather:ok", "review:clean", "report:ok"}
	if got := path(t, g, a); !slices.Equal(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
}

func TestUnattendedTakesDefault(t *testing.T) {
	g, m, ran := release(), &fakeModel{outcomes: []string{"findings"}}, []string{}
	a := newAgent(t, g, m, &ran, Unattended())
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("reason = %s, err = %v", end.Reason, end.Err)
	}
	want := []string{"gather:ok", "review:findings", "triage:report", "report:ok"}
	if got := path(t, g, a); !slices.Equal(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
}

func TestUndeclaredOutcomeIsNotProgress(t *testing.T) {
	g, m, ran := release(), &fakeModel{outcomes: []string{"looks fine", "clean"}}, []string{}
	a := newAgent(t, g, m, &ran)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("reason = %s, err = %v", end.Reason, end.Err)
	}
	want := []string{"gather:ok", "review:clean", "report:ok"}
	if got := path(t, g, a); !slices.Equal(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
}

func TestFailedScriptStops(t *testing.T) {
	g := release()
	failing := agenttool.New("script", "Run a script.", func(context.Context, scriptArgs) (Result, error) {
		return Result{}, errors.New("exit status 1")
	})
	cfg, err := Apply(g, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: []agenttool.Tool{failing}})
	if err != nil {
		t.Fatal(err)
	}
	end, _ := agentturn.New(cfg).Prompt(context.Background(), openresponses.UserText("go"))
	if end == nil || end.Reason != agentturn.ReasonError || !errors.Is(end.Err, ErrStepFailed) {
		t.Fatalf("end = %+v", end)
	}
}

func TestVisitLimit(t *testing.T) {
	g, m, ran := release(), &fakeModel{outcomes: []string{"findings", "ok", "findings"}}, []string{}
	g.node("gather").MaxVisits = 1
	a := newAgent(t, g, m, &ran)
	end := start(t, a)
	ans, err := Answer(end.Pending[0], Result{Outcome: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	end, _ = a.Resume(context.Background(), ans)
	if end == nil || !errors.Is(end.Err, ErrVisits) {
		t.Fatalf("end = %+v", end)
	}
}

func TestPolicyStillDecides(t *testing.T) {
	g, ran := release(), []string{}
	cfg, err := Apply(g, agentturn.Config{
		Model: &fakeModel{outcomes: []string{"clean"}}, ModelName: "fake",
		Tools: []agenttool.Tool{scriptTool(&ran)},
		BeforeToolCall: func(_ context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
			if info.Call.Name == "script" {
				return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: "ask first"}, nil
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end := start(t, a)
	if end.Reason != agentturn.ReasonInputRequired || len(ran) != 0 {
		t.Fatalf("reason = %s, ran = %v", end.Reason, ran)
	}
	if end, err = a.Resume(context.Background(), agentturn.Approve(end.Pending[0].Call.CallID)); err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired || !slices.Equal(ran, []string{"gather"}) {
		t.Fatalf("reason = %s, ran = %v", end.Reason, ran)
	}
}

func TestValidate(t *testing.T) {
	if err := release().Validate(); err != nil {
		t.Fatal(err)
	}
	g := release()
	g.node("review").Next["maybe"] = "nowhere"
	if err := g.Validate(); err == nil {
		t.Fatal("want an error")
	}
}
