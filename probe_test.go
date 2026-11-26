package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

func die() { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL); select {} }

type killingModel struct{}

func (killingModel) CreateStream(context.Context, openresponses.Request, openresponses.EventSink) error {
	die()
	return nil
}

func probeTool(replay agenttool.Replay, fn func(string)) agenttool.Tool {
	return agenttool.New("script", "Run a script.", func(_ context.Context, a scriptArgs) (Result, error) {
		fn(a.Name)
		return Result{Outcome: OK}, nil
	}, agenttool.WithReplay(func(context.Context, json.RawMessage) agenttool.Replay { return replay }))
}

// runAndDie records a session and lets cfg kill the process.
func runAndDie(t *testing.T, dir string, cfg agentturn.Config) {
	ctx := context.Background()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/id", []byte(s.ID()), 0o600); err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	rec.Attach(a)
	end := start(t, a)
	t.Fatalf("not killed: %+v", end)
}

// killed runs this test again as a child that dies, then resumes its
// session with cfg.
func killed(t *testing.T, childCfg func() agentturn.Config, cfg agentturn.Config) *agentturn.Agent {
	t.Helper()
	if dir := os.Getenv(childEnv); dir != "" {
		runAndDie(t, dir, childCfg())
	}
	ctx := context.Background()
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	child.Env = append(os.Environ(), childEnv+"="+dir)
	out, err := child.CombinedOutput()
	id, readErr := os.ReadFile(dir + "/id")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != -1 || readErr != nil {
		t.Fatalf("child was not killed: err = %v, read = %v\n%s", err, readErr, out)
	}
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Resume(ctx, store, string(id))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := session.AgentOptions(s, rec.ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg, seed...)
	rec.Attach(a)
	return a
}

func apply(t *testing.T, g *Graph, m openresponses.Streamer, tool agenttool.Tool) agentturn.Config {
	t.Helper()
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{tool}})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestKillDuringScript(t *testing.T) {
	for _, tc := range []struct {
		name   string
		replay agenttool.Replay
	}{{"safe", agenttool.ReplaySafe}, {"unknown", agenttool.ReplayUnknown}} {
		t.Run(tc.name, func(t *testing.T) {
			var ran []string
			a := killed(t,
				func() agentturn.Config {
					return apply(t, release(), &fakeModel{}, probeTool(tc.replay, func(string) { die() }))
				},
				apply(t, release(), &fakeModel{outcomes: []string{"clean"}}, probeTool(tc.replay, func(n string) { ran = append(ran, n) })))
			p := a.State().Pending
			if len(p) != 1 || !p[0].MayHaveRun() {
				t.Fatalf("pending = %+v", p)
			}
			end, err := a.Resume(context.Background(), agentturn.Approve(p[0].Call.CallID))
			if tc.replay == agenttool.ReplaySafe {
				if err != nil || end.Reason != agentturn.ReasonDone || !slices.Equal(ran, []string{"gather", "report"}) {
					t.Fatalf("end = %+v, err = %v, ran = %v", end, err, ran)
				}
				return
			}
			// A script that cannot run again on its own is the person's
			// call.
			if !errors.Is(err, agentturn.ErrAmbiguousCall) || len(ran) != 0 {
				t.Fatalf("err = %v, ran = %v", err, ran)
			}
			end, err = a.Resume(context.Background(), agentturn.OutcomeUnknown(p[0].Call.CallID))
			if err != nil {
				t.Fatal(err)
			}
			end = answer(t, a, end, "retry")
			if end.Reason != agentturn.ReasonDone || !slices.Equal(ran, []string{"gather", "report"}) {
				t.Fatalf("end = %+v, ran = %v", end, ran)
			}
		})
	}
}

