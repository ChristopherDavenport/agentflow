package workflow_test

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/workflow"
)

const isolated = "---\ncontext: fresh\nhidden: true\n---\n# t\n\n```mermaid\nflowchart TD\n  say[[say]]\n```\n\n## say\n```yaml\nrun: scripts/say.sh\n```\n"

func TestACalleesDeclarationsReadBack(t *testing.T) {
	g, err := workflow.Parse("t", []byte(isolated))
	if err != nil {
		t.Fatal(err)
	}
	if g.Context != agentflow.ContextFresh || !g.Hidden {
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
	src := strings.Replace(isolated, "context: fresh", "context: session", 1)
	if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), `context "session"`) {
		t.Errorf("context session: %v", err)
	}
}
