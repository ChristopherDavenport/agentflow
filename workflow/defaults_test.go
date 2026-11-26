package workflow_test

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow/workflow"
)

const defaulted = "---\ninputs:\n  who: {type: string, required: true}\n  how: {type: string, default: hello}\n  times: {type: integer, default: 2}\n---\n# t\n\n```mermaid\nflowchart TD\n" +
	"  say[[say]]\n```\n\n## say\n```yaml\nrun: scripts/say.sh\nwith:\n  how: ${{ inputs.how }}\n```\n"

func TestInputDefaultsReadBack(t *testing.T) {
	g, err := workflow.Parse("t", []byte(defaulted))
	if err != nil {
		t.Fatal(err)
	}
	if g.InputDefaults["how"] != "hello" || g.InputDefaults["times"] != 2 || len(g.InputDefaults) != 2 {
		t.Errorf("defaults = %v", g.InputDefaults)
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

func TestInputDefaultsAreCheckedWhenParsed(t *testing.T) {
	for change, want := range map[[2]string]string{
		{"how: {type: string, default: hello}", "how: {type: string, required: true, default: hello}"}: "is required and has a default",
		{"times: {type: integer, default: 2}", "times: {type: integer, default: two}"}:                 "input times: default:",
		{"how: {type: string, default: hello}", "how: {type: string, default: '${{ inputs.who }}'}"}:   "a default is a literal",
		{"how: {type: string, default: hello}", "how: {type: object, properties: {a: {default: 1}}}"}:  "only a workflow's input has a default",
	} {
		src := strings.Replace(defaulted, change[0], change[1], 1)
		if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", change[1], err, want)
		}
	}
}