func TestKillDuringModelTurn(t *testing.T) {
	var ran []string
	g := release()
	a := killed(t,
		func() agentturn.Config {
			return apply(t, release(), killingModel{}, probeTool(agenttool.ReplayUnknown, func(string) {}))
		},
		apply(t, g, &fakeModel{outcomes: []string{"clean"}}, probeTool(agenttool.ReplayUnknown, func(n string) { ran = append(ran, n) })))
	end, err := a.Continue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"gather:ok", "review:clean", "report:ok"}
	if end.Reason != agentturn.ReasonDone || !slices.Equal(ran, []string{"report"}) || !slices.Equal(path(t, g, a), want) {
		t.Fatalf("end = %s, ran = %v, path = %v", end.Reason, ran, path(t, g, a))
	}
}

func TestTransformThatLosesPositionIsRefused(t *testing.T) {
	var ran []string
	g := release()
	cfg, err := Apply(g, agentturn.Config{
		Model: &fakeModel{outcomes: []string{"clean"}}, ModelName: "fake", MaxTurns: 8,
		Tools: []agenttool.Tool{scriptTool(&ran)},
		// Stands in for a compaction: only the latest call survives.
		Transform: func(_ context.Context, tr agentturn.Transcript) (agentturn.Transcript, error) {
			if len(tr) <= 3 {
				return tr, nil
			}
			return append(agentturn.Transcript{openresponses.UserText("summary")}, tr[len(tr)-2:]...), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	end, _ := agentturn.New(cfg).Prompt(context.Background(), openresponses.UserText("go"))
	if !errors.Is(end.Err, ErrPositionLost) {
		t.Fatalf("end = %s, err = %v", end.Reason, end.Err)
	}
}

// twoFinishes finishes this step and the next in one response, then
// finishes the next properly.
type twoFinishes struct{ turns int }

func (m *twoFinishes) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	steps := []string{"plan", "build"}
	if m.turns++; m.turns > 1 {
		steps = []string{"build"}
	}
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	for _, step := range steps {
		call, err := e.FunctionCall("", finishTool)
		if err != nil {
			return err
		}
		args, err := json.Marshal(finishArgs{Step: step, Outcome: OK})
		if err != nil {
			return err
		}
		if err := call.Arguments(string(args)); err != nil {
			return err
		}
		if err := call.Close(); err != nil {
			return err
		}
	}
	return e.Complete()
}

func TestOneTurnCannotFinishTheNextStep(t *testing.T) {
	g := &Graph{Name: "two", Nodes: []Node{
		{ID: "plan", Kind: Agent, Prompt: "Plan.", Next: map[string]string{OK: "build"}},
		{ID: "build", Kind: Agent, Prompt: "Build."},
	}}
	var ran []string
	m := &twoFinishes{}
	a := agentturn.New(apply(t, g, m, scriptTool(&ran)))
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s, err = %v", end.Reason, end.Err)
	}
	if m.turns != 2 {
		t.Errorf("model turns = %d, want 2", m.turns)
	}
	// Replay holds the rule without the gate, for a record made elsewhere.
	begin, _ := json.Marshal(startPayload{Workflow: g.Name, Digest: g.Digest(), Format: RecordFormat})
	forged := agentturn.Transcript{
		&openresponses.FunctionCall{CallID: g.startID("i"), Name: startTool, Arguments: string(begin)},
		&openresponses.FunctionCallOutput{CallID: g.startID("i"), Output: openresponses.FunctionCallOutputData{Text: `{"outcome":"ok"}`}},
	}
	for _, step := range []string{"plan", "build"} {
		args, _ := json.Marshal(finishArgs{Step: step, Outcome: OK})
		forged = append(forged, &openresponses.FunctionCall{CallID: step, Name: finishTool, Arguments: string(args)})
	}
	for _, step := range []string{"plan", "build"} {
		forged = append(forged, &openresponses.FunctionCallOutput{CallID: step, Output: openresponses.FunctionCallOutputData{Text: `{"outcome":"ok"}`}})
	}
	st, err := g.Replay(forged)
	if err != nil || st.Node == nil || st.Node.ID != "build" {
		t.Fatalf("state = %+v, err = %v", st, err)
	}
}
