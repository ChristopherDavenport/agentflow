package agentflow

import (
	"context"
	"errors"
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
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// recordOf is a History over a stored session.
func recordOf(store *jsonl.Store, id string) Option {
	return History(func(ctx context.Context) (agentturn.Transcript, error) {
		s, err := store.Open(ctx, id)
		if err != nil {
			return nil, err
		}
		return pathItems(s), nil
	})
}

// summarizer folds everything into one word, and keeps what it was shown.
type summarizer struct{ read []string }

func (m *summarizer) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.read = append(m.read, strings.Join(texts(req.Input), "\n"))
	return say(sink, req.Model, "SUMMARY")
}

// unverified counts the responses on the path whose request does not
// rebuild: with no hash, or another one.
func unverified(t *testing.T, store *jsonl.Store, id string) (total int, missing, wrong []string) {
	t.Helper()
	s, err := store.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range s.Path(s.Leaf()) {
		r, ok := e.(*agentsession.ResponseEntry)
		if !ok {
			continue
		}
		total++
		if r.RequestHash == "" {
			missing = append(missing, r.Model)
		} else if h, err := s.RequestHashFor(r.Parent, r); err != nil || h != r.RequestHash {
			wrong = append(wrong, r.Model)
		}
	}
	return total, missing, wrong
}

// noted is a graph whose later steps need a value an earlier one made.
func noted(freshFix bool) *Graph {
	g := judged()
	g.node("review").Data = map[string]*agenttool.Schema{"findings": {Type: "object", Required: []string{"note"},
		Properties: []agenttool.Property{{Name: "note", Schema: &agenttool.Schema{Type: "string"}}}}}
	g.node("fix").With = map[string]Value{"note": From("steps.review.outputs.note")}
	if freshFix {
		g.node("fix").Context = ContextFresh
	}
	return g
}

func TestCompactionDuringAWorkflow(t *testing.T) {
	for name, fresh := range map[string]bool{"a step that reads the conversation": false, "a fresh step": true} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, err := jsonl.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
			if err != nil {
				t.Fatal(err)
			}
			g, ran, sum := noted(fresh), []string{}, &summarizer{}
			m := &cast{script: []says{
				{finishTool, `{"step":"review","outcome":"findings","data":{"note":"param-7f3"}}`},
				unseen("report"),
				fixDone,
				{finishTool, `{"step":"review","outcome":"clean"}`},
			}}
			// A fold as hard as it can be made: everything but the last.
			fold := compact.NewLocal(sum, append(rec.CompactOptions(), compact.WithOnFold(rec.Fold), compact.WithFilter(SummaryFilter), compact.WithBudget(1), compact.WithKeepLast(1), compact.WithMinFold(0))...)
			history := recordOf(store, s.ID())
			cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "w", Transform: fold.Transform, Tools: []agenttool.Tool{scriptTool(&ran)}},
				history, Delegate(Shadow))
			if err != nil {
				t.Fatal(err)
			}
			a := agentturn.New(cfg)
			rec.Attach(a)
			end := start(t, a)
			ans, err := g.Answer(end.Pending[0], Result{Outcome: "fix"})
			if err != nil {
				t.Fatal(err)
			}
			if end, err = a.Resume(ctx, ans); err != nil {
				t.Fatal(err)
			}
			end, st, err := Drive(ctx, g, a, end, history)
			if err != nil || !st.Done {
				t.Fatalf("done = %v, %s %v", st.Done, end.Reason, err)
			}
			if want := []string{"gather:ok", "review:findings", "triage:fix", "fix:ok", "gather:ok", "review:clean", "report:ok"}; !slices.Equal(steps(st), want) {
				t.Errorf("path = %v", steps(st))
			}
			if want := []string{"gather", "gather", "report"}; !slices.Equal(ran, want) {
				t.Errorf("ran = %v", ran)
			}
			if len(sum.read) == 0 {
				t.Fatal("nothing was compacted, so this shows nothing")
			}
			// The step is handed the value, and the old items are not
			// brought back to hand it over.
			fix := m.requests[2]
			if !strings.Contains(fix.Instructions, `"note":"param-7f3"`) {
				t.Errorf("fix was not given its input: %s", fix.Instructions)
			}
			if in := strings.Join(texts(fix.Input), "\n"); strings.Contains(in, "review this PR") || strings.Contains(in, "call "+finishTool) {
				t.Errorf("fix was sent compacted items again: %q", in)
			}
			for _, read := range sum.read {
				for _, kept := range []string{secret, "call " + judgeTool, "call " + startTool} {
					if strings.Contains(read, kept) {
						t.Errorf("the summarizer was shown %q:\n%s", kept, read)
					}
				}
			}
			// A summarized notice reads, to a later step, as something the
			// person asked for that is still under way.
			for i, read := range sum.read {
				for _, notice := range []string{beginPrefix, nudgePrefix, noVerdict, invocationPrefix} {
					if strings.Contains(read, notice) {
						t.Errorf("summary %d was shown a workflow notice:\n%s", i, read)
					}
				}
			}
			total, missing, wrong := unverified(t, store, s.ID())
			t.Logf("%d responses, no hash %v, wrong hash %v, %d folds", total, missing, wrong, len(sum.read))
			if len(wrong) > 0 {
				t.Errorf("hashes that do not rebuild: %v", wrong)
			}
		})
	}
}

