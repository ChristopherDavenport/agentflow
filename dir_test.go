package agentflow

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
)

// dirPicker gives the next of dirs each time it is called.
func dirPicker(dirs []string, calls *int) agenttool.Tool {
	return agenttool.New("pick", "Pick a directory.", func(context.Context, struct{}) (Result, error) {
		d := dirs[min(*calls, len(dirs)-1)]
		*calls++
		return Result{Outcome: OK, Data: map[string]any{"dir": d}}, nil
	})
}

func pathSchema(kind string) map[string]*agenttool.Schema {
	return map[string]*agenttool.Schema{OK: {Type: "object", Required: []string{"dir"},
		Properties: []agenttool.Property{{Name: "dir", Schema: &agenttool.Schema{Type: kind}}}}}
}

// mover picks a directory and drafts in a kept child there, twice.
func mover() *Graph {
	return &Graph{Name: "mover", Nodes: []Node{
		{ID: "pick", Kind: Run, Tool: "pick", Data: pathSchema("string"), Next: map[string]string{OK: "draft"}},
		{ID: "draft", Kind: Agent, Prompt: "Draft it.", Context: ContextFresh, Child: "writer", Dir: From("steps.pick.outputs.dir"),
			Next: map[string]string{OK: "flip"}},
		{ID: "flip", Kind: Run, Tool: "flip", Outcomes: []string{"again", "done"}, Next: map[string]string{"again": "pick"}},
	}}
}

func TestADirFromDataIsCheckedAtLoad(t *testing.T) {
	for want, change := range map[string]func(g *Graph){
		"dir steps.pick.outputs.dir is no path: its schema says integer": func(g *Graph) { g.Nodes[0].Data = pathSchema("integer") },
		`does not certainly produce "where"`:                             func(g *Graph) { g.Nodes[1].Dir = From("steps.pick.outputs.where") },
		"leaves none to work in":                                         func(g *Graph) { g.Nodes[1].Dir = Optional("steps.pick.outputs.dir") },
		"a setting of a child or of a script":                            func(g *Graph) { g.Nodes[1].Context, g.Nodes[1].Child = ContextSession, "" },
		`node "flip": dir is a setting`:                                  func(g *Graph) { g.Nodes[2].Dir = Literal("docs") },
		`dir "../up"`:                                                    func(g *Graph) { g.Nodes[1].Dir = Literal("../up") },
		`no step "pik"; did you mean "pick"?`:                            func(g *Graph) { g.Nodes[1].Dir = From("steps.pik.outputs.dir") },
	} {
		g := mover()
		change(g)
		if err := g.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	if err := mover().Validate(); err != nil {
		t.Errorf("mover: %v", err)
	}
}

func TestAKeptChildCannotMoveDirectory(t *testing.T) {
	ctx := context.Background()
	var picks, flips int
	m := &fakeModel{outcomes: []string{OK, OK}}
	kids := newKin(m, "fake")
	g := mover()
	a := nestedAgent(t, g, m, []agenttool.Tool{dirPicker([]string{"a", "b"}, &picks), flip(&flips)}, kids.option(), Unattended())
	end, st, err := Drive(ctx, g, a, start(t, a))
	if len(kids.opened) != 1 || kids.opened[0].Settings.Dir != "a" {
		t.Fatalf("opened %+v", kids.opened)
	}
	// The second visit's dir is b: the call fails, and with nobody to ask
	// the run does too.
	if err == nil || !strings.Contains(err.Error(), `child "writer" was started in "a", and dir is now "b"`) {
		t.Fatalf("end = %+v, %+v, %v", end, st, err)
	}
}

func TestADirIsReadFromTheRecordOnResume(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	g := &Graph{Name: "mover", Nodes: mover().Nodes[:2]}
	g.Nodes[1].Next = nil
	var picks int
	// The child's model fails its first request, so the parent's call is
	// left held.
	m := &fakeModel{}
	kids := &recordedKin{store: store, rec: rec, model: m, name: "fake"}
	a := nestedAgent(t, g, m, []agenttool.Tool{dirPicker([]string{"work/a"}, &picks)}, kids.option())
	rec.Attach(a)
	if _, _, err := Drive(ctx, g, a, start(t, a)); err == nil {
		t.Fatal("the child's model did not fail")
	}
	if len(kids.opened) != 1 || kids.opened[0].Settings.Dir != "work/a" {
		t.Fatalf("opened %+v", kids.opened)
	}

	// A new agent has the store and the definition. Picking again would
	// give another directory; the record already holds the call's.
	rec, s, err = session.Resume(ctx, store, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	seed, err := session.AgentOptions(s, rec.ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	var again int
	m = &fakeModel{outcomes: []string{OK}}
	kids = &recordedKin{store: store, rec: rec, model: m, name: "fake"}
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{dirPicker([]string{"work/b"}, &again)}}, kids.option(), recordOf(store, s.ID()))
	if err != nil {
		t.Fatal(err)
	}
	a = agentturn.New(cfg, seed...)
	rec.Attach(a)
	end, st, err := Drive(ctx, g, a, nil)
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if again != 0 || len(kids.opened) != 1 || kids.opened[0].Settings.Dir != "work/a" {
		t.Errorf("picked %d times, opened %+v", again, kids.opened)
	}
	got, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, got)
}
