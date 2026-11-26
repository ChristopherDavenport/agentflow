package script_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/script"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
)

func workspace(t *testing.T, scripts map[string]string) agentworkspace.Workspace {
	t.Helper()
	dir := t.TempDir()
	for name, body := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws, err := agentworkspace.NewLocal(dir, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ws.Close(); err != nil {
			t.Error(err)
		}
	})
	return ws
}

func object(required ...string) *agenttool.Schema {
	s := &agenttool.Schema{Type: "object", Required: required}
	for _, name := range required {
		s.Properties = append(s.Properties, agenttool.Property{Name: name, Schema: &agenttool.Schema{Type: "string"}})
	}
	return s
}

// The release workflow: gather prints what a CLI would, jq reshapes it,
// the model summarises, a script picks the branch, a person names the
// version and a tool gets it.
var scripts = map[string]string{
	// A plain program: stdout is the data.
	"gather.sh": `read -r in
printf '{"pr": %s, "files": [{"path":"a.go","additions":900},{"path":"b.md","additions":3}]}' "$(printf '%s' "$in" | jq .pr)"`,
	"filter.sh": `jq -c '{paths: [.files[].path | select(endswith(".go"))], added: ([.files[].additions] | add)}'`,
	// Logs on stdout, the result in the file.
	"score.sh": `echo "scoring..."
jq -c '{outcome: (if .added > 100 then "big" else "small" end), data: {summary: .summary}}' > "$AGENTFLOW_OUTPUT"
echo "done"`,
}

func release() *agentflow.Graph {
	return &agentflow.Graph{
		Name:    "release",
		Inputs:  &agenttool.Schema{Type: "object", Required: []string{"pr"}, Properties: []agenttool.Property{{Name: "pr", Schema: &agenttool.Schema{Type: "integer"}}}},
		Outputs: map[string]agentflow.Value{"summary": agentflow.From("steps.score.outputs.summary")},
		Nodes: []agentflow.Node{
			{ID: "gather", Kind: agentflow.Run, Script: "gather.sh", With: map[string]agentflow.Value{"pr": agentflow.From("inputs.pr")},
				Next: map[string]string{"ok": "filter"}},
			{ID: "filter", Kind: agentflow.Run, Script: "filter.sh", With: map[string]agentflow.Value{"files": agentflow.From("steps.gather.outputs.files")},
				Next: map[string]string{"ok": "review"}},
			{ID: "review", Kind: agentflow.Agent, Prompt: "Summarise the change in one sentence, as data.summary.",
				With: map[string]agentflow.Value{"paths": agentflow.From("steps.filter.outputs.paths")},
				Data: map[string]*agenttool.Schema{"ok": object("summary")},
				Next: map[string]string{"ok": "score"}},
			{ID: "score", Kind: agentflow.Run, Script: "score.sh", Outcomes: []string{"big", "small"},
				With: map[string]agentflow.Value{"added": agentflow.From("steps.filter.outputs.added"), "summary": agentflow.From("steps.review.outputs.summary")},
				Data: map[string]*agenttool.Schema{"big": object("summary"), "small": object("summary")},
				Next: map[string]string{"big": "confirm"}},
			{ID: "confirm", Kind: agentflow.Person, Prompt: "Which version?", Outcomes: []string{"release", "abort"},
				With: map[string]agentflow.Value{"summary": agentflow.From("steps.score.outputs.summary")},
				Data: map[string]*agenttool.Schema{"release": object("version")},
				Next: map[string]string{"release": "tag"}},
			{ID: "tag", Kind: agentflow.Run, Tool: "git_tag", Args: json.RawMessage(`{"sign":true}`),
				With: map[string]agentflow.Value{"name": agentflow.From("steps.confirm.outputs.version")}},
		},
	}
}

type tagArgs struct {
	Name string `json:"name"`
	Sign bool   `json:"sign"`
}

// model answers the review step with data, wrong once first.
type model struct {
	requests []openresponses.Request
}

func (m *model) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.requests = append(m.requests, req)
	args := `{"step":"review","outcome":"ok","data":{"summary":"Adds a thing."}}`
	if len(m.requests) == 1 {
		args = `{"step":"review","outcome":"ok","data":{"note":"wrong shape"}}`
	}
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	call, err := e.FunctionCall("", "flow_finish")
	if err != nil {
		return err
	}
	if err := call.Arguments(withRun(req, args)); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

