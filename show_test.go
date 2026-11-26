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
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// bulk marks every row of the large result, so a test can say whether
// any of it reached what a model or a summarizer read.
const bulk = "BULK-ROW"

type bigArgs struct {
	Name string `json:"name"`
	Rows []any  `json:"rows,omitempty"`
}

// bigTool gives gather a large result, and records what report is given.
func bigTool(ran *[]string, got *[]any) agenttool.Tool {
	return agenttool.New("big", "Run a step that makes a lot of data.", func(_ context.Context, a bigArgs) (Result, error) {
		*ran = append(*ran, a.Name)
		if a.Name != "gather" {
			*got = a.Rows
			return Result{Outcome: OK}, nil
		}
		var rows []any
		for i := range 200 {
			rows = append(rows, map[string]any{"id": bulk, "n": i})
		}
		return Result{Outcome: OK, Data: map[string]any{"summary": "200 rows", "rows": rows}}, nil
	})
}

var rowsSchema = &agenttool.Schema{Type: "object", Required: []string{"summary", "rows"}, Properties: []agenttool.Property{
	{Name: "summary", Schema: &agenttool.Schema{Type: "string"}},
	{Name: "rows", Schema: &agenttool.Schema{Type: "array"}},
}}

// showing is gather, whose model sees only what show says of its large
// result; review, a model step that reads the conversation and is handed
// the rows; then report, a program step handed them too.
func showing(show *Show) *Graph {
	return &Graph{Name: "bulk", Nodes: []Node{
		{ID: "gather", Kind: Run, Tool: "big", Args: json.RawMessage(`{"name":"gather"}`),
			Data: map[string]*agenttool.Schema{OK: rowsSchema}, Show: show, Next: map[string]string{OK: "review"}},
		{ID: "review", Kind: Agent, Prompt: "Look at what was gathered.", With: map[string]Value{"rows": From("steps.gather.outputs.rows")},
			Next: map[string]string{OK: "report"}},
		{ID: "report", Kind: Run, Tool: "big", Args: json.RawMessage(`{"name":"report"}`),
			With: map[string]Value{"rows": From("steps.gather.outputs.rows")}},
	}}
}

var reviewed = says{finishTool, `{"step":"review","outcome":"ok"}`}

