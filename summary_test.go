package agentflow

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
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
	if root == nil || len(root.Calls) != 1 || root.Calls[0].Workflow != "middle" || !root.Calls[0].Summed {
		t.Fatalf("root summary = %+v", root)
	}
	// middle's model request is counted in middle, and in top through it;
	// so are the countdown's steps.
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
	if root == nil || len(root.Calls) != 1 || root.Calls[0].Workflow != "thinker" || !root.Calls[0].Summed || root.Usage.Requests != 1 {
		t.Fatalf("root summary = %+v", root)
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
