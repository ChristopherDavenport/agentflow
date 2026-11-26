package agentflow

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// noter records what it is told to note.
func noter(told *[]string) agenttool.Tool {
	return agenttool.New("note", "Note text.", func(_ context.Context, a struct {
		Text string `json:"text"`
	}) (Result, error) {
		*told = append(*told, a.Text)
		return Result{Outcome: OK}, nil
	})
}

// planner drafts a plan in a document, and a program step reads it back.
func planner() *Graph {
	return &Graph{Name: "planner", Documents: map[string]Document{"plan": {Text: "# Plan\n"}, "notes": {}}, Nodes: []Node{
		{ID: "draft", Kind: Agent, Prompt: "Draft the plan.", Edits: []string{"plan"}, Reads: []string{"notes"}, Next: map[string]string{OK: "keep"}},
		{ID: "keep", Kind: Run, Tool: "note", With: map[string]Value{"text": From("documents.plan")}},
	}}
}

func write(step, name, text string) says {
	b, _ := json.Marshal(map[string]string{"step": step, "name": name, "text": text})
	return says{docWriteTool, string(b)}
}

func read(step, name string) says {
	return says{docReadTool, `{"step":"` + step + `","name":"` + name + `"}`}
}

func TestADocumentIsReadAndWrittenThroughTheRecord(t *testing.T) {
	g := planner()
	m := &cast{script: []says{
		read("draft", "plan"),
		write("draft", "notes", "may not"),
		write("draft", "plan", "# Plan\n\n1. Do it.\n"),
		{finishTool, `{"step":"draft","outcome":"ok"}`},
	}}
	var told []string
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{noter(&told)}})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	if !slices.Equal(told, []string{"# Plan\n\n1. Do it.\n"}) {
		t.Errorf("noted %q", told)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if st.Documents["plan"] != "# Plan\n\n1. Do it.\n" || st.Documents["notes"] != "" {
		t.Errorf("documents = %q", st.Documents)
	}
	// The read's result holds the text, and the refused write says why.
	var reads, refusals []string
	for _, item := range a.State().Transcript {
		if out, ok := item.(*openresponses.FunctionCallOutput); ok {
			switch s := out.Output.String(); {
			case strings.Contains(s, `"text":"# Plan\n"`):
				reads = append(reads, s)
			case strings.Contains(s, "does not edit"):
				refusals = append(refusals, s)
			}
		}
	}
	if len(reads) != 1 || len(refusals) != 1 {
		t.Errorf("reads %q, refusals %q", reads, refusals)
	}
	// The model was told which documents it has, and offered only those.
	req := m.requests[0]
	if !strings.Contains(req.Instructions, "you may read notes, and you may read and edit plan") {
		t.Errorf("instructions: %s", req.Instructions)
	}
	for _, tool := range req.Tools {
		if fn, ok := tool.(*openresponses.FunctionTool); ok && fn.Name == docWriteTool && !strings.Contains(string(fn.Parameters), `"enum":["plan"]`) {
			t.Errorf("write offered as %s", fn.Parameters)
		}
	}
}

func TestAProgramStepWritesADocument(t *testing.T) {
	g := &Graph{Name: "drafts", Documents: map[string]Document{"plan": {}}, Nodes: []Node{
		{ID: "load", Kind: Run, Tool: "load", Writes: map[string]string{"plan": "text"}, Next: map[string]string{OK: "keep"}},
		{ID: "keep", Kind: Run, Tool: "note", With: map[string]Value{"text": From("documents.plan")}},
	}}
	load := agenttool.New("load", "Load.", func(context.Context, struct{}) (Result, error) {
		return Result{Outcome: OK, Data: map[string]any{"text": "loaded"}}, nil
	})
	var told []string
	cfg, err := Apply(g, agentturn.Config{Model: &cast{}, ModelName: "fake", Tools: []agenttool.Tool{load, noter(&told)}})
	if err != nil {
		t.Fatal(err)
	}
	if end := start(t, agentturn.New(cfg)); end.Reason != agentturn.ReasonDone || !slices.Equal(told, []string{"loaded"}) {
		t.Fatalf("end = %+v, told %q", end, told)
	}
}

