package agentflow

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// The live probe asks real models to take the Agent turn of a two-way
// review. Set AGENTFLOW_LIVE_URL and AGENTFLOW_LIVE_MODEL, and
// AGENTFLOW_LIVE_KEY where the server wants one.

const buggy = `func Average(xs []int) int {
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return sum / len(xs) // new: the empty-slice check was removed
}`

const fine = `// Sum adds the values.
func Sum(xs []int) int {
	sum := 0
	for _, x := range xs {
		sum += x
	}
	return sum
}`

func liveGraph() *Graph {
	return &Graph{Name: "pr", Nodes: []Node{
		{ID: "gather", Kind: Run, Tool: "gather", Next: map[string]string{OK: "review"}},
		{ID: "review", Kind: Agent, Outcomes: []string{"clean", "findings"},
			Prompt: "Review the code the gather step returned. Choose findings if it can crash or is wrong, else clean."},
	}}
}

func TestLive(t *testing.T) {
	url, model := os.Getenv("AGENTFLOW_LIVE_URL"), os.Getenv("AGENTFLOW_LIVE_MODEL")
	if url == "" || model == "" {
		t.Skip("set AGENTFLOW_LIVE_URL and AGENTFLOW_LIVE_MODEL")
	}
	var opts []openresponses.ClientOption
	if k := os.Getenv("AGENTFLOW_LIVE_KEY"); k != "" {
		opts = append(opts, openresponses.WithAPIKey(k))
	}
	adapter := openresponses.NewClient(url, opts...).AsAdapter()
	trials, _ := strconv.Atoi(os.Getenv("AGENTFLOW_LIVE_TRIALS"))
	trials = max(trials, 1)

	mode := os.Getenv("AGENTFLOW_LIVE_MODE")
	var nudged, finished, right, silent, errored, rejected, usageIn, usageOut int
	for i := range trials {
		code, want := buggy, "findings"
		if i%2 == 1 {
			code, want = fine, "clean"
		}
		gather := agenttool.New("gather", "Collect the code under review.", func(context.Context, struct{}) (Result, error) {
			return Result{Outcome: OK, Data: map[string]any{"code": code}}, nil
		})
		g := liveGraph()
		base := agentturn.Config{Model: adapter, ModelName: model, Tools: []agenttool.Tool{gather}, MaxTurns: 6}
		limit := 2000
		base.Request.MaxOutputTokens = &limit
		if mode == "force" {
			base.Request.ToolChoice = openresponses.ToolChoice{Mode: openresponses.ToolChoiceRequired}
		}
		cfg, err := Apply(g, base)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		a := agentturn.New(cfg)
		unsub := a.Subscribe(func(_ context.Context, ev agentturn.Event) error {
			if e, ok := ev.(*agentturn.TurnEnd); ok && e.Response != nil && e.Response.Usage != nil {
				usageIn += e.Response.Usage.InputTokens
				usageOut += e.Response.Usage.OutputTokens
			}
			return nil
		})
		began := time.Now()
		end, err := a.Prompt(ctx, openresponses.UserText("Run the review workflow."))
		if mode == "nudge" && err == nil && end.Err == nil {
			if st, serr := g.Replay(a.State().Transcript); serr == nil && st.Node != nil {
				nudged++
				end, err = a.Prompt(ctx, openresponses.UserText("The step is not finished. Call the flow_finish tool now; text is not read."))
			}
		}
		unsub()
		cancel()
		if end == nil || end.Err != nil {
			errored++
			if end != nil {
				err = end.Err
			}
			t.Logf("trial %d: error: %v", i, err)
			continue
		}
		st, err := g.Replay(a.State().Transcript)
		if err != nil {
			t.Fatal(err)
		}
		var calls []string
		for _, item := range a.State().Transcript {
			switch it := item.(type) {
			case *openresponses.FunctionCall:
				if it.Name != "gather" {
					calls = append(calls, it.Name+" "+it.Arguments)
				}
			case *openresponses.FunctionCallOutput:
				if s := it.Output.String(); !json.Valid([]byte(s)) {
					rejected++
					calls = append(calls, "error: "+s)
				}
			}
		}
		answer, _ := end.Answer()
		outcome := ""
		switch {
		case len(st.Steps) == 2:
			finished++
			outcome = st.Steps[1].Result.Outcome
			if outcome == want {
				right++
			}
		default:
			silent++
		}
		t.Logf("trial %d: want=%s got=%q end=%s in %s\n  calls: %s\n  text: %.200s", i, want, outcome, end.Reason,
			time.Since(began).Round(time.Second), strings.Join(calls, " | "), strings.ReplaceAll(answer, "\n", " "))
	}
	t.Logf("%s: mode=%s nudged=%d trials=%d finished=%d right=%d unfinished=%d errors=%d rejected=%d tokens in=%d out=%d",
		model, mode, nudged, trials, finished, right, silent, errored, rejected, usageIn, usageOut)
}
