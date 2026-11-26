package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// cutOff is a model whose first stream waits until its context ends, and
// keeps the deadline it was held to and why it ended.
type cutOff struct {
	fakeModel
	mu       sync.Mutex
	cut      bool
	deadline time.Time
	err      error
}

func (m *cutOff) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	if m.cut {
		m.mu.Unlock()
		return m.fakeModel.CreateStream(ctx, req, sink)
	}
	m.cut = true
	m.requests = append(m.requests, req)
	m.deadline, _ = ctx.Deadline()
	m.mu.Unlock()
	<-ctx.Done()
	m.mu.Lock()
	m.err = ctx.Err()
	m.mu.Unlock()
	return ctx.Err()
}

// musing is a model step with a timeout of its own, none for 0, and a
// cleanup path.
func musing(own time.Duration) *Graph {
	return &Graph{Name: "musing", OnAbort: "tidy", Nodes: []Node{
		{ID: "work", Kind: Agent, Prompt: "Work.", Timeout: own},
		{ID: "tidy", Kind: Run, Tool: "script", Args: json.RawMessage(`{"name":"tidied"}`)},
	}}
}

// clockOf is the deadline the record holds for step's clock, and how often
// the clock was read for it.
func clockOf(t *testing.T, items agentturn.Transcript, step string) (time.Time, int) {
	t.Helper()
	ids := map[string]bool{}
	for _, item := range items {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == clockTool && strings.HasSuffix(c.CallID, "-clock") {
			var a clockArgs
			if json.Unmarshal([]byte(c.Arguments), &a) == nil && a.Step == step {
				ids[c.CallID] = true
			}
		}
	}
	var at time.Time
	for _, item := range items {
		if out, ok := item.(*openresponses.FunctionCallOutput); ok && ids[out.CallID] {
			var read struct {
				Data struct {
					Deadline time.Time `json:"deadline"`
				} `json:"data"`
			}
			if err := json.Unmarshal([]byte(out.Output.String()), &read); err != nil {
				t.Fatal(err)
			}
			at = read.Data.Deadline
		}
	}
	return at, len(ids)
}

// expiredFor says the record holds the program's word that step's clock ran
// out, under the run run.
func expiredFor(items agentturn.Transcript, run, step string) bool {
	for _, item := range items {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == clockTool && strings.HasSuffix(c.CallID, "-timeout") && strings.HasPrefix(c.CallID, reserved+run+"_") {
			var a clockArgs
			if json.Unmarshal([]byte(c.Arguments), &a) == nil && a.Step == step && a.Expired {
				return true
			}
		}
	}
	return false
}

func TestAModelStepIsCutOffByTheCallItRunsIn(t *testing.T) {
	const want = `timeout: call "call" of impatient ran past its timeout of 80ms`
	for name, own := range map[string]time.Duration{"no timeout of its own": 0, "a longer one": time.Hour} {
		t.Run(name, func(t *testing.T) {
			g := linked(t, impatient("musing", 80*time.Millisecond, "", true), musing(own))
			var ran []string
			m := &cutOff{}
			a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)})
			end, st, err := Drive(context.Background(), g, a, run(a))
			if err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("end = %+v, %v", end, err)
			}
			items := a.State().Transcript
			// The stream was cut at the call's deadline, as the record holds
			// it, and not at the step's own.
			call, reads := clockOf(t, items, "call")
			if call.IsZero() || reads != 1 {
				t.Fatalf("the call's clock: %v, read %d times", call, reads)
			}
			if !errors.Is(m.err, context.DeadlineExceeded) || !m.deadline.Equal(call) {
				t.Errorf("stream ended %v at %v, want the call's deadline %v", m.err, m.deadline, call)
			}
			if own > 0 {
				if work, _ := clockOf(t, items, "work"); !work.After(call) {
					t.Errorf("the step's own deadline %v is not the later one", work)
				}
			}
			runs, err := Runs(items, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := tree(runs); !slices.Equal(got, []string{"impatient:done:ok", " musing:abandoned:"}) {
				t.Fatalf("tree = %v", got)
			}
			if !expiredFor(items, runs[0].ID, "call") {
				t.Error("no expiry under the caller's key")
			}
			if callee := runs[0].Children[0]; callee.Reason != want || callee.State.Cleanup != "done" {
				t.Errorf("callee = %q, cleanup %q", callee.Reason, callee.State.Cleanup)
			}
			if !slices.Equal(ran, []string{"tidied", want}) || st.Steps[0].Result.Outcome != Failed {
				t.Errorf("ran %v, steps %+v", ran, st.Steps)
			}
			if len(m.requests) != 1 {
				t.Errorf("%d model requests", len(m.requests))
			}
		})
	}
}

