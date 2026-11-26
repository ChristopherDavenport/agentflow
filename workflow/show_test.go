package workflow_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow/workflow"
)

const showSource = "# t\n\n```mermaid\nflowchart TD\n" +
	"  gather[[gather]] --> list[[list]]\n  list --> review[review]\n```\n\n" +
	"## gather\n```yaml\nrun: scripts/gather.sh\noutputs:\n  ok:\n    summary: {type: string, required: true}\n    rows: {type: array, required: true}\nshow: [summary]\n```\n\n" +
	"## list\n```yaml\nrun: scripts/list.sh\nshow: none\n```\n\n" +
	"## review\n```yaml\nwith:\n  rows: ${{ steps.gather.outputs.rows }}\n```\nLook.\n"

func TestShowReadsBack(t *testing.T) {
	g, err := workflow.Parse("t", []byte(showSource))
	if err != nil {
		t.Fatal(err)
	}
	gather, list := g.Nodes[0], g.Nodes[1]
	switch {
	case gather.Show == nil || !slices.Equal(gather.Show.Members, []string{"summary"}):
		t.Errorf("gather shows %+v", gather.Show)
	case list.Show == nil || len(list.Show.Members) != 0:
		t.Errorf("list shows %+v", list.Show)
	case g.Nodes[2].Show != nil:
		t.Errorf("review shows %+v", g.Nodes[2].Show)
	}
	out, err := workflow.Format(g, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "show: none") || !strings.Contains(string(out), "show:\n    - summary") && !strings.Contains(string(out), "show: [summary]") {
		t.Errorf("written:\n%s", out)
	}
	back, err := workflow.Parse("t", out)
	if err != nil || back.OwnDigest() != g.OwnDigest() {
		t.Errorf("read back: %v\n%s", err, out)
	}
}

func TestShowIsChecked(t *testing.T) {
	for change, want := range map[[2]string]string{
		{"show: [summary]", "show: [sumary]"}:          `did you mean "summary"?`,
		{"show: [summary]", "show: []"}:                "show: want none, or a list",
		{"show: none", "show: all"}:                    "show: want none, or a list",
		{"show: none", "show: [rows]"}:                 "declares no outputs to hold them to",
		{"with:\n  rows", "show: none\nwith:\n  rows"}: "only a script or tool step has show",
	} {
		src := strings.Replace(showSource, change[0], change[1], 1)
		_, err := workflow.Parse("t", []byte(src))
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", change[1], err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: err = %v, want %q", change[1], err, want)
		}
	}
}
