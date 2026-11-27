package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
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

// scriptTool runs a script that gives a new result each time, as one
// that reads a changing repository does, so no loop it is on repeats.
func scriptTool(ran *[]string) agenttool.Tool {
	return agenttool.New("script", "Run a script.", func(_ context.Context, a scriptArgs) (Result, error) {
		*ran = append(*ran, a.Name)
		return Result{Outcome: OK, Data: map[string]any{"run": len(*ran)}}, nil
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
	// Outside a step, or asked for silence, it only talks.
	if step == "" || m.outcomes[0] == "" {
		m.outcomes = m.outcomes[1:]
		return say(sink, req.Model, "some text")
	}
	args, err := json.Marshal(finishArgs{Run: runIn(req), Step: step, Outcome: m.outcomes[0]})
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

// runIn is the run a request's instructions tell the model to name.
func runIn(req openresponses.Request) string {
	_, rest, ok := strings.Cut(req.Instructions, `, run "`)
	if !ok {
		return ""
	}
	run, _, _ := strings.Cut(rest, `"`)
	return run
}

// withRun adds to a finish or a verdict the run the request names, as a
// model reads it from its instructions.
func withRun(req openresponses.Request, tool, args string) string {
	if tool != finishTool && tool != judgeTool && tool != stuckTool && tool != docReadTool && tool != docWriteTool {
		return args
	}
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || m == nil || m["run"] != nil {
		return args
	}
	run := runIn(req)
	if run == "" {
		return args
	}
	m["run"] = run
	b, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return string(b)
}

func say(sink openresponses.EventSink, model, text string) error {
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: model})
	msg, err := e.Message("")
	if err != nil {
		return err
	}
	if err := msg.Text(text); err != nil {
		return err
	}
	if err := msg.Close(); err != nil {
		return err
	}
	return e.Complete()
}

func newAgent(t *testing.T, g *Graph, m *fakeModel, ran *[]string, opts ...Option) *agentturn.Agent {
	t.Helper()
	cfg, err := kinApply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(ran)}}, opts...)
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
	return through(t, a, end)
}

// through works on the children a's calls to workflows wait on, as Drive
// does, and goes on until a's run ends or something waits on a person. It
// leaves a fresh step's child and a judge's to the test, and reminds no
// model of a's.
func through(t *testing.T, a *agentturn.Agent, end *agentturn.RunEnd) *agentturn.RunEnd {
	t.Helper()
	out, err := drift(a, end)
	if out == nil {
		t.Fatal(err)
	}
	return out
}

// drift is through, with the error a child's work gave.
func drift(a *agentturn.Agent, end *agentturn.RunEnd) (*agentturn.RunEnd, error) {
	ctx := context.Background()
	f, ok := a.Config().Model.(*flow)
	for ok && f.children != nil && end != nil && end.Reason == agentturn.ReasonInputRequired {
		p, args, held := heldChild(end.Pending)
		if !held || args.Context != ContextSession {
			break
		}
		st, _, err := f.read(ctx, a.State().Transcript)
		if err != nil {
			return nil, err
		}
		ans, wait, _, err := f.settle(ctx, a.State().Transcript, st, p, args, nil)
		if ans == nil {
			return wait, err
		}
		next, err := a.Resume(ctx, *ans)
		if next == nil {
			return nil, err
		}
		end = next
	}
	return end, nil
}

