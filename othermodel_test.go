package agentflow

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// reasoner finishes each step it is asked for with ok, after a reasoning
// item signed with the model it was asked as and the step.
type reasoner struct {
	requests []openresponses.Request
}

func (m *reasoner) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.requests = append(m.requests, req)
	_, rest, _ := strings.Cut(req.Instructions, `with step "`)
	step, _, _ := strings.Cut(rest, `"`)
	if step == "" {
		return say(sink, req.Model, "some text")
	}
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	r, err := e.Reasoning()
	if err != nil {
		return err
	}
	if err := r.Summary("thought about " + step); err != nil {
		return err
	}
	if err := r.EndSummary(); err != nil {
		return err
	}
	r.EncryptedContent(req.Model + ":" + step)
	if err := r.Close(); err != nil {
		return err
	}
	args, err := json.Marshal(finishArgs{Run: runIn(req), Step: step, Outcome: OK})
	if err != nil {
		return err
	}
	call, err := e.FunctionCall("", finishTool)
	if err != nil {
		return err
	}
	if err := call.Arguments(string(args)); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

// asked is the request the model was sent for step.
func (m *reasoner) asked(t *testing.T, step string) openresponses.Request {
	t.Helper()
	for _, req := range m.requests {
		if strings.Contains(req.Instructions, `with step "`+step+`"`) {
			return req
		}
	}
	t.Fatalf("no request for step %q", step)
	return openresponses.Request{}
}

// signed lists the reasoning items holds, by what signed each.
func signed(items openresponses.Items) []string {
	var out []string
	for _, item := range items {
		if r, ok := item.(*openresponses.ReasoningItem); ok {
			out = append(out, r.EncryptedContent)
		}
	}
	return out
}

// otherOuter notes something, calls otherInner as model in the context
// given, and wraps up. It names no model of its own, so it is asked as
// its host's.
func otherOuter(model, context string) *Graph {
	return &Graph{Name: "outer", Nodes: []Node{
		{ID: "note", Kind: Agent, Prompt: "Note.", Next: map[string]string{OK: "call"}},
		{ID: "call", Kind: Run, Flow: "inner", Model: model, Context: context, Next: map[string]string{OK: "wrap"}},
		{ID: "wrap", Kind: Agent, Prompt: "Wrap up."},
	}}
}

// otherInner thinks, asks whether to go on, and says more.
func otherInner() *Graph {
	return &Graph{Name: "inner", Nodes: []Node{
		{ID: "think", Kind: Agent, Prompt: "Think.", Next: map[string]string{OK: "confirm"}},
		{ID: "confirm", Kind: Person, Prompt: "Go on?", Outcomes: []string{"yes", "no"}, Next: map[string]string{"yes": "more"}},
		{ID: "more", Kind: Agent, Prompt: "Say more."},
	}}
}

