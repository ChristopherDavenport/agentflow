package agentflow

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// fetcher is a model that calls the slow tool, then finishes; its stream
// numbered stall waits until its context ends.
type fetcher struct {
	mu    sync.Mutex
	n     int
	stall int
}

func (m *fetcher) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	m.n++
	n := m.n
	m.mu.Unlock()
	if n == m.stall {
		<-ctx.Done()
		return ctx.Err()
	}
	called := false
	for i := len(req.Input) - 1; i >= 0; i-- {
		if c, ok := req.Input[i].(*openresponses.FunctionCall); ok {
			called = c.Name == "slow"
			break
		}
	}
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	name, args := "slow", "{}"
	if called {
		b, err := json.Marshal(finishArgs{Run: runIn(req), Step: "fetch", Outcome: OK})
		if err != nil {
			return err
		}
		name, args = finishTool, string(b)
	}
	call, err := e.FunctionCall("", name)
	if err != nil {
		return err
	}
	if err := call.Arguments(args); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

// A step retried by the host's rules is cut off again, this time in the
// model's stream after its tool has answered: the step fails again and is
// asked about again, and the run does not end in an error.
func TestARetriedStepCutOffInItsStreamAsksAgain(t *testing.T) {
	g := &Graph{Name: "slow", Nodes: []Node{
		{ID: "fetch", Kind: Agent, Prompt: "Fetch.", Tools: []string{"slow"}, Timeout: 80 * time.Millisecond},
	}}
	var calls int
	slow := agenttool.New("slow", "Fetch something slowly.", func(ctx context.Context, _ struct{}) (string, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "fetched", nil
	})
	retries := 0
	rules := Reply(func(_ context.Context, q Question) (Result, bool) {
		if q.Retry {
			retries++
			return Result{Outcome: retryOutcome}, true
		}
		return Result{}, false
	})
	// Streams: 1 calls slow (cut in the tool); 2 calls slow again; 3 is
	// cut; 4 calls slow; 5 finishes.
	m := &fetcher{stall: 3}
	a := nestedAgent(t, g, m, []agenttool.Tool{slow}, rules)
	ctx := context.Background()
	end, err := a.Prompt(ctx, mustInvocation(t, g))
	if end == nil {
		t.Fatal(err)
	}
	end, st, err := Drive(ctx, g, a, end, rules)
	if err != nil {
		t.Fatalf("end %v: %v", end.Reason, err)
	}
	var got []string
	for _, s := range st.Steps {
		got = append(got, s.Node+":"+s.Result.Outcome)
	}
	if end.Reason != agentturn.ReasonDone || !slices.Equal(got, []string{"fetch:ok"}) || retries != 2 {
		t.Errorf("end %v; steps %v; retries %d; slow called %d times", end.Reason, got, retries, calls)
	}
}

func mustInvocation(t *testing.T, g *Graph) openresponses.Item {
	t.Helper()
	item, err := g.Invocation(nil)
	if err != nil {
		t.Fatal(err)
	}
	return item
}
