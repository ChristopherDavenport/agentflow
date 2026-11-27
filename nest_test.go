package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

var counted = &agenttool.Schema{Type: "object", Required: []string{"n"},
	Properties: []agenttool.Property{{Name: "n", Schema: &agenttool.Schema{Type: "integer"}}}}

// countdown calls itself until n reaches 0.
func countdown() *Graph {
	return &Graph{Name: "countdown", Inputs: counted, Nodes: []Node{
		{ID: "check", Kind: Run, Tool: "count", With: map[string]Value{"n": From("inputs.n")}, Outcomes: []string{"again", "zero"},
			Data: map[string]*agenttool.Schema{"again": counted}, Next: map[string]string{"again": "recurse"}},
		{ID: "recurse", Kind: Run, Flow: "countdown", With: map[string]Value{"n": From("steps.check.outputs.n")}},
	}}
}

// middle thinks, then counts down from 2, then asks whether to go on.
func middle() *Graph {
	return &Graph{Name: "middle", Nodes: []Node{
		{ID: "think", Kind: Agent, Prompt: "Think.", Next: map[string]string{OK: "count"}},
		{ID: "count", Kind: Run, Flow: "countdown", With: map[string]Value{"n": Literal(2)}, Next: map[string]string{"zero": "confirm"}},
		{ID: "confirm", Kind: Person, Prompt: "Go on?", Outcomes: []string{"yes", "no"}, Unattended: Fallback{Outcome: "yes"}},
	}, OutputsBy: map[string]map[string]Value{"yes": {"said": Literal("yes")}}}
}

// top calls middle and reports.
func top() *Graph {
	return &Graph{Name: "top", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "middle", Next: map[string]string{"yes": "report"}},
		{ID: "report", Kind: Run, Tool: "script", With: map[string]Value{"name": From("steps.call.outputs.said")}},
	}}
}

func linked(t *testing.T, g *Graph, flows ...*Graph) *Graph {
	t.Helper()
	if err := g.Link(flows...); err != nil {
		t.Fatal(err)
	}
	return g
}

func countTool(calls *[]int) agenttool.Tool {
	return agenttool.New("count", "Count down.", func(_ context.Context, a struct {
		N int `json:"n"`
	}) (Result, error) {
		*calls = append(*calls, a.N)
		if a.N == 0 {
			return Result{Outcome: "zero"}, nil
		}
		return Result{Outcome: "again", Data: map[string]any{"n": a.N - 1}}, nil
	})
}