// dies kills its own process when asked for its nth answer.
type dies struct {
	cast
	after int
}

func (m *dies) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if len(m.requests) == m.after {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	return m.cast.CreateStream(ctx, req, sink)
}

func TestResumeAfterKillInAModelStep(t *testing.T) {
	const mode = "AGENTFLOW_KILL"
	graph := func(fresh bool) *Graph {
		g := noted(fresh)
		g.node("triage").Unattended, g.node("triage").Judge = "fix", nil
		return g
	}
	// The process dies while the model works on fix.
	if v := os.Getenv(mode); v != "" {
		fresh, dir, _ := strings.Cut(v, " ")
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
		var ran []string
		m := &dies{cast: cast{script: []says{{finishTool, `{"step":"review","outcome":"findings","data":{"note":"param-7f3"}}`}}}, after: 1}
		cfg, err := Apply(graph(fresh == "true"), agentturn.Config{Model: m, ModelName: "w", Tools: []agenttool.Tool{scriptTool(&ran)}},
			Unattended(), recordOf(store, s.ID()))
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		rec.Attach(a)
		_, _ = a.Prompt(ctx, openresponses.UserText("go"))
		t.Fatal("it did not die")
	}
	for _, fresh := range []string{"false", "true"} {
		t.Run("fresh="+fresh, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			child := exec.Command(os.Args[0], "-test.run=^TestResumeAfterKillInAModelStep$")
			child.Env = append(os.Environ(), mode+"="+fresh+" "+dir)
			out, err := child.CombinedOutput()
			id, readErr := os.ReadFile(dir + "/id")
			if err == nil || readErr != nil {
				t.Fatalf("child: %v, %v\n%s", err, readErr, out)
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
			g := graph(fresh == "true")
			m := &cast{script: []says{fixDone, {finishTool, `{"step":"review","outcome":"clean"}`}}}
			cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "w", Tools: []agenttool.Tool{scriptTool(&ran)}},
				Unattended(), recordOf(store, string(id)))
			if err != nil {
				t.Fatal(err)
			}
			a := agentturn.New(cfg, seed...)
			rec.Attach(a)
			t.Logf("resumed with %d of the items", len(a.State().Transcript))
			end, err := a.Prompt(ctx, openresponses.UserText("go on"))
			if err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("%v, %+v", err, end)
			}
			// What ran before the kill did not run again.
			if want := []string{"gather", "report"}; !slices.Equal(ran, want) {
				t.Errorf("ran = %v, want %v", ran, want)
			}
			if !strings.Contains(m.requests[0].Instructions, `"note":"param-7f3"`) {
				t.Errorf("fix was not given its input: %s", m.requests[0].Instructions)
			}
			total, missing, wrong := unverified(t, store, string(id))
			t.Logf("%d responses, no hash %v, wrong hash %v; the model was sent %d items", total, missing, wrong, len(m.requests[0].Input))
			if len(wrong) > 0 {
				t.Errorf("hashes that do not rebuild: %v", wrong)
			}
			got, err := store.Open(ctx, string(id))
			if err != nil {
				t.Fatal(err)
			}
			runs, err := Instances(pathItems(got))
			if err != nil || len(runs) != 1 || runs[0].Status != Done || runs[0].Problem != "" {
				t.Errorf("runs = %+v, %v", runs, err)
			}
		})
	}
}

func TestARecordThatIsBehindIsRefused(t *testing.T) {
	var ran []string
	var frozen agentturn.Transcript
	cfg, err := Apply(release(), agentturn.Config{Model: &cast{script: []says{clean}}, ModelName: "w", Tools: []agenttool.Tool{scriptTool(&ran)}},
		History(func(context.Context) (agentturn.Transcript, error) { return frozen, nil }))
	if err != nil {
		t.Fatal(err)
	}
	// A recorder that never records: after one step the transcript has a
	// call the record does not.
	end := run(agentturn.New(cfg))
	if !errors.Is(end.Err, ErrRecordBehind) || len(ran) != 0 {
		t.Errorf("err = %v, ran = %v", end.Err, ran)
	}
}

