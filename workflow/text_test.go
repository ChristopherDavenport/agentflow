package workflow_test

import (
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/agentflow/workflow"
	"github.com/ChristopherDavenport/agentskill"
)

const described = "---\ndescription: Lint, then fix.\n---\n# t\n\n```mermaid\nflowchart TD\n" +
	"  lint[[lint]] --> fix[fix]\n```\n\n" +
	"## lint\n```yaml\ndescription: Runs the linter over the change.\nrun: scripts/lint.sh\n```\n\n" +
	"## fix\n```yaml\ndescription: |\n  Fixes what the linter found.\n  Nothing else.\n```\nFix it.\n"

func TestDescriptionsReadBack(t *testing.T) {
	g, err := workflow.Parse("t", []byte(described))
	if err != nil {
		t.Fatal(err)
	}
	if g.Description != "Lint, then fix." || g.Describe("lint") != "Runs the linter over the change." || g.Describe("fix") != "Fixes what the linter found.\nNothing else." {
		t.Errorf("descriptions = %q, %q, %q", g.Description, g.Describe("lint"), g.Describe("fix"))
	}
	if g.Nodes[1].Prompt != "Fix it." {
		t.Errorf("prompt = %q", g.Nodes[1].Prompt)
	}
	out, err := workflow.Format(g, "t")
	if err != nil {
		t.Fatal(err)
	}
	back, err := workflow.Parse("t", out)
	if err != nil || back.OwnDigest() != g.OwnDigest() {
		t.Errorf("read back: %v\n%s", err, out)
	}
}

func TestAWorkflowSaysWhatItsSkillSays(t *testing.T) {
	src := "# t\n\n```mermaid\nflowchart TD\n  fix[fix]\n```\n\n## fix\nFix it.\n"
	fsys := fstest.MapFS{
		"SKILL.md":    {Data: []byte("---\nname: tidy\ndescription: Tidy a change.\nmetadata:\n  agentflow: WORKFLOW.md\n---\nTidy by hand.\n")},
		"WORKFLOW.md": {Data: []byte(src)},
	}
	s, err := agentskill.Load(fsys, "tidy")
	if err != nil {
		t.Fatal(err)
	}
	w, err := workflow.Load(s)
	if err != nil {
		t.Fatal(err)
	}
	if w.Graph.Description != "Tidy a change." {
		t.Errorf("description = %q", w.Graph.Description)
	}
}
