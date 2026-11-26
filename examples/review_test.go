package examples

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/script"
	"github.com/ChristopherDavenport/agentflow/workflow"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
)

func load(t *testing.T) *workflow.Workflow { t.Helper(); return loadFrom(t, "skills/pr-review") }

func loadFrom(t *testing.T, dir string) *workflow.Workflow {
	t.Helper()
	s, err := agentskill.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := workflow.Load(s)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestSkillLoads(t *testing.T) { load(t) }

const maxRead = 12000

type readArgs struct {
	Path string `json:"path"`
	From int    `json:"from,omitempty" jsonschema:"first line, from 1"`
}

type grepArgs struct {
	Pattern string `json:"pattern" jsonschema:"a regular expression"`
}

// tools are the two a model step may use. The workspace's file system
// is rooted, so neither reads outside the clone.
func tools(fsys fs.FS) []agenttool.Tool {
	read := agenttool.New("read", "Read a file of the repository.", func(_ context.Context, a readArgs) (string, error) {
		b, err := fs.ReadFile(fsys, a.Path)
		if err != nil {
			return "", err
		}
		lines := strings.Split(string(b), "\n")
		var out strings.Builder
		for i := max(a.From, 1) - 1; i < len(lines); i++ {
			if out.Len() > maxRead {
				fmt.Fprintf(&out, "[more: read from line %d]", i+1)
				break
			}
			fmt.Fprintf(&out, "%d\t%s\n", i+1, lines[i])
		}
		return out.String(), nil
	})
	grep := agenttool.New("grep", "Search the repository's files.", func(_ context.Context, a grepArgs) (string, error) {
		re, err := regexp.Compile(a.Pattern)
		if err != nil {
			return "", err
		}
		var out []string
		err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && d.Name() == ".git":
				return fs.SkipDir
			case !d.Type().IsRegular() || len(out) > 40:
				return nil
			}
			b, err := fs.ReadFile(fsys, p)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(b), "\n") {
				if re.MatchString(line) && len(out) <= 40 {
					out = append(out, fmt.Sprintf("%s:%d: %.200s", p, i+1, line))
				}
			}
			return nil
		})
		if len(out) > 40 {
			out[40] = "[more matches not shown]"
		}
		if len(out) == 0 {
			return "no matches", err
		}
		return strings.Join(out, "\n"), err
	})
	return []agenttool.Tool{read, grep}
}

// TestLiveReview reviews a real pull request with nobody there. Set
// AGENTFLOW_LIVE_URL, AGENTFLOW_LIVE_MODEL, AGENTFLOW_REVIEW_DIR (a
// checkout of the pull request's head), AGENTFLOW_REVIEW_REPO and
// AGENTFLOW_REVIEW_PR. It reads from GitHub and posts nothing.
func TestLiveReview(t *testing.T) {
	url, model, dir := os.Getenv("AGENTFLOW_LIVE_URL"), os.Getenv("AGENTFLOW_LIVE_MODEL"), os.Getenv("AGENTFLOW_REVIEW_DIR")
	if url == "" || model == "" || dir == "" {
		t.Skip("set AGENTFLOW_LIVE_URL, AGENTFLOW_LIVE_MODEL and AGENTFLOW_REVIEW_DIR")
	}
	pr, err := strconv.Atoi(os.Getenv("AGENTFLOW_REVIEW_PR"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	ws, err := agentworkspace.NewLocal(dir, []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ws.Close(); err != nil {
			t.Error(err)
		}
	})
	w := load(t)
	if dir := os.Getenv("AGENTFLOW_REVIEW_SKILL"); dir != "" {
		w = loadFrom(t, dir)
	}
	g := w.Graph
	live := openresponses.NewClient(url).AsAdapter()
	all := append(tools(ws.FS()), script.New(ws, 2*time.Minute, script.Files(w.Files)))
	cfg, err := agentflow.Apply(g, agentturn.Config{
		Model:     live,
		ModelName: model,
		MaxTurns:  40,
		Tools:     all,
	}, agentflow.Unattended(), inMemory(live, model, all))
	if err != nil {
		t.Fatal(err)
	}
	sessions := os.Getenv("AGENTFLOW_REVIEW_SESSIONS")
	if sessions == "" {
		sessions = t.TempDir()
	}
	store, err := jsonl.Open(sessions)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	rec.Attach(a)
	began := time.Now()
	a.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		e, ok := ev.(*agentturn.TurnEnd)
		if !ok || e.Response == nil {
			return nil
		}
		var did []string
		for _, item := range e.Response.Output {
			switch it := item.(type) {
			case *openresponses.FunctionCall:
				did = append(did, fmt.Sprintf("%s %.300s", it.Name, it.Arguments))
			case *openresponses.Message:
				did = append(did, fmt.Sprintf("said %.300q", it.Text()))
			}
		}
		in, out := 0, 0
		if u := e.Response.Usage; u != nil {
			in, out = u.InputTokens, u.OutputTokens
		}
		fmt.Printf("%6s %s in=%d out=%d | %s\n", time.Since(began).Round(time.Second), e.Response.Model, in, out, strings.Join(did, " | "))
		for _, r := range e.ToolResults {
			if o := r.Output.String(); strings.HasPrefix(o, "Error") {
				fmt.Printf("       refused: %.300s\n", o)
			}
		}
		return nil
	})

	item, err := g.Invocation(map[string]any{"repo": os.Getenv("AGENTFLOW_REVIEW_REPO"), "pr": pr})
	if err != nil {
		t.Fatal(err)
	}
	end, err := a.Prompt(ctx, item)
	if err != nil {
		t.Fatal(err)
	}
	end, st, err := agentflow.Drive(ctx, g, a, end)
	var path []string
	for _, step := range st.Steps {
		path = append(path, step.Node+":"+step.Result.Outcome)
		b, _ := json.Marshal(step.Result)
		if step.Node != "gather" {
			fmt.Printf("%s -> %s\n", step.Node, b)
		}
	}
	fmt.Println("path:", strings.Join(path, " "), "nudges:", st.Nudges)
	if err != nil {
		t.Fatalf("after %s: %v", time.Since(began).Round(time.Second), err)
	}
	if !st.Done {
		t.Fatalf("not done: end = %s %s", end.Reason, end.Cause)
	}
	last := st.Steps[len(st.Steps)-1].Result.Data.(map[string]any)
	fmt.Println(last["report"])

	got, err := store.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	hashes, bad := 0, 0
	for _, e := range got.Path(got.Leaf()) {
		if r, ok := e.(*agentsession.ResponseEntry); ok {
			hashes++
			if h, err := got.RequestHashFor(r.Parent, r); err != nil || h != r.RequestHash {
				bad++
			}
		}
	}
	fmt.Printf("session %s: %d request hashes, %d wrong\n", s.ID(), hashes, bad)
}
