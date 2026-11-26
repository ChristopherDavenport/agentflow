package workflow_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/workflow"
	"github.com/ChristopherDavenport/agentskill"
)

const filed = "# t\n\n```mermaid\nflowchart TD\n  list[[list]] --> work[work]\n```\n\n" +
	"## list\n```yaml\nrun: scripts/list.sh\n```\n\n" +
	"## work\n```yaml\nfiles: [docs/spec.md, \"${{ steps.list.outputs }}\", ./prompts/style.md]\n```\nWork.\n"

// loadFiled loads the workflow with the skill's files, src as its
// WORKFLOW.md.
func loadFiled(t *testing.T, src string, extra fstest.MapFS) (*workflow.Workflow, error) {
	t.Helper()
	fsys := fstest.MapFS{
		"SKILL.md":         {Data: []byte("---\nname: filed\ndescription: Work.\nmetadata:\n  agentflow: WORKFLOW.md\n---\nWork by hand.\n")},
		"WORKFLOW.md":      {Data: []byte(src)},
		"scripts/list.sh":  {Data: []byte("#!/bin/sh\n")},
		"prompts/style.md": {Data: []byte("Be terse.")},
	}
	for k, v := range extra {
		fsys[k] = v
	}
	s, err := agentskill.Load(fsys, "filed")
	if err != nil {
		t.Fatal(err)
	}
	return workflow.Load(s)
}

func TestFilesLoadAndReadBack(t *testing.T) {
	w, err := loadFiled(t, filed, nil)
	if err != nil {
		t.Fatal(err)
	}
	work := w.Graph.Nodes[1]
	if len(work.Files) != 3 || work.Files[1].From != "steps.list.outputs" || work.SkillFiles["prompts/style.md"] != "Be terse." {
		t.Errorf("work = %+v", work)
	}
	out, err := workflow.Format(w.Graph, "t")
	if err != nil {
		t.Fatal(err)
	}
	if back, err := workflow.Parse("filed", out); err != nil || len(back.Nodes[1].Files) != 3 {
		t.Errorf("read back: %v\n%s", err, out)
	}
}

func TestASkillsFileIsCheckedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		path  string
		extra fstest.MapFS
		want  string
	}{
		{"./prompts/styel.md", nil, `not in the skill; did you mean "./prompts/style.md"?`},
		{"./prompts", nil, "is a directory"},
		{"./../outside.md", nil, "clean path"},
		{"./big.md", fstest.MapFS{"big.md": {Data: []byte(strings.Repeat("x", agentflow.MaxFile+1))}}, "over"},
		{"./bin.md", fstest.MapFS{"bin.md": {Data: []byte{0xff, 0xfe}}}, "not valid UTF-8"},
	} {
		src := strings.Replace(filed, "./prompts/style.md", tc.path, 1)
		if _, err := loadFiled(t, src, tc.extra); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.path, err, tc.want)
		}
	}
	if _, err := workflow.Parse("t", []byte(strings.Replace(filed, "docs/spec.md", "../spec.md", 1))); err == nil || !strings.Contains(err.Error(), "inside the workspace") {
		t.Errorf("err = %v", err)
	}
}
