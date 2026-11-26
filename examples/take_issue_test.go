package examples

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/script"
	"github.com/ChristopherDavenport/agentflow/workflow"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
)

// turn is one thing the scripted model does, on the step it expects.
type turn struct{ step, tool, args string }

type scripted struct {
	t     *testing.T
	turns []turn
}

func (m *scripted) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	_, rest, ok := strings.Cut(req.Instructions, `You are on step "`)
	if !ok {
		_, rest, _ = strings.Cut(req.Instructions, `You are judging step "`)
	}
	step, _, _ := strings.Cut(rest, `"`)
	if len(m.turns) == 0 || m.turns[0].step != step {
		m.t.Errorf("the model was asked about step %q, want %+v", step, m.turns)
		return errors.New("off script")
	}
	next := m.turns[0]
	m.turns = m.turns[1:]
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	call, err := e.FunctionCall("", next.tool)
	if err != nil {
		return err
	}
	if err := call.Arguments(next.args); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

func finish(step, outcome, data string) turn {
	return turn{step, "flow_finish", `{"step":"` + step + `","outcome":"` + outcome + `","data":` + data + `}`}
}

func plan(file string) string {
	return `{"plan":{"title":"Fix the greeting","summary":"Says it properly.","files":["` + file + `"],"steps":["edit it"]}}`
}

func write(step, content string) turn {
	b, _ := json.Marshal(map[string]string{"path": "greeting.txt", "content": content})
	return turn{step, "write", string(b)}
}

// repository is a clone with a remote, a check, and a gh that answers for
// issue 7.
type repository struct {
	dir, origin, bin string
	env              []string
}

