package agentflow

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// metered reports that each response used 10 tokens in and 2 out.
type metered struct{ openresponses.Streamer }

func (m metered) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	return m.Streamer.CreateStream(ctx, req, openresponses.EventSinkFunc(func(ev openresponses.StreamEvent) error {
		if done, ok := ev.(*openresponses.ResponseCompletedEvent); ok && done.Response != nil {
			done.Response.Usage = &openresponses.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
		}
		return sink.Send(ev)
	}))
}

// halfCent prices a request of the model "fake" at half a cent.
func halfCent(model string, _ openresponses.Usage) (float64, bool) { return 0.005, model == "fake" }

func TestARunEndsWithASummary(t *testing.T) {
	g := release()
	var ran []string
	cfg, err := Apply(g, agentturn.Config{Model: metered{&fakeModel{outcomes: []string{"clean"}}}, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}}, Prices(halfCent))
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	ends := endings(a.State().Transcript)
	if len(ends) != 1 || ends[0].Summary == nil {
		t.Fatalf("ends = %+v", ends)
	}
	s := ends[0].Summary
	switch {
	case s.Steps != 3 || s.Started == nil || s.Ended == nil || s.Ended.Before(*s.Started):
		t.Errorf("summary = %+v", s)
	case !reflect.DeepEqual(s.Usage, Usage{Requests: 1, InputTokens: 10, OutputTokens: 2, Cost: 0.005}):
		t.Errorf("usage = %+v", s.Usage)
	}
	// The same summary reads back from the record.
	runs, err := Runs(a.State().Transcript, nil)
	if err != nil || runs[0].Summary == nil || !reflect.DeepEqual(runs[0].Summary.Usage, s.Usage) {
		t.Errorf("runs: %v, %+v", err, runs[0].Summary)
	}
}

func TestASummaryTakesInWhatTheRunCalled(t *testing.T) {
	g := linked(t, top(), middle(), countdown())
	var counts []int
	var ran []string
	a := nestedAgent(t, g, metered{&fakeModel{outcomes: []string{OK}}}, []agenttool.Tool{countTool(&counts), scriptTool(&ran)}, Unattended())
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	ends := endings(a.State().Transcript)
	root := ends[len(ends)-1].Summary
	key := ends[len(ends)-1].Instance
	if root == nil || len(root.Calls) != 4 || root.Calls[0].Workflow != "middle" || root.Calls[0].Parent != key || !root.Calls[0].Summed {
		t.Fatalf("root summary = %+v", root)
	}
	// The whole tree is listed, each run under the one that called it:
	// middle, and the countdown from 2, 1 and 0 below it.
	parents := map[string]string{}
	for _, c := range root.Calls {
		parents[c.Run] = c.Parent
	}
	depth := func(run string) (d int) {
		for ; run != key; run = parents[run] {
			d++
		}
		return d
	}
	for i, want := range []int{1, 2, 3, 4} {
		if c := root.Calls[i]; depth(c.Run) != want || i > 0 && c.Workflow != "countdown" {
			t.Errorf("call %d = %+v, depth %d", i, c, depth(c.Run))
		}
	}
	// middle's model request is counted in middle, and in top through it;
	// so are the countdown's steps, which middle's totals already hold.
	middle := root.Calls[0]
	if middle.Usage.Requests != 1 || root.Usage.Requests != 1 || root.Usage.Unpriced == nil || root.Usage.Unpriced[0] != "fake" {
		t.Errorf("middle %+v, root %+v", middle.Usage, root.Usage)
	}
	if want := 2 + middle.Steps; root.Steps != want || middle.Steps < 6 {
		t.Errorf("steps: root %d, want %d; middle %d", root.Steps, want, middle.Steps)
	}
}