// A run that stalls inside a model step leaves a session whose rebuilt
// transcript has lost items, to an omission, a fold or both.
func TestResumeWhenTheTranscriptHasLostItems(t *testing.T) {
	for _, c := range []struct {
		name             string
		fresh, compacted bool
	}{
		{"omitted", false, false}, {"omitted, fresh step", true, false},
		{"compacted", false, true}, {"compacted, fresh step", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := jsonl.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
			if err != nil {
				t.Fatal(err)
			}
			g := noted(c.fresh)
			g.node("triage").Unattended, g.node("triage").Judge = "fix", nil
			var ran []string
			agent := func(rec *session.Recorder, id string, m *cast, seed ...agentturn.Option) *agentturn.Agent {
				base := agentturn.Config{Model: m, ModelName: "w", Tools: []agenttool.Tool{scriptTool(&ran)}}
				if c.compacted {
					base.Transform = compact.NewLocal(&summarizer{}, append(rec.CompactOptions(),
						compact.WithOnFold(rec.Fold), compact.WithFilter(SummaryFilter), compact.WithBudget(1), compact.WithKeepLast(1), compact.WithMinFold(0))...).Transform
				}
				cfg, err := Apply(g, base, Unattended(), recordOf(store, id))
				if err != nil {
					t.Fatal(err)
				}
				a := agentturn.New(cfg, seed...)
				rec.Attach(a)
				return a
			}
			first := agent(rec, s.ID(), &cast{script: []says{
				{finishTool, `{"step":"review","outcome":"findings","data":{"note":"param-7f3"}}`}, {}}})
			if end := run(first); end.Reason != agentturn.ReasonDone {
				t.Fatalf("first: %s %v", end.Reason, end.Err)
			}

			rec2, s2, err := session.Resume(ctx, store, s.ID())
			if err != nil {
				t.Fatal(err)
			}
			seed, err := session.AgentOptions(s2, rec2.ReadOptions()...)
			if err != nil {
				t.Fatal(err)
			}
			whole := len(pathItems(s2))
			ran = nil
			m := &cast{script: []says{fixDone, {finishTool, `{"step":"review","outcome":"clean"}`}}}
			b := agent(rec2, s.ID(), m, seed...)
			if had := len(b.State().Transcript); had >= whole {
				t.Fatalf("resumed with %d of %d items, so this shows nothing", had, whole)
			} else {
				t.Logf("resumed with %d of %d items", had, whole)
			}
			if end := run(b); end.Reason != agentturn.ReasonDone {
				t.Fatalf("resumed: %s %v", end.Reason, end.Err)
			}
			if want := []string{"gather", "report"}; !slices.Equal(ran, want) {
				t.Errorf("ran = %v, want %v", ran, want)
			}
			if !strings.Contains(m.requests[0].Instructions, `"note":"param-7f3"`) {
				t.Errorf("fix was not given its input: %s", m.requests[0].Instructions)
			}
			total, missing, wrong := unverified(t, store, s.ID())
			t.Logf("%d responses, no hash %v, wrong %v", total, missing, wrong)
			if len(wrong) > 0 {
				t.Errorf("hashes that do not rebuild: %v", wrong)
			}
			got, err := store.Open(ctx, s.ID())
			if err != nil {
				t.Fatal(err)
			}
			runs, err := Instances(pathItems(got))
			if err != nil || len(runs) != 1 || runs[0].Status != Done || runs[0].Problem != "" {
				t.Errorf("runs = %+v, %v", runs, err)
			}
		})
	}
}

func TestSummaryFilterDropsOnlyWhatTheWorkflowSays(t *testing.T) {
	inv, err := release().Invocation(nil)
	if err != nil {
		t.Fatal(err)
	}
	in := agentturn.Transcript{
		inv,
		openresponses.UserText(beginning("fix") + " Do this step now."),
		openresponses.UserText(nudgePrefix + " Text is not read."),
		openresponses.UserText(noVerdict),
		openresponses.UserText("please fix it"),
		openresponses.DeveloperText("Be brief."),
		openresponses.UserText("quoting " + beginPrefix),
		&openresponses.FunctionCall{CallID: "flow_pr_1_gather_v1", Name: "script"},
	}
	want := []string{"please fix it", "Be brief.", "quoting " + beginPrefix, "call script"}
	if got := texts(openresponses.Items(SummaryFilter(in))); !slices.Equal(got, want) {
		t.Errorf("kept %q, want %q", got, want)
	}
	if len(in) != 8 || texts(openresponses.Items(in))[4] != "please fix it" {
		t.Error("the input was changed")
	}
}
