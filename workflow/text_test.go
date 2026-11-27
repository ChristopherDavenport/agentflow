package workflow_test

import (
	"os"
	"path/filepath"
	"strings"
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

const skilled = "# t\n\n```mermaid\nflowchart TD\n  fix[fix]\n```\n\n## fix\n```yaml\nskills: [go-style]\n```\nFix it.\n"

// plainSkill writes a skill that is not a workflow under dir.
func plainSkill(t *testing.T, dir, name, body string) *agentskill.Skill {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "---\nname: " + name + "\ndescription: How to write Go here.\nallowed-tools: Bash(rm:*)\n---\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := agentskill.LoadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAStepsSkillsArePinned(t *testing.T) {
	dir := t.TempDir()
	root := skill(t, dir, "tidy", map[string]string{"WORKFLOW.md": skilled})
	plainSkill(t, dir, "go-style", "Wrap errors with %w.")
	catalog, err := agentskill.DiscoverDirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workflow.LoadWith(root, workflow.FromCatalog(catalog))
	if err != nil {
		t.Fatal(err)
	}
	sk := w.Graph.Nodes[0].Skills
	if len(sk) != 1 || sk[0].Name != "go-style" || !strings.Contains(sk[0].Instructions, "Wrap errors with %w.") {
		t.Fatalf("skills = %+v", sk)
	}
	// What the model is told is in the definition, so its digest moves
	// with the skill, and the skill on disk changing later changes
	// nothing already loaded.
	before := w.Graph.Digest()
	if err := os.WriteFile(filepath.Join(dir, "go-style", "SKILL.md"), []byte("---\nname: go-style\ndescription: x\n---\nPanic freely.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Graph.Nodes[0].Skills[0].Instructions, "Wrap errors") {
		t.Error("a loaded skill's text changed with the file")
	}
	again, err := workflow.LoadWith(root, func(name string) (*agentskill.Skill, error) {
		return agentskill.LoadDir(filepath.Join(dir, name))
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Graph.Digest() == before {
		t.Error("a changed skill keeps the digest")
	}
	out, err := workflow.Format(w.Graph, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "skills:\n    - go-style") && !strings.Contains(string(out), "skills: [go-style]") {
		t.Errorf("format wrote:\n%s", out)
	}
}

func TestAStepsSkillsMustBeFound(t *testing.T) {
	dir := t.TempDir()
	root := skill(t, dir, "tidy", map[string]string{"WORKFLOW.md": skilled})
	if _, err := workflow.Load(root); err == nil || !strings.Contains(err.Error(), "nothing was given to find skills") {
		t.Errorf("load without find: %v", err)
	}
	catalog, err := agentskill.DiscoverDirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.LoadWith(root, workflow.FromCatalog(catalog)); err == nil || !strings.Contains(err.Error(), `node "fix" names the skill go-style: no such skill`) {
		t.Errorf("an unknown skill: %v", err)
	}
	person := strings.Replace(skilled, "fix[fix]", "fix([fix])", 1)
	if _, err := workflow.Parse("t", []byte(person)); err == nil || !strings.Contains(err.Error(), "skills are a model step's") {
		t.Errorf("a person's skills: %v", err)
	}
}
