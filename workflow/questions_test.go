package workflow_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentflow/workflow"
)

const questions = "# q\n\n```mermaid\nflowchart TD\n" +
	"  list[[list]] --> pick([pick])\n  pick -->|chosen| review([review])\n  review -->|change| fix[[fix]]\n```\n\n" +
	"## list\n```yaml\ntool: list\noutputs:\n  ok:\n    files: {type: array, required: true, items: {type: string}}\n```\n\n" +
	"## pick\n```yaml\noutcomes: [chosen, none]\nchoices:\n  from: ${{ steps.list.outputs.files }}\n  select: many\n  outcome: chosen\n  as: files\n```\nWhich files?\n\n" +
	"## review\n```yaml\noutcomes: [go, change, park]\ntext: change\ntimeout: 4h\non_timeout:\n  outcome: park\n  data: {why: nobody answered}\noutputs:\n  park:\n    why: {type: string, required: true}\n```\nShip it?\n\n" +
	"## fix\n```yaml\ntool: fix\nwith:\n  files: ${{ steps.pick.outputs.files }}\n  note: ${{ steps.review.outputs.text }}\n```\n"

func TestQuestionSettingsReadBack(t *testing.T) {
	g, err := workflow.Parse("q", []byte(questions))
	if err != nil {
		t.Fatal(err)
	}
	pick, review := g.Nodes[1], g.Nodes[2]
	switch c := pick.Choices; {
	case c == nil || !c.Many || c.Outcome != "chosen" || c.As != "files" || c.From.From != "steps.list.outputs.files":
		t.Errorf("choices = %+v", c)
	case review.Text == nil || review.Text.Outcome != "change" || review.Text.As != "":
		t.Errorf("text = %+v", review.Text)
	case review.Timeout != 4*time.Hour || review.OnTimeout != "park" || review.OnTimeoutData == nil:
		t.Errorf("timeout = %s, %q, %v", review.Timeout, review.OnTimeout, review.OnTimeoutData)
	}
	out, err := workflow.Format(g, "q")
	if err != nil {
		t.Fatal(err)
	}
	back, err := workflow.Parse("q", out)
	if err != nil || back.OwnDigest() != g.OwnDigest() {
		t.Errorf("read back: %v\n%s", err, out)
	}
}

// A question with one outcome needs no outcome named for its pick.
func TestOneOutcomeTakesThePick(t *testing.T) {
	src := strings.Replace(questions, "outcomes: [chosen, none]\n", "outcomes: [chosen]\n", 1)
	src = strings.Replace(src, "  outcome: chosen\n", "", 1)
	g, err := workflow.Parse("q", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if c := g.Nodes[1].Choices; c.Outcome != "chosen" {
		t.Errorf("choices = %+v", c)
	}
}

func TestQuestionKeysAreChecked(t *testing.T) {
	for change, want := range map[[2]string]string{
		{"  select: many", "  select: some"}:                                                  `select "some"`,
		{"  as: files", "  as: files\n  label: x"}:                                            `choices: unknown key "label"`,
		{"text: change", "text: {outcome: change, said: x}"}:                                  `text: unknown key "said"`,
		{"  outcome: park\n", "  outcome: park\n  why: x\n"}:                                  `on_timeout: unknown key "why"`,
		{"timeout: 4h\n", ""}:                                                                 "on_timeout needs a timeout",
		{"  from: ${{ steps.list.outputs.files }}", "  from: [a, b]"}:                         "from is a reference",
		{"  from: ${{ steps.list.outputs.files }}", "  from: ${{ steps.list.outputs.file }}"}: "does not certainly produce",
		{"tool: fix\n", "tool: fix\ntext: go\n"}:                                              "only a person node takes free text",
		{"text: change", "text: ship"}:                                                        `text: outcome "ship" is not declared`,
	} {
		src := strings.Replace(questions, change[0], change[1], 1)
		if _, err := workflow.Parse("q", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", change[1], err, want)
		}
	}
}
