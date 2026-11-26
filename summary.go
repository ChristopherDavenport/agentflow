package agentflow

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"time"

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
	// Calls are the runs it called, in order, each as its own end says.
	Calls []Called `json:"calls,omitempty"`
}

// Usage is what model requests used, counted as they were served.
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

// Called is a run a run called, with the totals its end summed up: its
// own summary, with the runs it called in turn, is in its own end.
type Called struct {
	Workflow string `json:"workflow"`
	Run      string `json:"run"`
	Status   string `json:"status"`
	Outcome  string `json:"outcome,omitempty"`
	// Summed says the run's end wrote a summary, which Steps and Usage are.
	Summed bool  `json:"summed,omitempty"`
	Steps  int   `json:"steps,omitempty"`
	Usage  Usage `json:"usage"`
}

// calledOf is what a run that called the one s sums up lists of it.
func calledOf(workflow, run, status, outcome string, s *Summary) Called {
	c := Called{Workflow: workflow, Run: run, Status: status, Outcome: outcome}
	if s != nil {
		c.Summed, c.Steps, c.Usage = true, s.Steps, s.Usage
	}
	return c
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
// write when they end. It returns false for a model it has no price for.
func Prices(price func(model string, u openresponses.Usage) (float64, bool)) Option {
	return func(f *flow) { f.prices = price }
}

// usageBook counts what each run's model requests use, as they are
// served, by run key. It lives as long as the process: what a run used
// before a restart is in the record's responses, not here, so a run whose
// start this process did not see counts short and says so.
type usageBook struct {
	mu   sync.Mutex
	runs map[string]*tally
}

type tally struct {
	Usage
	// whole says this process saw the run start, so every request it made
	// since was counted.
	whole bool
}

func (b *usageBook) entry(run string) *tally {
	if b.runs == nil {
		b.runs = map[string]*tally{}
	}
	t := b.runs[run]
	if t == nil {
		t = &tally{}
		b.runs[run] = t
	}
	return t
}

// started says this process wrote the run's start.
func (b *usageBook) started(run string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.entry(run).whole = true
}

// served counts one request of run's, priced by price when it can be.
func (b *usageBook) served(run, model string, u *openresponses.Usage, price func(string, openresponses.Usage) (float64, bool)) {
	if b == nil || run == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.entry(run)
	t.Requests++
	if u == nil {
		return
	}
	t.InputTokens += u.InputTokens
	t.OutputTokens += u.OutputTokens
	if price != nil {
		if cost, ok := price(model, *u); ok {
			t.Cost += cost
			return
		}
	}
	if !slices.Contains(t.Unpriced, model) {
		t.Unpriced = append(t.Unpriced, model)
		slices.Sort(t.Unpriced)
	}
}

// of is what run used, as far as this process saw.
func (b *usageBook) of(run string) Usage {
	if b == nil {
		return Usage{Partial: "no usage was counted: the host's agent does not follow the workflow's config"}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.entry(run)
	u := t.Usage
	u.Unpriced = slices.Clone(u.Unpriced)
	if !t.whole {
		u.Partial = "the run started before this process, and the requests it made before then are not counted"
	}
	return u
}

// summarize sums up the run st is, as its end is written.
func (f *flow) summarize(st State) *Summary {
	s := &Summary{Steps: st.treeSteps, Calls: slices.Clone(st.called), Usage: f.book.of(st.Instance)}
	if !st.started.IsZero() {
		at := st.started
		s.Started = &at
	}
	now := time.Now().UTC()
	s.Ended = &now
	for _, c := range st.called {
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

// counting passes a model's stream to sink, and counts what its response
// used toward the run the request was for.
func (f *flow) counting(sink openresponses.EventSink, model string) openresponses.EventSink {
	run := ""
	if f.due != nil {
		run = f.due.forRun()
	}
	return openresponses.EventSinkFunc(func(ev openresponses.StreamEvent) error {
		var resp *openresponses.Response
		switch e := ev.(type) {
		case *openresponses.ResponseCompletedEvent:
			resp = e.Response
		case *openresponses.ResponseIncompleteEvent:
			resp = e.Response
		case *openresponses.ResponseFailedEvent:
			resp = e.Response
		}
		if resp != nil {
			f.book.served(run, model, resp.Usage, f.prices)
		}
		return sink.Send(ev)
	})
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

// called is what a call into a child says of the run it ran: its result,
// its failure or its verdict, with the summary its end wrote.
func calledFresh(c callArgs, output string) Called {
	var said struct {
		Outcome string   `json:"outcome"`
		Failure string   `json:"failure"`
		Limit   string   `json:"limit"`
		Summary *Summary `json:"summary"`
	}
	_ = decode([]byte(output), &said)
	out := calledOf(c.Workflow, c.Run, Done, said.Outcome, said.Summary)
	switch {
	case said.Failure != "" || said.Limit != "" || said.Outcome == Failed:
		out.Status, out.Outcome = Failed, ""
	case said.Outcome == "" && !strings.Contains(output, `"outcome"`):
		out.Status = Failed
	}
	return out
}

// argsOf reads a call into a child's arguments.
func argsOf(c *openresponses.FunctionCall) callArgs {
	var a callArgs
	_ = decode([]byte(c.Arguments), &a)
	return a
}
