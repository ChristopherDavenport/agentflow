package workflow_test

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow/workflow"
)

func TestADirTakesAReference(t *testing.T) {
	const src = "# t\n\n```mermaid\nflowchart TD\n  setup[[setup]] --> build[build]\n  build --> check[[check]]\n```\n\n" +
		"## setup\n```yaml\nrun: scripts/setup.sh\noutputs:\n  ok:\n    dir: {type: string, required: true}\n```\n\n" +
		"## build\n```yaml\ncontext: fresh\ndir: ${{ steps.setup.outputs.dir }}\n```\nBuild it.\n\n" +
		"## check\n```yaml\nrun: scripts/check.sh\ndir: ${{ steps.setup.outputs.dir }}\n```\n"
	g, err := workflow.Parse("t", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 2} {
		if d := g.Nodes[i].Dir; d.From != "steps.setup.outputs.dir" {
			t.Errorf("node %s dir = %+v", g.Nodes[i].ID, d)
		}
	}
	out, err := workflow.Format(g, "t")
	if err != nil {
		t.Fatal(err)
	}
	back, err := workflow.Parse("t", out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if back.Digest() != g.Digest() {
		t.Errorf("the dir did not survive Format:\n%s", out)
	}
	for src, want := range map[string]string{
		strings.Replace(src, "dir: ${{ steps.setup.outputs.dir }}\n```\nBuild", "dir: ${{ steps.setup.outputs.where }}\n```\nBuild", 1): `does not certainly produce "where"`,
		strings.Replace(src, "dir: {type: string", "dir: {type: integer", 1):                                                            "its schema says integer",
		strings.Replace(src, "dir: ${{ steps.setup.outputs.dir }}\n```\nBuild", "dir: [a, b]\n```\nBuild", 1):                           "dir is a path or a reference to one",
		strings.Replace(src, "dir: ${{ steps.setup.outputs.dir }}\n```\nBuild", "dir: a/${{ inputs.x }}\n```\nBuild", 1):                "a reference is the whole value",
		strings.Replace(src, "context: fresh\n", "", 1):                                                                                 "dir is a setting of a child or of a script",
	} {
		if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}
