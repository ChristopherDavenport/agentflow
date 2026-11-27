package agentflow_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/summary"
	"github.com/ChristopherDavenport/openresponses"
)

// The runs these tests sum up are made in sums_test.go, with this
// package's test hosts. A summary reads them as a host does.

// halfCent prices a request of the model "fake" at half a cent.
func halfCent(model string, _ openresponses.Usage) (float64, bool) { return 0.005, model == "fake" }

// of sums up the i'th run of s, priced by price.
func of(s agentflow.Sums, i int, price func(string, openresponses.Usage) (float64, bool)) *summary.Summary {
	o := summary.Options{Price: price}
	if s.Responses != nil {
		o.Responses = func(spawn string) ([]summary.Response, error) {
			rs, err := s.Responses(spawn)
			out := make([]summary.Response, len(rs))
			for i, r := range rs {
				out[i] = summary.Response{Model: r.Model, Usage: r.Usage, At: r.At}
			}
			return out, err
		}
	}
	return summary.Of(s.Runs[i], o)
}

func TestARunIsSummedUpFromItsRecord(t *testing.T) {
	sums := agentflow.SumsOfARun(t)
	s := of(sums, 0, halfCent)
	switch {
	case s.Steps != 3 || s.Started == nil || s.Ended == nil || s.Ended.Before(*s.Started):
		t.Errorf("summary = %+v", s)
	// What the requests used is read from the host's responses, and this
	// host gives none.
	case s.Usage.Requests != 0 || !strings.Contains(s.Usage.Partial, "no Responses were given"):
		t.Errorf("usage = %+v", s.Usage)
	}
	// Asked again, it is the same: nothing of it was written down.
	if again := of(sums, 0, halfCent); !reflect.DeepEqual(again, s) {
		t.Errorf("a second summary = %+v, the first %+v", again, s)
	}
}

func TestASummaryTakesInWhatTheRunCalled(t *testing.T) {
	sums := agentflow.SumsOfANest(t)
	root, key := of(sums, 0, nil), sums.Runs[0].ID
	if len(root.Calls) != 4 || root.Calls[0].Workflow != "middle" || root.Calls[0].Parent != key || !root.Calls[0].Read {
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
		if c := root.Calls[i]; depth(c.Run) != want || i > 0 && c.Workflow != "countdown" || c.Status != agentflow.Done {
			t.Errorf("call %d = %+v, depth %d", i, c, depth(c.Run))
		}
	}
	// The countdown's steps are counted in middle, and in top through it.
	middle := root.Calls[0]
	if want := 2 + middle.Steps; root.Steps != want || middle.Steps < 6 {
		t.Errorf("steps: root %d, want %d; middle %d", root.Steps, want, middle.Steps)
	}
	// The steps are the ones the tree's bound counted: no step here is
	// person-paced.
	if root.Steps != sums.Runs[0].State.Entries {
		t.Errorf("steps = %d, and the tree entered %d", root.Steps, sums.Runs[0].State.Entries)
	}
}

func TestASummaryTakesInFreshChildren(t *testing.T) {
	root := of(agentflow.SumsOfFreshChildren(t, true), 0, nil)
	if len(root.Calls) != 2 || root.Calls[0].Workflow != "thinker" || !root.Calls[0].Read {
		t.Fatalf("root summary = %+v", root)
	}
	// The run two children down is listed too, under the one that called it.
	if c := root.Calls[1]; c.Workflow != "asker" || c.Parent != root.Calls[0].Run || !c.Read {
		t.Errorf("grandchild = %+v", c)
	}
}

func TestAChildNotReadIsSummedUpFromItsCall(t *testing.T) {
	read, unread := agentflow.SumsOfFreshChildren(t, true), agentflow.SumsOfFreshChildren(t, false)
	whole, short := of(read, 0, nil), of(unread, 0, nil)
	// The child's steps are what its call's result says its tree spent,
	// the same count here, and the run below it is not listed.
	if short.Steps != whole.Steps || len(short.Calls) != 1 || short.Calls[0].Read || short.Calls[0].Status != agentflow.Done {
		t.Errorf("summary = %+v, and read whole %+v", short, whole)
	}
}