func nestedAgent(t *testing.T, g *Graph, m openresponses.Streamer, tools []agenttool.Tool, opts ...Option) *agentturn.Agent {
	t.Helper()
	cfg, kids, err := kinApplied(g, agentturn.Config{Model: m, ModelName: "fake", Tools: tools}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	kins.Store(a, kids)
	return a
}

// tree draws runs as workflow:status, with their children indented.
func tree(runs []*Instance) []string {
	var out []string
	var walk func(rs []*Instance, depth int)
	walk = func(rs []*Instance, depth int) {
		for _, r := range rs {
			out = append(out, fmt.Sprintf("%s%s:%s:%s", strings.Repeat(" ", depth), r.Workflow, r.Status, r.Outcome))
			if r.Problem != "" {
				out = append(out, "problem: "+r.Problem)
			}
			walk(r.Children, depth+1)
		}
	}
	walk(runs, 0)
	return out
}

func TestFlowsNestThreeDeepAndCallThemselves(t *testing.T) {
	g := linked(t, top(), middle(), countdown())
	var counts []int
	var ran []string
	a := nestedAgent(t, g, &fakeModel{outcomes: []string{"ok"}}, []agenttool.Tool{countTool(&counts), scriptTool(&ran)}, Unattended())
	end := start(t, a)
	if end.Reason != agentturn.ReasonDone || end.Err != nil {
		t.Fatalf("end = %s, %v", end.Reason, end.Err)
	}
	if !slices.Equal(counts, []int{2, 1, 0}) || !slices.Equal(ran, []string{"yes"}) {
		t.Errorf("counted %v, ran %v", counts, ran)
	}
	items := a.State().Transcript
	runs, err := Runs(items, under(a))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"top:done:ok", " middle:done:yes", "  countdown:done:zero", "   countdown:done:zero", "    countdown:done:zero"}
	if got := tree(runs); !slices.Equal(got, want) {
		t.Errorf("tree =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if got := steps(runs[0].State); !slices.Equal(got, []string{"call:yes", "report:ok"}) {
		t.Errorf("top's path = %v", got)
	}
	// Each run's steps are its own.
	if got := steps(runs[0].Children[0].State); !slices.Equal(got, []string{"think:ok", "count:zero", "confirm:yes"}) {
		t.Errorf("middle's path = %v", got)
	}
	deepest := runs[0].Children[0].Children[0].Children[0].Children[0]
	if deepest.Depth != 4 || deepest.Parent != runs[0].Children[0].Children[0].Children[0].ID {
		t.Errorf("deepest = %+v", deepest)
	}
	st, err := g.Replay(items)
	if err != nil || !st.Done || st.Entries != 2+3+2+2+1 {
		t.Errorf("state = %+v, %v", st, err)
	}
	// Every call ID fits, and none names an ancestor.
	for _, item := range items {
		if c, ok := item.(*openresponses.FunctionCall); ok && (len(c.CallID) > 64 || strings.Count(c.CallID, "_") > 4) {
			t.Errorf("call ID %s", c.CallID)
		}
	}
	// The report read what middle gave back.
	for _, item := range items {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == "script" && c.Arguments != `{"name":"yes"}` {
			t.Errorf("report got %s", c.Arguments)
		}
	}
}

// fragile fails its only step, a tool that errors.
func fragile() *Graph {
	return &Graph{Name: "fragile", Nodes: []Node{{ID: "try", Kind: Run, Tool: "flaky"}}}
}

func failing(fails *int) agenttool.Tool {
	return agenttool.New("flaky", "Fails at first.", func(context.Context, struct{}) (Result, error) {
		if *fails > 0 {
			*fails--
			return Result{}, errors.New("not yet")
		}
		return Result{Outcome: OK}, nil
	})
}

func TestAFailedCallRoutesOrFailsTheStep(t *testing.T) {
	t.Run("routed", func(t *testing.T) {
		g := linked(t, &Graph{Name: "careful", Nodes: []Node{
			{ID: "call", Kind: Run, Flow: "fragile", Next: map[string]string{Failed: "report"}},
			{ID: "report", Kind: Run, Tool: "script", With: map[string]Value{"name": From("steps.call.outputs.reason")}},
		}}, fragile())
		fails := 1
		var ran []string
		a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{failing(&fails), scriptTool(&ran)}, Unattended())
		if end := start(t, a); end.Reason != agentturn.ReasonDone || end.Err != nil {
			t.Fatalf("end = %s, %v", end.Reason, end.Err)
		}
		runs, err := Runs(a.State().Transcript, under(a))
		if err != nil {
			t.Fatal(err)
		}
		if got := tree(runs); !slices.Equal(got, []string{"careful:done:ok", " fragile:failed:"}) {
			t.Errorf("tree = %v", got)
		}
		if got := steps(runs[0].State); !slices.Equal(got, []string{"call:failed", "report:ok"}) {
			t.Errorf("path = %v", got)
		}
	})
	t.Run("retried", func(t *testing.T) {
		g := linked(t, &Graph{Name: "plain", Nodes: []Node{{ID: "call", Kind: Run, Flow: "fragile"}}}, fragile())
		fails := 1
		a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{failing(&fails)})
		end := start(t, a)
		// The inner run asks first, about its own step.
		q := question(t, end)
		if q.Workflow != "fragile" || q.Step != "try" || !q.Retry || len(q.Path) != 2 {
			t.Fatalf("question = %+v", q)
		}
		end = reply(t, g, a, end, stopOutcome)
		// Stopped there, the inner run has failed, so the calling step
		// has: its own question is next.
		q = question(t, end)
		if q.Workflow != "plain" || q.Step != "call" || !q.Retry || !strings.Contains(q.Question, "fragile") {
			t.Fatalf("question = %+v", q)
		}
		end = reply(t, g, a, end, retryOutcome)
		if end.Reason != agentturn.ReasonDone || end.Err != nil {
			t.Fatalf("end = %s, %v", end.Reason, end.Err)
		}
		runs, err := Runs(a.State().Transcript, under(a))
		if err != nil {
			t.Fatal(err)
		}
		if got := tree(runs); !slices.Equal(got, []string{"plain:done:ok", " fragile:failed:", " fragile:done:ok"}) {
			t.Errorf("tree = %v", got)
		}
		if runs[0].Children[0].ID == runs[0].Children[1].ID {
			t.Error("a retry called under the same key")
		}
	})
	t.Run("unattended", func(t *testing.T) {
		g := linked(t, &Graph{Name: "plain", Nodes: []Node{{ID: "call", Kind: Run, Flow: "fragile"}}}, fragile())
		fails := 9
		a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{failing(&fails)}, Unattended())
		end := run(a)
		if !errors.Is(end.Err, ErrStepFailed) || !strings.Contains(end.Err.Error(), "fragile") {
			t.Fatalf("end = %s, %v", end.Reason, end.Err)
		}
		runs, _ := Runs(a.State().Transcript, under(a))
		if got := tree(runs); !slices.Equal(got, []string{"plain:failed:", " fragile:failed:"}) {
			t.Errorf("tree = %v", got)
		}
		if !strings.Contains(runs[0].Reason, "call:") || !strings.Contains(runs[0].Reason, "not yet") {
			t.Errorf("reason = %q", runs[0].Reason)
		}
	})
}