func invoke(t *testing.T, g *agentflow.Graph, a *agentturn.Agent, inputs any) *agentturn.RunEnd {
	t.Helper()
	item, err := g.Invocation(inputs)
	if err != nil {
		t.Fatal(err)
	}
	end, err := a.Prompt(context.Background(), item)
	if end == nil {
		t.Fatal(err)
	}
	return end
}

func TestDataThroughEveryKindOfStep(t *testing.T) {
	ctx := context.Background()
	g, m := release(), &model{}
	var tagged []tagArgs
	tag := agenttool.New("git_tag", "Tag.", func(_ context.Context, a tagArgs) (agentflow.Result, error) {
		tagged = append(tagged, a)
		return agentflow.Result{Outcome: "ok"}, nil
	})
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: m, ModelName: "fake", MaxTurns: 20,
		Tools: []agenttool.Tool{script.New(workspace(t, scripts), time.Minute), tag}})
	if err != nil {
		t.Fatal(err)
	}
	store := agentsession.NewMemoryStore()
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	rec.Attach(a)

	end := invoke(t, g, a, map[string]any{"pr": 12})
	if end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 {
		t.Fatalf("end = %s, err = %v, %v", end.Reason, end.Err, transcript(a))
	}
	// The model was given jq's output and the shape wanted of it.
	if got := m.requests[0].Instructions; !strings.Contains(got, `Inputs: {"paths":["a.go"]}`) || !strings.Contains(got, `"summary"`) {
		t.Errorf("instructions = %s", got)
	}
	if len(m.requests) != 2 {
		t.Errorf("model turns = %d, want 2: the wrong shape is refused", len(m.requests))
	}
	var q agentflow.Question
	if err := json.Unmarshal([]byte(end.Pending[0].Call.Arguments), &q); err != nil {
		t.Fatal(err)
	}
	if q.With["summary"] != "Adds a thing." || q.Form["release"] == nil {
		t.Errorf("question = %+v", q)
	}

	// An answer without the version is refused, and the question waits.
	if _, err := g.Answer(end.Pending[0], agentflow.Result{Outcome: "release"}); err == nil {
		t.Fatal("an answer with no data was accepted")
	}
	ans, err := g.Answer(end.Pending[0], agentflow.Result{Outcome: "release", Data: map[string]any{"version": "v1.2.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if end, err = a.Resume(ctx, ans); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, err = %v", end, err)
	}
	if want := []tagArgs{{Name: "v1.2.0", Sign: true}}; !reflect.DeepEqual(tagged, want) {
		t.Errorf("tagged = %+v, want %+v", tagged, want)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil || !st.Done || st.Outputs["summary"] != "Adds a thing." {
		t.Fatalf("state = %+v, %v", st, err)
	}
	got, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	hashes := 0
	for _, e := range got.Path(got.Leaf()) {
		if r, ok := e.(*agentsession.ResponseEntry); ok {
			h, err := got.RequestHashFor(r.Parent, r)
			if err != nil || h != r.RequestHash || h == "" {
				t.Errorf("hash of %s: %q, %q, %v", r.ResponseID, r.RequestHash, h, err)
			}
			hashes++
		}
	}
	t.Logf("%d request hashes verified", hashes)
}

func transcript(a *agentturn.Agent) []string {
	var out []string
	for _, item := range a.State().Transcript {
		b, _ := json.Marshal(item)
		out = append(out, string(b))
	}
	return out
}

func TestScriptContract(t *testing.T) {
	ws := workspace(t, map[string]string{
		"silent.sh":   `true`,
		"text.sh":     `echo done`,
		"two.sh":      `echo '{} {}'`,
		"fail.sh":     `echo boom >&2; exit 3`,
		"big.sh":      `head -c 2000000 /dev/zero | tr '\0' 1`,
		"number.sh":   `echo 9007199254740993`,
		"array.sh":    `echo '[1,2]'`,
		"noout.sh":    `echo '{"data":1}'`,
		"leftover.sh": `ls -a | grep -c agentflow-output`,
	})
	tool := script.New(ws, time.Minute)
	for _, tc := range []struct {
		name, run, want, fails string
		outcomes               []string
	}{
		{name: "no output", run: "silent.sh", want: `{"outcome":"ok"}`},
		{name: "array", run: "array.sh", want: `{"outcome":"ok","data":[1,2]}`},
		{name: "number kept", run: "number.sh", want: `{"outcome":"ok","data":9007199254740993}`},
		{name: "not json", run: "text.sh", fails: "not JSON"},
		{name: "two values", run: "two.sh", fails: "more than one"},
		{name: "exit status", run: "fail.sh", fails: "exit 3"},
		{name: "too big", run: "big.sh", fails: "over"},
		{name: "branch without outcome", run: "noout.sh", outcomes: []string{"a"}, fails: "outcomes prints"},
		{name: "outside", run: "../x.sh", fails: "not a path within"},
		{name: "only its own result file", run: "leftover.sh", want: `{"outcome":"ok","data":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(agentflow.ScriptCall{Run: tc.run, Outcomes: tc.outcomes})
			if err != nil {
				t.Fatal(err)
			}
			res, err := tool.Execute(context.Background(), agenttool.Call{ID: "c", Args: args})
			if tc.fails != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fails) {
					t.Fatalf("err = %v, want %q", err, tc.fails)
				}
				return
			}
			if err != nil || res.Output.String() != tc.want {
				t.Fatalf("output = %s, err = %v, want %s", res.Output.String(), err, tc.want)
			}
		})
	}
}

func TestReferencesAreCheckedAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*agentflow.Graph)
		want   string
	}{
		{"valid", func(*agentflow.Graph) {}, ""},
		{"later step", func(g *agentflow.Graph) { g.Nodes[0].With["x"] = agentflow.From("steps.tag.outputs") }, "without running"},
		{"unknown step", func(g *agentflow.Graph) { g.Nodes[1].With["x"] = agentflow.From("steps.nope.outputs") }, "no step"},
		{"unknown input", func(g *agentflow.Graph) { g.Nodes[0].With["pr"] = agentflow.From("inputs.repo") }, "not a required input"},
		{"field of another outcome", func(g *agentflow.Graph) { g.Nodes[4].Next["abort"] = "tag" }, "does not certainly produce"},
		{"field never declared", func(g *agentflow.Graph) {
			g.Nodes[5].With["name"] = agentflow.From("steps.confirm.outputs.tag")
		}, "does not certainly produce"},
		{"a path skips it", func(g *agentflow.Graph) { g.Nodes[3].Next["small"] = "tag" }, "without running"},
		{"workflow output skipped", func(g *agentflow.Graph) { g.Outputs["v"] = agentflow.From("steps.confirm.outputs.version") }, "without running"},
		{"syntax", func(g *agentflow.Graph) { g.Nodes[0].With["pr"] = agentflow.From("gather.files") }, "invalid reference"},
		{"unattended data", func(g *agentflow.Graph) { g.Nodes[4].Unattended = "release" }, "unattended data"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := release()
			tc.break_(g)
			err := g.Validate()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestFailingScriptAsks(t *testing.T) {
	g := &agentflow.Graph{Name: "f", Nodes: []agentflow.Node{{ID: "bad", Kind: agentflow.Run, Script: "bad.sh"}}}
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: &model{}, ModelName: "fake",
		Tools: []agenttool.Tool{script.New(workspace(t, map[string]string{"bad.sh": "echo nope"}), time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	end, err := agentturn.New(cfg).Prompt(context.Background(), openresponses.UserText("go"))
	if err != nil || end.Reason != agentturn.ReasonInputRequired || !strings.Contains(end.Pending[0].Call.Arguments, "not JSON") {
		t.Fatalf("end = %+v, %v", end, err)
	}
	st, err := g.Replay(nil)
	if err != nil || errors.Is(err, agentflow.ErrStepFailed) || st.Started {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

// TestLiveChain runs the release workflow with a real model on the
// review step. Set AGENTFLOW_LIVE_URL and AGENTFLOW_LIVE_MODEL.
func TestLiveChain(t *testing.T) {
	url, name := os.Getenv("AGENTFLOW_LIVE_URL"), os.Getenv("AGENTFLOW_LIVE_MODEL")
	if url == "" || name == "" {
		t.Skip("set AGENTFLOW_LIVE_URL and AGENTFLOW_LIVE_MODEL")
	}
	trials, _ := strconv.Atoi(os.Getenv("AGENTFLOW_LIVE_TRIALS"))
	finished := 0
	for i := range max(trials, 1) {
		g := release()
		tag := agenttool.New("git_tag", "Tag.", func(context.Context, tagArgs) (agentflow.Result, error) {
			return agentflow.Result{Outcome: "ok"}, nil
		})
		cfg, err := agentflow.Apply(g, agentturn.Config{Model: openresponses.NewClient(url).AsAdapter(), ModelName: name, MaxTurns: 20,
			Tools: []agenttool.Tool{script.New(workspace(t, scripts), time.Minute), tag}})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		a := agentturn.New(cfg)
		item, err := g.Invocation(map[string]any{"pr": 12})
		if err != nil {
			t.Fatal(err)
		}
		end, err := a.Prompt(ctx, item)
		if err == nil {
			end, _, err = agentflow.Drive(ctx, g, a, end)
		}
		cancel()
		if err == nil && end.Reason == agentturn.ReasonInputRequired {
			finished++
			t.Logf("trial %d: reached the question: %s", i, end.Pending[0].Call.Arguments)
			continue
		}
		t.Logf("trial %d: %v; last: %.600s", i, err, transcript(a)[len(a.State().Transcript)-3:])
	}
	t.Logf("%s: %d of %d reached the question", name, finished, max(trials, 1))
}

// caller calls one tool with the arguments given, then talks.
type caller struct {
	name, args string
	called     bool
}

func (m *caller) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	if m.called {
		msg, err := e.Message("")
		if err != nil {
			return err
		}
		if err := msg.Text("ok"); err != nil {
			return err
		}
		if err := msg.Close(); err != nil {
			return err
		}
		return e.Complete()
	}
	m.called = true
	call, err := e.FunctionCall("", m.name)
	if err != nil {
		return err
	}
	if err := call.Arguments(m.args); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

func TestLargeNumbersReachTheScript(t *testing.T) {
	g := &agentflow.Graph{Name: "n", Nodes: []agentflow.Node{
		{ID: "make", Kind: agentflow.Run, Script: "make.sh", Next: map[string]string{"ok": "use"}},
		{ID: "use", Kind: agentflow.Run, Script: "use.sh", With: map[string]agentflow.Value{"id": agentflow.From("steps.make.outputs.id")}},
	}}
	ws := workspace(t, map[string]string{"make.sh": `echo '{"id": 9007199254740993}'`, "use.sh": "cat"})
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: &model{}, ModelName: "fake", Tools: []agenttool.Tool{script.New(ws, time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if _, err := a.Prompt(context.Background(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil || !st.Done {
		t.Fatalf("state = %+v, %v", st, err)
	}
	got, err := json.Marshal(st.Steps[1].Result.Data)
	if err != nil || string(got) != `{"id":9007199254740993}` {
		t.Fatalf("the script read %s, %v", got, err)
	}
}

func TestTheModelCannotRunScripts(t *testing.T) {
	g := &agentflow.Graph{Name: "m", Nodes: []agentflow.Node{{ID: "one", Kind: agentflow.Run, Script: "one.sh"}}}
	ws := workspace(t, map[string]string{"one.sh": "true", "evil.sh": "touch ran"})
	m := &caller{name: agentflow.ScriptTool, args: `{"run":"evil.sh"}`}
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: m, ModelName: "fake", MaxTurns: 6, Tools: []agenttool.Tool{script.New(ws, time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	for range 2 { // the workflow, then a conversation after it
		if _, err := a.Prompt(context.Background(), openresponses.UserText("go")); err != nil {
			t.Fatal(err)
		}
	}
	if !m.called {
		t.Fatal("the model never took a turn")
	}
	if _, err := os.Stat(filepath.Join(ws.Root(), "ran")); !os.IsNotExist(err) {
		t.Fatalf("the model ran a script: %v", err)
	}
}

// withRun names in a finish or a verdict the run the request's
// instructions name, as a model reads it from them.
func withRun(req openresponses.Request, args string) string {
	_, rest, ok := strings.Cut(req.Instructions, `, run "`)
	if !ok || !strings.HasPrefix(args, "{") || strings.Contains(args, `"run":`) {
		return args
	}
	run, _, _ := strings.Cut(rest, `"`)
	return `{"run":"` + run + `",` + args[1:]
}

func TestSubConfinesAWorkspaceToADirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "svc", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.txt"), []byte("top"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := agentworkspace.NewLocal(dir, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	sub, err := script.Sub(ws, "svc/api")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "svc", "api"); sub.Root() != want || sub.Descriptor().Root != ws.Descriptor().Root && sub.Descriptor().Root != want {
		t.Errorf("root = %s", sub.Root())
	}
	if err := sub.WriteFile(ctx, "note.txt", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "svc", "api", "note.txt")); err != nil || string(b) != "hi" {
		t.Errorf("wrote %q, %v", b, err)
	}
	if _, err := fs.ReadFile(sub.FS(), "note.txt"); err != nil {
		t.Error(err)
	}
	out, err := sub.Exec(ctx, agentworkspace.Command{Args: []string{"pwd"}})
	if err != nil || strings.TrimSpace(string(out.Stdout)) != filepath.Join(dir, "svc", "api") {
		t.Errorf("pwd = %q, %v", out.Stdout, err)
	}
	for _, name := range []string{"../top.txt", "/top.txt"} {
		if err := sub.WriteFile(ctx, name, nil, 0o644); !errors.Is(err, agentworkspace.ErrOutside) {
			t.Errorf("wrote %s: %v", name, err)
		}
		if _, err := sub.Exec(ctx, agentworkspace.Command{Args: []string{"true"}, Dir: name}); !errors.Is(err, agentworkspace.ErrOutside) {
			t.Errorf("ran in %s: %v", name, err)
		}
	}
	for _, bad := range []string{"..", "/svc", "missing", "top.txt"} {
		if _, err := script.Sub(ws, bad); err == nil {
			t.Errorf("Sub(%q) was allowed", bad)
		}
	}
}