func newRepository(t *testing.T) repository {
	t.Helper()
	for _, p := range []string{"git", "jq", "make"} {
		if _, err := exec.LookPath(p); err != nil {
			t.Skipf("no %s", p)
		}
	}
	root := t.TempDir()
	r := repository{dir: filepath.Join(root, "clone"), origin: filepath.Join(root, "origin.git"), bin: filepath.Join(root, "bin")}
	r.env = []string{"PATH=" + r.bin + ":" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid"}
	file := func(path, content string, mode os.FileMode) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	file(filepath.Join(r.bin, "gh"), `#!/bin/sh
case "$1 $2" in
"issue view") echo '{"title":"The greeting is wrong","body":"Say hello.","state":"OPEN"}' ;;
"pr create") echo "https://example.invalid/pull/1" ;;
*) exit 1 ;;
esac
`, 0o755)
	file(filepath.Join(r.dir, "greeting.txt"), "helo\n", 0o644)
	file(filepath.Join(r.dir, "Makefile"), "check:\n\tgrep -qx hello greeting.txt\n", 0o644)
	r.git(t, root, "init", "--quiet", "--bare", r.origin)
	r.git(t, r.dir, "init", "--quiet", "--initial-branch=main")
	r.git(t, r.dir, "add", ".")
	r.git(t, r.dir, "commit", "--quiet", "-m", "start")
	r.git(t, r.dir, "remote", "add", "origin", r.origin)
	return r
}

func (r repository) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir, c.Env = dir, r.env
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// run takes the issue once, answering questions in order. It returns the
// finished state.
func (r repository) run(t *testing.T, m *scripted, answers []agentflow.Result, opts ...agentflow.Option) agentflow.State {
	t.Helper()
	ctx := context.Background()
	w := loadFrom(t, "skills/take-issue")
	ws, err := agentworkspace.NewLocal(r.dir, r.env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	type writeArgs struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	writer := agenttool.New("write", "Write a file.", func(ctx context.Context, a writeArgs) (string, error) {
		return "written", ws.WriteFile(ctx, a.Path, []byte(a.Content), 0o644)
	})
	cfg, err := agentflow.Apply(w.Graph, agentturn.Config{Model: m, ModelName: "scripted",
		Tools: append(tools(ws.FS()), writer, script.New(ws, time.Minute, script.Files(w.Files)))}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	item, err := w.Graph.Invocation(map[string]any{"repo": "o/r", "issue": 7})
	if err != nil {
		t.Fatal(err)
	}
	end, err := a.Prompt(ctx, item)
	for err == nil && end.Reason == agentturn.ReasonInputRequired {
		if len(answers) == 0 {
			t.Fatalf("an unexpected question: %s", end.Pending[0].Call.Arguments)
		}
		ans, err2 := w.Graph.Answer(end.Pending[0], answers[0])
		if err2 != nil {
			t.Fatalf("%v: %s", err2, end.Pending[0].Call.Arguments)
		}
		answers = answers[1:]
		end, err = a.Resume(ctx, ans)
	}
	if err != nil {
		t.Fatal(err)
	}
	st, err := w.Graph.Replay(a.State().Transcript)
	if err != nil || !st.Done || end.Reason != agentturn.ReasonDone {
		t.Fatalf("done = %v, %v, end = %s %v", st.Done, err, end.Reason, end.Err)
	}
	if len(m.turns) != 0 || len(answers) != 0 {
		t.Errorf("left over: turns %+v, answers %+v", m.turns, answers)
	}
	return st
}

func visited(st agentflow.State) []string {
	var out []string
	for _, s := range st.Steps {
		out = append(out, s.Node+":"+s.Result.Outcome)
	}
	return out
}

func TestTakeIssueToAPullRequest(t *testing.T) {
	r := newRepository(t)
	head := r.git(t, r.dir, "rev-parse", "HEAD")
	m := &scripted{t: t, turns: []turn{
		finish("draft", "ok", plan("missing.txt")),
		finish("revise", "ok", plan("greeting.txt")),
		finish("revise", "ok", plan("greeting.txt")),
		write("build", "hullo\n"), finish("build", "ok", "{}"),
		write("repair", "hello\n"), finish("repair", "ok", "{}"),
	}}
	st := r.run(t, m, []agentflow.Result{
		{Outcome: "ok", Data: map[string]any{"focus": "keep it small"}},
		{Outcome: "change", Data: map[string]any{"notes": "one step is enough"}},
		{Outcome: "go"},
		{Outcome: "publish"},
	})
	want := []string{"intake:ready", "focus:ok", "draft:ok", "lint:flawed", "revise:ok", "lint:sound",
		"approve:change", "revise:ok", "lint:sound", "approve:go", "guard:clear", "build:ok",
		"test:fail", "repair:ok", "test:pass", "ship:publish", "publish:ok"}
	if got := visited(st); !slices.Equal(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
	if st.Outputs["url"] != "https://example.invalid/pull/1" || st.Outputs["saved"] != nil {
		t.Errorf("outputs = %v", st.Outputs)
	}
	if got := r.git(t, r.origin, "diff", "--name-only", head, "issue-7"); got != "greeting.txt" {
		t.Errorf("published %q, want the one changed file", got)
	}
	if got := r.git(t, r.dir, "rev-parse", "HEAD"); got != head || r.git(t, r.dir, "branch", "--show-current") != "main" {
		t.Errorf("the checkout moved to %s", got)
	}
}

func TestTakeIssueUnattendedParksAndResumes(t *testing.T) {
	r := newRepository(t)
	draft := func() *scripted {
		return &scripted{t: t, turns: []turn{finish("draft", "ok", plan("greeting.txt"))}}
	}
	first := r.run(t, draft(), nil, agentflow.Unattended())
	want := []string{"intake:ready", "focus:ok", "draft:ok", "lint:sound", "approve:park", "park:ok"}
	if got := visited(first); !slices.Equal(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
	if first.Outputs["saved"] != ".agentflow-plans/7.json" || first.Outputs["url"] != nil {
		t.Errorf("outputs = %v", first.Outputs)
	}

	// The saved plan is taken up without planning again.
	again := r.run(t, &scripted{t: t}, nil, agentflow.Unattended())
	want = []string{"intake:resume", "load:current", "lint:sound", "approve:park", "park:ok"}
	if got := visited(again); !slices.Equal(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}

	// Once the code has moved, it is planned again.
	r.git(t, r.dir, "commit", "--quiet", "--allow-empty", "-m", "moved")
	moved := r.run(t, draft(), nil, agentflow.Unattended())
	want = []string{"intake:resume", "load:outdated", "outdated:redo", "focus:ok", "draft:ok", "lint:sound", "approve:park", "park:ok"}
	if got := visited(moved); !slices.Equal(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
}

func TestTakeIssueWillNotStartDirty(t *testing.T) {
	r := newRepository(t)
	if err := os.WriteFile(filepath.Join(r.dir, "greeting.txt"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := r.run(t, &scripted{t: t}, nil)
	if st.Outputs["blocked"] != "the working tree has uncommitted changes" {
		t.Errorf("outputs = %v after %v", st.Outputs, visited(st))
	}
}

func TestTakeIssueCanBeRewritten(t *testing.T) {
	w := loadFrom(t, "skills/take-issue")
	if _, err := workflow.Format(w.Graph, "Take an issue"); err != nil {
		t.Fatal(err)
	}
}

func verdict(step, outcome string) turn {
	return turn{step, "flow_judge", `{"step":"` + step + `","outcome":"` + outcome + `","reason":"it does what the issue asks"}`}
}

// The same run, nobody there, as the host trusts a judge further.
func TestTakeIssueFromAPersonToAModel(t *testing.T) {
	// Advising: the judge is heard and the default still holds.
	r := newRepository(t)
	st := r.run(t, &scripted{t: t, turns: []turn{
		finish("draft", "ok", plan("greeting.txt")), verdict("approve", "go"),
	}}, nil, agentflow.Unattended(), agentflow.Delegate(agentflow.Advise))
	want := []string{"intake:ready", "focus:ok", "draft:ok", "lint:sound", "approve:park", "park:ok"}
	if got := visited(st); !slices.Equal(got, want) {
		t.Errorf("advising: path = %v, want %v", got, want)
	}

	// Deciding the plan only: it builds, and publishing waits.
	r = newRepository(t)
	build := []turn{finish("draft", "ok", plan("greeting.txt")), verdict("approve", "go"),
		write("build", "hello\n"), finish("build", "ok", "{}")}
	st = r.run(t, &scripted{t: t, turns: build}, nil, agentflow.Unattended(), agentflow.Delegate(agentflow.Decide, "approve"))
	if got := visited(st); got[len(got)-1] != "ship:hold" || st.Outputs["url"] != nil {
		t.Errorf("plan only: path = %v", got)
	}

	// Deciding both: nobody is asked anything.
	r = newRepository(t)
	st = r.run(t, &scripted{t: t, turns: append(build, verdict("ship", "publish"))}, nil,
		agentflow.Unattended(), agentflow.Delegate(agentflow.Decide, "approve", "ship"))
	want = []string{"intake:ready", "focus:ok", "draft:ok", "lint:sound", "approve:go", "guard:clear",
		"build:ok", "test:pass", "ship:publish", "publish:ok"}
	if got := visited(st); !slices.Equal(got, want) || st.Outputs["url"] == nil {
		t.Errorf("deciding: path = %v, outputs = %v", got, st.Outputs)
	}
	var by []string
	for _, s := range st.Steps {
		if s.Result.By != "" {
			by = append(by, s.Node+" "+s.Result.By)
		}
	}
	if want := []string{"focus unattended default", "approve ai", "ship ai"}; !slices.Equal(by, want) {
		t.Errorf("by = %v, want %v", by, want)
	}
}
