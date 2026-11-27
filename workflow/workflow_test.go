package workflow_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/script"
	"github.com/ChristopherDavenport/agentflow/workflow"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
)

// copySkill copies the test skill somewhere a test may change it.
func copySkill(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "release")
	if err := os.CopyFS(dir, os.DirFS("testdata/release")); err != nil {
		t.Fatal(err)
	}
	return dir
}

func load(t *testing.T, dir string) (*agentskill.Skill, *workflow.Workflow) {
	t.Helper()
	s, err := agentskill.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workflow.Load(s)
	if err != nil {
		t.Fatal(err)
	}
	return s, w
}

type model struct{ instructions []string }

func (m *model) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.instructions = append(m.instructions, req.Instructions)
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	call, err := e.FunctionCall("", "flow_finish")
	if err != nil {
		return err
	}
	if err := call.Arguments(withRun(req, `{"step":"review","outcome":"ok","data":{"summary":"Adds a.go."}}`)); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

type tagArgs struct {
	Name string `json:"name"`
	Sign bool   `json:"sign"`
}

func TestSkillRunsAsAWorkflow(t *testing.T) {
	dir := copySkill(t)
	s, w := load(t, dir)
	// It is still a valid skill, and its frontmatter says what it is.
	if p := s.Validate(); len(p) != 0 {
		t.Errorf("problems = %v", p)
	}
	if file, ok := workflow.Marker(s); !ok || file != "WORKFLOW.md" {
		t.Errorf("marker = %q, %v", file, ok)
	}

	// What changes on disk after loading is not what runs.
	if err := os.WriteFile(filepath.Join(dir, "scripts/gather.sh"), []byte("#!/bin/sh\nexit 9\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Nor is a file of the same name in the workspace.
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	ws, err := agentworkspace.NewLocal(root, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ws.Close(); err != nil {
			t.Error(err)
		}
	})
	before := list(t, root)

	var tagged []tagArgs
	tag := agenttool.New("git_tag", "Tag.", func(_ context.Context, a tagArgs) (agentflow.Result, error) {
		tagged = append(tagged, a)
		return agentflow.Result{Outcome: "ok"}, nil
	})
	m := &model{}
	g := w.Graph
	all := []agenttool.Tool{script.New(ws, time.Minute, script.Files(w.Files)), tag}
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: m, ModelName: "fake", MaxTurns: 20, Tools: all}, inMemory(m, "fake", all))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	item, err := g.Invocation(map[string]any{"pr": 12})
	if err != nil {
		t.Fatal(err)
	}
	end, err := a.Prompt(context.Background(), item)
	if err == nil {
		end, _, err = agentflow.Drive(context.Background(), g, a, end)
	}
	if err != nil || end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("end = %+v, %v", end, err)
	}
	// The prose, its own heading and fence included, is the prompt.
	for _, want := range []string{"Summarise the change", "### An example", "```json", `Inputs: {"paths":["a.go"]}`, "Be terse."} {
		if !strings.Contains(m.instructions[0], want) {
			t.Errorf("instructions lack %q:\n%s", want, m.instructions[0])
		}
	}
	ans, err := g.Answer(end.Pending[0], agentflow.Result{Outcome: "release", Data: map[string]any{"version": "v1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if end, err = a.Resume(context.Background(), ans); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if want := []tagArgs{{Name: "v1.0.0", Sign: true}}; !reflect.DeepEqual(tagged, want) {
		t.Errorf("tagged = %+v", tagged)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil || st.Outputs["summary"] != "Adds a.go." {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if after := list(t, root); !reflect.DeepEqual(before, after) {
		t.Errorf("the workspace was left with %v, was %v", after, before)
	}

	// The files on disk are another definition now.
	_, changed := load(t, dir)
	if changed.Graph.Digest() == g.Digest() {
		t.Error("a changed script did not change the digest")
	}
}

func list(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	if err := fs.WalkDir(os.DirFS(root), ".", func(p string, _ fs.DirEntry, err error) error {
		out = append(out, p)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFormatRoundTrips(t *testing.T) {
	_, w := load(t, "testdata/release")
	out, err := workflow.Format(w.Graph, "Release")
	if err != nil {
		t.Fatal(err)
	}
	back, err := workflow.Parse(w.Graph.Name, out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	again, err := workflow.Format(back, "Release")
	if err != nil || string(again) != string(out) {
		t.Fatalf("not stable: %v\n%s\n%s", err, out, again)
	}
	t.Logf("%s", out)
	for _, want := range []string{"context: fresh", "model: fake", "instructions: prompts/review.md"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("%q was not written", want)
		}
	}

	// A graph the file cannot say is refused rather than changed.
	g := *back
	g.Nodes = append([]agentflow.Node(nil), back.Nodes...)
	g.Nodes[2].Prompt = "Do this.\n## gather\nThen that."
	if _, err := workflow.Format(&g, "x"); err == nil {
		t.Error("a prompt with a section heading was written")
	}
}

func TestEverythingUnknownIsRefused(t *testing.T) {
	const good = "# t\n\n```mermaid\nflowchart TD\n  a[[a]] --> b([b])\n```\n\n## a\n```yaml\ntool: x\n```\n\n## b\nWell?\n"
	if _, err := workflow.Parse("t", []byte(good)); err != nil {
		t.Fatal(err)
	}
	edit := func(old, new string) string {
		if !strings.Contains(good, old) {
			t.Fatalf("no %q to edit", old)
		}
		return strings.Replace(good, old, new, 1)
	}
	for name, tc := range map[string]struct{ src, want string }{
		"no diagram":          {edit("```mermaid", "```text"), "one mermaid block"},
		"two diagrams":        {edit("## a", "```mermaid\n```\n## a"), "one mermaid block"},
		"another direction":   {edit("flowchart TD", "flowchart BT"), "flowchart TD"},
		"dotted edge":         {edit("-->", "-.->"), "not a node or an edge"},
		"chain":               {edit("b([b])", "b([b]) --> a"), "not a node or an edge"},
		"subgraph":            {edit("flowchart TD", "flowchart TD\n subgraph s"), "not a node or an edge"},
		"style":               {edit("flowchart TD", "flowchart TD\n style a fill:#f00"), "not a node or an edge"},
		"another shape":       {edit("b([b])", "b{b}"), "not a node or an edge"},
		"no shape":            {edit("b([b])", "b"), "has no shape"},
		"two shapes":          {edit("b([b])", "b([b])\n  b[[b]]"), "two shapes"},
		"reserved id":         {edit("b([b])", "end([end])"), "reserves"},
		"keyword as an id":    {edit("b([b])", "style([style])"), "reserves"},
		"two edges, one way":  {edit("b([b])", "b([b])\n  a --> a"), "already goes to"},
		"undeclared outcome":  {edit("-->", "-->|nope|"), "undeclared outcome"},
		"no section":          {edit("## b\nWell?\n", ""), "has no section"},
		"unknown section":     {edit("Well?", "Well?\n## c"), "not a node"},
		"section twice":       {edit("Well?", "Well?\n## a"), "twice"},
		"unknown setting":     {edit("tool: x", "tool: x\nretries: 3"), "retries"},
		"context on a tool":   {edit("tool: x", "tool: x\ncontext: fresh"), "only a model node or a call has a context"},
		"late settings":       {edit("Well?", "Well?\n```yaml\nmax_visits: 3\n```"), "opens the section"},
		"tool on a person":    {edit("Well?", "```yaml\ntool: x\n```\nWell?"), "only a program node"},
		"tools on a program":  {edit("tool: x", "tool: x\ntools: [y]"), "only a model node"},
		"embedded reference":  {edit("tool: x", "tool: x\nwith: {n: \"v-${{ inputs.n }}\"}"), "whole value"},
		"reference in args":   {edit("tool: x", "tool: x\nargs: {n: \"${{ inputs.n }}\"}"), "under with"},
		"expression":          {edit("tool: x", "tool: x\nwith: {n: \"${{ inputs.n == 1 }}\"}"), "whole value"},
		"pattern in a schema": {edit("Well?", "```yaml\noutputs: {ok: {v: {type: string, pattern: x}}}\n```\nWell?"), "pattern"},
		"unknown type":        {edit("Well?", "```yaml\noutputs: {ok: {v: {type: date}}}\n```\nWell?"), "type"},
		"unknown frontmatter": {"---\non: push\n---\n" + good, "frontmatter"},
		"unclosed fence":      {good + "```\n", "never closed"},
		"settings up front":   {edit("# t\n", "# t\n\n```yaml\nmax_steps: 3\n```\n"), "sets nothing"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := workflow.Parse("t", []byte(tc.src))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q\n%s", err, tc.want, tc.src)
			}
		})
	}
}

func TestLoadRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		change func(dir string) error
		want   string
	}{
		"an ordinary skill": {func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: release\ndescription: d\n---\nbody\n"), 0o644)
		}, "not a workflow"},
		"a missing file":   {func(dir string) error { return os.Remove(filepath.Join(dir, "WORKFLOW.md")) }, "not in it"},
		"a missing script": {func(dir string) error { return os.Remove(filepath.Join(dir, "scripts/score.sh")) }, "not in it"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := copySkill(t)
			if err := tc.change(dir); err != nil {
				t.Fatal(err)
			}
			s, err := agentskill.LoadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := workflow.Load(s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// The example in the RFC loads, and its loop can go round.
func TestRFCExample(t *testing.T) {
	doc, err := os.ReadFile("../docs/rfcs/0001-agentflow.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(doc), "````markdown\n")
	src, _, closed := strings.Cut(rest, "````\n")
	if !ok || !closed {
		t.Fatal("no example")
	}
	g, err := workflow.Parse("pr-review", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: loopModel{}, ModelName: "fake", MaxTurns: 40,
		Tools: []agenttool.Tool{stubScripts(), stubTool("read"), stubTool("grep")}}, agentflow.Reply(func(_ context.Context, q agentflow.Question) (agentflow.Result, bool) {
		return agentflow.Result{Outcome: "fix"}, true
	}))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end, err := a.Prompt(context.Background(), openresponses.UserText("review"))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil || !st.Done {
		t.Fatalf("state = %+v, %v", st, err)
	}
	var path []string
	for _, s := range st.Steps {
		path = append(path, s.Node)
	}
	want := []string{"gather", "review", "triage", "fix", "gather", "review", "report"}
	if !reflect.DeepEqual(path, want) {
		t.Errorf("path = %v, want %v", path, want)
	}
}

// loopModel finds something once, fixes it and then finds nothing.
type loopModel struct{}

func (loopModel) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	args := `{"step":"fix","outcome":"ok"}`
	if strings.Contains(req.Instructions, `step "review"`) {
		fixed := slices.ContainsFunc(req.Input, func(item openresponses.Item) bool {
			c, ok := item.(*openresponses.FunctionCall)
			return ok && strings.Contains(c.Arguments, `"step":"fix"`)
		})
		args = `{"step":"review","outcome":"findings","data":{"findings":["x"]}}`
		if fixed {
			args = `{"step":"review","outcome":"clean"}`
		}
	}
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	call, err := e.FunctionCall("", "flow_finish")
	if err != nil {
		return err
	}
	if err := call.Arguments(withRun(req, args)); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

func stubScripts() agenttool.Tool {
	return agenttool.New(agentflow.ScriptTool, "Run.", func(context.Context, agentflow.ScriptCall) (agentflow.Result, error) {
		return agentflow.Result{Outcome: "ok", Data: map[string]any{"files": []string{"a.go"}}}, nil
	})
}

func stubTool(name string) agenttool.Tool {
	return agenttool.New(name, "Stub.", func(context.Context, struct{}) (string, error) { return "", nil })
}

const branching = "# t\n\n```mermaid\nflowchart TD\n  a[[a]] -->|yes| b[[b]]\n  a -->|no| stop((stop))\n  b --> stop\n```\n\n" +
	"## a\n```yaml\ntool: x\noutcomes: [yes, no]\n```\n\n## b\n```yaml\ntool: x\nwith:\n  v: ${{ optional(steps.a.outputs, steps.b.outputs) }}\n  w: ${{ latest(steps.a.outputs, steps.b.outputs) }}\n```\n"

func TestEndsAndAlternatives(t *testing.T) {
	g, err := workflow.Parse("t", []byte(branching))
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Nodes) != 2 || g.Nodes[0].Next["no"] != "" || g.Nodes[1].Next["ok"] != "" {
		t.Errorf("an end became a node or an edge: %+v", g.Nodes)
	}
	want := agentflow.Value{From: "steps.a.outputs", Or: []string{"steps.b.outputs"}, Optional: true}
	if got := g.Nodes[1].With["v"]; !reflect.DeepEqual(got, want) {
		t.Errorf("v = %+v", got)
	}
	out, err := workflow.Format(g, "t")
	if err != nil {
		t.Fatal(err)
	}
	// An end's name is the outcome the workflow ends with.
	if got := g.Outcomes(); !slices.Equal(got, []string{"stop"}) {
		t.Errorf("outcomes = %v, want [stop]", got)
	}
	for _, line := range []string{"a -->|no| stop((stop))", "b --> stop((stop))", "${{ optional(steps.a.outputs, steps.b.outputs) }}", "${{ latest(steps.a.outputs, steps.b.outputs) }}"} {
		if !strings.Contains(string(out), line) {
			t.Errorf("not written: %s\n%s", line, out)
		}
	}
	for name, c := range map[string]struct{ old, new, want string }{
		"an undeclared outcome": {"|no| stop", "|nope| stop", "undeclared outcome"},
		"leaving an end":        {"b --> stop", "b --> stop\n  stop --> a", "is an end"},
		"two destinations":      {"b --> stop", "a -->|yes| stop", "already goes"},
		"ends then goes on":     {"b --> stop", "a -->|no| b", "already ends"},
		"a reserved name":       {"b --> stop", "b --> end((end))", "is an end"},
		"an end with a section": {"## b", "## stop\nx\n\n## b", "not a node"},
		"latest of one":         {"latest(steps.a.outputs, steps.b.outputs)", "latest(steps.a.outputs)", "needs no latest"},
		"an empty argument":     {"latest(steps.a.outputs, steps.b.outputs)", "latest(steps.a.outputs,)", "separated by commas"},
		"another function":      {"latest(", "first(", "whole value"},
	} {
		if !strings.Contains(branching, c.old) {
			t.Fatalf("%s: no %q to edit", name, c.old)
		}
		_, err := workflow.Parse("t", []byte(strings.Replace(branching, c.old, c.new, 1)))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

// withRun names in a finish or a verdict the run the request's
// instructions name, as a model reads it from them.
func withRun(req openresponses.Request, args string) string {
	_, rest, ok := strings.Cut(req.Instructions, `, run "`)
	if !ok || !strings.HasPrefix(args, "{") || strings.Contains(args, `"run":`) {
		return args
	}
	run, _, _ := strings.Cut(rest, `"`)
	return `{"run":"` + run + `",` + args[1:]
}
