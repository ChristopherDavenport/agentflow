// Package summary sums up how a run of a workflow went, from its record:
// when it ran, how many steps it and what it called entered, and what
// its model requests used. Nothing of it is written into the record. It
// is read back from what [agentflow.Runs] gives and from the responses
// the host's sessions hold, so it comes out the same whenever it is
// asked for.
package summary

import (
	"slices"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/openresponses"
)

// Summary is how a run went, beside the path, outcome and reason its
// ending holds: when it ran, how many steps it and what it called
// entered, and what its model requests used, everything it called
// included.
type Summary struct {
	// Started is when the run's start was written, and Ended when its end
	// was, nil where the record has none.
	Started *time.Time `json:"started,omitempty"`
	Ended   *time.Time `json:"ended,omitempty"`
	// Steps counts the steps the run entered, attempts included, and every
	// step of the runs it called.
	Steps int   `json:"steps"`
	Usage Usage `json:"usage"`
	// Calls are every run it called, at any depth, each listed before the
	// runs it called and with the key of the run that called it: a flat
	// list, from which a reader rebuilds the tree by Parent. A run's totals
	// take in those of the runs below it.
	Calls []Called `json:"calls,omitempty"`
}

// Usage is what model requests used.
type Usage struct {
	Requests     int `json:"requests,omitempty"`
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
	// Cost is what the prices given make of them, and Unpriced the models
	// there is no price for, whose requests Cost leaves out.
	Cost     float64  `json:"cost,omitempty"`
	Unpriced []string `json:"unpriced,omitempty"`
	// Partial says why the counts fall short, empty when they are whole.
	Partial string `json:"partial,omitempty"`
}

// Called is a run a run called, at any depth, with its totals.
type Called struct {
	Workflow string `json:"workflow"`
	Run      string `json:"run"`
	// Parent is the key of the run that called it.
	Parent  string `json:"parent"`
	Status  string `json:"status"`
	Outcome string `json:"outcome,omitempty"`
	// Read says the run was read from its own session, so Steps and Usage
	// are its record's. For one that was not, Steps is what its call's
	// result says its tree spent of the steps, which leaves out
	// person-paced visits, the runs below it are not listed, and no usage
	// is counted.
	Read  bool  `json:"read,omitempty"`
	Steps int   `json:"steps,omitempty"`
	Usage Usage `json:"usage"`
}

// Response is one model response a session holds, as a host reads it
// back: the model asked, what it used, and At, how many items of the
// session's record came before the response's own. The record is the
// transcript [agentflow.Runs] was given for that session, so for a
// forked child it is the child's own items, counted from its first.
type Response struct {
	Model string
	Usage openresponses.Usage
	At    int
}

// Options are what a summary is made with beside the run.
type Options struct {
	// Responses reads the responses a session holds: the session the run
	// given to [Of] was read from for the empty spawn, and otherwise the
	// child session a spawn names, as the open given to [agentflow.Runs]
	// names it. Without it no usage is counted, and the summary says so.
	Responses func(spawn string) ([]Response, error)
	// Price prices what a model's request used, false for a model it has
	// no price for. Without it nothing is priced.
	Price func(model string, u openresponses.Usage) (float64, bool)
}

// Of sums up run, one of the runs [agentflow.Runs] gave, with every run
// below it. Give Runs an open, or a run called into a child is summed up
// only from what its call's result says.
func Of(run *agentflow.Instance, o Options) *Summary {
	s := &summer{o: o, read: map[string][]Response{}, failed: map[string]string{}}
	steps, usage, calls := s.sum(run, "")
	out := &Summary{Steps: steps, Usage: usage, Calls: calls}
	if !run.Started.IsZero() {
		at := run.Started
		out.Started = &at
	}
	if !run.Ended.IsZero() {
		at := run.Ended
		out.Ended = &at
	}
	return out
}

type summer struct {
	o Options
	// read holds each session's responses, read once: a kept child's
	// session holds a run for every visit.
	read   map[string][]Response
	failed map[string]string
}

// sum is what in and the runs below it came to, and those runs as a
// caller lists them. spawn names the session in was read from.
func (s *summer) sum(in *agentflow.Instance, spawn string) (steps int, usage Usage, calls []Called) {
	if in.Start < 0 {
		// Known only from its call's result.
		return in.State.Entries, Usage{Partial: "run " + in.ID + " was not read from its session"}, nil
	}
	steps, usage = in.State.Entered, s.own(in, spawn)
	for _, kid := range in.Children {
		ks, ku, kc := s.sum(kid, kid.Spawn)
		steps += ks
		usage.add(ku)
		calls = append(calls, Called{Workflow: kid.Workflow, Run: kid.ID, Parent: in.ID, Status: kid.Status, Outcome: kid.Outcome,
			Read: kid.Start >= 0, Steps: ks, Usage: ku})
		calls = append(calls, kc...)
	}
	return steps, usage, calls
}

// own is what in's own requests used: the responses of its session that
// fall inside its span, the program's turns left out.
func (s *summer) own(in *agentflow.Instance, spawn string) Usage {
	if s.o.Responses == nil {
		return Usage{Partial: "no usage was counted: no Responses were given"}
	}
	responses, ok := s.read[spawn]
	if why, bad := s.failed[spawn]; bad {
		return Usage{Partial: why}
	}
	if !ok {
		var err error
		if responses, err = s.o.Responses(spawn); err != nil {
			s.failed[spawn] = "the session's responses could not be read: " + err.Error()
			return Usage{Partial: s.failed[spawn]}
		}
		s.read[spawn] = responses
	}
	// A request is the run's when it was made after its start and before
	// its end, or before the session's next run where it has no end.
	last := in.Stop
	if in.End >= 0 {
		last = in.End
	}
	var u Usage
	for _, r := range responses {
		if agentflow.IsProgramModel(r.Model) || r.At <= in.Start || r.At >= last {
			continue
		}
		u.Requests++
		u.InputTokens += r.Usage.InputTokens
		u.OutputTokens += r.Usage.OutputTokens
		if s.o.Price != nil {
			if cost, ok := s.o.Price(r.Model, r.Usage); ok {
				u.Cost += cost
				continue
			}
		}
		if !slices.Contains(u.Unpriced, r.Model) {
			u.Unpriced = append(u.Unpriced, r.Model)
			slices.Sort(u.Unpriced)
		}
	}
	return u
}

// add adds u's counts to s's.
func (s *Usage) add(u Usage) {
	s.Requests += u.Requests
	s.InputTokens += u.InputTokens
	s.OutputTokens += u.OutputTokens
	s.Cost += u.Cost
	for _, m := range u.Unpriced {
		if !slices.Contains(s.Unpriced, m) {
			s.Unpriced = append(s.Unpriced, m)
		}
	}
	slices.Sort(s.Unpriced)
	if s.Partial == "" {
		s.Partial = u.Partial
	}
}
