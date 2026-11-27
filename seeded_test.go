package agentflow

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// seedOuter notes something, calls seedInner in the session's context,
// reports what it said, and wraps up.
func seedOuter() *Graph {
	return &Graph{Name: "outer", Nodes: []Node{
		{ID: "note", Kind: Agent, Prompt: "Note.", Next: map[string]string{OK: "call"}},
		{ID: "call", Kind: Run, Flow: "inner", Next: map[string]string{"yes": "report"}},
		{ID: "report", Kind: Run, Tool: "script", With: map[string]Value{"name": From("steps.call.outputs.said")}, Next: map[string]string{OK: "wrap"}},
		{ID: "wrap", Kind: Agent, Prompt: "Wrap up."},
	}}
}

// seedInner thinks, then asks whether to go on.
func seedInner() *Graph {
	return &Graph{Name: "inner", Nodes: []Node{
		{ID: "think", Kind: Agent, Prompt: "Think.", Next: map[string]string{OK: "confirm"}},
		{ID: "confirm", Kind: Person, Prompt: "Go on?", Outcomes: []string{"yes", "no"}},
	}, OutputsBy: map[string]map[string]Value{"yes": {"said": Literal("yes")}}}
}

// finishes lists the steps whose finish calls items hold.
func finishes(items openresponses.Items) []string {
	var out []string
	for _, item := range items {
		c, ok := item.(*openresponses.FunctionCall)
		if !ok || c.Name != finishTool {
			continue
		}
		var a finishArgs
		if json.Unmarshal([]byte(c.Arguments), &a) == nil {
			out = append(out, a.Step)
		}
	}
	return out
}

// requestFor is the request the model was sent for step.
func requestFor(t *testing.T, m *fakeModel, step string) openresponses.Request {
	t.Helper()
	for _, req := range m.requests {
		if strings.Contains(req.Instructions, `with step "`+step+`"`) {
			return req
		}
	}
	t.Fatalf("no request for step %q", step)
	return openresponses.Request{}
}

// names lists the calls items hold, by tool.
func names(items agentturn.Transcript) []string {
	var out []string
	for _, item := range items {
		if c, ok := item.(*openresponses.FunctionCall); ok {
			out = append(out, c.Name)
		}
	}
	return out
}