func TestAModelStepCutOffByItsCallAsksToRetryTheCall(t *testing.T) {
	g := linked(t, impatient("musing", 60*time.Millisecond, "", false), musing(0))
	var ran []string
	m := &cutOff{}
	a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)})
	end, _, err := Drive(context.Background(), g, a, run(a))
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, end)
	if !q.Retry || q.Step != "call" || !strings.Contains(q.Question, "ran past its timeout of 60ms") {
		t.Fatalf("question = %+v", q)
	}
	if !errors.Is(m.err, context.DeadlineExceeded) || !slices.Equal(ran, []string{"tidied"}) {
		t.Errorf("stream ended %v; ran %v", m.err, ran)
	}
}

func TestAModelStepInAFreshCallIsCutOffByTheCall(t *testing.T) {
	g := linked(t, impatient("musing", 100*time.Millisecond, ContextFresh, true), musing(0))
	var ran []string
	tools := []agenttool.Tool{scriptTool(&ran)}
	m := &cutOff{}
	kids := newKin(m, "fake", tools...)
	a := nestedAgent(t, g, m, tools, kids.option())
	end, st, err := Drive(context.Background(), g, a, run(a))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	call, _ := clockOf(t, a.State().Transcript, "call")
	if call.IsZero() || !errors.Is(m.err, context.DeadlineExceeded) || !m.deadline.Equal(call) {
		t.Errorf("stream ended %v at %v, want the call's deadline %v", m.err, m.deadline, call)
	}
	if len(ran) != 2 || ran[0] != "tidied" || !strings.Contains(ran[1], `call "call" of impatient ran past its timeout of 100ms`) || st.Steps[0].Result.Outcome != Failed {
		t.Errorf("ran %v, steps %+v", ran, st.Steps)
	}
	if len(kids.agents) != 1 {
		t.Fatalf("%d children", len(kids.agents))
	}
	for _, child := range kids.agents {
		runs, err := Runs(child.State().Transcript, nil)
		if err != nil || len(runs) != 1 || runs[0].Status != Abandoned || !strings.HasPrefix(runs[0].Reason, timedOutPrefix) || runs[0].State.Cleanup != "done" {
			t.Errorf("child runs = %+v, %v", runs, err)
		}
	}
}

// A process killed while the model works, past the call's deadline, is
// resumed held to the deadline its record holds: the clock is not read
// again, and the model is not asked.
func TestAModelStepCutOffByItsCallStaysCutAfterAKill(t *testing.T) {
	g := func() *Graph { return linked(t, impatient("musing", 150*time.Millisecond, "", true), musing(0)) }
	var ran []string
	childCfg := func() agentturn.Config {
		return must(Apply(g(), agentturn.Config{Model: killingModel{}, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}}))
	}
	m := &fakeModel{outcomes: []string{OK}}
	graph := g()
	b := killedRecord(t, childCfg, func(*jsonl.Store, string) agentturn.Config {
		return must(Apply(graph, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}}))
	})
	call, reads := clockOf(t, b.State().Transcript, "call")
	if call.IsZero() || reads != 1 {
		t.Fatalf("the call's clock: %v, read %d times", call, reads)
	}
	time.Sleep(time.Until(call) + 20*time.Millisecond)
	ctx := context.Background()
	end, err := b.Continue(ctx)
	if end == nil {
		t.Fatal(err)
	}
	end, st, err := Drive(ctx, graph, b, end)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(m.requests) != 0 {
		t.Errorf("the model was asked %d times after the deadline", len(m.requests))
	}
	items := b.State().Transcript
	if again, reads := clockOf(t, items, "call"); reads != 1 || !again.Equal(call) {
		t.Errorf("the call's clock was read again: %v, %d reads", again, reads)
	}
	if len(ran) != 2 || ran[0] != "tidied" || !strings.Contains(ran[1], "ran past its timeout of 150ms") || st.Steps[0].Result.Outcome != Failed {
		t.Errorf("ran %v, steps %+v", ran, st.Steps)
	}
}
