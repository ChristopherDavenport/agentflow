package agentflow

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// Summary is what flow_end records of how a run went, beside the path,
// outcome and reason the ending holds: when it ran, how many steps it and
// what it called entered, and what its model requests used, everything
// it called included.
type Summary struct {
	// Started is when the run's start was recorded, as flow_start's result
	// says, and Ended when the program wrote its end.
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
	// Cost is what the host's prices make of them, and Unpriced the
	// models it has no price for, whose requests Cost leaves out.
	Cost     float64  `json:"cost,omitempty"`
	Unpriced []string `json:"unpriced,omitempty"`
	// Partial says why the counts fall short, empty when they are whole.
	Partial string `json:"partial,omitempty"`
}

// Called is a run a run called, at any depth, with the totals its end
// summed up.
type Called struct {
	Workflow string `json:"workflow"`
	Run      string `json:"run"`
	// Parent is the key of the run that called it.
	Parent  string `json:"parent"`
	Status  string `json:"status"`
	Outcome string `json:"outcome,omitempty"`
	// Summed says the run's end wrote a summary, which Steps and Usage are.
	Summed bool  `json:"summed,omitempty"`
	Steps  int   `json:"steps,omitempty"`
	Usage  Usage `json:"usage"`
}

// calledOf is what a run, parent, that called the one s sums up lists of
// it: that run, and every run below it, as its own summary lists them.
func calledOf(parent, workflow, run, status, outcome string, s *Summary) []Called {
	c := Called{Workflow: workflow, Run: run, Parent: parent, Status: status, Outcome: outcome}
	if s == nil {
		return []Called{c}
	}
	c.Summed, c.Steps, c.Usage = true, s.Steps, s.Usage
	return append([]Called{c}, s.Calls...)
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

// Prices prices what a model's request used, for the summaries runs
// write when they end from the host's [Responses]. It returns false for a
// model it has no price for.
func Prices(price func(model string, u openresponses.Usage) (float64, bool)) Option {
	return func(f *flow) { f.prices = price }
}

// summarize sums up the run st is, as its end is written, from all, the
// record.
func (f *flow) summarize(ctx context.Context, st State, all agentturn.Transcript) *Summary {
	// What the requests used is read from the session's responses, which
	// only the host can give.
	own := Usage{Partial: "no usage was counted: the host gave no Responses"}
	if f.responses != nil {
		own = f.recorded(ctx, all, st.Instance)
	}
	s := &Summary{Steps: st.treeSteps, Calls: slices.Clone(st.called), Usage: own}
	if !st.started.IsZero() {
		at := st.started
		s.Started = &at
	}
	now := time.Now().UTC()
	s.Ended = &now
	// A run's totals take in the runs it called itself, whose own take in
	// those below them.
	for _, c := range st.called {
		if c.Parent != st.Instance {
			continue
		}
		if !c.Summed {
			if s.Usage.Partial == "" {
				s.Usage.Partial = "run " + c.Run + " it called ended with no summary"
			}
			continue
		}
		s.Usage.add(c.Usage)
	}
	return s
}

// ResponseUsage is one model response a session recorded, as a host reads
// it back for [Responses]: the model asked, what it used, and At, how many
// of the record's items came before the response's own.
type ResponseUsage struct {
	Model string
	Usage openresponses.Usage
	At    int
}

// Responses gives the responses the session the record is of holds, with
// what each used, as a host reads them from its session's response
// entries. A run's summary counts what its requests used from them, each
// by the run that was current where it falls in the record, so a run that
// started before a restart counts every request it made. It must be
// current when called, as [History] must; for a child, the opener gives
// the child's own with Child.Apply. Without it a summary counts no usage,
// and says so.
func Responses(read func(ctx context.Context) ([]ResponseUsage, error)) Option {
	return func(f *flow) { f.responses = read }
}

// recorded is what run's requests used, as the host's responses say.
func (f *flow) recorded(ctx context.Context, all agentturn.Transcript, run string) Usage {
	responses, err := f.responses(ctx)
	if err != nil {
		return Usage{Partial: "the host's responses could not be read: " + err.Error()}
	}
	owners := f.g.owners(all)
	var u Usage
	for _, r := range responses {
		// The program's own turns use nothing.
		if strings.HasPrefix(r.Model, programPrefix) || r.At < 0 || r.At >= len(owners) || owners[r.At] != run {
			continue
		}
		u.Requests++
		u.InputTokens += r.Usage.InputTokens
		u.OutputTokens += r.Usage.OutputTokens
		if f.prices != nil {
			if cost, ok := f.prices(r.Model, r.Usage); ok {
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

// startedAt reads the time a flow_start's result says the run started.
func startedAt(output string) time.Time {
	var res struct {
		Data struct {
			At time.Time `json:"at"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(output), &res)
	return res.Data.At
}

// calledFresh is what a call of parent's into a child says of the run it
// ran, and of every run below it: its result, its failure or its verdict,
// with the summary its end wrote.
func calledFresh(parent string, c callArgs, output string) []Called {
	var said struct {
		Outcome string   `json:"outcome"`
		Failure string   `json:"failure"`
		Limit   string   `json:"limit"`
		Summary *Summary `json:"summary"`
	}
	_ = decode([]byte(output), &said)
	out := calledOf(parent, c.Workflow, c.Run, Done, said.Outcome, said.Summary)
	switch {
	case said.Failure == abandonedText || said.Failure == overriddenText:
		// Set aside as the runs were abandoned, or for an override.
		out[0].Status, out[0].Outcome = Abandoned, ""
	case said.Failure != "" || said.Limit != "" || said.Outcome == Failed:
		out[0].Status, out[0].Outcome = Failed, ""
	case said.Outcome == "" && !strings.Contains(output, `"outcome"`):
		out[0].Status = Failed
	}
	return out
}

// argsOf reads a call into a child's arguments.
func argsOf(c *openresponses.FunctionCall) callArgs {
	var a callArgs
	_ = decode([]byte(c.Arguments), &a)
	return a
}