func TestASummaryIsWholeAfterAKill(t *testing.T) {
	sums := agentflow.SumsAfterAKill(t)
	s := of(sums, 0, halfCent)
	// The review before the kill is counted, with the fix and the review
	// after it, and nothing is partial.
	if !reflect.DeepEqual(s.Usage, summary.Usage{Requests: 3, InputTokens: 30, OutputTokens: 6, Cost: 0.015}) {
		t.Errorf("summary = %+v", s)
	}
	// The steps before the kill are counted with those after it.
	if s.Started == nil || s.Ended == nil || s.Steps != sums.Runs[0].State.Entries || s.Steps != 7 {
		t.Errorf("summary = %+v", s)
	}
}

func TestResponsesCountForTheRunTheyServed(t *testing.T) {
	root := of(agentflow.SumsOfARecordedNest(t), 0, nil)
	// The one request was middle's, counted there, and in top only through
	// middle; nothing is partial.
	if root.Usage.Requests != 1 || root.Usage.Partial != "" || root.Calls[0].Workflow != "middle" || root.Calls[0].Usage.Requests != 1 {
		t.Fatalf("root summary = %+v", root)
	}
	if !slicesEqual(root.Usage.Unpriced, []string{"fake"}) {
		t.Errorf("unpriced = %v", root.Usage.Unpriced)
	}
	for _, c := range root.Calls[1:] {
		if c.Usage.Requests != 0 || c.Usage.Partial != "" {
			t.Errorf("countdown %s = %+v", c.Run, c.Usage)
		}
	}
}

func TestAKeptChildsVisitsAreSummedUpApart(t *testing.T) {
	sums := agentflow.SumsOfAKeptChild(t)
	root := of(sums, 0, nil)
	// Two drafts and a polish, each a run of the one kept session, each
	// with its own one request.
	if len(root.Calls) != 3 || root.Usage.Requests != 3 || root.Usage.Partial != "" {
		t.Fatalf("root summary = %+v", root)
	}
	spawn := sums.Runs[0].Children[0].Spawn
	for i, c := range root.Calls {
		if c.Usage.Requests != 1 || c.Steps != 1 || !c.Read || sums.Runs[0].Children[i].Spawn != spawn {
			t.Errorf("visit %d = %+v", i, c)
		}
	}
	// The caller entered its own five steps, and each visit one.
	if root.Steps != 5+3 {
		t.Errorf("steps = %d", root.Steps)
	}
}

func TestAnAbandonedRunIsSummedUpToWhereItStopped(t *testing.T) {
	sums := agentflow.SumsOfAnAbandonedNest(t)
	if len(sums.Runs) != 2 {
		t.Fatalf("runs = %d", len(sums.Runs))
	}
	first, second := of(sums, 0, nil), of(sums, 1, nil)
	if first.Ended == nil || first.Calls[0].Status != agentflow.Abandoned || first.Steps == 0 {
		t.Errorf("the abandoned run = %+v", first)
	}
	// The run under way has no end yet, and is summed up so far.
	if second.Ended != nil || second.Started == nil || second.Calls[0].Status != agentflow.Running || second.Steps != first.Steps {
		t.Errorf("the run under way = %+v, the abandoned %+v", second, first)
	}
}

func TestAnOverriddenRunListsTheChildItSetAside(t *testing.T) {
	for _, read := range []bool{true, false} {
		s := of(agentflow.SumsOfAnOverride(t, read), 0, nil)
		// The call, and the child's question and cleanup.
		if s.Steps != 1+2 || len(s.Calls) != 1 || s.Calls[0].Status != agentflow.Abandoned || s.Calls[0].Steps != 2 || s.Calls[0].Read != read {
			t.Errorf("read %v: summary = %+v", read, s)
		}
	}
}

func slicesEqual(a, b []string) bool { return reflect.DeepEqual(a, b) }
