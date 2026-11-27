package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// undoTool undoes what a run did, saying for whom; fail makes it fail.
func undoTool(ran *[]string, fail func(string) bool) agenttool.Tool {
	return agenttool.New("undo", "Undo.", func(_ context.Context, a struct {
		Who string `json:"who"`
	}) (Result, error) {
		*ran = append(*ran, a.Who)
		if fail != nil && fail(a.Who) {
			return Result{}, errors.New("cannot undo")
		}
		return Result{Outcome: OK}, nil
	})
}

// tidyIn waits on a person, and undoes its work when abandoned.
func tidyIn() *Graph {
	return &Graph{Name: "tidyin", OnAbort: "undo", Nodes: []Node{
		{ID: "wait", Kind: Person, Prompt: "Go on?"},
		{ID: "undo", Kind: Run, Tool: "undo", With: map[string]Value{"who": Literal("inner")}},
	}}
}

// tidyOut calls tidyIn, and when abandoned undoes its own work and asks
// a person to confirm.
func tidyOut() *Graph {
	return &Graph{Name: "tidyout", OnAbort: "undo", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "tidyin"},
		{ID: "undo", Kind: Run, Tool: "undo", With: map[string]Value{"who": Literal("outer")}, Next: map[string]string{OK: "confirm"}},
		{ID: "confirm", Kind: Person, Prompt: "Undone. Anything else to clear up?", Unattended: Fallback{Outcome: OK}},
	}}
}

// abandoned asks for a new run of g over the ones under way, as a host
// does with Invoke: the children's runs are abandoned in their sessions,
// and the calls that wait on them refused.
func abandoned(t *testing.T, g *Graph, a *agentturn.Agent, _ *agentturn.RunEnd) *agentturn.RunEnd {
	t.Helper()
	next, _, err := Invoke(context.Background(), g, a, nil)
	if next == nil {
		t.Fatal(err)
	}
	return next
}

// endingOf is the ending of the last run of workflow that ended in t.
func endingOf(t *testing.T, items agentturn.Transcript, workflow string) Ending {
	t.Helper()
	runs := map[string]string{}
	for _, item := range items {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == startTool {
			var p startPayload
			if json.Unmarshal([]byte(c.Arguments), &p) == nil {
				runs[p.Run] = p.Workflow
			}
		}
	}
	var out *Ending
	for _, e := range endings(items) {
		if runs[e.Instance] == workflow {
			out = &e
		}
	}
	if out == nil {
		t.Fatalf("no ending of %s", workflow)
	}
	return *out
}

// endings are the endings written in t, in order.
func endings(t agentturn.Transcript) []Ending {
	var out []Ending
	for _, item := range t {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == endTool {
			var e Ending
			_ = json.Unmarshal([]byte(c.Arguments), &e)
			out = append(out, e)
		}
	}
	return out
}

func TestAnAbandonedStackCleansUpInnermostFirst(t *testing.T) {
	g := linked(t, tidyOut(), tidyIn())
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{undoTool(&ran, nil)})
	end := start(t, a)
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	end = abandoned(t, g, a, end)
	// The outer cleanup asks its person before the run ends.
	if q := question(t, end); q.Workflow != "tidyout" || q.Step != "confirm" {
		t.Fatalf("question = %+v", q)
	}
	if !slices.Equal(ran, []string{"inner", "outer"}) {
		t.Errorf("ran %v", ran)
	}
	end = reply(t, g, a, end, OK)
	// The new run starts once both have ended.
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	in, out := endingOf(t, whole(a), "tidyin"), endingOf(t, whole(a), "tidyout")
	if len(endings(whole(a))) != 2 || in.Status != Abandoned || in.Cleanup != "done" || out.Status != Abandoned || out.Cleanup != "done" {
		t.Fatalf("ends = %+v", endings(whole(a)))
	}
	runs, err := Runs(a.State().Transcript, under(a))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"tidyout:abandoned:", " tidyin:abandoned:", "tidyout:running:", " tidyin:running:"}
	if got := tree(runs); !slices.Equal(got, want) {
		t.Errorf("tree =\n%s", strings.Join(got, "\n"))
	}
	if steps := runs[0].State.Steps; len(steps) != 2 || steps[1].Node != "confirm" {
		t.Errorf("steps = %+v", steps)
	}
}

func TestACleanupThatFailsStillEnds(t *testing.T) {
	g := linked(t, tidyOut(), tidyIn())
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{undoTool(&ran, func(who string) bool { return who == "inner" })}, Unattended())
	// Unattended, the inner question waits; it has no default.
	end := start(t, a)
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	end = abandoned(t, g, a, end)
	// The outer confirm takes its default, and nothing asks again.
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	in, out := endingOf(t, whole(a), "tidyin"), endingOf(t, whole(a), "tidyout")
	if len(endings(whole(a))) != 2 || !strings.HasPrefix(in.Cleanup, "failed at undo: ") || !strings.Contains(in.Cleanup, "cannot undo") || out.Cleanup != "done" {
		t.Fatalf("ends = %+v", endings(whole(a)))
	}
}

func TestASecondAbandonEndsACleanup(t *testing.T) {
	g := linked(t, tidyOut(), tidyIn())
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{undoTool(&ran, nil)})
	end := abandoned(t, g, a, start(t, a))
	if q := question(t, end); q.Step != "confirm" {
		t.Fatalf("question = %+v", q)
	}
	end = abandoned(t, g, a, end)
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	in, out := endingOf(t, whole(a), "tidyin"), endingOf(t, whole(a), "tidyout")
	// The question the cleanup waited on was refused as the host abandoned
	// the run again, which cuts the cleanup short there: it did not fail.
	if len(endings(whole(a))) != 2 || out.Cleanup != cutShort || in.Cleanup != "done" || !slices.Equal(ran, []string{"inner", "outer"}) {
		t.Fatalf("ends = %+v, ran %v", endings(whole(a)), ran)
	}
}

func TestACleanupPathIsCheckedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		g    *Graph
		want string
	}{
		{"no such step", &Graph{Name: "c", OnAbort: "undoo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"}, {ID: "undo", Kind: Run, Tool: "t"}}},
			`on_abort names no step "undoo"; did you mean "undo"?`},
		{"shared with the workflow", &Graph{Name: "c", OnAbort: "undo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t", Next: map[string]string{OK: "undo"}}, {ID: "undo", Kind: Run, Tool: "t"}}},
			"on both the workflow's path and its cleanup"},
		{"a model step", &Graph{Name: "c", OnAbort: "undo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"}, {ID: "undo", Kind: Agent, Prompt: "x"}}},
			"no model steps"},
		{"a loop", &Graph{Name: "c", OnAbort: "undo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"},
			{ID: "undo", Kind: Run, Tool: "t", Next: map[string]string{OK: "again"}}, {ID: "again", Kind: Run, Tool: "t", Next: map[string]string{OK: "undo"}}}},
			"no loop"},
		{"a step that may not have run", &Graph{Name: "c", OnAbort: "undo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"},
			{ID: "undo", Kind: Run, Tool: "t", With: map[string]Value{"x": From("steps.a.outputs")}}}},
			`some path gets here without running "a"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.g.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// A cleanup reads what may not have run as optional, and its ends are
	// no outcomes of the workflow.
	g := &Graph{Name: "c", OnAbort: "undo", Nodes: []Node{{ID: "a", Kind: Run, Tool: "t"},
		{ID: "undo", Kind: Run, Tool: "t", With: map[string]Value{"x": Optional("steps.a.outputs")}, Outcomes: []string{"undone"}}}}
	if err := g.Validate(); err != nil || !slices.Equal(g.Outcomes(), []string{OK}) {
		t.Errorf("err = %v, outcomes %v", err, g.Outcomes())
	}
}

// An abandon cleans the callee up in its child session, then the caller.
// A process killed in the caller's cleanup leaves the callee's done, and
// a new one goes on from the store: the cleanup that was cut off, not the
// one that finished.
func TestACleanupCutOffByAKillGoesOn(t *testing.T) {
	g := func() *Graph { return linked(t, tidyOut(), tidyIn()) }
	ctx := context.Background()
	if dir := os.Getenv(childEnv); dir != "" {
		var mine []string
		h := hosted(t, dir, "", g(), &fakeModel{}, []agenttool.Tool{undoTool(&mine, func(who string) bool {
			if who == "outer" {
				die()
			}
			return false
		})}, false)
		if err := os.WriteFile(dir+"/id", []byte(h.id), 0o600); err != nil {
			t.Fatal(err)
		}
		first, err := h.a.Prompt(ctx, openresponses.UserText("go"))
		if err != nil {
			t.Fatal(err)
		}
		if end, _, err := Drive(ctx, h.g, h.a, first, h.hist); err != nil || question(t, end).Step != "wait" {
			t.Fatalf("end = %+v, %v", end, err)
		}
		// Abandoned while it waits, it dies in the outer cleanup.
		_, _, err = Invoke(ctx, h.g, h.a, nil, h.hist)
		t.Fatalf("not killed: %v", err)
	}
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	child.Env = append(os.Environ(), childEnv+"="+dir)
	out, err := child.CombinedOutput()
	id, readErr := os.ReadFile(dir + "/id")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != -1 || readErr != nil {
		t.Fatalf("child was not killed: err = %v, read = %v\n%s", err, readErr, out)
	}
	var ran []string
	h := hosted(t, dir, string(id), g(), &fakeModel{}, []agenttool.Tool{undoTool(&ran, nil)}, false)
	p := h.a.State().Pending
	if len(p) != 1 || p[0].Call.Name != "undo" {
		t.Fatalf("pending = %+v", p)
	}
	// The undo may have run: the person says it did not.
	end, _, err := Resume(ctx, h.g, h.a, []agentturn.Answer{agentturn.OutcomeUnknown(p[0].Call.CallID)}, h.hist)
	if end == nil {
		t.Fatal(err)
	}
	// The cleanup step failed, so the cleanup is over, the run ends, and
	// the run the abandon asked for begins: its callee waits on a person.
	if q := question(t, end); q.Step != "wait" {
		t.Fatalf("question = %+v", q)
	}
	// The callee's cleanup was done before the kill and did not run again.
	if len(ran) != 0 {
		t.Errorf("ran %v", ran)
	}
	runs := h.runs(t)
	if got, want := tree(runs), []string{"tidyout:abandoned:", " tidyin:abandoned:", "tidyout:running:", " tidyin:running:"}; !slices.Equal(got, want) {
		t.Fatalf("tree = %v, want %v", got, want)
	}
	outer := endings(pathItems(h.session(t, "")))
	inner := endings(pathItems(h.session(t, runs[0].Children[0].Spawn)))
	if len(outer) != 1 || !strings.HasPrefix(outer[0].Cleanup, "failed at undo") || len(inner) != 1 || inner[0].Cleanup != "done" {
		t.Fatalf("ends = %+v, %+v", outer, inner)
	}
}
