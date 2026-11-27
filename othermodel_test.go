package agentflow

import (
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
// given, and wraps up.
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

// A call may name a model and open with its caller's context. The child
// is asked as that model, and reads what its caller's model saw without
// the reasoning, which the loop leaves out for any model but the one that
// wrote it. Its own reasoning it keeps, every request rebuilds from its
// session, and a host with only the store goes on the same way.
func TestACallOnAnotherModelOpensWithoutItsCallersReasoning(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := linked(t, otherOuter("other", ContextSession), otherInner())
	m := &reasoner{}
	h := hosted(t, dir, "", g, m, nil, false)
	end, _, err := Drive(ctx, g, h.a, start(t, h.a), h.hist)
	if err != nil || end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(h.kids.opened) == 0 {
		t.Fatal("no child was opened")
	}
	if c := h.kids.opened[0]; !c.Seeded || c.Settings.Model != "other" {
		t.Fatalf("child seeded %v, model %q", c.Seeded, c.Settings.Model)
	}
	think := m.asked(t, "think")
	if think.Model != "other" {
		t.Errorf("the callee was asked as %q", think.Model)
	}
	if got := finishes(think.Input); !slices.Equal(got, []string{"note"}) {
		t.Errorf("the callee's model saw the finishes of %v, want the caller's note", got)
	}
	if got := signed(think.Input); len(got) != 0 {
		t.Errorf("the callee's model was sent the reasoning %v, which another model wrote", got)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	// A new host: the store and the definitions, and nothing else.
	m = &reasoner{}
	h = hosted(t, dir, h.id, linked(t, otherOuter("other", ContextSession), otherInner()), m, nil, false)
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
	// The child keeps its own reasoning, and still none of its caller's.
	more := m.asked(t, "more")
	if got, want := signed(more.Input), []string{"other:think"}; !slices.Equal(got, want) {
		t.Errorf("the callee's model was sent the reasoning %v after a restart, want its own %v", got, want)
	}
	if got := finishes(more.Input); !slices.Equal(got, []string{"note", "think"}) {
		t.Errorf("the callee's model saw the finishes of %v", got)
	}
	// The caller's model reads its own reasoning as before.
	wrap := m.asked(t, "wrap")
	if got, want := signed(wrap.Input), []string{"fake:note"}; !slices.Equal(got, want) {
		t.Errorf("the caller's model was sent the reasoning %v, want %v", got, want)
	}
	runs := h.runs(t)
	if got, want := tree(runs), []string{"outer:done:ok", " inner:done:ok"}; !slices.Equal(got, want) {
		t.Fatalf("tree = %v, want %v", got, want)
	}
	// The reasoning is in what the child's session opened with: it is the
	// view that leaves it out, and every request still rebuilds.
	cs := h.session(t, runs[0].Children[0].ID)
	if got, want := signed(openresponses.Items(prefixItems(cs))), []string{"fake:note"}; !slices.Equal(got, want) {
		t.Errorf("the child's session opens with the reasoning %v, want %v", got, want)
	}
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

// A child asked as its caller's model reads its caller's reasoning, as
// the caller's model did.
func TestACallOnItsCallersModelOpensWithItsReasoning(t *testing.T) {
	ctx := context.Background()
	for name, model := range map[string]string{"no model": "", "the host's model": "fake"} {
		t.Run(name, func(t *testing.T) {
			g := linked(t, otherOuter(model, ContextSession), otherInner())
			m := &reasoner{}
			h := hosted(t, t.TempDir(), "", g, m, nil, false)
			if _, _, err := Drive(ctx, g, h.a, start(t, h.a), h.hist); err != nil {
				t.Fatal(err)
			}
			if got, want := signed(m.asked(t, "think").Input), []string{"fake:note"}; !slices.Equal(got, want) {
				t.Errorf("the callee's model was sent the reasoning %v, want %v", got, want)
			}
		})
	}
}

// A call that names another model and says nothing of its context opens
// with none of its caller's, as a model step that names one does.
func TestACallOnAnotherModelIsFreshUnlessItSaysSession(t *testing.T) {
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

// Whose reasoning a request carries is told item by item: a child asked
// as another model reads none of its caller's, and a child it calls in
// its context, asked as the first caller's model again, reads the first
// caller's and not the second's.
func TestEachChildReadsTheReasoningOfItsOwnModel(t *testing.T) {
	ctx := context.Background()
	top := &Graph{Name: "top", Nodes: []Node{
		{ID: "note", Kind: Agent, Prompt: "Note.", Next: map[string]string{OK: "call"}},
		{ID: "call", Kind: Run, Flow: "mid", Model: "other", Context: ContextSession, Next: map[string]string{OK: "wrap"}},
		{ID: "wrap", Kind: Agent, Prompt: "Wrap up."},
	}}
	leaf := &Graph{Name: "leaf", Nodes: []Node{{ID: "think", Kind: Agent, Prompt: "Think."}}}
	g := linked(t, top, seedMid(), leaf)
	m := &reasoner{}
	h := hosted(t, t.TempDir(), "", g, m, nil, false)
	end, _, err := Drive(ctx, g, h.a, start(t, h.a), h.hist)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	for step, want := range map[string][]string{
		"plan":  nil,
		"think": {"fake:note"},
		"sum":   {"other:plan"},
		"wrap":  {"fake:note"},
	} {
		req := m.asked(t, step)
		if got := signed(req.Input); !slices.Equal(got, want) {
			t.Errorf("the model at %s, asked as %s, was sent the reasoning %v, want %v", step, req.Model, got, want)
		}
	}
	if got := finishes(m.asked(t, "think").Input); !slices.Equal(got, []string{"note", "plan"}) {
		t.Errorf("the leaf's model saw the finishes of %v", got)
	}
	runs := h.runs(t)
	mid := runs[0].Children[0]
	for _, s := range []*agentsession.Session{h.session(t, ""), h.session(t, mid.ID), h.session(t, mid.Children[0].ID)} {
		checkHashes(t, s)
	}
}

// A workflow that names its own model is asked as it whoever calls it,
// so its child is opened as that model, and reads none of the reasoning
// of a caller asked as another.
func TestAChildIsOpenedAsTheModelItsWorkflowNames(t *testing.T) {
	ctx := context.Background()
	inner := otherInner()
	inner.Model = "other"
	g := linked(t, otherOuter("", ""), inner)
	m := &reasoner{}
	h := hosted(t, t.TempDir(), "", g, m, nil, false)
	if _, _, err := Drive(ctx, g, h.a, start(t, h.a), h.hist); err != nil {
		t.Fatal(err)
	}
	if len(h.kids.opened) == 0 {
		t.Fatal("no child was opened")
	}
	if c := h.kids.opened[0]; !c.Seeded || c.Settings.Model != "other" {
		t.Fatalf("child seeded %v, model %q", c.Seeded, c.Settings.Model)
	}
	think := m.asked(t, "think")
	if got := signed(think.Input); think.Model != "other" || len(got) != 0 {
		t.Errorf("the callee was asked as %q and sent the reasoning %v", think.Model, got)
	}
	if got := finishes(think.Input); !slices.Equal(got, []string{"note"}) {
		t.Errorf("the callee's model saw the finishes of %v", got)
	}
}

// A host with no session to fork seeds the child's agent itself, with no
// record of who wrote what, so a child asked as another model is handed
// a seed without the reasoning, and one asked as its caller's keeps it.
func TestASeedForAnotherModelHoldsNoReasoning(t *testing.T) {
	ctx := context.Background()
	for model, want := range map[string][]string{"other": nil, "": {"fake:note"}} {
		t.Run("model "+model, func(t *testing.T) {
			g := linked(t, otherOuter(model, ContextSession), otherInner())
			m := &reasoner{}
			a := nestedAgent(t, g, m, nil)
			if _, _, err := Drive(ctx, g, a, start(t, a)); err != nil {
				t.Fatal(err)
			}
			k, _ := kins.Load(a)
			opened := k.(*kin).opened
			if len(opened) == 0 || !opened[0].Seeded {
				t.Fatalf("opened = %+v", opened)
			}
			if got := signed(opened[0].Seed); !slices.Equal(got, want) {
				t.Errorf("the seed holds the reasoning %v, want %v", got, want)
			}
			think := m.asked(t, "think")
			if got := signed(think.Input); !slices.Equal(got, want) {
				t.Errorf("the callee's model was sent the reasoning %v, want %v", got, want)
			}
			if got := finishes(think.Input); !slices.Equal(got, []string{"note"}) {
				t.Errorf("the callee's model saw the finishes of %v", got)
			}
		})
	}
}