// A call in the session's context runs in a child whose model begins
// with what the caller's saw. The caller's context afterwards holds the
// call and its result, and nothing the callee did.
func TestACallInContextRunsInASeededChild(t *testing.T) {
	ctx := context.Background()
	g := linked(t, seedOuter(), seedInner())
	var ran []string
	m := &fakeModel{outcomes: []string{"ok", "ok", "ok"}}
	kids := newKin(m, "fake", scriptTool(&ran))
	a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)}, kids.option())
	first, err := a.Prompt(ctx, openresponses.UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	end, st, err := Drive(ctx, g, a, first)
	if err != nil {
		t.Fatal(err)
	}
	// The question is the child's, and reaches the person from the caller.
	q := question(t, end)
	if q.Workflow != "inner" || q.Step != "confirm" || st.Workflow != "inner" || st.Depth != 1 {
		t.Fatalf("question = %+v, state %s depth %d", q, st.Workflow, st.Depth)
	}
	if len(kids.opened) == 0 || !kids.opened[0].Seeded || len(kids.opened[0].Seed) == 0 {
		t.Fatalf("opened = %+v", kids.opened)
	}
	// The callee's model read what the caller's did before the call.
	think := requestFor(t, m, "think")
	if got := finishes(think.Input); !slices.Equal(got, []string{"note"}) {
		t.Errorf("the callee's model saw the finishes of %v, want the caller's note", got)
	}
	// It was kept what the caller's was: the caller's start, which holds
	// the definitions, is in what the child's agent holds and in no request.
	if got := names(agentturn.Transcript(think.Input)); slices.Contains(got, startTool) {
		t.Errorf("the callee's model was sent %v", got)
	}
	if got := names(kids.agents[kids.opened[0].Spawn].State().Transcript); !slices.Contains(got, startTool) {
		t.Fatalf("the child's agent holds %v, so this shows nothing", got)
	}
	// The child's record is its own: its replay reads no run of the
	// caller's, and its position is the callee's.
	child := kids.agents[kids.opened[0].Spawn]
	cst, err := kids.opened[0].g.Replay(child.State().Transcript[len(kids.opened[0].Seed):])
	if err != nil || cst.Workflow != "inner" || cst.Node == nil || cst.Node.ID != "confirm" {
		t.Fatalf("child at %+v, %v", cst.Node, err)
	}
	// The caller's runs are untouched by the child's invocation.
	pst, err := g.Replay(a.State().Transcript)
	if err != nil || pst.Ended || pst.Node == nil || pst.Node.ID != "call" {
		t.Fatalf("caller at %+v ended %v, %v", pst.Node, pst.Ended, err)
	}
	ans, err := g.Answer(end.Pending[0], Result{Outcome: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	end, _, err = Resume(ctx, g, a, []agentturn.Answer{ans})
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if got := path(t, g, a); !slices.Equal(got, []string{"note:ok", "call:yes", "report:ok", "wrap:ok"}) {
		t.Errorf("path = %v", got)
	}
	if !slices.Equal(ran, []string{"yes"}) {
		t.Errorf("ran %v", ran)
	}
	// The caller's model, afterwards, sees the call and what it came to,
	// and none of the callee's work.
	wrap := requestFor(t, m, "wrap")
	if got := finishes(wrap.Input); !slices.Equal(got, []string{"note"}) {
		t.Errorf("the caller's model saw the finishes of %v after the call", got)
	}
	var call, result string
	for _, item := range wrap.Input {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			if it.Name == callTool {
				call = it.CallID
			}
		case *openresponses.FunctionCallOutput:
			if it.CallID == call {
				result = it.Output.String()
			}
		}
	}
	if !strings.Contains(result, `"said":"yes"`) {
		t.Errorf("the caller's model saw the call come to %q", result)
	}
	if got, want := names(a.State().Transcript), []string{startTool, finishTool, callTool, "script", finishTool, endTool}; !slices.Equal(got, want) {
		t.Errorf("the caller's transcript holds %v, want %v", got, want)
	}
}

// storeHost is a host with a store: a recorder, the root's agent seeded
// from the session, and children in child sessions, forked for a seeded
// one.
type storeHost struct {
	store *jsonl.Store
	rec   *session.Recorder
	id    string
	a     *agentturn.Agent
	kids  *recordedKin
	hist  Option
	g     *Graph
}

// hosted opens the session id in dir, or starts one when id is empty, and
// applies g to an agent of it. fold, if set, compacts the root and every
// child before each request.
func hosted(t *testing.T, dir, id string, g *Graph, m openresponses.Streamer, tools []agenttool.Tool, fold bool, opts ...Option) *storeHost {
	t.Helper()
	ctx := context.Background()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	h := &storeHost{store: store, g: g}
	var seed []agentturn.Option
	if id == "" {
		var s *agentsession.Session
		if h.rec, s, err = session.Start(ctx, store, agentsession.Header{CWD: dir}); err != nil {
			t.Fatal(err)
		}
		h.id = s.ID()
	} else {
		var s *agentsession.Session
		if h.rec, s, err = session.Resume(ctx, store, id); err != nil {
			t.Fatal(err)
		}
		if seed, err = session.AgentOptions(s, h.rec.ReadOptions()...); err != nil {
			t.Fatal(err)
		}
		h.id = id
	}
	h.hist = recordOf(store, h.id)
	h.kids = &recordedKin{store: store, rec: h.rec, model: m, name: "fake", tools: tools}
	base := agentturn.Config{Model: m, ModelName: "fake", Tools: tools}
	if fold {
		folder := func() func(context.Context, agentturn.Transcript) (agentturn.Transcript, error) {
			return compact.NewLocal(&summarizer{}, append(h.rec.CompactOptions(), compact.WithOnFold(h.rec.Fold), compact.WithFilter(SummaryFilter),
				compact.WithBudget(1), compact.WithKeepLast(1), compact.WithMinFold(0))...).Transform
		}
		h.kids.fold, base.Transform = folder, folder()
	}
	cfg, err := Apply(g, base, append([]Option{h.kids.option(), h.hist}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	h.a = agentturn.New(cfg, seed...)
	h.rec.Attach(h.a)
	return h
}

// session opens the session of a spawn, or the root's for "".
func (h *storeHost) session(t *testing.T, spawn string) *agentsession.Session {
	t.Helper()
	id := h.id
	if spawn != "" {
		id = spawnedID(t, h.store, spawn)
	}
	s, err := h.store.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// runs reads the tree from the store.
func (h *storeHost) runs(t *testing.T) []*Instance {
	t.Helper()
	runs, err := Runs(pathItems(h.session(t, "")), childSessions(t, h.store))
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// A seeded child in a recorded session is a fork of its caller's: its
// session opens with what the caller's held and every request of it
// rebuilds, its record is what it wrote itself, and a host that has only
// the store goes on with it, seeding nothing.
func TestASeededChildResumesFromTheStore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var ran []string
	tools := func() []agenttool.Tool { return []agenttool.Tool{scriptTool(&ran)} }
	g := linked(t, seedOuter(), seedInner())
	m := &fakeModel{outcomes: []string{"ok", "ok"}}
	h := hosted(t, dir, "", g, m, tools(), false)
	end, _, err := Drive(ctx, g, h.a, start(t, h.a), h.hist)
	if err != nil || end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(h.kids.opened) == 0 || !h.kids.opened[0].Seeded {
		t.Fatalf("opened = %+v", h.kids.opened)
	}
	// The callee's model read the caller's note, and not its start.
	think := requestFor(t, m, "think")
	if got := finishes(think.Input); !slices.Equal(got, []string{"note"}) {
		t.Errorf("the callee's model saw the finishes of %v, want the caller's note", got)
	}
	if got := names(agentturn.Transcript(think.Input)); slices.Contains(got, startTool) {
		t.Errorf("the callee's model was sent %v", got)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	// A new host: the store and the definitions, and nothing else.
	m = &fakeModel{outcomes: []string{"ok"}}
	h = hosted(t, dir, h.id, linked(t, seedOuter(), seedInner()), m, tools(), false)
	g = h.g
	end, st, err := Drive(ctx, g, h.a, nil, h.hist)
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, end)
	if q.Workflow != "inner" || q.Step != "confirm" || st.Workflow != "inner" {
		t.Fatalf("question = %+v at %s", q, st.Workflow)
	}
	ans, err := g.Answer(end.Pending[0], Result{Outcome: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	end, _, err = Resume(ctx, g, h.a, []agentturn.Answer{ans}, h.hist)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	// The caller's model, afterwards, sees none of the callee's work.
	wrap := requestFor(t, m, "wrap")
	if got := finishes(wrap.Input); !slices.Equal(got, []string{"note"}) {
		t.Errorf("the caller's model saw the finishes of %v after the call", got)
	}
	runs := h.runs(t)
	if got, want := tree(runs), []string{"outer:done:ok", " inner:done:yes"}; !slices.Equal(got, want) {
		t.Fatalf("tree = %v, want %v", got, want)
	}
	// The child's record is what its session wrote, and its session opens
	// with what the caller's held before the call.
	cs := h.session(t, runs[0].Children[0].ID)
	if got, want := names(pathItems(cs)), []string{startTool, finishTool, askTool, endTool}; !slices.Equal(got, want) {
		t.Errorf("the child's record holds %v, want %v", got, want)
	}
	if got, want := names(prefixItems(cs)), []string{startTool, finishTool}; !slices.Equal(got, want) {
		t.Errorf("the child's session opens with %v, want %v", got, want)
	}
	// The fork was made inside the caller's run, which its session closes
	// before anything of its own. That is no item, and no position is read
	// from it.
	if own := session.OwnPath(cs); len(own) == 0 {
		t.Error("the child's session wrote nothing")
	} else if _, ok := own[0].(*agentsession.RunEntry); !ok {
		t.Errorf("the child's session begins with a %T, so this shows nothing of the run the fork closed", own[0])
	}
	if kid, err := Runs(pathItems(cs), nil); err != nil || len(kid) != 1 || kid[0].Workflow != "inner" || kid[0].Status != Done {
		t.Errorf("the child's session holds the runs %v, %v", tree(kid), err)
	}
	// Every request of both sessions rebuilds from its session.
	for _, s := range []*agentsession.Session{h.session(t, ""), cs} {
		checkHashes(t, s)
		if err := s.VerifyRecords(s.Leaf()); err != nil {
			t.Errorf("session %s: %v", s.ID(), err)
		}
	}
	if n, _, _ := unverified(t, h.store, cs.ID()); n == 0 {
		t.Error("the child's session holds no response, so its hashes show nothing")
	}
}

// A compaction in a seeded child folds what the child's session opened
// with along with its own. Position is still read from the child's own
// record, the caller's runs are never the child's, and every request
// still rebuilds.
func TestASeededChildFoldsWhatItOpenedWith(t *testing.T) {
	ctx := context.Background()
	var ran []string
	g := linked(t, seedOuter(), seedInner())
	m := &fakeModel{outcomes: []string{"ok", "ok", "ok"}}
	h := hosted(t, t.TempDir(), "", g, m, []agenttool.Tool{scriptTool(&ran)}, true)
	first, err := h.a.Prompt(ctx, openresponses.UserText("go"))
	if err != nil {
		t.Fatal(err)
	}
	end, st, err := Drive(ctx, g, h.a, first, h.hist)
	if err != nil || end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if q := question(t, end); q.Workflow != "inner" || q.Step != "confirm" || st.Workflow != "inner" {
		t.Fatalf("question = %+v at %s", q, st.Workflow)
	}
	// The child folded, and the fold took in what it inherited: its
	// model's request for the step holds none of the caller's calls.
	spawn := h.kids.opened[0].Spawn
	cs := h.session(t, spawn)
	folds := 0
	for _, e := range session.OwnPath(cs) {
		if _, ok := e.(*agentsession.CompactionEntry); ok {
			folds++
		}
	}
	if folds == 0 {
		t.Fatal("the child did not fold, so this shows nothing")
	}
	if got := finishes(requestFor(t, m, "think").Input); len(got) != 0 {
		t.Fatalf("the callee's request holds the finishes of %v, so the fold did not cover what it inherited", got)
	}
	// What the session opened with is still on its path, whole.
	if got, want := names(prefixItems(cs)), []string{startTool, finishTool}; !slices.Equal(got, want) {
		t.Errorf("the child's session opens with %v, want %v", got, want)
	}
	ans, err := g.Answer(end.Pending[0], Result{Outcome: "yes"})
	if err != nil {
		t.Fatal(err)
	}
	end, _, err = Resume(ctx, g, h.a, []agentturn.Answer{ans}, h.hist)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	runs := h.runs(t)
	if got, want := tree(runs), []string{"outer:done:ok", " inner:done:yes"}; !slices.Equal(got, want) {
		t.Fatalf("tree = %v, want %v", got, want)
	}
	if !slices.Equal(ran, []string{"yes"}) {
		t.Errorf("ran %v", ran)
	}
	cs = h.session(t, spawn)
	if kid, err := Runs(pathItems(cs), nil); err != nil || len(kid) != 1 || kid[0].Workflow != "inner" || kid[0].Status != Done {
		t.Errorf("the child's session holds the runs %v, %v", tree(kid), err)
	}
	for _, s := range []*agentsession.Session{h.session(t, ""), cs} {
		checkHashes(t, s)
	}
}

// seedMid plans, calls seedLeaf in its context, and sums up.
func seedMid() *Graph {
	return &Graph{Name: "mid", Nodes: []Node{
		{ID: "plan", Kind: Agent, Prompt: "Plan.", Next: map[string]string{OK: "deeper"}},
		{ID: "deeper", Kind: Run, Flow: "leaf", Next: map[string]string{OK: "sum"}},
		{ID: "sum", Kind: Agent, Prompt: "Sum up."},
	}}
}

// A seeded child that calls in its own context forks again: the callee
// begins with what the first caller's model saw and what the second's
// did, and each caller sees only its own call's result.
func TestASeededChildCallsASeededChild(t *testing.T) {
	ctx := context.Background()
	top := &Graph{Name: "top", Nodes: []Node{
		{ID: "note", Kind: Agent, Prompt: "Note.", Next: map[string]string{OK: "call"}},
		{ID: "call", Kind: Run, Flow: "mid", Next: map[string]string{OK: "wrap"}},
		{ID: "wrap", Kind: Agent, Prompt: "Wrap up."},
	}}
	leaf := &Graph{Name: "leaf", Nodes: []Node{{ID: "think", Kind: Agent, Prompt: "Think."}}}
	for name, recorded := range map[string]bool{"in memory": false, "recorded": true} {
		t.Run(name, func(t *testing.T) {
			g := linked(t, top, seedMid(), leaf)
			m := &fakeModel{outcomes: []string{"ok", "ok", "ok", "ok", "ok"}}
			var a *agentturn.Agent
			var h *storeHost
			var opts []Option
			if recorded {
				h = hosted(t, t.TempDir(), "", g, m, nil, false)
				a, opts = h.a, []Option{h.hist}
			} else {
				a = nestedAgent(t, g, m, nil)
			}
			end, _, err := Drive(ctx, g, a, start(t, a), opts...)
			if err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("end = %+v, %v", end, err)
			}
			for step, want := range map[string][]string{
				"plan":  {"note"},
				"think": {"note", "plan"},
				"sum":   {"note", "plan"},
				"wrap":  {"note"},
			} {
				req := requestFor(t, m, step)
				if got := finishes(req.Input); !slices.Equal(got, want) {
					t.Errorf("the model at %s saw the finishes of %v, want %v", step, got, want)
				}
				if got := names(agentturn.Transcript(req.Input)); slices.Contains(got, startTool) {
					t.Errorf("the model at %s was sent %v", step, got)
				}
			}
			if !recorded {
				return
			}
			runs := h.runs(t)
			if got, want := tree(runs), []string{"top:done:ok", " mid:done:ok", "  leaf:done:ok"}; !slices.Equal(got, want) {
				t.Fatalf("tree = %v, want %v", got, want)
			}
			mid := runs[0].Children[0]
			deep := h.session(t, mid.Children[0].ID)
			// The second fork opens with the first's prefix and what the
			// first child wrote before its call.
			if got, want := finishes(openresponses.Items(prefixItems(deep))), []string{"note", "plan"}; !slices.Equal(got, want) {
				t.Errorf("the leaf's session opens with the finishes of %v, want %v", got, want)
			}
			if got, want := names(pathItems(deep)), []string{startTool, finishTool, endTool}; !slices.Equal(got, want) {
				t.Errorf("the leaf's record holds %v, want %v", got, want)
			}
			for _, s := range []*agentsession.Session{h.session(t, ""), h.session(t, mid.ID), deep} {
				checkHashes(t, s)
			}
		})
	}
}

// syncAll flushes every session of the store, for a process about to be
// killed.
func syncAll(t *testing.T, store *jsonl.Store) {
	t.Helper()
	ctx := context.Background()
	for sum, err := range store.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Sync(ctx, sum.Header.ID); err != nil {
			t.Fatal(err)
		}
	}
}
