package workflow_test

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow/workflow"
)

const pacedLoop = "# t\n\n```mermaid\nflowchart TD\n" +
	"  draft[[draft]] --> review([review])\n  review -->|change| revise[[revise]]\n  revise --> review\n```\n\n" +
	"## draft\n```yaml\nrun: scripts/draft.sh\n```\n\n" +
	"## review\n```yaml\noutcomes: [change, done]\nmax_visits: person\n```\nGood?\n\n" +
	"## revise\n```yaml\nrun: scripts/revise.sh\nmax_visits: person\n```\n"

func TestMaxVisitsPersonReadsBack(t *testing.T) {
	g, err := workflow.Parse("t", []byte(pacedLoop))
	if err != nil {
		t.Fatal(err)
	}
	if n := g.Nodes[1]; !n.PersonPaced || n.MaxVisits != 0 || !g.Nodes[2].PersonPaced || g.Nodes[0].PersonPaced {
		t.Errorf("nodes = %+v", g.Nodes)
	}
	out, err := workflow.Format(g, "t")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "max_visits: person") {
		t.Errorf("format:\n%s", out)
	}
	back, err := workflow.Parse("t", out)
	if err != nil || back.OwnDigest() != g.OwnDigest() {
		t.Errorf("read back: %v\n%s", err, out)
	}
}

func TestMaxVisitsPersonIsChecked(t *testing.T) {
	for change, want := range map[[2]string]string{
		{"max_visits: person\n```\nGood?", "max_visits: often\n```\nGood?"}:      "want a number from 1 to 99, or person",
		{"max_visits: person\n```\nGood?", "max_visits: [person]\n```\nGood?"}:   "want a number from 1 to 99, or person",
		{"run: scripts/revise.sh\nmax_visits: person", "run: scripts/revise.sh"}: `it is on a loop through "review"`,
		{"run: scripts/draft.sh", "run: scripts/draft.sh\nmax_visits: person"}:   "no path comes back",
	} {
		src := strings.Replace(pacedLoop, change[0], change[1], 1)
		if src == pacedLoop {
			t.Fatalf("%q is not in the source", change[0])
		}
		if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", change[1], err, want)
		}
	}
	// A number is still a number.
	src := strings.Replace(pacedLoop, "run: scripts/revise.sh\nmax_visits: person", "run: scripts/revise.sh\nmax_visits: 50", 1)
	g, err := workflow.Parse("t", []byte(src))
	if err != nil || g.Nodes[2].MaxVisits != 50 || g.Nodes[2].PersonPaced {
		t.Errorf("max_visits: 50 = %+v, %v", g.Nodes[2], err)
	}
}
