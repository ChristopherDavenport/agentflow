package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// reader reads workspace files, which hold their own names, and records
// what it read; a path under missing/ is not there.
func reader(read *[]string) agenttool.Tool {
	return agenttool.New("read", "Read a file.", func(_ context.Context, a struct {
		Path string `json:"path"`
	}) (string, error) {
		*read = append(*read, a.Path)
		if strings.HasPrefix(a.Path, "missing/") {
			return "", errors.New("no such file")
		}
		return "contents of " + a.Path, nil
	})
}

// given is a step that is given files: a literal, a list an earlier step
// made, and one of the skill's.
func given() *Graph {
	return &Graph{Name: "given", Nodes: []Node{
		{ID: "list", Kind: Run, Tool: "list", Data: map[string]*agenttool.Schema{OK: fileList}, Next: map[string]string{OK: "work"}},
		{ID: "work", Kind: Agent, Prompt: "Work.", Files: []Value{Literal("docs/spec.md"), From("steps.list.outputs.files"), Literal("./prompts/style.md")},
			SkillFiles: map[string]string{"prompts/style.md": "Be terse."}},
	}}
}

// filesGiven is what the flow_files results of t hold, by path.
func filesGiven(t agentturn.Transcript) []string {
	ids := map[string]bool{}
	var out []string
	for _, item := range t {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			ids[it.CallID] = it.Name == filesTool
		case *openresponses.FunctionCallOutput:
			if !ids[it.CallID] {
				continue
			}
			var res struct {
				Data struct{ Files []fileText } `json:"data"`
			}
			if json.Unmarshal([]byte(it.Output.String()), &res) != nil {
				out = append(out, "error: "+it.Output.String())
			}
			for _, f := range res.Data.Files {
				out = append(out, f.Path+"="+f.Text)
			}
		}
	}
	return out
}

func TestAStepIsGivenItsFilesInOrder(t *testing.T) {
	g := given()
	var told []map[string]any
	var read []string
	m := &fakeModel{outcomes: []string{OK}}
	a := nestedAgent(t, g, m, append(lister(&told), reader(&read)))
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	want := []string{"docs/spec.md=contents of docs/spec.md", "a.go=contents of a.go", "b.go=contents of b.go", "c.go=contents of c.go", "./prompts/style.md=Be terse."}
	if got := filesGiven(a.State().Transcript); !slices.Equal(got, want) {
		t.Errorf("files =\n%s", strings.Join(got, "\n"))
	}
	if !slices.Equal(read, []string{"docs/spec.md", "a.go", "b.go", "c.go"}) {
		t.Errorf("read %v", read)
	}
	// The model was sent the program's call and its result.
	sent := false
	for _, item := range m.requests[0].Input {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == filesTool {
			sent = true
		}
	}
	if !sent || !strings.Contains(m.requests[0].Instructions, "files were given to you") {
		t.Error("the model was not given its files")
	}
}

func TestAMissingFileFailsTheStep(t *testing.T) {
	g := given()
	g.Nodes[1].Files[0] = Literal("missing/spec.md")
	var told []map[string]any
	var read []string
	a := nestedAgent(t, g, &fakeModel{}, append(lister(&told), reader(&read)))
	q := question(t, start(t, a))
	if !q.Retry || !strings.Contains(q.Question, "missing/spec.md") || !strings.Contains(q.Question, "no such file") {
		t.Fatalf("question = %+v", q)
	}
}

func TestAFileIsReadUnderTheStepsPolicy(t *testing.T) {
	g := given()
	g.Nodes[1].Files[0] = Literal("secrets/key")
	g.Nodes[1].Policy = &Policy{Deny: []PolicyRule{{Rule: "read(secrets/*)", Note: "keys stay out of context"}}}
	var told []map[string]any
	var read []string
	a := nestedAgent(t, g, &fakeModel{}, append(lister(&told), reader(&read)), matchers)
	q := question(t, start(t, a))
	if !q.Retry || !strings.Contains(q.Question, "read(secrets/*)") || !strings.Contains(q.Question, "keys stay out of context") {
		t.Fatalf("question = %+v", q)
	}
	if slices.Contains(read, "secrets/key") {
		t.Errorf("read %v", read)
	}
}

