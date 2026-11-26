package workflow_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/workflow"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

const greetFlow = "---\ninputs:\n  who: {type: string, required: true}\n---\n# Greet\n\n```mermaid\nflowchart TD\n  hello[hello]\n```\n\n" +
	"## hello\n```yaml\nwith:\n  who: ${{ inputs.who }}\n```\nGreet them.\n"

// conversant finishes any step it is given, and in conversation makes
// the skill calls it was scripted with, one a turn, then says so.
type conversant struct {
	asks     []string
	requests []openresponses.Request
}

func (m *conversant) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.requests = append(m.requests, req)
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	if strings.Contains(req.Instructions, "You are on step") {
		call, err := e.FunctionCall("", "flow_finish")
		if err != nil {
			return err
		}
		if err := call.Arguments(withRun(req, `{"step":"hello","outcome":"ok"}`)); err != nil {
			return err
		}
		if err := call.Close(); err != nil {
			return err
		}
		return e.Complete()
	}
	if len(m.asks) == 0 {
		msg, err := e.Message("")
		if err != nil {
			return err
		}
		if err := msg.Text("Done."); err != nil {
			return err
		}
		if err := msg.Close(); err != nil {
			return err
		}
		return e.Complete()
	}
	args := m.asks[0]
	m.asks = m.asks[1:]
	call, err := e.FunctionCall("", agentskill.ToolName)
	if err != nil {
		return err
	}
	if err := call.Arguments(args); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

// startable is a catalog of a workflow, greet, another, other, and a
// plain skill, with greet's and other's workflows loaded.
func startable(t *testing.T) (*agentskill.Catalog, []*workflow.Workflow) {
	t.Helper()
	dir := t.TempDir()
	skill(t, dir, "greet", map[string]string{"WORKFLOW.md": greetFlow})
	skill(t, dir, "other", map[string]string{"WORKFLOW.md": greetFlow})
	plainSkill(t, dir, "go-style", "Wrap errors with %w.")
	catalog, err := agentskill.DiscoverDirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	var flows []*workflow.Workflow
	for _, name := range []string{"greet", "other"} {
		s, ok := catalog.Lookup(name)
		if !ok {
			t.Fatalf("no skill %s", name)
		}
		w, err := workflow.Load(s)
		if err != nil {
			t.Fatal(err)
		}
		flows = append(flows, w)
	}
	return catalog, flows
}

// outputs lists what the skill tool answered, in order.
func outputs(t agentturn.Transcript) []string {
	calls := map[string]string{}
	var out []string
	for _, item := range t {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			calls[it.CallID] = it.Name
		case *openresponses.FunctionCallOutput:
			if calls[it.CallID] == agentskill.ToolName {
				out = append(out, it.Output.String())
			}
		}
	}
	return out
}

func TestAModelStartsAWorkflowByAskingForItsSkill(t *testing.T) {
	ctx := context.Background()
	catalog, flows := startable(t)
	tool, err := workflow.SkillTool(catalog, flows, workflow.ModelInvokes("greet"))
	if err != nil {
		t.Fatal(err)
	}
	g := flows[0].Graph
	m := &conversant{asks: []string{`{"name":"greet","inputs":{}}`, `{"name":"greet","inputs":{"who":"bob"}}`}}
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: m, ModelName: "fake", MaxTurns: 20, Tools: []agenttool.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if _, st, err := agentflow.Invoke(ctx, g, a, map[string]any{"who": "ann"}); err != nil || !st.Done {
		t.Fatalf("first run: %+v, %v", st, err)
	}

	// In conversation, the model asks: its first inputs are wrong, it is
	// told, and the second start the workflow, which the program invokes.
	end, err := a.Prompt(ctx, openresponses.UserText("Greet bob too."))
	if err != nil || end.Reason != agentturn.ReasonStopped || end.Cause != agentturn.StopTerminate {
		t.Fatalf("end = %+v, err = %v", end, err)
	}
	said := outputs(a.State().Transcript)
	if len(said) != 2 || !strings.Contains(said[0], "who") || said[1] != agentflow.StartText("greet") {
		t.Fatalf("the tool said %q", said)
	}
	req, ok := agentflow.Requested(a.State().Transcript)
	if !ok || req.Workflow != "greet" {
		t.Fatalf("request = %+v, %v", req, ok)
	}
	end, st, err := agentflow.Drive(ctx, g, a, end)
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("drive: end = %+v, state = %+v, err = %v", end, st, err)
	}
	if who := st.Inputs.(map[string]any)["who"]; who != "bob" {
		t.Errorf("inputs = %+v", st.Inputs)
	}
	if _, ok := agentflow.Requested(a.State().Transcript); ok {
		t.Error("a request already met is still read as one")
	}

	// The record: the model's call and the tool's answer, then the
	// program's invocation, then the run.
	var seq []string
	for _, item := range a.State().Transcript {
		switch it := item.(type) {
		case *openresponses.FunctionCallOutput:
			if it.Output.String() == agentflow.StartText("greet") {
				seq = append(seq, "asked")
			}
		case *openresponses.Message:
			if it.Role == openresponses.RoleDeveloper && strings.HasPrefix(it.Text(), "agentflow: start greet") {
				seq = append(seq, "invoked")
			}
		case *openresponses.FunctionCall:
			if it.Name == "flow_start" {
				seq = append(seq, "started")
			}
		}
	}
	if want := []string{"invoked", "started", "asked", "invoked", "started"}; !slices.Equal(seq, want) {
		t.Errorf("record = %v, want %v", seq, want)
	}
	runs, err := agentflow.Runs(a.State().Transcript, nil)
	if err != nil || len(runs) != 2 || runs[1].Status != agentflow.Done {
		t.Errorf("runs = %+v, %v", runs, err)
	}
}