func TestAFreshStepEditsADocumentInItsChild(t *testing.T) {
	g := planner()
	g.Nodes[0].Context = ContextFresh
	m := &cast{script: []says{
		read("draft", "plan"),
		write("draft", "plan", "from the child"),
		{finishTool, `{"step":"draft","outcome":"ok"}`},
	}}
	var told []string
	kids := newKin(m, "fake")
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{noter(&told)}}, kids.option())
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if end, _, err := Drive(t.Context(), g, a, start(t, a)); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if !slices.Equal(told, []string{"from the child"}) {
		t.Errorf("noted %q", told)
	}
	// The child read what its call passed in, in its own session.
	child := kids.agents[kids.opened[0].Spawn]
	found := false
	for _, item := range child.State().Transcript {
		if out, ok := item.(*openresponses.FunctionCallOutput); ok && strings.Contains(out.Output.String(), `"text":"# Plan\n"`) {
			found = true
		}
	}
	if !found {
		t.Error("the child did not read the plan it was passed")
	}
}

// castThenDie plays its script, then kills the process.
type castThenDie struct{ cast }

func (m *castThenDie) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if len(m.script) == 0 {
		die()
	}
	return m.cast.CreateStream(ctx, req, sink)
}

func TestADocumentEditedBeforeAKillIsReadBack(t *testing.T) {
	g := planner()
	var told []string
	childCfg := func() agentturn.Config {
		m := &castThenDie{cast{script: []says{write("draft", "plan", "before the kill")}}}
		cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{noter(&told)}})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	m := &cast{script: []says{{finishTool, `{"step":"draft","outcome":"ok"}`}}}
	a := killedRecord(t, childCfg, func(store *jsonl.Store, id string) agentturn.Config {
		cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{noter(&told)}}, recordOf(store, id))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	})
	end, err := a.Continue(t.Context())
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if !slices.Equal(told, []string{"before the kill"}) {
		t.Errorf("noted %q", told)
	}
}

func TestDocumentsAreCheckedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    *Graph
		want string
	}{
		{"unknown document", &Graph{Name: "d", Documents: map[string]Document{"plan": {}}, Nodes: []Node{
			{ID: "a", Kind: Agent, Prompt: "x", Edits: []string{"plam"}}}}, `no document "plam"; did you mean "plan"?`},
		{"a person reads", &Graph{Name: "d", Documents: map[string]Document{"plan": {}}, Nodes: []Node{
			{ID: "a", Kind: Person, Prompt: "x", Reads: []string{"plan"}}}}, "only a model step reads or edits"},
		{"a reference", &Graph{Name: "d", Documents: map[string]Document{"plan": {}}, Nodes: []Node{
			{ID: "a", Kind: Run, Tool: "t", With: map[string]Value{"x": From("documents.plam")}}}}, `no document "plam"; did you mean "plan"?`},
		{"an input", &Graph{Name: "d", Inputs: &agenttool.Schema{Type: "object", Properties: []agenttool.Property{{Name: "n", Schema: &agenttool.Schema{Type: "integer"}}}},
			Documents: map[string]Document{"plan": {Input: "n"}}, Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"}}}, "not text"},
		{"a write of a member that is not there", &Graph{Name: "d", Documents: map[string]Document{"plan": {}}, Nodes: []Node{
			{ID: "a", Kind: Run, Tool: "t", Writes: map[string]string{"plan": "txt"}, Data: map[string]*agenttool.Schema{OK: {Type: "object", Properties: []agenttool.Property{{Name: "text", Schema: &agenttool.Schema{Type: "string"}}}}}}}},
			`declares "txt"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.g.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