func TestAFreshStepIsGivenItsFilesInItsChild(t *testing.T) {
	g := given()
	g.Nodes[1].Context = ContextFresh
	var told []map[string]any
	var read []string
	m := &fakeModel{outcomes: []string{OK}}
	kids := newKin(m, "fake", reader(&read))
	a := nestedAgent(t, g, m, lister(&told), kids.option())
	end, _, err := Drive(t.Context(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	child := kids.agents[kids.opened[0].Spawn]
	if got := filesGiven(child.State().Transcript); len(got) != 5 || got[4] != "./prompts/style.md=Be terse." {
		t.Errorf("the child was given %q", got)
	}
	if got := filesGiven(a.State().Transcript); len(got) != 0 {
		t.Errorf("the parent holds %q", got)
	}
}

func TestFilesGivenBeforeAKillAreNotReadAgain(t *testing.T) {
	g := given()
	var read []string
	tools := func(read *[]string) []agenttool.Tool {
		var told []map[string]any
		return append(lister(&told), reader(read))
	}
	childCfg := func() agentturn.Config {
		var theirs []string
		cfg, err := Apply(given(), agentturn.Config{Model: killingModel{}, ModelName: "fake", Tools: tools(&theirs)})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	m := &fakeModel{outcomes: []string{OK}}
	a := killedRecord(t, childCfg, func(store *jsonl.Store, id string) agentturn.Config {
		cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: tools(&read)}, recordOf(store, id))
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	})
	end, err := a.Continue(t.Context())
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(read) != 0 {
		t.Errorf("read again: %v", read)
	}
	// The model the resumed run asked was sent the files it was given.
	sent := false
	for _, item := range m.requests[0].Input {
		if o, ok := item.(*openresponses.FunctionCallOutput); ok && strings.Contains(o.Output.String(), "Be terse.") {
			sent = true
		}
	}
	if !sent {
		t.Error("the files were not sent after the resume")
	}
}

func TestACompactionKeepsAStepsFiles(t *testing.T) {
	g := given()
	var told []map[string]any
	var read []string
	m := &fakeModel{outcomes: []string{OK}}
	var a *agentturn.Agent
	// A transform that drops every call's items, as a fold might.
	drop := func(_ context.Context, t agentturn.Transcript) (agentturn.Transcript, error) {
		var out agentturn.Transcript
		for _, item := range t {
			switch item.(type) {
			case *openresponses.FunctionCall, *openresponses.FunctionCallOutput:
				continue
			}
			out = append(out, item)
		}
		return out, nil
	}
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: append(lister(&told), reader(&read)), Transform: drop},
		History(func(context.Context) (agentturn.Transcript, error) { return a.State().Transcript, nil }))
	if err != nil {
		t.Fatal(err)
	}
	a = agentturn.New(cfg)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	kept := 0
	for _, item := range m.requests[0].Input {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			if it.Name == filesTool {
				kept++
			}
		case *openresponses.FunctionCallOutput:
			if strings.Contains(it.Output.String(), "Be terse.") {
				kept++
			}
		}
	}
	if kept != 2 {
		t.Errorf("kept %d of the files call and its result", kept)
	}
}

func TestFilesAreCheckedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    Node
		want string
	}{
		{"outside", Node{ID: "w", Kind: Agent, Prompt: "x", Files: []Value{Literal("../etc/passwd")}}, "inside the workspace"},
		{"absolute", Node{ID: "w", Kind: Agent, Prompt: "x", Files: []Value{Literal("/etc/passwd")}}, "inside the workspace"},
		{"a person's", Node{ID: "w", Kind: Person, Prompt: "x", Files: []Value{Literal("a.md")}}, "only a model step"},
		{"not a path", Node{ID: "w", Kind: Agent, Prompt: "x", Files: []Value{Literal(3)}}, "a path or a reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &Graph{Name: "f", Nodes: []Node{tc.n}}
			if err := g.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// A skill's file never read, and a config with no read tool, are
	// refused by Apply.
	g := given()
	g.Nodes[1].SkillFiles = nil
	var told []map[string]any
	var read []string
	if _, err := Apply(g, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: append(lister(&told), reader(&read))}); err == nil || !strings.Contains(err.Error(), "never read") {
		t.Errorf("err = %v", err)
	}
	if _, err := Apply(given(), agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: lister(&told)}); err == nil || !strings.Contains(err.Error(), "does not offer") {
		t.Errorf("err = %v", err)
	}
}
