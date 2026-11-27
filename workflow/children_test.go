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
	return inMemoryOf(map[string]*agentturn.Agent{}, model, name, tools)
}

// inMemoryOf is inMemory, with the agents it makes kept in agents by spawn.
//
// With no session to fork, a seeded child's agent is given what its
// caller's held, once, and its record is what it holds past that.
func inMemoryOf(agents map[string]*agentturn.Agent, model openresponses.Streamer, name string, tools []agenttool.Tool) agentflow.Option {
	seeds := map[string]agentturn.Transcript{}
	return agentflow.Children(func(ctx context.Context, c agentflow.Child) (context.Context, *agentturn.Agent, error) {
		var opts []agentflow.Option
		if c.Seeded {
			if _, ok := seeds[c.Spawn]; !ok {
				seeds[c.Spawn] = agentturn.Transcript(c.Seed)
			}
			seed := seeds[c.Spawn]
			opts = []agentflow.Option{
				agentflow.History(func(context.Context) (agentturn.Transcript, error) {
					if a := agents[c.Spawn]; a != nil {
						return a.State().Transcript[len(seed):], nil
					}
					return nil, nil
				}),
				agentflow.Inherited(func(context.Context) (agentturn.Transcript, error) { return seed, nil }),
			}
		}
		cfg, err := c.Apply(agentturn.Config{Model: model, ModelName: cmp.Or(c.Settings.Model, name), Tools: tools}, opts...)
		if err != nil {
			return nil, nil, err
		}
		if a := agents[c.Spawn]; a != nil {
			return ctx, a, a.SetConfig(cfg)
		}
		a := agentturn.New(cfg, agentturn.WithTranscript(seeds[c.Spawn]))
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
	if n := g.Nodes[0]; n.Dir.Literal != "services/api" || n.Model != "small" || n.Child != "helper" || n.Context != agentflow.ContextFresh {
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