func TestAModelStartsAnotherWorkflowThroughItsHost(t *testing.T) {
	ctx := context.Background()
	catalog, flows := startable(t)
	tool, err := workflow.SkillTool(catalog, flows, workflow.ModelInvokes("greet", "other"))
	if err != nil {
		t.Fatal(err)
	}
	g, other := flows[0].Graph, flows[1].Graph
	m := &conversant{asks: []string{`{"name":"other","inputs":{"who":"cy"}}`}}
	base := agentturn.Config{Model: m, ModelName: "fake", MaxTurns: 20, Tools: []agenttool.Tool{tool}}
	cfg, err := agentflow.Apply(g, base)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if _, _, err := agentflow.Invoke(ctx, g, a, map[string]any{"who": "ann"}); err != nil {
		t.Fatal(err)
	}
	end, err := a.Prompt(ctx, openresponses.UserText("Now the other one."))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = agentflow.Drive(ctx, g, a, end)
	var req *agentflow.StartRequest
	if !errors.Is(err, agentflow.ErrStartRequested) || !errors.As(err, &req) || req.Workflow != "other" {
		t.Fatalf("drive: %v", err)
	}
	// The host applies the other workflow and invokes it.
	next, err := agentflow.Apply(other, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SetConfig(next); err != nil {
		t.Fatal(err)
	}
	if _, st, err := agentflow.Invoke(ctx, other, a, req.Inputs); err != nil || !st.Done || st.Workflow != "other" {
		t.Fatalf("other: %+v, %v", st, err)
	}
}

// ask calls the tool as a model would, with a transcript on the context.
func ask(t *testing.T, tool agenttool.Tool, transcript agentturn.Transcript, args string) (agenttool.Result, error) {
	t.Helper()
	ctx := agentturn.ContextWithTranscript(context.Background(), transcript)
	return tool.Execute(ctx, agenttool.Call{ID: "c1", Args: json.RawMessage(args)})
}

func TestWhatAModelMayNotStart(t *testing.T) {
	catalog, flows := startable(t)
	flows[1].Graph.Hidden = true
	tool, err := workflow.SkillTool(catalog, flows, workflow.ModelInvokes("greet", "other"))
	if err != nil {
		t.Fatal(err)
	}
	only, err := workflow.SkillTool(catalog, flows, workflow.ModelInvokes("greet"))
	if err != nil {
		t.Fatal(err)
	}
	// A run under way, its step a model's.
	g := flows[0].Graph
	m := &stuckModel{}
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: m, ModelName: "fake", MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if _, _, err := agentflow.Invoke(context.Background(), g, a, map[string]any{"who": "ann"}); err == nil {
		t.Log("the step's model stopped, and the run is under way")
	}
	running := a.State().Transcript

	for _, tc := range []struct {
		name       string
		tool       agenttool.Tool
		transcript agentturn.Transcript
		args, want string
	}{
		{"under way", tool, running, `{"name":"greet","inputs":{"who":"x"}}`, `the workflow "greet" is running`},
		{"not the model's", only, nil, `{"name":"other","inputs":{"who":"x"}}`, `not one you may start`},
		{"hidden", tool, nil, `{"name":"other","inputs":{"who":"x"}}`, `only for other workflows to call`},
		{"a file of one", tool, nil, `{"name":"greet","path":"WORKFLOW.md"}`, `whose files are not read`},
		{"inputs missing", tool, nil, `{"name":"greet"}`, `who`},
		{"inputs to a plain skill", tool, nil, `{"name":"go-style","inputs":{"a":1}}`, `not a workflow`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ask(t, tc.tool, tc.transcript, tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := workflow.SkillTool(catalog, flows, workflow.ModelInvokes("gret")); err == nil || !strings.Contains(err.Error(), `did you mean "greet"`) {
		t.Errorf("an unknown name: %v", err)
	}
}

func TestAPlainSkillIsServedAsBefore(t *testing.T) {
	catalog, flows := startable(t)
	tool, err := workflow.SkillTool(catalog, flows, workflow.ModelInvokes("greet"))
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"name":"go-style"}`)
	got, err := tool.Execute(context.Background(), agenttool.Call{ID: "c1", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	want, err := catalog.Tool().Execute(context.Background(), agenttool.Call{ID: "c1", Args: args})
	if err != nil {
		t.Fatal(err)
	}
	if got.Output.String() != want.Output.String() || !strings.Contains(got.Output.String(), "Wrap errors") {
		t.Errorf("served %q, want %q", got.Output.String(), want.Output.String())
	}
	if _, ok := got.Details.(agentskill.Read); !ok || got.Terminate {
		t.Errorf("details = %#v, terminate = %v", got.Details, got.Terminate)
	}
	if tool.Name() != agentskill.ToolName {
		t.Errorf("name = %q", tool.Name())
	}
	var params struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(tool.Parameters(), &params); err != nil || params.Properties["inputs"] == nil || params.Properties["name"] == nil {
		t.Errorf("parameters = %s, %v", tool.Parameters(), err)
	}

	prompt := workflow.StartPrompt(flows, workflow.ModelInvokes("greet"))
	if !strings.Contains(prompt, `<workflow name="greet"`) || strings.Contains(prompt, `"other"`) || !strings.Contains(prompt, "who") {
		t.Errorf("prompt = %s", prompt)
	}
	if workflow.StartPrompt(flows) != "" {
		t.Error("a prompt lists workflows no model may start")
	}
}

// stuckModel says something and stops, so its step stays under way.
type stuckModel struct{}

func (stuckModel) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	msg, err := e.Message("")
	if err != nil {
		return err
	}
	if err := msg.Text("Thinking."); err != nil {
		return err
	}
	if err := msg.Close(); err != nil {
		return err
	}
	return e.Complete()
}