func TestAScriptReportsProgress(t *testing.T) {
	ws := workspace(t, map[string]string{"long.sh": `printf '{"progress": 1, "total": 3, "message": "fetching"}\n' >> "$AGENTFLOW_PROGRESS"
sleep 0.3
printf 'compiling\n' >> "$AGENTFLOW_PROGRESS"
printf '{"progress": 3, "total": 3}' >> "$AGENTFLOW_PROGRESS"
echo '{"built": true}'`})
	tool := script.New(ws, time.Minute)
	var updates []agenttool.Result
	res, err := tool.Execute(context.Background(), agenttool.Call{ID: "c1", Args: json.RawMessage(`{"run":"long.sh"}`),
		OnUpdate: func(r agenttool.Result) { updates = append(updates, r) }})
	if err != nil {
		t.Fatal(err)
	}
	var got []agenttool.ProgressInfo
	var texts []string
	for _, u := range updates {
		got = append(got, u.Details.(agenttool.ProgressInfo))
		texts = append(texts, u.Output.String())
	}
	want := []agenttool.ProgressInfo{{Progress: 1, Total: 3, Message: "fetching"}, {Message: "compiling"}, {Progress: 3, Total: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("updates = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(texts, []string{"1/3 fetching", "compiling", "3/3"}) {
		t.Errorf("texts = %q", texts)
	}
	// The result is the script's, whatever it reported on the way.
	if !strings.Contains(res.Output.String(), `"built":true`) {
		t.Errorf("result = %s", res.Output.String())
	}
	// Nothing of it is left behind.
	entries, err := fs.ReadDir(ws.FS(), ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".agentflow") {
			t.Errorf("left %s", e.Name())
		}
	}
}

func TestAStepsProgressReachesTheLoop(t *testing.T) {
	g := &agentflow.Graph{Name: "p", Nodes: []agentflow.Node{{ID: "build", Kind: agentflow.Run, Script: "build.sh"}}}
	ws := workspace(t, map[string]string{"build.sh": `printf 'half\n' >> "$AGENTFLOW_PROGRESS"; echo '{"ok": true}'`})
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: &model{}, ModelName: "fake", Tools: []agenttool.Tool{script.New(ws, time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	var seen []string
	defer a.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		if u, ok := ev.(*agentturn.ToolUpdate); ok {
			seen = append(seen, u.Name+": "+u.Partial.Output.String())
		}
		return nil
	})()
	if _, err := a.Prompt(context.Background(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil || len(st.Steps) != 1 {
		t.Fatalf("steps = %+v, %v", st.Steps, err)
	}
	if !reflect.DeepEqual(seen, []string{agentflow.ScriptTool + ": half"}) {
		t.Errorf("updates = %q", seen)
	}
}