// A child that opens with its caller's session is asked as its caller's
// model, and so reads the reasoning that model wrote, as the caller's
// own later steps do. Its own it keeps, every request rebuilds from its
// session, and a host with only the store goes on the same way.
func TestASeededChildKeepsItsCallersModelAndReasoning(t *testing.T) {
	ctx := context.Background()
	for name, model := range map[string]string{"the host's model": "", "the workflow's model": "big"} {
		t.Run(name, func(t *testing.T) {
			asked := cmp.Or(model, "fake")
			graph := func() *Graph {
				outer := otherOuter(model, "")
				outer.Model = model
				return linked(t, outer, otherInner())
			}
			dir := t.TempDir()
			g := graph()
			m := &reasoner{}
			h := hosted(t, dir, "", g, m, nil, false)
			end, _, err := Drive(ctx, g, h.a, start(t, h.a), h.hist)
			if err != nil || end.Reason != agentturn.ReasonInputRequired {
				t.Fatalf("end = %+v, %v", end, err)
			}
			if len(h.kids.opened) == 0 {
				t.Fatal("no child was opened")
			}
			if c := h.kids.opened[0]; !c.Seeded || c.Settings.Model != asked {
				t.Fatalf("child seeded %v, model %q, want its caller's %q", c.Seeded, c.Settings.Model, asked)
			}
			think := m.asked(t, "think")
			if think.Model != asked {
				t.Errorf("the callee was asked as %q, want %q", think.Model, asked)
			}
			if got := finishes(think.Input); !slices.Equal(got, []string{"note"}) {
				t.Errorf("the callee's model saw the finishes of %v, want the caller's note", got)
			}
			if got, want := signed(think.Input), []string{asked + ":note"}; !slices.Equal(got, want) {
				t.Errorf("the callee's model was sent the reasoning %v, want its caller's %v", got, want)
			}
			if err := h.store.Close(); err != nil {
				t.Fatal(err)
			}

			// A new host: the store and the definitions, and nothing else.
			m = &reasoner{}
			h = hosted(t, dir, h.id, graph(), m, nil, false)
			g = h.g
			end, _, err = Drive(ctx, g, h.a, nil, h.hist)
			if err != nil {
				t.Fatal(err)
			}
			ans, err := g.Answer(end.Pending[0], Result{Outcome: "yes"})
			if err != nil {
				t.Fatal(err)
			}
			end, _, err = Resume(ctx, g, h.a, []agentturn.Answer{ans}, h.hist)
			if err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("end = %+v, %v", end, err)
			}
			more := m.asked(t, "more")
			if got, want := signed(more.Input), []string{asked + ":note", asked + ":think"}; !slices.Equal(got, want) {
				t.Errorf("the callee's model was sent the reasoning %v after a restart, want %v", got, want)
			}
			wrap := m.asked(t, "wrap")
			if got, want := signed(wrap.Input), []string{asked + ":note"}; !slices.Equal(got, want) {
				t.Errorf("the caller's model was sent the reasoning %v, want %v", got, want)
			}
			runs := h.runs(t)
			if got, want := tree(runs), []string{"outer:done:ok", " inner:done:ok"}; !slices.Equal(got, want) {
				t.Fatalf("tree = %v, want %v", got, want)
			}
			cs := h.session(t, runs[0].Children[0].ID)
			for _, s := range []*agentsession.Session{h.session(t, ""), cs} {
				checkHashes(t, s)
				if err := s.VerifyRecords(s.Leaf()); err != nil {
					t.Errorf("session %s: %v", s.ID(), err)
				}
			}
			if n, _, _ := unverified(t, h.store, cs.ID()); n == 0 {
				t.Error("the child's session holds no response, so its hashes show nothing")
			}
		})
	}
}

