package agentflow

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

const childEnv = "AGENTFLOW_SPIKE_STORE"

// askAndDie runs the workflow in a recorded session until it asks its
// question, writes the session ID and kills its own process, releasing
// nothing.
func askAndDie(t *testing.T, dir string) {
	ctx := context.Background()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	a := newAgent(t, release(), &fakeModel{outcomes: []string{"findings"}}, &ran)
	rec.Attach(a)
	if end := start(t, a); end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("reason = %s, err = %v", end.Reason, end.Err)
	}
	if err := store.Sync(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/id", []byte(s.ID()), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	select {}
}

func TestResumeAfterKill(t *testing.T) {
	if dir := os.Getenv(childEnv); dir != "" {
		askAndDie(t, dir)
		return
	}
	ctx := context.Background()
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestResumeAfterKill$")
	child.Env = append(os.Environ(), childEnv+"="+dir)
	out, err := child.CombinedOutput()
	id, readErr := os.ReadFile(dir + "/id")
	if err == nil || readErr != nil {
		t.Fatalf("child: err = %v, read = %v\n%s", err, readErr, out)
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
	var ran []string
	g, m := release(), &fakeModel{outcomes: []string{"ok", "clean"}}
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg, seed...)
	rec.Attach(a)

	pending := a.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	var q Question
	if err := json.Unmarshal([]byte(pending[0].Call.Arguments), &q); err != nil || q.Step != "triage" {
		t.Fatalf("question = %+v, %v", q, err)
	}
	ans, err := Answer(pending[0], Result{Outcome: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	end, err := a.Resume(ctx, ans)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("resume: %v, %+v", err, end)
	}

	// Only the steps after the question ran in this process.
	if want := []string{"gather", "report"}; !slices.Equal(ran, want) {
		t.Errorf("ran = %v, want %v", ran, want)
	}
	want := []string{"gather:ok", "review:findings", "triage:fix", "fix:ok", "gather:ok", "review:clean", "report:ok"}
	if got := path(t, g, a); !slices.Equal(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}
	checkRecord(t, store, string(id))
	if got, want := verdicts(t, store, string(id)), []string{"hold by policy", "proceed by human"}; !slices.Equal(got, want) {
		t.Errorf("verdicts = %v, want %v", got, want)
	}
}

// checkRecord holds the session to what it claims: every request hash
// reproduces, and each response says who took the turn.
func checkRecord(t *testing.T, store *jsonl.Store, id string) {
	t.Helper()
	s, err := store.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var models []string
	hashes := 0
	for _, e := range s.Path(s.Leaf()) {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(line), `"type":"run"`) || strings.Contains(string(line), `"type":"decision"`) {
			t.Logf("%s", line)
		}
		r, ok := e.(*agentsession.ResponseEntry)
		if !ok {
			continue
		}
		models = append(models, r.Model)
		got, err := s.RequestHashFor(r.Parent, r)
		if err != nil {
			t.Fatal(err)
		}
		if r.RequestHash == "" || r.RequestHash != got {
			t.Errorf("response %s (%s): request hash %q, rebuilt %q", r.ResponseID, r.Model, r.RequestHash, got)
		}
		hashes++
	}
	const p, f = "agentflow/pr", "fake"
	// gather, review, triage | fix, gather, review, report, summary
	want := []string{p, f, p, f, p, f, p, p}
	if !slices.Equal(models, want) {
		t.Errorf("models = %v, want %v", models, want)
	}
	t.Logf("%d request hashes verified", hashes)
}

// verdicts lists the decisions on the path.
func verdicts(t *testing.T, store *jsonl.Store, id string) []string {
	t.Helper()
	s, err := store.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range s.Path(s.Leaf()) {
		if d, ok := e.(*agentsession.DecisionEntry); ok {
			out = append(out, string(d.Verdict)+" by "+string(d.By))
		}
	}
	return out
}

var _ openresponses.Streamer = (*fakeModel)(nil)
