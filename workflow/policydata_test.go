package workflow_test

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow/workflow"
)

const fromPlan = "# t\n\n```mermaid\nflowchart TD\n  plan[[plan]] --> edit[edit]\n```\n\n" +
	"## plan\n```yaml\nrun: scripts/plan.sh\noutputs:\n  ok:\n    files: {type: array, items: {type: string}, required: true}\n```\n\n" +
	"## edit\n```yaml\ntools: [write]\npolicy:\n  allow:\n    - {rule: write, path: '${{ steps.plan.outputs.files }}', note: the plan's files}\n```\nEdit.\n"

func TestAPolicyRuleFromTheRecordReadsBack(t *testing.T) {
	g, err := workflow.Parse("t", []byte(fromPlan))
	if err != nil {
		t.Fatal(err)
	}
	r := g.Nodes[1].Policy.Allow[0]
	if r.Rule != "write" || r.Path.From != "steps.plan.outputs.files" || r.Note != "the plan's files" {
		t.Errorf("rule = %+v", r)
	}
	out, err := workflow.Format(g, "t")
	if err != nil {
		t.Fatal(err)
	}
	back, err := workflow.Parse("t", out)
	if err != nil || back.OwnDigest() != g.OwnDigest() {
		t.Errorf("read back: %v\n%s", err, out)
	}
	src := strings.Replace(fromPlan, "path: '${{ steps.plan.outputs.files }}'", "paths: '${{ steps.plan.outputs.files }}'", 1)
	if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), `did you mean "path"?`) {
		t.Errorf("a misspelt key: %v", err)
	}
}
