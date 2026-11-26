package script_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/script"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
)

// worker writes a file with its here tool, then finishes its step.
type worker struct{ requests []openresponses.Request }

func (m *worker) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.requests = append(m.requests, req)
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	name, args := "here", `{"name":"made.txt"}`
	if last := req.Input[len(req.Input)-1]; last.ItemType() == "function_call_output" {
		name, args = "flow_finish", withRun(req, `{"step":"build","outcome":"ok"}`)
	}
	call, err := e.FunctionCall("", name)
	if err != nil {
		return err
	}
	if err := call.Arguments(args); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

// here writes a file in the workspace it is made for.
func here(ws agentworkspace.Workspace) agenttool.Tool {
	return agenttool.New("here", "Write a file here.", func(ctx context.Context, a struct {
		Name string `json:"name"`
	}) (string, error) {
		return ws.Root(), ws.WriteFile(ctx, a.Name, []byte("made"), 0o644)
	})
}

// worktree makes a directory for the run, then a fresh step works in it
// and a script runs there.
func worktree(made string) *agentflow.Graph {
	path := &agenttool.Schema{Type: "object", Required: []string{"dir"}, Properties: []agenttool.Property{{Name: "dir", Schema: &agenttool.Schema{Type: "string"}}}}
	return &agentflow.Graph{Name: "worktree", Nodes: []agentflow.Node{
		{ID: "setup", Kind: agentflow.Run, Script: "setup.sh", With: map[string]agentflow.Value{"dir": agentflow.Literal(made)},
			Data: map[string]*agenttool.Schema{"ok": path}, Next: map[string]string{"ok": "build"}},
		{ID: "build", Kind: agentflow.Agent, Prompt: "Build it.", Context: agentflow.ContextFresh, Tools: []string{"here"},
			Dir: agentflow.From("steps.setup.outputs.dir"), Next: map[string]string{"ok": "check"}},
		{ID: "check", Kind: agentflow.Run, Script: "where.sh", Dir: agentflow.From("steps.setup.outputs.dir")},
	}}
}

var worktreeScripts = map[string]string{
	"setup.sh": `dir=$(jq -r .dir)
case "$dir" in ../*) ;; *) mkdir -p "$dir" ;; esac
printf '{"dir": "%s"}' "$dir"`,
	"where.sh": `printf '{"pwd": "%s", "made": "%s"}' "$(pwd)" "$(cat made.txt)"`,
}

// opener opens each child on a workspace of its dir, as a host would.
func opener(t *testing.T, ws agentworkspace.Workspace, m openresponses.Streamer, opened *[]agentflow.Child) agentflow.Option {
	agents := map[string]*agentturn.Agent{}
	return agentflow.Children(func(ctx context.Context, c agentflow.Child) (context.Context, *agentturn.Agent, error) {
		*opened = append(*opened, c)
		at := ws
		if c.Settings.Dir != "" {
			var err error
			if at, err = script.Sub(ws, c.Settings.Dir); err != nil {
				return nil, nil, err
			}
		}
		cfg, err := c.Apply(agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{here(at)}})
		if err != nil {
			return nil, nil, err
		}
		if a := agents[c.Spawn]; a != nil {
			return ctx, a, a.SetConfig(cfg)
		}
		agents[c.Spawn] = agentturn.New(cfg)
		return ctx, agents[c.Spawn], nil
	})
}

