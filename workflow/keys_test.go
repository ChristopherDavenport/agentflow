package workflow_test

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow/workflow"
	"github.com/ChristopherDavenport/agentskill"
)

const spelt = "---\ndescription: d\ninputs:\n  pr: {type: integer, required: true}\n---\n# t\n\n```mermaid\nflowchart TD\n" +
	"  gather[[gather]] --> review[review]\n  review -->|clean| ask([ask])\n  review -->|findings| ask\n```\n\n" +
	"## gather\n```yaml\nrun: scripts/gather.sh\nwith:\n  pr: ${{ inputs.pr }}\n```\n\n" +
	"## review\n```yaml\noutcomes: [clean, findings]\noutputs:\n  findings:\n    list: {type: array, required: true}\n```\nReview it.\n\n" +
	"## ask\n```yaml\noutcomes: [go, note, stop]\nunattended: {outcome: stop}\ntext: {outcome: note, as: words}\ntimeout: 1h\non_timeout: {outcome: stop}\njudge:\n  criteria: c\n  outcomes: [go]\n```\nGo?\n"

func TestMisspeltKeysAndNamesAreNamed(t *testing.T) {
	if _, err := workflow.Parse("t", []byte(spelt)); err != nil {
		t.Fatal(err)
	}
	for change, want := range map[[2]string]string{
		{"description: d\n", "descripton: d\n"}:                                       `unknown key "descripton"; did you mean "description"?`,
		{"run: scripts/gather.sh", "runn: scripts/gather.sh"}:                         `unknown key "runn"; did you mean "run"?`,
		{"outcomes: [clean, findings]", "outcome: [clean, findings]"}:                 `unknown key "outcome"; did you mean "outcomes"?`,
		{"  criteria: c", "  criterea: c"}:                                            `unknown key "criterea"; did you mean "criteria"?`,
		{"unattended: {outcome: stop}", "unattended: {outcom: stop}"}:                 `unattended: unknown key "outcom"; did you mean "outcome"?`,
		{"text: {outcome: note, as: words}", "text: {outcome: note, ass: words}"}:     `text: unknown key "ass"; did you mean "as"?`,
		{"on_timeout: {outcome: stop}", "on_timeout: {outcme: stop}"}:                 `on_timeout: unknown key "outcme"; did you mean "outcome"?`,
		{"list: {type: array, required: true}", "list: {typ: array, required: true}"}: `unknown key "typ"; did you mean "type"?`,
		{"## review\n", "## reveiw\n"}:                                                `section "reveiw" is not a node of the diagram; did you mean "review"?`,
		{"review -->|clean| ask", "review -->|clen| ask"}:                             `undeclared outcome "clen"; did you mean "clean"?`,
		{"  findings:\n    list", "  finding:\n    list"}:                             `undeclared outcome "finding"; did you mean "findings"?`,
		{"pr: ${{ inputs.pr }}", "pr: ${{ inputs.p }}"}:                               `not an input of the workflow; did you mean "pr"?`,
	} {
		src := strings.Replace(spelt, change[0], change[1], 1)
		if src == spelt {
			t.Fatalf("%q is not in the file", change[0])
		}
		if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", change[1], err, want)
		}
	}
}

func TestMisspeltSkillsAndFilesAreNamed(t *testing.T) {
	dir := t.TempDir()
	plainSkill(t, dir, "go-style", "Wrap errors.")
	root := skill(t, dir, "tidy", map[string]string{
		"WORKFLOW.md":    strings.Replace(skilled, "go-style", "go-styel", 1),
		"flows/check.md": "# c\n",
	})
	catalog, err := agentskill.DiscoverDirs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.LoadWith(root, workflow.FromCatalog(catalog)); err == nil || !strings.Contains(err.Error(), `no such skill; did you mean "go-style"?`) {
		t.Errorf("a misspelt skill: %v", err)
	}
	calls := "# t\n\n```mermaid\nflowchart TD\n  check[[check]]\n```\n\n## check\n```yaml\nflow: ./flows/chek.md\n```\n"
	other := skill(t, t.TempDir(), "tidy", map[string]string{"WORKFLOW.md": calls, "flows/check.md": "# c\n"})
	if _, err := workflow.Load(other); err == nil || !strings.Contains(err.Error(), `did you mean "./flows/check.md"?`) {
		t.Errorf("a misspelt file: %v", err)
	}
}
