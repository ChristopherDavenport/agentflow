package agentflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

const (
	reserved   = "flow_"
	askTool    = "flow_ask"
	finishTool = "flow_finish"
)

var (
	// ErrStepFailed is a run or person node whose output is not a
	// [Result] with a declared outcome.
	ErrStepFailed = errors.New("agentflow: step failed")
	// ErrVisits is a node reached more often than it allows.
	ErrVisits = errors.New("agentflow: visit limit reached")
)

// Result is the output of every step. A tool a Run node calls returns
// one. Anything else, an error or a refusal among them, fails the step:
// a transcript carries no failure flag, so success has to be said.
type Result struct {
	Outcome string         `json:"outcome"`
	Data    map[string]any `json:"data,omitempty"`
	// By says who chose, when it was not whoever the node names.
	By string `json:"by,omitempty"`
}

// Step is one finished node.
type Step struct {
	Node   string
	Visit  int
	Result Result
}

// State is where a workflow stands.
type State struct {
	// Node is the node to run next, nil when the workflow is done.
	Node *Node
	// Visit counts this arrival at Node, from 1.
	Visit int
	// Steps are the finished nodes, oldest first.
	Steps []Step
}

// CallID is the ID of the call the program makes for the state's node.
func (g *Graph) CallID(node string, visit int) string {
	return reserved + g.Name + "_" + node + "_" + strconv.Itoa(visit)
}

// Replay reads where the workflow stands from a transcript. It is a
// pure function of the graph and the transcript. On error the state
// holds the node that failed.
func (g *Graph) Replay(t agentturn.Transcript) (State, error) {
	visits := map[string]int{}
	st := State{Node: &g.Nodes[0], Visit: 1}
	visits[st.Node.ID] = 1
	calls := map[string]*openresponses.FunctionCall{}
	// at is where each call was made, and since is where the current
	// node began: a finish made before then was not about it.
	at, since := map[string]int{}, -1
	for i, item := range t {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			calls[it.CallID] = it
			at[it.CallID] = i
		case *openresponses.FunctionCallOutput:
			n := st.Node
			if n == nil {
				return st, nil
			}
			res, ok := parseResult(n, it.Output.String())
			if n.Kind == Agent {
				// A finish that was rejected or is for another node is
				// not progress; the model saw the error and goes on.
				c := calls[it.CallID]
				if !ok || c == nil || c.Name != finishTool || finishStep(c.Arguments) != n.ID || at[it.CallID] < since {
					continue
				}
			} else {
				if it.CallID != g.CallID(n.ID, st.Visit) {
					continue
				}
				if !ok {
					return st, fmt.Errorf("%w: %s: %s", ErrStepFailed, n.ID, it.Output.String())
				}
			}
			st.Steps = append(st.Steps, Step{Node: n.ID, Visit: st.Visit, Result: res})
			since = i
			next := n.Next[res.Outcome]
			if next == "" {
				st.Node, st.Visit = nil, 0
				continue
			}
			st.Node = g.node(next)
			visits[next]++
			st.Visit = visits[next]
			if st.Visit > max(1, st.Node.MaxVisits) {
				return st, fmt.Errorf("%w: %s", ErrVisits, next)
			}
		}
	}
	return st, nil
}

func parseResult(n *Node, output string) (Result, bool) {
	var res Result
	if err := json.Unmarshal([]byte(output), &res); err != nil {
		return Result{}, false
	}
	return res, slices.Contains(n.outcomes(), res.Outcome)
}

func finishStep(args string) string {
	var a finishArgs
	_ = json.Unmarshal([]byte(args), &a)
	return a.Step
}