// The caller's model goes down with the session at any depth: a seeded
// child of a seeded child is asked as the first caller's model, and
// reads the reasoning of both callers. A host with no session to fork is
// told the same model and handed the reasoning in the seed.
func TestTheCallersModelGoesDownWithTheSession(t *testing.T) {
	ctx := context.Background()
	graph := func() *Graph {
		top := &Graph{Name: "top", Nodes: []Node{
			{ID: "note", Kind: Agent, Prompt: "Note.", Next: map[string]string{OK: "call"}},
			{ID: "call", Kind: Run, Flow: "mid", Next: map[string]string{OK: "wrap"}},
			{ID: "wrap", Kind: Agent, Prompt: "Wrap up."},
		}}
		leaf := &Graph{Name: "leaf", Nodes: []Node{{ID: "think", Kind: Agent, Prompt: "Think."}}}
		return linked(t, top, seedMid(), leaf)
	}
	check := func(t *testing.T, m *reasoner, opened []Child) {
		t.Helper()
		if len(opened) != 2 {
			t.Fatalf("opened %d children, want 2", len(opened))
		}
		for _, c := range opened {
			if !c.Seeded || c.Settings.Model != "fake" {
				t.Errorf("child of %s: seeded %v, model %q, want its caller's", c.Workflow, c.Seeded, c.Settings.Model)
			}
		}
		for step, want := range map[string][]string{
			"plan":  {"fake:note"},
			"think": {"fake:note", "fake:plan"},
			"sum":   {"fake:note", "fake:plan"},
			"wrap":  {"fake:note"},
		} {
			req := m.asked(t, step)
			if got := signed(req.Input); req.Model != "fake" || !slices.Equal(got, want) {
				t.Errorf("the model at %s, asked as %s, was sent the reasoning %v, want %v", step, req.Model, got, want)
			}
		}
	}
	t.Run("recorded", func(t *testing.T) {
		g := graph()
		m := &reasoner{}
		h := hosted(t, t.TempDir(), "", g, m, nil, false)
		end, _, err := Drive(ctx, g, h.a, start(t, h.a), h.hist)
		if err != nil || end.Reason != agentturn.ReasonDone {
			t.Fatalf("end = %+v, %v", end, err)
		}
		check(t, m, h.kids.opened)
		runs := h.runs(t)
		mid := runs[0].Children[0]
		for _, s := range []*agentsession.Session{h.session(t, ""), h.session(t, mid.ID), h.session(t, mid.Children[0].ID)} {
			checkHashes(t, s)
		}
	})
	t.Run("no store", func(t *testing.T) {
		g := graph()
		m := &reasoner{}
		a := nestedAgent(t, g, m, nil)
		if _, _, err := Drive(ctx, g, a, start(t, a)); err != nil {
			t.Fatal(err)
		}
		k, _ := kins.Load(a)
		opened := k.(*kin).opened
		check(t, m, opened)
		if got, want := signed(opened[0].Seed), []string{"fake:note"}; !slices.Equal(got, want) {
			t.Errorf("the seed holds the reasoning %v, want %v", got, want)
		}
	})
}

// A session keeps its model: a step or a call on another model that says
// it reads the session is refused at load, by Validate where the node
// names the model and by Link where the workflow called does.
func TestAnotherModelMayNotReadTheSession(t *testing.T) {
	step := func(workflow, model, context string) error {
		g := &Graph{Name: "steps", Model: workflow, Nodes: []Node{
			{ID: "plan", Kind: Agent, Prompt: "Plan.", Next: map[string]string{OK: "check"}},
			{ID: "check", Kind: Agent, Prompt: "Check.", Model: model, Context: context},
		}}
		return g.Validate()
	}
	call := func(workflow, model, context, callee string) error {
		outer := otherOuter(model, context)
		outer.Model = workflow
		inner := otherInner()
		inner.Model = callee
		if err := outer.Validate(); err != nil {
			return err
		}
		return outer.Link(inner)
	}
	for name, c := range map[string]struct {
		err  error
		want string
	}{
		"a step on another model":                        {step("big", "small", ContextSession), `model "small" is not the workflow's, "big"`},
		"a step on a model, in a workflow with none":     {step("", "small", ContextSession), "which names none and so has the host's"},
		"a step on another model, fresh":                 {step("big", "small", ContextFresh), ""},
		"a step on another model, saying nothing":        {step("big", "small", ""), ""},
		"a step on the workflow's model":                 {step("big", "big", ContextSession), ""},
		"a call on another model":                        {call("big", "small", ContextSession, ""), `model "small" is not the workflow's, "big"`},
		"a call on a model, in a workflow with none":     {call("", "small", ContextSession, ""), "which names none and so has the host's"},
		"a call on another model, fresh":                 {call("big", "small", ContextFresh, ""), ""},
		"a call on the workflow's model":                 {call("big", "big", ContextSession, ""), ""},
		"a callee with a model of its own":               {call("big", "", ContextSession, "small"), `it names its own model, "small", which is not this workflow's, "big"`},
		"a callee with a model, called from none":        {call("", "", ContextSession, "small"), "which names none and so has the host's"},
		"a callee with a model of its own, fresh":        {call("big", "", ContextFresh, "small"), ""},
		"a callee with a model, the call saying nothing": {call("big", "", "", "small"), ""},
		"a callee with its caller's model":               {call("big", "", ContextSession, "big"), ""},
	} {
		switch {
		case c.want == "" && c.err != nil:
			t.Errorf("%s: %v", name, c.err)
		case c.want != "" && (c.err == nil || !strings.Contains(c.err.Error(), c.want) || !strings.Contains(c.err.Error(), "context: "+ContextFresh)):
			t.Errorf("%s: err = %v, want %q and the way out", name, c.err, c.want)
		}
	}
}