func question(t *testing.T, end *agentturn.RunEnd) Question {
	t.Helper()
	if end == nil || end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 {
		t.Fatalf("end = %+v", end)
	}
	var q Question
	if err := json.Unmarshal([]byte(end.Pending[0].Call.Arguments), &q); err != nil {
		t.Fatal(err)
	}
	return q
}

func reply(t *testing.T, g *Graph, a *agentturn.Agent, end *agentturn.RunEnd, outcome string) *agentturn.RunEnd {
	t.Helper()
	ans, err := g.Answer(end.Pending[0], Result{Outcome: outcome})
	if err != nil {
		t.Fatal(err)
	}
	next, err := carry(t, a, ans)
	if err != nil && next == nil {
		t.Fatal(err)
	}
	return next
}

func TestAnInvocationAbandonsTheWholeStack(t *testing.T) {
	g := linked(t, top(), middle(), countdown())
	var counts []int
	var ran []string
	a := nestedAgent(t, g, &fakeModel{outcomes: []string{"ok", "ok"}}, []agenttool.Tool{countTool(&counts), scriptTool(&ran)})
	end := start(t, a)
	if q := question(t, end); q.Workflow != "middle" || q.Step != "confirm" {
		t.Fatalf("question = %+v", q)
	}
	// A new invocation abandons every run of the tree, each in its own
	// session, before the next starts.
	end, _, err := Invoke(context.Background(), g, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if q := question(t, end); q.Workflow != "middle" {
		t.Fatalf("question = %+v", q)
	}
	runs, err := Runs(a.State().Transcript, under(a))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"top:abandoned:", " middle:abandoned:", "  countdown:done:zero", "   countdown:done:zero", "    countdown:done:zero",
		"top:running:", " middle:running:", "  countdown:done:zero", "   countdown:done:zero", "    countdown:done:zero"}
	if got := tree(runs); !slices.Equal(got, want) {
		t.Errorf("tree =\n%s", strings.Join(got, "\n"))
	}
}

const nestedEnv = "AGENTFLOW_NESTED_STORE"