func TestASummaryTakesInAChild(t *testing.T) {
	g := linked(t, outer(), thinker(), asker())
	var ran []string
	m := metered{&fakeModel{outcomes: []string{OK}}}
	kids := newKin(m, "fake")
	a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)}, kids.option(), Unattended(), Reply(func(context.Context, Question) (Result, bool) {
		return Result{Outcome: "yes"}, true
	}))
	end, _, err := Drive(t.Context(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	ends := endings(a.State().Transcript)
	root := ends[len(ends)-1].Summary
	if root == nil || len(root.Calls) != 2 || root.Calls[0].Workflow != "thinker" || !root.Calls[0].Summed || root.Usage.Requests != 1 {
		t.Fatalf("root summary = %+v", root)
	}
	// The run two children down is listed too, under the one that called it.
	if c := root.Calls[1]; c.Workflow != "asker" || c.Parent != root.Calls[0].Run || c.Parent == ends[len(ends)-1].Instance {
		t.Errorf("grandchild = %+v", c)
	}
}

func TestASummaryAfterAResumeSaysItIsPartial(t *testing.T) {
	g := release()
	var ran []string
	a := killed(t,
		func() agentturn.Config {
			return apply(t, release(), killingModel{}, probeTool(agenttool.ReplayUnknown, func(string) {}))
		},
		apply(t, g, metered{&fakeModel{outcomes: []string{"clean"}}}, probeTool(agenttool.ReplayUnknown, func(n string) { ran = append(ran, n) })))
	end, err := a.Continue(context.Background())
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	ends := endings(a.State().Transcript)
	s := ends[0].Summary
	if s == nil || s.Usage.Requests != 1 || !strings.Contains(s.Usage.Partial, "started before this process") || !slices.Equal(s.Usage.Unpriced, []string{"fake"}) {
		t.Errorf("summary = %+v", s)
	}
}

// recordedResponses reads a session's responses back as a host does for
// Responses: each with the items on the path before its own.
func recordedResponses(store *jsonl.Store, id string) Option {
	return Responses(func(ctx context.Context) ([]ResponseUsage, error) {
		s, err := store.Open(ctx, id)
		if err != nil {
			return nil, err
		}
		path := s.Path(s.Leaf())
		first, n := map[string]int{}, 0
		for _, e := range path {
			if it, ok := e.(*agentsession.ItemEntry); ok {
				if _, seen := first[it.ResponseID]; it.ResponseID != "" && !seen {
					first[it.ResponseID] = n
				}
				n++
			}
		}
		var out []ResponseUsage
		n = 0
		for _, e := range path {
			switch e := e.(type) {
			case *agentsession.ItemEntry:
				n++
			case *agentsession.ResponseEntry:
				at, ok := first[e.ResponseID]
				if !ok {
					at = n
				}
				r := ResponseUsage{Model: e.Model, At: at}
				if e.Usage != nil {
					r.Usage = *e.Usage
				}
				out = append(out, r)
			}
		}
		return out, nil
	})
}

func TestASummaryFromTheRecordedResponsesIsWhole(t *testing.T) {
	var ran []string
	tool := func() agenttool.Tool { return scriptTool(&ran) }
	a := killedRecord(t,
		func() agentturn.Config {
			return apply(t, release(), metered{&fakeModel{outcomes: []string{"findings"}}}, tool())
		},
		func(store *jsonl.Store, id string) agentturn.Config {
			cfg, err := Apply(release(), agentturn.Config{Model: metered{&fakeModel{outcomes: []string{OK, "clean"}}}, ModelName: "fake", Tools: []agenttool.Tool{tool()}},
				recordOf(store, id), recordedResponses(store, id), Prices(halfCent))
			if err != nil {
				t.Fatal(err)
			}
			return cfg
		},
		// One model request, then the process dies at the question.
		func(t *testing.T, a *agentturn.Agent) {
			question(t, start(t, a))
			die()
		})
	g := release()
	end := reply(t, g, a, &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: a.State().Pending}, "fix")
	if end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	ends := endings(a.State().Transcript)
	s := ends[len(ends)-1].Summary
	// The review before the kill is counted, with the fix and the review
	// after it, and nothing is partial.
	if s == nil || !reflect.DeepEqual(s.Usage, Usage{Requests: 3, InputTokens: 30, OutputTokens: 6, Cost: 0.015}) {
		t.Errorf("summary = %+v", s)
	}
}

func TestRecordedResponsesCountForTheRunTheyServed(t *testing.T) {
	ctx := context.Background()
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
	if err != nil {
		t.Fatal(err)
	}
	g := linked(t, top(), middle(), countdown())
	var counts []int
	var ran []string
	a := nestedAgent(t, g, metered{&fakeModel{outcomes: []string{OK}}}, []agenttool.Tool{countTool(&counts), scriptTool(&ran)},
		Unattended(), recordOf(store, s.ID()), recordedResponses(store, s.ID()))
	rec.Attach(a)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	ends := endings(a.State().Transcript)
	root := ends[len(ends)-1].Summary
	// The one request was middle's, counted there, and in top only through
	// middle; nothing is partial.
	if root == nil || root.Usage.Requests != 1 || root.Usage.Partial != "" || root.Calls[0].Workflow != "middle" || root.Calls[0].Usage.Requests != 1 {
		t.Fatalf("root summary = %+v", root)
	}
	for _, c := range root.Calls[1:] {
		if c.Usage.Requests != 0 || c.Usage.Partial != "" {
			t.Errorf("countdown %s = %+v", c.Run, c.Usage)
		}
	}
}
