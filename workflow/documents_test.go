package workflow_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/agentflow/workflow"
	"github.com/ChristopherDavenport/agentskill"
)

const documented = "---\ninputs:\n  brief: {type: string}\ndocuments:\n  plan: {from: templates/plan.md}\n  brief: {input: brief}\n  notes:\n---\n# t\n\n```mermaid\nflowchart TD\n" +
	"  load[[load]] --> draft[draft]\n```\n\n" +
	"## load\n```yaml\nrun: scripts/load.sh\ndocuments:\n  write: {notes: text}\n```\n\n" +
	"## draft\n```yaml\ndocuments:\n  read: [brief, notes]\n  edit: [plan]\n```\nDraft the plan.\n"

func TestDocumentsLoadAndReadBack(t *testing.T) {
	fsys := fstest.MapFS{
		"SKILL.md":          {Data: []byte("---\nname: drafts\ndescription: Draft.\nmetadata:\n  agentflow: WORKFLOW.md\n---\nDraft by hand.\n")},
		"WORKFLOW.md":       {Data: []byte(documented)},
		"templates/plan.md": {Data: []byte("# Plan\n")},
		"scripts/load.sh":   {Data: []byte("#!/bin/sh\n")},
	}
	s, err := agentskill.Load(fsys, "drafts")
	if err != nil {
		t.Fatal(err)
	}
	w, err := workflow.Load(s)
	if err != nil {
		t.Fatal(err)
	}
	g := w.Graph
	if d := g.Documents["plan"]; d.Text != "# Plan\n" || d.From != "templates/plan.md" {
		t.Errorf("plan = %+v", d)
	}
	if g.Documents["brief"].Input != "brief" || g.Nodes[0].Writes["notes"] != "text" || g.Nodes[1].Edits[0] != "plan" {
		t.Errorf("graph = %+v", g)
	}
	out, err := workflow.Format(g, "t")
	if err != nil {
		t.Fatal(err)
	}
	back, err := workflow.Parse("drafts", out)
	if err != nil || !strings.Contains(string(out), "from: templates/plan.md") || back.Nodes[1].Reads[1] != "notes" {
		t.Errorf("read back: %v\n%s", err, out)
	}
}

func TestDocumentSettingsAreChecked(t *testing.T) {
	for change, want := range map[[2]string]string{
		{"  plan: {from: templates/plan.md}", "  plan: {frm: templates/plan.md}"}: `unknown key "frm"; did you mean "from"?`,
		{"  edit: [plan]", "  edits: [plan]"}:                                     `unknown key "edits"; did you mean "edit"?`,
		{"  edit: [plan]", "  edit: [pla]"}:                                       `no document "pla"; did you mean "plan"?`,
	} {
		src := strings.Replace(documented, change[0], change[1], 1)
		if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", change[1], err, want)
		}
	}
}