// carry answers what the tree under a waits on, wherever it waits, and
// goes on as through does.
func carry(t *testing.T, a *agentturn.Agent, ans agentturn.Answer) (*agentturn.RunEnd, error) {
	t.Helper()
	ctx := context.Background()
	if slices.ContainsFunc(a.State().Pending, func(p agentturn.PendingCall) bool { return p.Call != nil && p.Call.CallID == ans.CallID }) {
		end, err := a.Resume(ctx, ans)
		if end == nil {
			return nil, err
		}
		return through(t, a, end), err
	}
	f, ok := a.Config().Model.(*flow)
	if !ok {
		return a.Resume(ctx, ans)
	}
	st, _, err := f.read(ctx, a.State().Transcript)
	if err != nil {
		return nil, err
	}
	p, args, held := heldChild(a.State().Pending)
	if !held {
		return a.Resume(ctx, ans)
	}
	let, wait, _, err := f.settle(ctx, a.State().Transcript, st, p, args, []agentturn.Answer{ans})
	if let == nil {
		return wait, err
	}
	end, err := a.Resume(ctx, *let)
	if end == nil {
		return nil, err
	}
	return through(t, a, end), err
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
	if want := []string{finishTool, stuckTool}; !slices.Equal(offered, want) {
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
	ans, err := g.Answer(end.Pending[0], Result{Outcome: "fix"})
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

// flaky is a script tool that fails its first failures calls.
func flaky(failures int, ran *[]string) agenttool.Tool {
	return agenttool.New("script", "Run a script.", func(_ context.Context, a scriptArgs) (Result, error) {
		*ran = append(*ran, a.Name)
		if len(*ran) <= failures {
			return Result{}, errors.New("exit status 1")
		}
		return Result{Outcome: OK}, nil
	})
}

func answer(t *testing.T, a *agentturn.Agent, end *agentturn.RunEnd, outcome string) *agentturn.RunEnd {
	t.Helper()
	if end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 {
		t.Fatalf("end = %s, err = %v", end.Reason, end.Err)
	}
	ans, err := release().Answer(end.Pending[0], Result{Outcome: outcome})
	if err != nil {
		t.Fatal(err)
	}
	next, err := a.Resume(context.Background(), ans)
	if next == nil {
		t.Fatal(err)
	}
	return next
}

func TestFailedScriptAsksToRetry(t *testing.T) {
	var ran []string
	g := release()
	a := agentturn.New(apply(t, g, &fakeModel{outcomes: []string{"clean"}}, flaky(2, &ran)))
	end := start(t, a)
	end = answer(t, a, end, "retry")
	end = answer(t, a, end, "retry")
	if end.Reason != agentturn.ReasonDone || end.Err != nil {
		t.Fatalf("end = %s, err = %v", end.Reason, end.Err)
	}
	if want := []string{"gather", "gather", "gather", "report"}; !slices.Equal(ran, want) {
		t.Errorf("ran = %v, want %v", ran, want)
	}
}

func TestStoppedStepEndsTheWorkflow(t *testing.T) {
	var ran []string
	a := agentturn.New(apply(t, release(), &fakeModel{}, flaky(1, &ran)))
	end := answer(t, a, start(t, a), "stop")
	if !errors.Is(end.Err, ErrStepFailed) || len(ran) != 1 {
		t.Fatalf("end = %+v, ran = %v", end, ran)
	}
}

func TestUnattendedFailureIsFinal(t *testing.T) {
	declines := Reply(func(context.Context, Question) (Result, bool) { return Result{}, false })
	for name, opts := range map[string][]Option{"alone": {Unattended()}, "rules decline": {Unattended(), declines}} {
		t.Run(name, func(t *testing.T) {
			var ran []string
			cfg, err := kinApply(release(), agentturn.Config{Model: &fakeModel{}, ModelName: "fake", MaxTurns: 10, Tools: []agenttool.Tool{flaky(1, &ran)}}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			end, _ := agentturn.New(cfg).Prompt(context.Background(), openresponses.UserText("go"))
			if end == nil || !errors.Is(end.Err, ErrStepFailed) || len(ran) != 1 {
				t.Fatalf("end = %+v, ran = %v", end, ran)
			}
		})
	}
}

func TestRulesAnswerBeforeThePerson(t *testing.T) {
	g, ran := release(), []string{}
	a := newAgent(t, g, &fakeModel{outcomes: []string{"findings", "ok", "clean"}}, &ran,
		Reply(func(_ context.Context, q Question) (Result, bool) { return Result{Outcome: "fix"}, q.Step == "triage" }))
	if end := start(t, a); end.Reason != agentturn.ReasonDone || end.Err != nil {
		t.Fatalf("end = %s, err = %v", end.Reason, end.Err)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil || len(st.Steps) != 7 || st.Steps[2].Result.By != "rules" {
		t.Fatalf("steps = %+v, %v", st.Steps, err)
	}
}

func TestAfterTheWorkflow(t *testing.T) {
	g, ran := release(), []string{}
	m := &fakeModel{outcomes: []string{"clean", "", "clean"}}
	a := newAgent(t, g, m, &ran)
	start(t, a)
	first, err := g.Replay(a.State().Transcript)
	if err != nil {
		t.Fatal(err)
	}

	// A plain prompt is a conversation with the model, with its tools.
	ran = ran[:0]
	if got, _ := start(t, a).Answer(); got != "some text" || len(ran) != 0 {
		t.Fatalf("answer = %q, ran = %v", got, ran)
	}
	last := m.requests[len(m.requests)-1]
	if len(last.Tools) != 1 || last.Tools[0].(*openresponses.FunctionTool).Name != "script" {
		t.Errorf("tools = %+v", last.Tools)
	}

	// An invocation starts another instance.
	again, err := g.Invocation(nil)
	if err != nil {
		t.Fatal(err)
	}
	if end, err := a.Prompt(context.Background(), again); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	second, err := g.Replay(a.State().Transcript)
	if err != nil || second.Instance == first.Instance || len(second.Steps) != 3 {
		t.Fatalf("second = %+v, %v", second, err)
	}
	if want := []string{"gather", "report"}; !slices.Equal(ran, want) {
		t.Errorf("ran = %v, want %v", ran, want)
	}
}

func TestChangedDefinitionIsRefused(t *testing.T) {
	g, ran := release(), []string{}
	a := newAgent(t, g, &fakeModel{outcomes: []string{"findings"}}, &ran)
	start(t, a)
	changed := release()
	changed.node("report").Args = json.RawMessage(`{"name":"other"}`)
	if _, err := changed.Replay(a.State().Transcript); !errors.Is(err, ErrChanged) {
		t.Fatalf("err = %v", err)
	}
}

func TestDriveRemindsOnce(t *testing.T) {
	for _, tc := range []struct {
		name     string
		outcomes []string
		err      error
	}{
		{"finishes", []string{"", "clean"}, nil},
		{"stalls", []string{"", ""}, ErrStalled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, ran := release(), []string{}
			m := &fakeModel{outcomes: tc.outcomes}
			a := newAgent(t, g, m, &ran)
			end, st, err := Drive(context.Background(), g, a, start(t, a))
			if !errors.Is(err, tc.err) || end.Reason != agentturn.ReasonDone {
				t.Fatalf("end = %+v, err = %v", end, err)
			}
			if tc.err == nil && !st.Done || tc.err != nil && st.Node.ID != "review" {
				t.Errorf("state = %+v", st)
			}
			if len(m.requests) != 2 {
				t.Errorf("model turns = %d, want 2", len(m.requests))
			}
		})
	}
}

func TestCallIDsFitProviders(t *testing.T) {
	g := &Graph{Name: strings.Repeat("n", 64), Nodes: []Node{{ID: strings.Repeat("i", 20), Kind: Run, Tool: "script", MaxVisits: 99}}}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	key, err := newKey()
	if err != nil {
		t.Fatal(err)
	}
	st := State{Instance: key, prefix: keyPrefix(key), Node: &g.Nodes[0], Visit: 99, Attempt: 99}
	if id := st.retryID(); len(id) > 64 {
		t.Errorf("%d characters: %s", len(id), id)
	}
}

func TestVisitLimit(t *testing.T) {
	g, m, ran := release(), &fakeModel{outcomes: []string{"findings", "ok", "findings"}}, []string{}
	g.node("gather").MaxVisits = 1
	a := newAgent(t, g, m, &ran)
	end := start(t, a)
	ans, err := g.Answer(end.Pending[0], Result{Outcome: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	end, _ = a.Resume(context.Background(), ans)
	end = stopAt(t, g, a, end, "visits")
	if end == nil || !errors.Is(end.Err, ErrVisits) {
		t.Fatalf("end = %+v", end)
	}
}

func TestPolicyStillDecides(t *testing.T) {
	g, ran := release(), []string{}
	cfg, err := kinApply(g, agentturn.Config{
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

func TestLoopBounds(t *testing.T) {
	loop := func(maxVisits int) *Graph {
		return &Graph{Name: "l", Nodes: []Node{
			{ID: "once", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"once"}`), Next: map[string]string{OK: "again"}},
			{ID: "again", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"again"}`), Next: map[string]string{OK: "again"}, MaxVisits: maxVisits},
		}}
	}
	for name, tc := range map[string]struct {
		max, runs int
		says      string
	}{
		"default": {0, DefaultLoopVisits, "the default for a step on a loop"},
		"its own": {2, 2, "its max_visits"},
		"higher":  {8, 8, "its max_visits"},
	} {
		t.Run(name, func(t *testing.T) {
			var ran []string
			g := loop(tc.max)
			cfg := apply(t, g, &fakeModel{}, scriptTool(&ran))
			cfg.MaxTurns = 40
			a := agentturn.New(cfg)
			end, _ := a.Prompt(context.Background(), openresponses.UserText("go"))
			end = stopAt(t, g, a, end, "visits")
			if end == nil || !errors.Is(end.Err, ErrVisits) {
				t.Fatalf("end = %+v", end)
			}
			if len(ran) != tc.runs+1 {
				t.Errorf("ran %d times, want %d and the step before", len(ran), tc.runs)
			}
			for _, want := range []string{`step "again"`, tc.says, "set max_visits"} {
				if !strings.Contains(end.Err.Error(), want) {
					t.Errorf("error lacks %q: %v", want, end.Err)
				}
			}
			if g.visits(&g.Nodes[0]) != 1 {
				t.Error("a step off the loop may run more than once")
			}
		})
	}
}

func TestDataSentAsAStringIsRead(t *testing.T) {
	object := &agenttool.Schema{Type: "object", Required: []string{"n"}, Properties: []agenttool.Property{{Name: "n", Schema: &agenttool.Schema{Type: "integer"}}}}
	for _, tc := range []struct {
		schema   *agenttool.Schema
		in, want any
	}{
		{object, `{"n":9007199254740993}`, map[string]any{"n": json.Number("9007199254740993")}},
		{&agenttool.Schema{Type: "array"}, `[true]`, []any{true}},
		// Left alone: not JSON, more than one value, a step that wants a
		// string, and one with no schema.
		{object, `n is 1`, `n is 1`},
		{object, `{} {}`, `{} {}`},
		{&agenttool.Schema{Type: "string"}, `{"n":1}`, `{"n":1}`},
		{nil, `{"n":1}`, `{"n":1}`},
	} {
		if got := unquote(tc.schema, tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("unquote(%v) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}
