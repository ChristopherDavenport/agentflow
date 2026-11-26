package examples

import (
	"cmp"
	"context"

	"github.com/ChristopherDavenport/agentflow"
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
