package workflow_test

import (
	"cmp"
	"context"
	"maps"
	"os"
	"path/filepath"
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

// skill writes a workflow skill of files under dir and loads it.
func skill(t *testing.T, dir, name string, files map[string]string) *agentskill.Skill {
	t.Helper()
	root := filepath.Join(dir, name)
	files["SKILL.md"] = "---\nname: " + name + "\ndescription: A test of nesting.\nmetadata:\n  agentflow: WORKFLOW.md\n---\nDo it.\n"
	for p, body := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s, err := agentskill.LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const parentFlow = "---\noutputs_by:\n  done:\n    said: ${{ steps.call.outputs.said }}\n---\n# Parent\n\n```mermaid\nflowchart TD\n" +
	"  sub[[sub]] --> call[[call]]\n  call -->|done| done((done))\n```\n\n" +
	"## sub\n```yaml\nflow: ./flows/sub.md\n```\n\n" +
	"## call\n```yaml\nflow: child\nwith:\n  word: ${{ steps.sub.outputs.word }}\n```\n"

const subFlow = "---\noutputs:\n  word: ${{ steps.say.outputs.word }}\n---\n# Sub\n\n```mermaid\nflowchart TD\n  say[[say]]\n```\n\n" +
	"## say\n```yaml\nrun: scripts/say.sh\noutputs:\n  ok:\n    word: {type: string, required: true}\n```\n"

const childFlow = "---\ninputs:\n  word: {type: string, required: true}\noutputs_by:\n  done:\n    said: ${{ steps.echo.outputs.word }}\n---\n# Child\n\n" +
	"```mermaid\nflowchart TD\n  echo[[echo]] --> done((done))\n```\n\n" +
	"## echo\n```yaml\nrun: scripts/echo.sh\nwith:\n  word: ${{ inputs.word }}\noutputs:\n  ok:\n    word: {type: string, required: true}\n```\n"

func TestWorkflowsCallFilesAndOtherSkills(t *testing.T) {
	dir := t.TempDir()
	parent := skill(t, dir, "parent", map[string]string{
		"WORKFLOW.md":    parentFlow,
		"flows/sub.md":   subFlow,
		"scripts/say.sh": "#!/bin/sh\necho '{\"word\":\"hi\"}'\n",
	})
	child := skill(t, dir, "child", map[string]string{
		"WORKFLOW.md":     childFlow,
		"scripts/echo.sh": "#!/bin/sh\ncat\n",
	})
	find := func(name string) (*agentskill.Skill, error) {
		if name == "child" {
			return child, nil
		}
		return nil, os.ErrNotExist
	}
	w, err := workflow.LoadWith(parent, find)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range w.Flows {
		names = append(names, g.Name)
	}
	if !slices.Equal(names, []string{"parent/flows/sub", "child"}) || len(w.Skills) != 2 {
		t.Errorf("flows = %v, skills = %d", names, len(w.Skills))
	}
	// A call that does not route on failed fails its step: failed is
	// not a way out of the workflow.
	if got := w.Graph.Outcomes(); !slices.Equal(got, []string{"done"}) {
		t.Errorf("outcomes = %v", got)
	}
	// What a file loads is written back the same.
	if _, err := workflow.Format(w.Graph, "Parent"); err != nil {
		t.Error(err)
	}

	root := t.TempDir()
	ws, err := agentworkspace.NewLocal(root, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	tools := []agenttool.Tool{script.New(ws, time.Minute, script.Files(w.Files), script.Skills(w.Skills))}
	kids := map[string]*agentturn.Agent{}
	cfg, err := agentflow.Apply(w.Graph, agentturn.Config{Model: &model{}, ModelName: "fake", Tools: tools},
		agentflow.Unattended(), inMemoryOf(kids, &model{}, "fake", tools))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end, err := a.Prompt(context.Background(), openresponses.UserText("go"))
	if err == nil {
		end, _, err = agentflow.Drive(context.Background(), w.Graph, a, end)
	}
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	st, err := w.Graph.Replay(a.State().Transcript)
	if err != nil || st.Outcome != "done" || st.Outputs["said"] != "hi" {
		t.Errorf("state = %+v, %v", st, err)
	}
	// A script of another skill says which; one of the skill's own file
	// does not.
	// Each call ran in a child, which holds the scripts it ran after what
	// it was seeded with.
	var skills []string
	seen := map[string]bool{}
	all := a.State().Transcript
	for _, spawn := range slices.Sorted(maps.Keys(kids)) {
		all = append(all, kids[spawn].State().Transcript...)
	}
	for _, item := range all {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == agentflow.ScriptTool && !seen[c.CallID] {
			seen[c.CallID] = true
			skills = append(skills, c.Arguments)
		}
	}
	slices.SortFunc(skills, func(x, y string) int { return cmp.Compare(strings.Count(x, `"skill"`), strings.Count(y, `"skill"`)) })
	if len(skills) != 2 || strings.Contains(skills[0], `"skill"`) || !strings.Contains(skills[1], `"skill":"child"`) {
		t.Errorf("script calls = %v", skills)
	}
}

func TestLoadingRefusesABrokenChain(t *testing.T) {
	dir := t.TempDir()
	for name, c := range map[string]struct {
		files map[string]string
		find  bool
		want  string
	}{
		"a missing file": {map[string]string{"WORKFLOW.md": strings.Replace(parentFlow, "./flows/sub.md", "./flows/none.md", 1),
			"flows/sub.md": subFlow, "scripts/say.sh": ""}, true, "which is not in skill"},
		"no way to find skills": {map[string]string{"WORKFLOW.md": parentFlow, "flows/sub.md": subFlow, "scripts/say.sh": ""}, false, "nothing was given to find skills"},
		"a broken contract": {map[string]string{"WORKFLOW.md": strings.Replace(parentFlow, "  word: ${{ steps.sub.outputs.word }}\n", "  other: 1\n", 1),
			"flows/sub.md": subFlow, "scripts/say.sh": ""}, true, "requires the input word"},
		"a flow on a model step": {map[string]string{"WORKFLOW.md": strings.Replace(parentFlow, "sub[[sub]]", "sub[sub]", 1),
			"flows/sub.md": subFlow, "scripts/say.sh": ""}, true, "only a program node has a script, a tool, args or a flow"},
	} {
		t.Run(name, func(t *testing.T) {
			parent := skill(t, filepath.Join(dir, strings.ReplaceAll(name, " ", "-")), "parent", c.files)
			child := skill(t, filepath.Join(dir, strings.ReplaceAll(name, " ", "-")), "child", map[string]string{"WORKFLOW.md": childFlow, "scripts/echo.sh": ""})
			var find workflow.Find
			if c.find {
				find = func(string) (*agentskill.Skill, error) { return child, nil }
			}
			if _, err := workflow.LoadWith(parent, find); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
		})
	}
}

func TestOutputsByOutcomeAndDepthReadBack(t *testing.T) {
	src := "---\nmax_depth: 3\noutputs_by:\n  yes:\n    v: ${{ steps.a.outputs.v }}\n  no: {}\n---\n# t\n\n```mermaid\nflowchart TD\n" +
		"  a[[a]] -->|yes| yes((yes))\n  a -->|no| no((no))\n```\n\n## a\n```yaml\ntool: x\noutcomes: [yes, no]\noutputs:\n  yes:\n    v: {type: string, required: true}\n```\n"
	g, err := workflow.Parse("t", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if g.MaxDepth != 3 || g.Outputs != nil || len(g.OutputsBy) != 2 || g.OutputsBy["yes"]["v"].From != "steps.a.outputs.v" {
		t.Errorf("graph = %+v", g)
	}
	out, err := workflow.Format(g, "t")
	if err != nil {
		t.Fatal(err)
	}
	back, err := workflow.Parse("t", out)
	if err != nil || back.OwnDigest() != g.OwnDigest() {
		t.Errorf("read back: %v\n%s", err, out)
	}
	// An outcome's outputs are held to what reaches its end.
	bad := strings.Replace(src, "  no: {}\n", "  no:\n    v: ${{ steps.a.outputs.v }}\n", 1)
	if _, err := workflow.Parse("t", []byte(bad)); err == nil || !strings.Contains(err.Error(), "output v after a no") {
		t.Errorf("err = %v", err)
	}
}

// A workflow with outputs in the flat form calls another and does not
// route on failed: that failure fails the step, so the outputs are not
// asked for on a path that ends nothing. Found by a live trial.
func TestAnUnroutedFailureIsNoWayOut(t *testing.T) {
	middle, err := workflow.Parse("middle", []byte("---\noutputs:\n  parity: ${{ steps.inner.outputs.parity }}\n---\n"+
		"```mermaid\nflowchart TD\n  inner[[inner]] -->|ok| done((done))\n```\n\n## inner\n```yaml\nflow: leaf\n```\n"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := workflow.Parse("leaf", []byte("---\noutputs:\n  parity: ${{ steps.say.outputs.parity }}\n---\n"+
		"```mermaid\nflowchart TD\n  say[[say]]\n```\n\n## say\n```yaml\ntool: say\noutputs:\n  ok:\n    parity: {type: string, required: true}\n```\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := middle.Link(leaf); err != nil {
		t.Fatalf("link: %v", err)
	}
	if got := middle.Outcomes(); !slices.Equal(got, []string{"done"}) {
		t.Errorf("outcomes = %v", got)
	}
}

// Which way a workflow's outputs are read is its key's to say: an output
// named for an outcome, holding a mapping, is an output.
func TestOutputsAreByOutcomeOnlyUnderTheirOwnKey(t *testing.T) {
	const body = "# t\n\n```mermaid\nflowchart TD\n  a[[a]]\n```\n\n## a\n```yaml\ntool: x\n```\n"
	g, err := workflow.Parse("t", []byte("---\noutputs:\n  ok:\n    a: 1\n---\n"+body))
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := g.Outputs["ok"].Literal.(map[string]any); g.OutputsBy != nil || !ok || got["a"] != 1 {
		t.Errorf("outputs = %+v, by outcome %+v", g.Outputs, g.OutputsBy)
	}
	out, err := workflow.Format(g, "t")
	if err != nil {
		t.Fatal(err)
	}
	if back, err := workflow.Parse("t", out); err != nil || back.Digest() != g.Digest() {
		t.Errorf("it did not survive Format: %v\n%s", err, out)
	}
	by, err := workflow.Parse("t", []byte("---\noutputs_by:\n  ok:\n    a: 1\n---\n"+body))
	if err != nil || by.Outputs != nil || by.OutputsBy["ok"]["a"].Literal != 1 {
		t.Errorf("by outcome = %+v, %v", by, err)
	}
	for src, want := range map[string]string{
		"---\noutputs_by: [a]\n---\n" + body:                                "want a mapping of outcomes",
		"---\noutputs:\n  a: 1\noutputs_by:\n  ok:\n    a: 1\n---\n" + body: "either by outcome or for every outcome",
		"---\noutputs_by:\n  nope:\n    a: 1\n---\n" + body:                 "nope",
	} {
		if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
}
