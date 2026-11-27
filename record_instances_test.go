package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

var clean = says{finishTool, `{"step":"review","outcome":"clean"}`}

// recorded runs a graph in a stored session and returns a function that
// reads the instances back from the store.
func recorded(t *testing.T, g *Graph, tool agenttool.Tool, m *cast, opts ...Option) (*agentturn.Agent, func() (agentturn.Transcript, []*Instance)) {
	t.Helper()
	ctx := context.Background()
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := kinApply(g, agentturn.Config{Model: m, ModelName: "worker", Tools: []agenttool.Tool{tool}}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	rec.Attach(a)
	return a, func() (agentturn.Transcript, []*Instance) {
		got, err := store.Open(ctx, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		checkHashes(t, got)
		items := pathItems(got)
		all, err := Runs(items, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range all {
			if in.Problem != "" {
				t.Errorf("instance %s: %s", in.ID, in.Problem)
			}
		}
		return items, all
	}
}

// run prompts, for a run that is expected to end in an error.
func run(a *agentturn.Agent) *agentturn.RunEnd {
	end, _ := a.Prompt(context.Background(), openresponses.UserText("go"))
	if out, _ := drift(a, end); out != nil {
		return out
	}
	return end
}

// pathItems is every item of the session's path. The context rebuilt
// from it leaves out what the last request did.
func pathItems(s *agentsession.Session) (out agentturn.Transcript) {
	// A forked session's path opens with what its caller's held, which is
	// no part of its record.
	for _, e := range session.OwnPath(s) {
		if item, ok := e.(*agentsession.ItemEntry); ok {
			out = append(out, item.Item)
		}
	}
	return out
}

// A session rebuilt for a run that stopped inside a model step has lost
// the items its last request left out. That is refused, not begun again.
func TestATrimmedTranscriptIsRefused(t *testing.T) {
	ctx := context.Background()
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	g := release()
	config := func(m *cast) agentturn.Config {
		cfg, err := kinApply(g, agentturn.Config{Model: m, ModelName: "w", Tools: []agenttool.Tool{scriptTool(&ran)}})
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	// The model only talks, so the run ends in the middle of review.
	a := agentturn.New(config(&cast{script: []says{{}}}))
	rec.Attach(a)
	start(t, a)
	got, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	trimmed, err := session.Transcript(got)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Replay(trimmed); !errors.Is(err, ErrPositionLost) {
		t.Errorf("replay: %v", err)
	}
	if _, err := Runs(trimmed, nil); !errors.Is(err, ErrPositionLost) {
		t.Errorf("instances: %v", err)
	}
	ran = nil
	b := agentturn.New(config(&cast{script: []says{clean}}), agentturn.WithTranscript(trimmed))
	if end := run(b); !errors.Is(end.Err, ErrPositionLost) || len(ran) != 0 {
		t.Errorf("resumed: %v, ran %v", end.Err, ran)
	}
	// Every item of the path still says where it stood.
	st, err := g.Replay(pathItems(got))
	if err != nil || st.Node == nil || st.Node.ID != "review" {
		t.Errorf("path items: %+v, %v", st, err)
	}
}

func steps(st State) (out []string) {
	for _, s := range st.Steps {
		out = append(out, s.Node+":"+s.Result.Outcome)
	}
	return out
}

func TestARecordSaysWhenARunWasEnteredAndLeft(t *testing.T) {
	var ran []string
	g := judged()
	m := &cast{script: []says{clean, found, verdict("report")}}
	a, read := recorded(t, g, scriptTool(&ran), m, Delegate(Decide))
	start(t, a)
	again, err := g.Invocation(nil)
	if err != nil {
		t.Fatal(err)
	}
	end, err := a.Prompt(context.Background(), again)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Drive(context.Background(), g, a, end); err != nil {
		t.Fatal(err)
	}
	// The model is never shown the definition, which holds the criteria.
	for i, req := range m.requests {
		all := everything(t, req)
		if strings.Contains(all, `"definition"`) || !strings.Contains(req.Instructions, "judging") && strings.Contains(all, "Fix real bugs.") {
			t.Errorf("request %d was shown the definition: %s", i, all)
		}
	}
	// The workflow changes. What is recorded does not.
	g.node("review").Prompt = "Something else entirely."
	g.Nodes = g.Nodes[:3]

	items, all := read()
	if len(all) != 2 {
		t.Fatalf("instances = %d", len(all))
	}
	want := [][]string{
		{"gather:ok", "review:clean", "report:ok"},
		{"gather:ok", "review:findings", "triage:report", "report:ok"},
	}
	for i, in := range all {
		if in.Status != Done || in.Workflow != "pr" || in.End <= in.Start || !slices.Equal(steps(in.State), want[i]) {
			t.Errorf("instance %d: %s %d-%d %v", i, in.Status, in.Start, in.End, steps(in.State))
		}
		if c, ok := items[in.End].(*openresponses.FunctionCall); !ok || c.Name != endTool {
			t.Errorf("instance %d ends at %T", i, items[in.End])
		}
	}
	if all[0].ID == all[1].ID || all[0].End >= all[1].Start || all[1].Graph.node("review").Prompt != "Review the diff." {
		t.Errorf("instances are not told apart: %+v", all)
	}
	// The state at any point between.
	var seen []string
	for i := all[1].Start; i <= all[1].End; i++ {
		st, err := all[1].At(items, i)
		if err != nil {
			t.Fatal(err)
		}
		at := "done"
		if st.Node != nil {
			at = st.Node.ID
		} else if !st.Done {
			at = "starting"
		}
		if len(seen) == 0 || seen[len(seen)-1] != at {
			seen = append(seen, at)
		}
	}
	if want := []string{"starting", "gather", "review", "triage", "report", "done"}; !slices.Equal(seen, want) {
		t.Errorf("states = %v, want %v", seen, want)
	}
}

func TestEveryWayOfLeavingIsWritten(t *testing.T) {
	var ran []string
	t.Run("visit limit", func(t *testing.T) {
		g := judged()
		g.node("gather").MaxVisits = 1
		fixed := says{finishTool, `{"step":"fix","outcome":"ok"}`}
		a, read := recorded(t, g, scriptTool(&ran), &cast{script: []says{found, verdict("fix"), fixed}}, Delegate(Decide))
		end, _, _ := Drive(context.Background(), g, a, run(a))
		if end = stopAt(t, g, a, end, "visits"); !errors.Is(end.Err, ErrVisits) {
			t.Errorf("err = %v", end.Err)
		}
		_, all := read()
		if len(all) != 1 || all[0].Status != Failed || !strings.Contains(all[0].Reason, "visit limit") || all[0].End < 0 {
			t.Errorf("instances = %+v", all)
		}
		// Asked again, it says so again and writes nothing more.
		if end := run(a); !errors.Is(end.Err, ErrVisits) {
			t.Errorf("again: %v", end.Err)
		}
		if _, all := read(); len(all) != 1 || all[0].Status != Failed {
			t.Errorf("after asking again: %+v", all)
		}
	})
	t.Run("failed step with nobody there", func(t *testing.T) {
		a, read := recorded(t, release(), flaky(99, new([]string)), &cast{}, Unattended())
		if end := run(a); !errors.Is(end.Err, ErrStepFailed) || strings.Count(end.Err.Error(), "step failed") != 1 ||
			!strings.Contains(end.Err.Error(), "exit status 1") {
			t.Errorf("err = %v", end.Err)
		}
		_, all := read()
		if len(all) != 1 || all[0].Status != Failed || !strings.Contains(all[0].Reason, "gather") {
			t.Errorf("instances = %+v", all)
		}
	})
	t.Run("a person stops", func(t *testing.T) {
		g := release()
		a, read := recorded(t, g, flaky(99, new([]string)), &cast{})
		end := start(t, a)
		_, all := read()
		if len(all) != 1 || all[0].Status != Running || all[0].End != -1 || all[0].State.Failure == "" {
			t.Fatalf("waiting: %+v", all)
		}
		ans, err := g.Answer(end.Pending[0], Result{Outcome: stopOutcome})
		if err != nil {
			t.Fatal(err)
		}
		if end, _ = a.Resume(context.Background(), ans); end == nil || !errors.Is(end.Err, ErrStepFailed) {
			t.Fatalf("%+v", end)
		}
		if _, all := read(); all[0].Status != Failed {
			t.Errorf("stopped: %+v", all)
		}
	})
	t.Run("another is asked for", func(t *testing.T) {
		g := release()
		m := &cast{script: []says{found, clean}}
		a, read := recorded(t, g, scriptTool(&ran), m)
		end := start(t, a)
		if end.Reason != agentturn.ReasonInputRequired {
			t.Fatalf("reason = %s", end.Reason)
		}
		inv, err := g.Invocation(nil)
		if err != nil {
			t.Fatal(err)
		}
		// The question is left, and something else asked for.
		if _, err := a.Resume(context.Background(), agentturn.Refuse(&openresponses.FunctionCallOutput{CallID: end.Pending[0].Call.CallID, Output: openresponses.FunctionCallOutputData{Text: "never mind"}})); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Prompt(context.Background(), inv); err != nil {
			t.Fatal(err)
		}
		_, all := read()
		if len(all) != 2 || all[0].Status != Abandoned || all[0].End < 0 || all[0].End >= all[1].Start || all[1].Status != Done {
			t.Errorf("instances = %+v", all)
		}
		if got := steps(all[0].State); !reflect.DeepEqual(got, []string{"gather:ok", "review:findings"}) {
			t.Errorf("abandoned after %v", got)
		}
	})
}

func TestADefinitionReadsBackTheSame(t *testing.T) {
	g := judged()
	g.node("triage").Data = nil
	g.node("fix").Next = map[string]string{OK: "gather"}
	g.node("gather").Args = []byte(`{"name":"gather","n":12345678901234567890}`)
	g.node("review").Data = map[string]*agenttool.Schema{"findings": {
		Type: "object", Description: "d", NoAdditional: true, Required: []string{"z"},
		Properties: []agenttool.Property{
			{Name: "z", Schema: &agenttool.Schema{Type: "string", Nullable: true, Enum: []any{"b", "a"}, Format: "f"}},
			{Name: "a", Schema: &agenttool.Schema{Type: "array", Items: &agenttool.Schema{Type: "object", Properties: []agenttool.Property{}}}},
			{Name: "m", Schema: &agenttool.Schema{Type: "object", AdditionalProperties: &agenttool.Schema{Type: "integer"}}},
		},
	}}
	g.node("review").With = map[string]Value{"v": Optional("steps.gather.outputs"), "n": Literal(7)}
	g.Inputs = &agenttool.Schema{Type: "object"}
	g.Sources = "sha256:x"
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	back, err := decodeGraph(raw)
	if err != nil {
		t.Fatal(err)
	}
	if back.Digest() != g.Digest() {
		again, _ := json.Marshal(back)
		t.Errorf("read back differently:\n%s\n%s", raw, again)
	}
}