// A call that names another model and says nothing of its context opens
// with none of its caller's, as a model step that names one does.
func TestACallOnAnotherModelIsFresh(t *testing.T) {
	ctx := context.Background()
	g := linked(t, otherOuter("other", ""), otherInner())
	m := &reasoner{}
	a := nestedAgent(t, g, m, nil)
	if _, _, err := Drive(ctx, g, a, start(t, a)); err != nil {
		t.Fatal(err)
	}
	k, _ := kins.Load(a)
	opened := k.(*kin).opened
	if len(opened) == 0 || opened[0].Seeded || opened[0].Settings.Model != "other" {
		t.Fatalf("opened = %+v", opened)
	}
	think := m.asked(t, "think")
	if got := finishes(think.Input); len(got) != 0 || think.Model != "other" {
		t.Errorf("the callee was asked as %q and saw the finishes of %v", think.Model, got)
	}
	// A call that names its workflow's own model names no other.
	same := otherOuter("fast", "")
	same.Model = "fast"
	if err := same.Validate(); err != nil {
		t.Fatal(err)
	}
	if same.node("call").fresh() {
		t.Error("a call that names its workflow's model is fresh")
	}
}

// A workflow that names its own model is asked as it whoever calls it:
// a call to it is fresh, its child is opened as that model, whatever
// model the call names, and it reads nothing of its caller's. Its own
// reasoning it keeps.
func TestAWorkflowWithItsOwnModelIsCalledFresh(t *testing.T) {
	ctx := context.Background()
	for name, onCall := range map[string]string{"the call names none": "", "the call names a third": "third"} {
		t.Run(name, func(t *testing.T) {
			inner := otherInner()
			inner.Model = "other"
			g := linked(t, otherOuter(onCall, ""), inner)
			m := &reasoner{}
			h := hosted(t, t.TempDir(), "", g, m, nil, false)
			end, _, err := Drive(ctx, g, h.a, start(t, h.a), h.hist)
			if err != nil || end.Reason != agentturn.ReasonInputRequired {
				t.Fatalf("end = %+v, %v", end, err)
			}
			if len(h.kids.opened) == 0 {
				t.Fatal("no child was opened")
			}
			if c := h.kids.opened[0]; c.Seeded || c.Settings.Model != "other" {
				t.Fatalf("child seeded %v, model %q", c.Seeded, c.Settings.Model)
			}
			think := m.asked(t, "think")
			if got := signed(think.Input); think.Model != "other" || len(got) != 0 {
				t.Errorf("the callee was asked as %q and sent the reasoning %v", think.Model, got)
			}
			if got := finishes(think.Input); len(got) != 0 {
				t.Errorf("the callee's model saw the finishes of %v", got)
			}
			ans, err := g.Answer(end.Pending[0], Result{Outcome: "yes"})
			if err != nil {
				t.Fatal(err)
			}
			if end, _, err = Resume(ctx, g, h.a, []agentturn.Answer{ans}, h.hist); err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("end = %+v, %v", end, err)
			}
			if got, want := signed(m.asked(t, "more").Input), []string{"other:think"}; !slices.Equal(got, want) {
				t.Errorf("the callee's model was sent the reasoning %v, want its own %v", got, want)
			}
			if got, want := signed(m.asked(t, "wrap").Input), []string{"fake:note"}; !slices.Equal(got, want) {
				t.Errorf("the caller's model was sent the reasoning %v, want %v", got, want)
			}
		})
	}
}