// sent is a request's input as JSON, every word of it.
func sent(t *testing.T, req openresponses.Request) string {
	t.Helper()
	b, err := json.Marshal(req.Input)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// recordedShow runs g to its end, recorded in a jsonl store.
func recordedShow(t *testing.T, g *Graph, m openresponses.Streamer, ran *[]string, got *[]any, transform func(*session.Recorder) func(context.Context, agentturn.Transcript) (agentturn.Transcript, error)) (*jsonl.Store, string) {
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
	cfg := agentturn.Config{Model: m, ModelName: "w", Tools: []agenttool.Tool{bigTool(ran, got)}}
	if transform != nil {
		cfg.Transform = transform(rec)
	}
	history := recordOf(store, s.ID())
	cfg, err = Apply(g, cfg, history)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	rec.Attach(a)
	end, st, err := Drive(ctx, g, a, start(t, a), history)
	if err != nil || !st.Done {
		t.Fatalf("done = %v, %+v, %v", st.Done, end, err)
	}
	return store, s.ID()
}

func TestAModelSeesOnlyWhatAStepShows(t *testing.T) {
	var ran []string
	var got []any
	m := &cast{script: []says{reviewed, {}}}
	g := showing(&Show{Members: []string{"summary"}})
	store, id := recordedShow(t, g, m, &ran, &got, nil)

	review := m.requests[0]
	in := sent(t, review)
	if strings.Contains(in, bulk) {
		t.Errorf("review was sent the whole result:\n%s", in)
	}
	if !strings.Contains(in, `\"summary\":\"200 rows\"`) || !strings.Contains(in, shownTool) {
		t.Errorf("review was not sent what gather shows:\n%s", in)
	}
	// What a step is handed is its with, whatever was shown.
	if !strings.Contains(review.Instructions, bulk) {
		t.Errorf("review was not handed the rows it names")
	}
	if len(got) != 200 {
		t.Errorf("report was handed %d rows", len(got))
	}
	if want := []string{"gather", "report"}; !slices.Equal(ran, want) {
		t.Errorf("ran = %v", ran)
	}

	// The record holds the whole result, the shown one beside it, and the
	// request that left the whole one out still rebuilds to its hash.
	total, missing, wrong := unverified(t, store, id)
	if total == 0 || len(missing) > 0 || len(wrong) > 0 {
		t.Errorf("%d responses, no hash %v, wrong hash %v", total, missing, wrong)
	}
	s, err := store.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	items := pathItems(s)
	if record, _ := json.Marshal(items); !strings.Contains(string(record), bulk) {
		t.Error("the record lost the whole result")
	}
	runs, err := Runs(items, nil)
	if err != nil || len(runs) != 1 || runs[0].Status != Done || runs[0].Problem != "" {
		t.Errorf("runs = %+v, %v", runs, err)
	}
}

func TestShowNoneShowsTheOutcome(t *testing.T) {
	var ran []string
	var got []any
	m := &cast{script: []says{reviewed, {}}}
	g := showing(&Show{})
	g.node("review").With = nil
	recordedShow(t, g, m, &ran, &got, nil)
	in := sent(t, m.requests[0])
	if strings.Contains(in, bulk) || strings.Contains(in, "200 rows") {
		t.Errorf("review was sent more than the outcome:\n%s", in)
	}
	if !strings.Contains(in, `{\"outcome\":\"ok\"}`) {
		t.Errorf("review was not told gather's outcome:\n%s", in)
	}
}

func TestWithoutShowAModelSeesItAll(t *testing.T) {
	var ran []string
	var got []any
	m := &cast{script: []says{reviewed, {}}}
	g := showing(nil)
	g.node("review").With = nil
	recordedShow(t, g, m, &ran, &got, nil)
	if in := sent(t, m.requests[0]); !strings.Contains(in, bulk) || strings.Contains(in, shownTool) {
		t.Errorf("review was not sent gather's result as it is:\n%s", in)
	}
}

func TestNoSummaryTakesInAWholeShownResult(t *testing.T) {
	var ran []string
	var got []any
	sum := &summarizer{}
	m := &cast{script: []says{{}, reviewed, {}}}
	g := showing(&Show{Members: []string{"summary"}})
	g.node("review").With = nil
	recordedShow(t, g, m, &ran, &got, func(rec *session.Recorder) func(context.Context, agentturn.Transcript) (agentturn.Transcript, error) {
		return compact.NewLocal(sum, append(rec.CompactOptions(), compact.WithOnFold(rec.Fold), compact.WithFilter(SummaryFilter),
			compact.WithBudget(1), compact.WithKeepLast(1), compact.WithMinFold(0))...).Transform
	})
	if len(sum.read) == 0 {
		t.Fatal("nothing was compacted, so this shows nothing")
	}
	noted := false
	for _, read := range sum.read {
		if strings.Contains(read, bulk) {
			t.Errorf("a summarizer read the whole result:\n%s", read)
		}
		noted = noted || strings.Contains(read, "200 rows")
	}
	if !noted {
		t.Errorf("no summarizer read what gather shows: %q", sum.read)
	}
}

// SummaryFilter leaves a shown result out on its own, with no record
// beside it, so a host without History summarises none of it either.
func TestSummaryFilterLeavesOutAShownResult(t *testing.T) {
	whole, _ := json.Marshal(Result{Outcome: OK, Data: map[string]any{"summary": "200 rows", "rows": []any{bulk}}})
	shown, _ := json.Marshal(Result{Outcome: OK, Data: map[string]any{"summary": "200 rows"}})
	args, _ := json.Marshal(shownArgs{Run: "3fa9c2d1e0", Step: "gather", Visit: 1, Of: "flow_3fa9c2d1e0_gather_v1"})
	items := agentturn.Transcript{
		&openresponses.FunctionCall{CallID: "flow_3fa9c2d1e0_gather_v1", Name: "big", Arguments: `{"name":"gather"}`},
		&openresponses.FunctionCallOutput{CallID: "flow_3fa9c2d1e0_gather_v1", Output: openresponses.FunctionCallOutputData{Text: string(whole)}},
		&openresponses.FunctionCall{CallID: "flow_3fa9c2d1e0_gather_v1-shown", Name: shownTool, Arguments: string(args)},
		&openresponses.FunctionCallOutput{CallID: "flow_3fa9c2d1e0_gather_v1-shown", Output: openresponses.FunctionCallOutputData{Text: string(shown)}},
	}
	out, _ := json.Marshal(SummaryFilter(items))
	if strings.Contains(string(out), bulk) || !strings.Contains(string(out), `ran step \"gather\", which shows of its result`) {
		t.Errorf("filtered: %s", out)
	}
	if got, _ := json.Marshal(withoutHidden(openresponses.Items(items))); strings.Contains(string(got), bulk) {
		t.Errorf("a model would be sent: %s", got)
	}
}

func TestShowIsCheckedAtLoad(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Graph)
		want string
	}{
		"an unknown member": {func(g *Graph) { g.node("gather").Show = &Show{Members: []string{"sumary"}} }, `no outcome declares "sumary"; did you mean "summary"?`},
		"a member twice":    {func(g *Graph) { g.node("gather").Show = &Show{Members: []string{"rows", "rows"}} }, `show names "rows" twice`},
		"a model step":      {func(g *Graph) { g.node("review").Show = &Show{} }, `only a script or tool step has show`},
		"no schema": {func(g *Graph) {
			g.node("gather").Data, g.node("review").With, g.node("report").With = nil, nil, nil
		}, `declares no outputs to hold them to`},
	} {
		t.Run(name, func(t *testing.T) {
			g := showing(&Show{Members: []string{"summary"}})
			tc.edit(g)
			if err := g.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
	// Showing nothing of a result needs nothing to hold it to.
	g := showing(&Show{})
	g.node("gather").Data, g.node("review").With, g.node("report").With = nil, nil, nil
	if err := g.Validate(); err != nil {
		t.Errorf("show none with no schema: %v", err)
	}
}

// A process that dies between a result and the flow_shown that shows it
// is resumed with the result still kept from the model: the program shows
// it first, and nothing runs again.
func TestResumeBetweenAResultAndItsShowing(t *testing.T) {
	const mode = "AGENTFLOW_KILL_SHOW"
	g := showing(&Show{Members: []string{"summary"}})
	if dir := os.Getenv(mode); dir != "" {
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
		var got []any
		m := &cast{}
		cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "w", Tools: []agenttool.Tool{bigTool(&ran, &got)},
			// The turn after gather's result, before it is shown.
			BeforeModelCall: func(_ context.Context, req *openresponses.Request) error {
				var resulted, shown bool
				for _, item := range req.Input {
					switch it := item.(type) {
					case *openresponses.FunctionCallOutput:
						resulted = resulted || strings.HasSuffix(it.CallID, "_gather_v1")
					case *openresponses.FunctionCall:
						shown = shown || it.Name == shownTool
					}
				}
				if resulted && !shown {
					_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
					select {}
				}
				return nil
			}}, recordOf(store, s.ID()))
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		rec.Attach(a)
		_, _ = a.Prompt(ctx, openresponses.UserText("go"))
		t.Fatal("it did not die")
	}

	ctx := context.Background()
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestResumeBetweenAResultAndItsShowing$")
	child.Env = append(os.Environ(), mode+"="+dir)
	out, err := child.CombinedOutput()
	raw, readErr := os.ReadFile(dir + "/id")
	if err == nil || readErr != nil {
		t.Fatalf("child: %v, %v\n%s", err, readErr, out)
	}
	id := string(raw)
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Resume(ctx, store, id)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := session.AgentOptions(s, rec.ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	var ran []string
	var got []any
	m := &cast{script: []says{reviewed, {}}}
	history := recordOf(store, id)
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "w", Tools: []agenttool.Tool{bigTool(&ran, &got)}}, history)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg, seed...)
	rec.Attach(a)
	st, err := g.Replay(pathItems(s))
	if err != nil || st.show == nil {
		t.Fatalf("after the kill, no result waits to be shown: %v", err)
	}
	end, err := a.Prompt(ctx, openresponses.UserText("go on"))
	if err != nil {
		t.Fatal(err)
	}
	if end, st, err = Drive(ctx, g, a, end, history); err != nil || !st.Done {
		t.Fatalf("done = %v, %+v, %v", st.Done, end, err)
	}
	if want := []string{"report"}; !slices.Equal(ran, want) {
		t.Errorf("ran = %v, want %v", ran, want)
	}
	if in := sent(t, m.requests[0]); strings.Contains(in, bulk) || !strings.Contains(in, "200 rows") {
		t.Errorf("review after the resume was sent:\n%s", in)
	}
	if len(got) != 200 {
		t.Errorf("report was handed %d rows", len(got))
	}
	total, missing, wrong := unverified(t, store, id)
	if total == 0 || len(missing) > 0 || len(wrong) > 0 {
		t.Errorf("%d responses, no hash %v, wrong hash %v", total, missing, wrong)
	}
}
