package workflow_test

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow/workflow"
)

const asking = "# t\n\n```mermaid\nflowchart TD\n  build[[build]] --> ship([ship])\n```\n\n" +
	"## build\n```yaml\nrun: scripts/build.sh\noutputs:\n  ok:\n    version: {type: string, required: true}\n```\n\n" +
	"## ship\n```yaml\noutcomes: [yes, no]\n```\nShip ${{ steps.build.outputs.version }}?\n"

func TestAQuestionsTemplateReadsBack(t *testing.T) {
	g, err := workflow.Parse("t", []byte(asking))
	if err != nil {
		t.Fatal(err)
	}
	if g.Nodes[1].Prompt != "Ship ${{ steps.build.outputs.version }}?" {
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
	src := strings.Replace(asking, "ship([ship])", "ship[ship]", 1)
	src = strings.Replace(src, "outcomes: [yes, no]", "outcomes: [yes, no]\ntools: []", 1)
	if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), "only a person step's question takes ${{ }}") {
		t.Errorf("a model step's prompt: %v", err)
	}
}
