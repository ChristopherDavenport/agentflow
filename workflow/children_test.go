package workflow_test

import (
	"cmp"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/workflow"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// inMemory opens children in memory, one agent for each spawn, with the
// model and tools the parent has. It records nothing of them.
func inMemory(model openresponses.Streamer, name string, tools []agenttool.Tool) agentflow.Option {
	agents := map[string]*agentturn.Agent{}
	return agentflow.Children(func(ctx context.Context, c agentflow.Child) (context.Context, *agentturn.Agent, error) {
		cfg, err := c.Apply(agentturn.Config{Model: model, ModelName: cmp.Or(c.Settings.Model, name), Tools: tools})
		if err != nil {
			return nil, nil, err
		}
		if a := agents[c.Spawn]; a != nil {
			return ctx, a, a.SetConfig(cfg)
		}
		a := agentturn.New(cfg)
		agents[c.Spawn] = a
		return ctx, a, nil
	})
}

func TestChildSettingsLoad(t *testing.T) {
	const good = "# t\n\n```mermaid\nflowchart TD\n  a[a]\n```\n\n## a\n```yaml\ncontext: fresh\ndir: services/api\nmodel: small\nchild: helper\n```\nDo it.\n"
	g, err := workflow.Parse("t", []byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if n := g.Nodes[0]; n.Dir != "services/api" || n.Model != "small" || n.Child != "helper" || n.Context != agentflow.ContextFresh {
		t.Errorf("node = %+v", n)
	}
	// A step that asks for another model than its workflow's runs in a
	// child unless it says otherwise.
	if g, err := workflow.Parse("t", []byte(strings.Replace(good, "context: fresh\n", "", 1))); err != nil || g.Nodes[0].Context != "" {
		t.Errorf("by model: %v", err)
	}
	for src, want := range map[string]string{
		strings.Replace(good, "context: fresh\n", "context: session\n", 1): "dir is a setting of a child",
		strings.Replace(good, "services/api", "../up", 1):                  `dir "../up"`,
		strings.Replace(good, "child: helper", "child: a b", 1):            `child "a b"`,
		strings.Replace(good, "model: small", "models: small", 1):          "models",
	} {
		if _, err := workflow.Parse("t", []byte(src)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
	// An instruction file the skill does not hold is a load error.
	dir := copySkill(t)
	path := filepath.Join(dir, "prompts", "review.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s, err := agentskill.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workflow.Load(s); err == nil || !strings.Contains(err.Error(), "prompts/review.md") {
		t.Errorf("err = %v", err)
	}
}
