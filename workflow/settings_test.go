package workflow_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/workflow"
)

const stepSettings = "---\nmax_steps: 40\nmodel: big\nthinking: low\n---\n# t\n\n```mermaid\nflowchart TD\n" +
	"  lint[[lint]] --> fix[fix]\n  fix --> ask([ask])\n  fix -->|stuck| ask\n```\n\n" +
	"## lint\n```yaml\nrun: scripts/lint.sh\ntimeout: 90s\n```\n\n" +
	"## fix\n```yaml\nmodel: small\nthinking: high\ntimeout: 10m\ntools: [read, edit]\npolicy:\n  write: [src/**]\n  no_read: [.env]\n  bash: [go test]\n```\nFix it.\n\n" +
	"## ask\n```yaml\noutcomes: [ok, no]\njudge:\n  criteria: Only if it is fixed.\n  outcomes: [ok]\n  tools: [read]\n  policy:\n    no_read: [secrets/**]\n```\nFixed?\n"

func TestStepSettingsReadBack(t *testing.T) {
	g, err := workflow.Parse("t", []byte(stepSettings))
	if err != nil {
		t.Fatal(err)
	}
	fix := g.Nodes[1]
	switch {
	case g.MaxSteps != 40 || g.Model != "big" || g.Thinking != "low":
		t.Errorf("graph = %+v", g)
	case g.Nodes[0].Timeout != 90*time.Second || fix.Timeout != 10*time.Minute:
		t.Errorf("timeouts = %s, %s", g.Nodes[0].Timeout, fix.Timeout)
	case fix.Model != "small" || fix.Thinking != "high" || fix.Policy == nil || fix.Policy.Write[0] != "src/**" || fix.Next[agentflow.Stuck] != "ask":
		t.Errorf("fix = %+v", fix)
	case g.Nodes[2].Judge.Policy == nil || g.Nodes[2].Judge.Policy.NoRead[0] != "secrets/**":
		t.Errorf("judge = %+v", g.Nodes[2].Judge)
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

func TestStepSettingsAreChecked(t *testing.T) {
	for change, want := range map[[2]string]string{
		{"timeout: 90s", "timeout: soon"}:                                       "timeout \"soon\"",
		{"thinking: high", "thinking: hard"}:                                    "thinking \"hard\"",
		{"  write: [src/**]", "  writes: [src/**]"}:                             "field writes not found",
		{"  no_read: [.env]", "  no_read: [/etc]"}:                              "below the workspace's root",
		{"tools: [read, edit]", "tools: [read, edit]\noutcomes: [ok, stuck]"}:   "flow_stuck reports",
		{"run: scripts/lint.sh", "run: scripts/lint.sh\npolicy:\n  bash: [ls]"}: "a policy is a model step's",
	} {
		src := strings.Replace(stepSettings, change[0], change[1], 1)
		if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", change[1], err, want)
		}
	}
}
