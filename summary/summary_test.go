package summary

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/openresponses"
)

func used(model string, at int) Response {
	return Response{Model: model, At: at, Usage: openresponses.Usage{InputTokens: 10, OutputTokens: 2}}
}

// run is a run read from a session, with its span and its own steps.
func run(id string, start, end, stop, entered int, kids ...*agentflow.Instance) *agentflow.Instance {
	return &agentflow.Instance{Workflow: "w", ID: id, Start: start, End: end, Stop: stop, Status: agentflow.Done,
		State: agentflow.State{Entered: entered}, Children: kids}
}

func TestOwnUsageIsWhatFallsInsideTheRunsSpan(t *testing.T) {
	responses := []Response{
		used("m", 0),           // before the run
		used("agentflow/w", 2), // the program's start
		used("m", 4),           // the run's
		used("other", 6),       // the run's, and unpriced
		used("agentflow/w", 8), // the program's end
		used("m", 10),          // after its end
		used("m", 14),          // past the next run's start
	}
	price := func(model string, _ openresponses.Usage) (float64, bool) { return 0.25, model == "m" }
	for _, tc := range []struct {
		name             string
		start, end, stop int
		want             Usage
	}{
		{"ended", 2, 8, 12, Usage{Requests: 2, InputTokens: 20, OutputTokens: 4, Cost: 0.25, Unpriced: []string{"other"}}},
		{"under way, to the session's next run", 2, -1, 12, Usage{Requests: 3, InputTokens: 30, OutputTokens: 6, Cost: 0.5, Unpriced: []string{"other"}}},
		{"under way, to the record's end", 2, -1, 20, Usage{Requests: 4, InputTokens: 40, OutputTokens: 8, Cost: 0.75, Unpriced: []string{"other"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Of(run("a", tc.start, tc.end, tc.stop, 3), Options{
				Responses: func(spawn string) ([]Response, error) {
					if spawn != "" {
						t.Errorf("read spawn %q for the run given", spawn)
					}
					return responses, nil
				},
				Price: price,
			})
			if !reflect.DeepEqual(s.Usage, tc.want) || s.Steps != 3 {
				t.Errorf("summary = %+v, want usage %+v", s, tc.want)
			}
		})
	}
}

func TestATreesTotalsTakeInEverySession(t *testing.T) {
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	grandchild := run("c", 0, 5, 6, 2)
	grandchild.Spawn, grandchild.Parent = "c", "b"
	child := run("b", 0, 9, 10, 3, grandchild)
	child.Spawn, child.Parent, child.Outcome = "b", "a", "ok"
	// A second visit of the same kept session.
	visit := run("b2", 10, 15, 16, 1)
	visit.Spawn, visit.Parent = "b", "a"
	// One whose session was not read: known from its call's result.
	unread := &agentflow.Instance{Workflow: "w", ID: "d", Parent: "a", Spawn: "d", Start: -1, End: -1, Status: agentflow.Abandoned,
		State: agentflow.State{Entries: 4}}
	root := run("a", 1, 30, 31, 5, child, visit, unread)
	root.Started, root.Ended = at, at.Add(time.Minute)

	reads := map[string]int{}
	s := Of(root, Options{Responses: func(spawn string) ([]Response, error) {
		reads[spawn]++
		switch spawn {
		case "":
			return []Response{used("m", 3)}, nil
		case "b":
			return []Response{used("m", 2), used("m", 4), used("m", 12)}, nil
		case "c":
			return nil, errors.New("gone")
		}
		return nil, errors.New("no session " + spawn)
	}})
	if s.Started == nil || !s.Started.Equal(at) || s.Ended == nil || !s.Ended.Equal(at.Add(time.Minute)) {
		t.Errorf("times = %v, %v", s.Started, s.Ended)
	}
	// Its own 5, the child's 3 and its child's 2, the visit's 1, and the
	// 4 the unread child's call says it spent.
	if s.Steps != 5+3+2+1+4 {
		t.Errorf("steps = %d", s.Steps)
	}
	if s.Usage.Requests != 1+2+1 || !strings.Contains(s.Usage.Partial, "could not be read: gone") {
		t.Errorf("usage = %+v", s.Usage)
	}
	want := []Called{
		{Workflow: "w", Run: "b", Parent: "a", Status: agentflow.Done, Outcome: "ok", Read: true, Steps: 5,
			Usage: Usage{Requests: 2, InputTokens: 20, OutputTokens: 4, Unpriced: []string{"m"}, Partial: "the session's responses could not be read: gone"}},
		{Workflow: "w", Run: "c", Parent: "b", Status: agentflow.Done, Read: true, Steps: 2,
			Usage: Usage{Partial: "the session's responses could not be read: gone"}},
		{Workflow: "w", Run: "b2", Parent: "a", Status: agentflow.Done, Read: true, Steps: 1,
			Usage: Usage{Requests: 1, InputTokens: 10, OutputTokens: 2, Unpriced: []string{"m"}}},
		{Workflow: "w", Run: "d", Parent: "a", Status: agentflow.Abandoned, Steps: 4,
			Usage: Usage{Partial: "run d was not read from its session"}},
	}
	if !reflect.DeepEqual(s.Calls, want) {
		t.Errorf("calls =\n%+v\nwant\n%+v", s.Calls, want)
	}
	// The kept session was read once for both its visits, and the unread
	// child's not at all.
	if reads["b"] != 1 || reads["d"] != 0 {
		t.Errorf("reads = %v", reads)
	}
}

func TestWithoutResponsesNoUsageIsCounted(t *testing.T) {
	s := Of(run("a", 0, 4, 5, 2), Options{})
	if s.Steps != 2 || s.Usage.Requests != 0 || !strings.Contains(s.Usage.Partial, "no Responses were given") || s.Started != nil || s.Ended != nil {
		t.Errorf("summary = %+v", s)
	}
}
