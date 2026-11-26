package workflow_test

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow/workflow"
)

const excluding = "---\nexclude_tools: [bash]\n---\n# t\n\n```mermaid\nflowchart TD\n" +
	"  look[look] --> fix[fix]\n```\n\n## look\n```yaml\ntools: all\nexclude_tools: [fetch]\n```\nLook.\n\n" +
	"## fix\n```yaml\ntools: [read, edit]\n```\nFix.\n"

func TestExcludedToolsReadBack(t *testing.T) {
	g, err := workflow.Parse("t", []byte(excluding))
	if err != nil {
		t.Fatal(err)
	}
	look := g.Nodes[0]
	if !look.AllTools || look.Tools != nil || len(look.ExcludeTools) != 1 || len(g.ExcludeTools) != 1 {
		t.Errorf("look = %+v, workflow excludes %v", look, g.ExcludeTools)
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

func TestExcludedToolsAreCheckedWhenParsed(t *testing.T) {
	for change, want := range map[[2]string]string{
		{"tools: all", "tools: every"}:                      "tools: want a list of tools, or all",
		{"tools: [read, edit]", "tools: [read, bash]"}:      "names the tool bash, which it excludes",
		{"exclude_tools: [fetch]", "exclude_tool: [fetch]"}: `unknown key "exclude_tool"; did you mean "exclude_tools"?`,
	} {
		src := strings.Replace(excluding, change[0], change[1], 1)
		if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", change[1], err, want)
		}
	}
}