func TestAStepWorksInADirectoryAScriptMade(t *testing.T) {
	ctx := context.Background()
	ws := workspace(t, worktreeScripts)
	m := &worker{}
	var opened []agentflow.Child
	g := worktree("trees/run-1")
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{script.New(ws, time.Minute)}},
		opener(t, ws, m, &opened))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end, st, err := agentflow.Drive(ctx, g, a, invoke(t, g, a, nil))
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(opened) != 1 || opened[0].Settings.Dir != "trees/run-1" {
		t.Fatalf("opened %+v", opened)
	}
	// The child wrote in the directory, and the script ran there and read
	// what it wrote.
	var check struct{ PWD, Made string }
	if err := json.Unmarshal(mustJSON(t, st.Steps[len(st.Steps)-1].Result.Data), &check); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ws.Root(), "trees", "run-1"); check.PWD != want || check.Made != "made" {
		t.Errorf("check ran in %s and read %q, want %s", check.PWD, check.Made, want)
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "made.txt")); err == nil {
		t.Error("the child wrote at the workspace's root")
	}
	// The calls record the directory they were given.
	var recorded []string
	for _, item := range a.State().Transcript {
		c, ok := item.(*openresponses.FunctionCall)
		if !ok {
			continue
		}
		var args struct{ Dir string }
		if json.Unmarshal([]byte(c.Arguments), &args) == nil && args.Dir == "trees/run-1" {
			recorded = append(recorded, c.Name)
		}
	}
	if strings.Join(recorded, ",") != "flow_call,run_script" {
		t.Errorf("calls recording the dir: %v", recorded)
	}
}

func TestADirOutsideTheWorkspaceFailsTheStep(t *testing.T) {
	ws := workspace(t, worktreeScripts)
	m := &worker{}
	var opened []agentflow.Child
	g := worktree("../outside")
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{script.New(ws, time.Minute)}},
		opener(t, ws, m, &opened))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end := invoke(t, g, a, nil)
	// The step failed, and asks whether to run it again: no child opened.
	if end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 {
		t.Fatalf("end = %+v", end)
	}
	var q agentflow.Question
	if err := json.Unmarshal([]byte(end.Pending[0].Call.Arguments), &q); err != nil {
		t.Fatal(err)
	}
	if !q.Retry || q.Step != "build" || !strings.Contains(q.Question, `dir "../outside": want a clean path below the workspace's root`) {
		t.Errorf("question = %+v", q)
	}
	if len(opened) != 0 {
		t.Errorf("opened %+v", opened)
	}
}

func TestAMissingDirAsksToRetry(t *testing.T) {
	ctx := context.Background()
	ws := workspace(t, worktreeScripts)
	m := &worker{}
	var opened []agentflow.Child
	g := &agentflow.Graph{Name: "late", Nodes: []agentflow.Node{
		{ID: "build", Kind: agentflow.Agent, Prompt: "Build it.", Context: agentflow.ContextFresh, Tools: []string{"here"},
			Dir: agentflow.Literal("trees/late"), Next: map[string]string{"ok": "check"}},
		{ID: "check", Kind: agentflow.Run, Script: "where.sh", Dir: agentflow.Literal("trees/late")},
	}}
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{script.New(ws, time.Minute)}},
		opener(t, ws, m, &opened))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	// The directory is not there: the opener refuses the child, and the
	// step asks whether to run again, with what the opener said.
	end, _, err := agentflow.Drive(ctx, g, a, invoke(t, g, a, nil))
	if err != nil || end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 {
		t.Fatalf("end = %+v, %v", end, err)
	}
	var q agentflow.Question
	if err := json.Unmarshal([]byte(end.Pending[0].Call.Arguments), &q); err != nil {
		t.Fatal(err)
	}
	if !q.Retry || q.Step != "build" || !strings.Contains(q.Question, `dir "trees/late"`) {
		t.Fatalf("question = %+v", q)
	}
	// Once it is made, the retry works there.
	if err := os.MkdirAll(filepath.Join(ws.Root(), "trees", "late"), 0o755); err != nil {
		t.Fatal(err)
	}
	ans, err := g.Answer(end.Pending[0], agentflow.Result{Outcome: "retry"})
	if err != nil {
		t.Fatal(err)
	}
	end, st, err := agentflow.Resume(ctx, g, a, []agentturn.Answer{ans})
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("end = %+v, %v", end, err)
	}
	var check struct{ PWD, Made string }
	if err := json.Unmarshal(mustJSON(t, st.Steps[len(st.Steps)-1].Result.Data), &check); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ws.Root(), "trees", "late"); check.PWD != want || check.Made != "made" {
		t.Errorf("check ran in %s and read %q, want %s", check.PWD, check.Made, want)
	}
	if len(opened) != 2 {
		t.Errorf("opened %d times, want the refusal and the retry", len(opened))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
