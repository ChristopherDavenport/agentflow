package agentflow

import (
	"cmp"
	"context"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	agentool "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"
)

// kin opens children in memory, one agent for each spawn, serving them
// the model and tools the parent has.
type kin struct {
	model  openresponses.Streamer
	name   string
	tools  []agenttool.Tool
	agents map[string]*agentturn.Agent
	opened []Child
}

func newKin(model openresponses.Streamer, name string, tools ...agenttool.Tool) *kin {
	return &kin{model: model, name: name, tools: tools, agents: map[string]*agentturn.Agent{}}
}

func (k *kin) open(ctx context.Context, c Child) (context.Context, *agentturn.Agent, error) {
	k.opened = append(k.opened, c)
	cfg, err := c.Apply(agentturn.Config{Model: k.model, ModelName: cmp.Or(c.Settings.Model, k.name), Tools: k.tools})
	if err != nil {
		return nil, nil, err
	}
	if a := k.agents[c.Spawn]; a != nil {
		return ctx, a, a.SetConfig(cfg)
	}
	a := agentturn.New(cfg)
	k.agents[c.Spawn] = a
	return ctx, a, nil
}

func (k *kin) option() Option { return Children(k.open) }

// recordedKin opens children in child sessions of a recorded one, as a
// host with agentturn/session does: a new agent each time, seeded from
// the child's session, which is the child's record.
type recordedKin struct {
	store  *jsonl.Store
	rec    *session.Recorder
	model  openresponses.Streamer
	name   string
	tools  []agenttool.Tool
	opened []Child
	// fold, if set, makes each child's compactor.
	fold func() func(context.Context, agentturn.Transcript) (agentturn.Transcript, error)
}

func (k *recordedKin) open(ctx context.Context, c Child) (context.Context, *agentturn.Agent, error) {
	k.opened = append(k.opened, c)
	cctx, err := k.rec.ContinueChild(ctx, c.Spawn)
	if errors.Is(err, session.ErrNoSpawn) {
		cctx, err = k.rec.StartChild(ctx, c.Spawn)
	}
	if err != nil {
		return nil, nil, err
	}
	id := session.SessionIDFromContext(cctx)
	history := History(func(ctx context.Context) (agentturn.Transcript, error) {
		s, err := k.store.Open(ctx, id)
		if errors.Is(err, agentsession.ErrNoSession) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return pathItems(s), nil
	})
	base := agentturn.Config{Model: k.model, ModelName: cmp.Or(c.Settings.Model, k.name), Tools: k.tools}
	if k.fold != nil {
		base.Transform = k.fold()
	}
	cfg, err := c.Apply(base, history)
	if err != nil {
		return nil, nil, err
	}
	var seed []agentturn.Option
	switch s, err := k.store.Open(ctx, id); {
	case err == nil:
		if seed, err = session.AgentOptions(s, k.rec.ReadOptions()...); err != nil {
			return nil, nil, err
		}
	case !errors.Is(err, agentsession.ErrNoSession):
		return nil, nil, err
	}
	a := agentturn.New(cfg, seed...)
	a.Subscribe(func(ctx context.Context, ev agentturn.Event) error {
		k.rec.Observe(ctx, ev)
		return nil
	})
	return agentool.ContextWithConfig(cctx, cfg), a, nil
}

func (k *recordedKin) option() Option { return Children(k.open) }

// childSessions reads a child's items by spawn, from under parent.
func childSessions(t *testing.T, store *jsonl.Store) func(string) (agentturn.Transcript, error) {
	t.Helper()
	return func(spawn string) (agentturn.Transcript, error) {
		ctx := context.Background()
		for sum, err := range store.List(ctx, agentsession.ListFilter{}) {
			if err != nil {
				return nil, err
			}
			if sum.Header.SpawnedBy != spawn {
				continue
			}
			s, err := store.Open(ctx, sum.Header.ID)
			if err != nil {
				return nil, err
			}
			return pathItems(s), nil
		}
		return nil, errors.New("no session of spawn " + spawn)
	}
}

// spawnedID is the ID of the session a spawn names.
func spawnedID(t *testing.T, store *jsonl.Store, spawn string) string {
	t.Helper()
	for sum, err := range store.List(context.Background(), agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		if sum.Header.SpawnedBy == spawn {
			return sum.Header.ID
		}
	}
	t.Fatalf("no session of spawn %s", spawn)
	return ""
}

// kinApply is Apply with children opened in memory, serving them the
// config's model and tools, unless a later option opens them otherwise.
func kinApply(g *Graph, cfg agentturn.Config, opts ...Option) (agentturn.Config, error) {
	kids := newKin(cfg.Model, cfg.ModelName, cfg.Tools...)
	return Apply(g, cfg, append([]Option{kids.option()}, opts...)...)
}