// A question in a called workflow is asked in its child session, after
// calls of its own that ran in theirs. A process that has only the store
// finds it through the call that waits, and the answer goes back up.
func TestAQuestionAtDepthResumesInANewProcess(t *testing.T) {
	g := func() *Graph { return linked(t, top(), middle(), countdown()) }
	var counts []int
	var ran []string
	tools := func() []agenttool.Tool { return []agenttool.Tool{countTool(&counts), scriptTool(&ran)} }
	if dir := os.Getenv(nestedEnv); dir != "" {
		ctx := context.Background()
		h := hosted(t, dir, "", g(), &fakeModel{outcomes: []string{"ok"}}, tools(), false)
		first, err := h.a.Prompt(ctx, openresponses.UserText("go"))
		if err != nil {
			t.Fatal(err)
		}
		if end, _, err := Drive(ctx, h.g, h.a, first, h.hist); err != nil || end.Reason != agentturn.ReasonInputRequired {
			t.Fatalf("end = %+v, %v", end, err)
		}
		syncAll(t, h.store)
		if err := os.WriteFile(dir+"/id", []byte(h.id), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	ctx := context.Background()
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestAQuestionAtDepthResumesInANewProcess$")
	child.Env = append(os.Environ(), nestedEnv+"="+dir)
	out, err := child.CombinedOutput()
	id, readErr := os.ReadFile(dir + "/id")
	if err == nil || readErr != nil {
		t.Fatalf("child: err = %v, read = %v\n%s", err, readErr, out)
	}
	h := hosted(t, dir, string(id), g(), &fakeModel{}, tools(), false)
	// The call waits here, and the question in the child it waits on.
	end, st, err := Drive(ctx, h.g, h.a, nil, h.hist)
	if err != nil || end == nil || len(end.Pending) != 1 {
		t.Fatalf("end = %+v, %v", end, err)
	}
	q := question(t, end)
	if q.Workflow != "middle" || q.Step != "confirm" || st.Workflow != "middle" ||
		len(q.Path) != 2 || q.Path[0].Workflow != "top" || q.Path[0].Step != "call" {
		t.Fatalf("question = %+v at %s", q, st.Workflow)
	}
	ans, err := h.g.Answer(end.Pending[0], Result{Outcome: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	end, _, err = Resume(ctx, h.g, h.a, []agentturn.Answer{ans}, h.hist)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("resume: %v, %+v", err, end)
	}
	// Nothing before the question ran again here.
	if len(counts) != 0 || !slices.Equal(ran, []string{"yes"}) {
		t.Errorf("counted %v, ran %v", counts, ran)
	}
	checkHashes(t, h.session(t, ""))
	runs := h.runs(t)
	if got := tree(runs); !slices.Equal(got, []string{"top:done:ok", " middle:done:yes", "  countdown:done:zero", "   countdown:done:zero", "    countdown:done:zero"}) {
		t.Errorf("tree = %v", got)
	}
	// Every session of the tree rebuilds its requests, the forks too.
	var check func(in *Instance)
	check = func(in *Instance) {
		for _, kid := range in.Children {
			checkHashes(t, h.session(t, kid.Spawn))
			check(kid)
		}
	}
	check(runs[0])
}

func TestDepthIsBoundedAcrossTheTree(t *testing.T) {
	for name, opts := range map[string][]Option{"the host's bound": {MaxDepth(2)}, "the workflow's bound": nil} {
		t.Run(name, func(t *testing.T) {
			g := linked(t, &Graph{Name: "deep", Nodes: []Node{
				{ID: "call", Kind: Run, Flow: "countdown", With: map[string]Value{"n": Literal(9)}, Next: map[string]string{Failed: "never"}},
				{ID: "never", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"never"}`)},
			}}, countdown())
			if opts == nil {
				g.MaxDepth = 2
			}
			var counts []int
			var ran []string
			a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{countTool(&counts), scriptTool(&ran)}, opts...)
			end := stopAt(t, g, a, run(a), "depth")
			if !errors.Is(end.Err, ErrDepth) || len(ran) != 0 {
				t.Fatalf("end = %s, %v, ran %v", end.Reason, end.Err, ran)
			}
			runs, err := Runs(a.State().Transcript, under(a))
			if err != nil {
				t.Fatal(err)
			}
			// No caller retries or routes on a bound across the tree.
			if got := tree(runs); !slices.Equal(got, []string{"deep:failed:", " countdown:failed:", "  countdown:failed:"}) {
				t.Errorf("tree = %v", got)
			}
		})
	}
}

func TestAChangeToACalleeStopsAResume(t *testing.T) {
	g := linked(t, top(), middle(), countdown())
	var counts []int
	var ran []string
	a := nestedAgent(t, g, &fakeModel{outcomes: []string{"ok"}}, []agenttool.Tool{countTool(&counts), scriptTool(&ran)})
	if end := start(t, a); end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("end = %s, %v", end.Reason, end.Err)
	}
	changed := countdown()
	changed.Nodes[0].MaxVisits = 3
	again := linked(t, top(), middle(), changed)
	_, err := again.Replay(a.State().Transcript)
	if !errors.Is(err, ErrChanged) || !strings.HasSuffix(err.Error(), ": countdown") {
		t.Errorf("err = %v", err)
	}
	// What ran reads back as it was pinned.
	runs, err := Runs(a.State().Transcript, under(a))
	if err != nil || runs[0].Problem != "" || len(runs[0].Children) != 1 {
		t.Errorf("runs = %v, %v", tree(runs), err)
	}
}

func TestAFormatOneRecordIsRefused(t *testing.T) {
	g := &Graph{Name: "old", Nodes: []Node{{ID: "review", Kind: Agent, Prompt: "Review it."}}}
	start, err := json.Marshal(map[string]any{"workflow": "old", "digest": g.Digest(), "format": 1})
	if err != nil {
		t.Fatal(err)
	}
	ok := openresponses.FunctionCallOutputData{Text: `{"outcome":"ok"}`}
	recorded := agentturn.Transcript{
		&openresponses.FunctionCall{CallID: "flow_old_ab12cd34_start", Name: startTool, Arguments: string(start)},
		&openresponses.FunctionCallOutput{CallID: "flow_old_ab12cd34_start", Output: ok},
	}
	if _, err := g.Replay(recorded); !errors.Is(err, ErrChanged) || !strings.Contains(err.Error(), "record format 1") {
		t.Errorf("replay: %v", err)
	}
	runs, err := Runs(recorded, nil)
	if err != nil || len(runs) != 1 || !strings.Contains(runs[0].Problem, "record format 1") {
		t.Errorf("runs = %v, %v", tree(runs), err)
	}
}

func TestContractsAreCheckedAtLink(t *testing.T) {
	caller := func(n Node) *Graph {
		n.ID, n.Kind = "call", Run
		return &Graph{Name: "caller", Nodes: []Node{n, {ID: "after", Kind: Run, Tool: "script",
			With: map[string]Value{"x": From("steps.call.outputs.said")}}}}
	}
	for name, c := range map[string]struct {
		g    *Graph
		want string
	}{
		"a missing workflow": {caller(Node{Flow: "midle", Next: map[string]string{"yes": "after"}}), `no such workflow; did you mean "middle"?`},
		"an unknown input":   {caller(Node{Flow: "countdown", With: map[string]Value{"nn": Literal(1)}}), `nn is not an input of it; did you mean "n"?`},
		"a missing input":    {caller(Node{Flow: "countdown", Next: map[string]string{"zero": "after"}}), "requires the input n"},
		"a wrong literal":    {caller(Node{Flow: "countdown", With: map[string]Value{"n": Literal("two")}}), "n: "},
		"another outcome":    {caller(Node{Flow: "middle", Next: map[string]string{"maybe": "after"}}), `edge for undeclared outcome "maybe"`},
		"other outcomes":     {caller(Node{Flow: "middle", Outcomes: []string{"yes"}, Next: map[string]string{"yes": "after"}}), "outcomes yes: want"},
		"an output not given on the way": {caller(Node{Flow: "middle", Next: map[string]string{"no": "after"}}),
			`"call" does not certainly produce "said"`},
	} {
		t.Run(name, func(t *testing.T) {
			err := c.g.Link(middle(), countdown())
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
	// Unlinked, a graph that calls is refused.
	if _, err := Apply(top(), agentturn.Config{Model: &fakeModel{}, ModelName: "fake"}); err == nil || !strings.Contains(err.Error(), "not linked") {
		t.Errorf("unlinked: %v", err)
	}
}

func TestAFinishNamesItsRun(t *testing.T) {
	g := linked(t, top(), middle(), countdown())
	var counts []int
	var ran []string
	m := &cast{script: []says{{finishTool, `{"run":"nope","step":"think","outcome":"ok"}`}, {finishTool, `{"step":"think","outcome":"ok"}`}}}
	a := nestedAgent(t, g, m, []agenttool.Tool{countTool(&counts), scriptTool(&ran)}, Unattended())
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s, %v", end.Reason, end.Err)
	}
	var refused string
	for _, item := range whole(a) {
		if out, ok := item.(*openresponses.FunctionCallOutput); ok && strings.Contains(out.Output.String(), `not of run "nope"`) {
			refused = out.Output.String()
		}
	}
	if refused == "" {
		t.Error("a finish naming another run was taken")
	}
	// The model was told its run, and the finish's schema allows no other.
	if !strings.Contains(m.requests[0].Instructions, `with step "think", run "`) || !strings.Contains(string(finishParams(m.requests[0])), `"run":{"enum":["`) {
		t.Errorf("instructions = %s", m.requests[0].Instructions)
	}
}

func finishParams(req openresponses.Request) []byte {
	for _, tool := range req.Tools {
		if fn, ok := tool.(*openresponses.FunctionTool); ok && fn.Name == finishTool {
			return fn.Parameters
		}
	}
	return nil
}
