package agentflow

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
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
	// seeds is what each agent was seeded with, which is no part of its
	// record.
	seeds  map[string]openresponses.Items
	opened []Child
}

// read gives a child's record by spawn, as Runs' open reads it: what its
// agent holds past its seed.
func (k *kin) read(spawn string) (agentturn.Transcript, error) {
	a := k.agents[spawn]
	if a == nil {
		return nil, errors.New("no child of spawn " + spawn)
	}
	return a.State().Transcript[len(k.seeds[spawn]):], nil
}

// kins are the in-memory children of the agents nestedAgent and newAgent
// make, so a test reads the whole tree.
var kins sync.Map

// under reads the children of a's tree, for Runs.
func under(a *agentturn.Agent) func(string) (agentturn.Transcript, error) {
	if k, ok := kins.Load(a); ok {
		return k.(*kin).read
	}
	return nil
}

func newKin(model openresponses.Streamer, name string, tools ...agenttool.Tool) *kin {
	return &kin{model: model, name: name, tools: tools, agents: map[string]*agentturn.Agent{}, seeds: map[string]openresponses.Items{}}
}

func (k *kin) open(ctx context.Context, c Child) (context.Context, *agentturn.Agent, error) {
	k.opened = append(k.opened, c)
	// With no session to fork, a seeded child's agent is given what its
	// caller's held, once, and its record is what it holds past that.
	var opts []Option
	if c.Seeded {
		if _, ok := k.seeds[c.Spawn]; !ok {
			k.seeds[c.Spawn] = c.Seed
		}
		seed := agentturn.Transcript(k.seeds[c.Spawn])
		opts = []Option{
			History(func(context.Context) (agentturn.Transcript, error) {
				if k.agents[c.Spawn] == nil {
					return nil, nil
				}
				return k.read(c.Spawn)
			}),
			Inherited(func(context.Context) (agentturn.Transcript, error) { return seed, nil }),
		}
	}
	cfg, err := c.Apply(agentturn.Config{Model: k.model, ModelName: cmp.Or(c.Settings.Model, k.name), Tools: k.tools}, opts...)
	if err != nil {
		return nil, nil, err
	}
	if a := k.agents[c.Spawn]; a != nil {
		return ctx, a, a.SetConfig(cfg)
	}
	a := agentturn.New(cfg, agentturn.WithTranscript(agentturn.Transcript(k.seeds[c.Spawn])))
	k.agents[c.Spawn] = a
	return ctx, a, nil
}

func (k *kin) option() Option { return Children(k.open) }

// recordedKin opens children in child sessions of a recorded one, as a
// host with agentturn/session does: a new agent each time, seeded from
// the child's session. A seeded child's session is a fork of its
// caller's before the call, so it opens with what the caller's held, and
// what it wrote itself is the child's record.
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
		cctx, err = startChild(ctx, k.store, k.rec, c)
	}
	if err != nil {
		return nil, nil, err
	}
	id := session.SessionIDFromContext(cctx)
	read := func(items func(*agentsession.Session) agentturn.Transcript) func(context.Context) (agentturn.Transcript, error) {
		return func(ctx context.Context) (agentturn.Transcript, error) {
			s, err := k.store.Open(ctx, id)
			if errors.Is(err, agentsession.ErrNoSession) {
				return nil, nil
			}
			if err != nil {
				return nil, err
			}
			return items(s), nil
		}
	}
	base := agentturn.Config{Model: k.model, ModelName: cmp.Or(c.Settings.Model, k.name), Tools: k.tools}
	if k.fold != nil {
		base.Transform = k.fold()
	}
	cfg, err := c.Apply(base, History(read(pathItems)), Inherited(read(prefixItems)))
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

// startChild makes the child session c works in, under the session ctx
// names: for a seeded child a fork of that session before the call that
// waits on the child, and otherwise an empty one.
func startChild(ctx context.Context, store *jsonl.Store, rec *session.Recorder, c Child) (context.Context, error) {
	if !c.Seeded {
		return rec.StartChild(ctx, c.Spawn)
	}
	return rec.ForkChildBefore(ctx, c.Spawn, c.Caller)
}

// prefixItems are the items a forked session opened with, which another
// session wrote.
func prefixItems(s *agentsession.Session) (out agentturn.Transcript) {
	for _, e := range session.PrefixPath(s) {
		if item, ok := e.(*agentsession.ItemEntry); ok {
			out = append(out, item.Item)
		}
	}
	return out
}

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

// whole is what a's tree holds: a's transcript, then each child's own
// items.
func whole(a *agentturn.Agent) agentturn.Transcript {
	out := slices.Clone(a.State().Transcript)
	if k, ok := kins.Load(a); ok {
		kids := k.(*kin)
		for _, spawn := range slices.Sorted(maps.Keys(kids.agents)) {
			own, _ := kids.read(spawn)
			out = append(out, own...)
		}
	}
	return out
}

// kinApply is Apply with children opened in memory, serving them the
// config's model and tools, unless a later option opens them otherwise.
func kinApply(g *Graph, cfg agentturn.Config, opts ...Option) (agentturn.Config, error) {
	cfg, _, err := kinApplied(g, cfg, opts...)
	return cfg, err
}

func kinApplied(g *Graph, cfg agentturn.Config, opts ...Option) (agentturn.Config, *kin, error) {
	kids := newKin(cfg.Model, cfg.ModelName, cfg.Tools...)
	out, err := Apply(g, cfg, append([]Option{kids.option()}, opts...)...)
	return out, kids, err
}
