package agentflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

const (
	reserved   = "flow_"
	startTool  = "flow_start"
	askTool    = "flow_ask"
	finishTool = "flow_finish"

	// The outcomes of the question a failed step raises.
	retryOutcome = "retry"
	stopOutcome  = "stop"

	invocationPrefix = "agentflow: start "
	nudgePrefix      = "agentflow: the step is not finished."
)

var (
	// ErrStepFailed is a run or person step whose output is not a
	// [Result] with a declared outcome, and that is not to be retried.
	ErrStepFailed = errors.New("agentflow: step failed")
	// ErrVisits is a node reached more often than it allows.
	ErrVisits = errors.New("agentflow: visit limit reached")
	// ErrChanged is a transcript whose instance started with another
	// definition.
	ErrChanged = errors.New("agentflow: the workflow changed since it started")
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
	// Started says the transcript holds an instance, named by Instance.
	Started  bool
	Instance string
	// Node is the node to run next, nil before the start and when done.
	Node *Node
	// Visit counts this arrival at Node and Attempt this try, from 1.
	Visit, Attempt int
	// Failure is the output of Node's failed attempt, until a retry.
	Failure string
	// Done says the last step has finished, and Reported that the
	// workflow has since said so. The conversation is then the model's.
	Done, Reported bool
	// Nudges counts the reminders given since Node became current.
	Nudges int
	// Steps are the finished nodes, oldest first.
	Steps []Step
}

// startPayload is flow_start's arguments.
type startPayload struct {
	Workflow string `json:"workflow"`
	Digest   string `json:"digest"`
}

// Invocation is the item that asks for a new instance. The first one
// needs none: a transcript with no instance starts one.
func (g *Graph) Invocation() openresponses.Item {
	return openresponses.DeveloperText(invocationPrefix + g.Name)
}

func (g *Graph) startID(instance string) string {
	return reserved + g.Name + "_" + instance + "_start"
}

// callID is the ID of the call the program makes for an attempt.
func (g *Graph) callID(st State) string {
	id := reserved + g.Name + "_" + st.Instance + "_" + st.Node.ID + "_v" + strconv.Itoa(st.Visit)
	if st.Attempt > 1 {
		id += "-a" + strconv.Itoa(st.Attempt)
	}
	return id
}

// retryID is the ID of the question a failed attempt raises.
func (g *Graph) retryID(st State) string { return g.callID(st) + "-retry" }

// Replay reads where the workflow stands from a transcript. It is a
// pure function of the graph and the transcript. On error the state is
// where the workflow stopped.
func (g *Graph) Replay(t agentturn.Transcript) (State, error) {
	var (
		st     State
		failed error
		visits = map[string]int{}
		calls  = map[string]*openresponses.FunctionCall{}
		// at is where each call was made, and since is where the current
		// node began: a finish made before then was not about it.
		at     = map[string]int{}
		since  = -1
		digest = g.Digest()
	)
	for i, item := range t {
		switch it := item.(type) {
		case *openresponses.Message:
			switch {
			case it.Role == openresponses.RoleDeveloper && it.Text() == invocationPrefix+g.Name:
				st, failed, visits = State{}, nil, map[string]int{}
			case it.Role == openresponses.RoleUser && strings.HasPrefix(it.Text(), nudgePrefix):
				st.Nudges++
			case it.Role == openresponses.RoleAssistant && st.Done:
				st.Reported = true
			}
		case *openresponses.FunctionCall:
			calls[it.CallID] = it
			at[it.CallID] = i
		case *openresponses.FunctionCallOutput:
			if failed != nil || st.Done {
				continue
			}
			c := calls[it.CallID]
			if !st.Started {
				var p startPayload
				if c == nil || c.Name != startTool || json.Unmarshal([]byte(c.Arguments), &p) != nil || p.Workflow != g.Name {
					continue
				}
				var res Result
				switch {
				case json.Unmarshal([]byte(it.Output.String()), &res) != nil || res.Outcome != OK:
					failed = fmt.Errorf("%w: start: %s", ErrStepFailed, it.Output.String())
				case p.Digest != digest:
					failed = ErrChanged
				default:
					instance := strings.TrimSuffix(strings.TrimPrefix(it.CallID, reserved+g.Name+"_"), "_start")
					st = State{Started: true, Instance: instance, Node: &g.Nodes[0], Visit: 1, Attempt: 1}
					visits[st.Node.ID], since = 1, i
				}
				continue
			}
			n := st.Node
			res, ok := parseResult(n, it.Output.String())
			switch {
			case st.Failure != "":
				if it.CallID != g.retryID(st) {
					continue
				}
				var answer Result
				_ = json.Unmarshal([]byte(it.Output.String()), &answer)
				if answer.Outcome == retryOutcome {
					st.Attempt, st.Failure = st.Attempt+1, ""
				} else {
					failed = fmt.Errorf("%w: %s: %s", ErrStepFailed, n.ID, st.Failure)
				}
				continue
			case n.Kind == Agent:
				// A finish that was rejected or is for another node is
				// not progress; the model saw the error and goes on.
				if !ok || c == nil || c.Name != finishTool || finishStep(c.Arguments) != n.ID || at[it.CallID] < since {
					continue
				}
			default:
				if it.CallID != g.callID(st) {
					continue
				}
				if !ok {
					st.Failure = it.Output.String()
					continue
				}
			}
			st.Steps = append(st.Steps, Step{Node: n.ID, Visit: st.Visit, Result: res})
			since, st.Nudges = i, 0
			next := n.Next[res.Outcome]
			if next == "" {
				st.Node, st.Visit, st.Attempt, st.Done = nil, 0, 0, true
				continue
			}
			st.Node = g.node(next)
			visits[next]++
			st.Visit, st.Attempt = visits[next], 1
			if st.Visit > max(1, st.Node.MaxVisits) {
				failed = fmt.Errorf("%w: %s", ErrVisits, next)
			}
		}
	}
	return st, failed
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
