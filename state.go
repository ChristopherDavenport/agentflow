package agentflow

import (
	"crypto/sha256"
	"encoding/hex"
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
	judgeTool  = "flow_judge"
	endTool    = "flow_end"

	// The outcomes of the question a failed step raises.
	retryOutcome = "retry"
	stopOutcome  = "stop"

	invocationPrefix = "agentflow: start "
	nudgePrefix      = "agentflow: the step is not finished."
	noVerdict        = "agentflow: no verdict is coming."
	beginPrefix      = "agentflow: begin step "
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
	Outcome string `json:"outcome"`
	Data    any    `json:"data,omitempty"`
	// By says who chose, when it was not whoever the node names.
	By string `json:"by,omitempty"`
	// Reason is why, in the words of whoever chose. It is for whoever
	// reads the record: no step reads it as data.
	Reason string `json:"reason,omitempty"`
}

// Step is one finished node.
type Step struct {
	Node   string
	Visit  int
	Result Result
	// Advice is what a judge said of this question, if one was asked.
	Advice *Verdict
	// Blind says whoever answered could not have seen Advice, which was
	// asked for afterwards.
	Blind bool
}

// State is where a workflow stands.
type State struct {
	// Started says the transcript holds an instance, named by Instance.
	Started  bool
	Instance string
	// Inputs is what the invocation carried, and Outputs what the done
	// workflow gives back.
	Inputs  any
	Outputs map[string]any
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
	// Begun says the transcript holds the message that opens this visit
	// of an Agent node, or opens a judge's look at a Person node.
	Begun bool
	// Advice is the judge's verdict on the question now being asked.
	Advice *Verdict
	// Held is a person's answer waiting for a judge to give its verdict
	// unseen, before the step finishes with it.
	Held *Result
	// Ended says flow_end has written down how this instance finished.
	Ended bool
	// abandoned is an earlier instance another was asked for over, with
	// nothing yet written of its end.
	abandoned *Ending
	// refused counts a judge's refused verdicts while an answer is held,
	// and talked says it replied in words.
	refused int
	talked  bool
	// unseen are the stretches of the transcript, ends included, where a
	// judge looked after a person had answered.
	unseen [][2]int
	// Steps are the finished nodes, oldest first.
	Steps []Step
}

// startPayload is flow_start's arguments.
type startPayload struct {
	Workflow string `json:"workflow"`
	Digest   string `json:"digest"`
	Inputs   any    `json:"inputs,omitempty"`
	Format   int    `json:"format"`
	// Definition is the graph itself, so the transcript says what ran
	// after the workflow has changed. The model is never shown it.
	Definition any `json:"definition,omitempty"`
}

// Invocation is the item that asks for a new instance, carrying its
// inputs, nil for none. A transcript with no instance starts one without
// it, with no inputs.
func (g *Graph) Invocation(inputs any) (openresponses.Item, error) {
	text := invocationPrefix + g.Name
	if inputs != nil {
		if err := checkData(g.Inputs, inputs); err != nil {
			return nil, fmt.Errorf("agentflow: inputs: %w", err)
		}
		b, err := json.Marshal(inputs)
		if err != nil {
			return nil, err
		}
		text += " " + string(b)
	}
	return openresponses.DeveloperText(text), nil
}

// invoked reads an invocation of g and the inputs it carries.
func (g *Graph) invoked(text string) (inputs any, ok bool) {
	rest, ok := strings.CutPrefix(text, invocationPrefix+g.Name)
	switch {
	case !ok || rest != "" && rest[0] != ' ':
		return nil, false
	case rest == "":
		return nil, true
	}
	return inputs, decode([]byte(rest), &inputs) == nil
}

// idPrefix starts every call ID. A long name is cut and told apart by
// its hash, since providers limit call IDs.
func (g *Graph) idPrefix() string {
	name := g.Name
	if len(name) > 16 {
		sum := sha256.Sum256([]byte(name))
		name = name[:9] + "-" + hex.EncodeToString(sum[:3])
	}
	return reserved + name + "_"
}

func (g *Graph) startID(instance string) string { return g.idPrefix() + instance + "_start" }

// callID is the ID of the call the program makes for an attempt.
func (g *Graph) callID(st State) string {
	id := g.idPrefix() + st.Instance + "_" + st.Node.ID + "_v" + strconv.Itoa(st.Visit)
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
		// unseen outlives an instance: a later one's model reads the
		// same transcript.
		unseen [][2]int
		// orphaned says something a run made is here with its start gone.
		orphaned bool
	)
	// finish ends the current node with step, at item i.
	finish := func(i int, step Step) {
		n := st.Node
		step.Node, step.Visit = n.ID, st.Visit
		st.Steps = append(st.Steps, step)
		since, st.Nudges, st.Begun, st.Advice, st.Held, st.refused, st.talked = i, 0, false, nil, nil, 0, false
		next := n.Next[step.Result.Outcome]
		if next == "" {
			st.Node, st.Visit, st.Attempt, st.Done = nil, 0, 0, true
			var err error
			if st.Outputs, err = st.resolveAll(g.Outputs); err != nil {
				failed = fmt.Errorf("agentflow: outputs: %w", err)
			}
			return
		}
		st.Node = g.node(next)
		visits[next]++
		st.Visit, st.Attempt = visits[next], 1
		if bound := g.visits(st.Node); st.Visit > bound {
			from := "its max_visits"
			if st.Node.MaxVisits == 0 {
				from = "the default for a step on a loop"
			}
			failed = fmt.Errorf("%w: step %q of workflow %q has run %d times, %s, and %q came back to it on %q; set max_visits for the step to change it",
				ErrVisits, next, g.Name, bound, from, n.ID, step.Result.Outcome)
		}
	}
	// release ends a held answer's step, with the verdict if one came.
	release := func(i int, advice *Verdict) {
		unseen = append(unseen, [2]int{since + 1, i})
		finish(i, Step{Result: *st.Held, Advice: advice, Blind: true})
	}
	for i, item := range t {
		switch it := item.(type) {
		case *openresponses.Message:
			if it.Role == openresponses.RoleDeveloper {
				if inputs, ok := g.invoked(it.Text()); ok {
					if st.Held != nil {
						unseen = append(unseen, [2]int{since + 1, i - 1})
					}
					left := st.abandoned
					if st.Started && !st.Ended {
						left = ending(st, failed)
					}
					st, failed, visits = State{Inputs: inputs, abandoned: left}, nil, map[string]int{}
					orphaned = false
					orphaned = false
					continue
				}
			}
			switch {
			// A judge that replied in words is waited on no longer: the
			// next thing said, the driver's or the person's, moves on.
			case !st.Started && it.Role == openresponses.RoleUser && strings.HasPrefix(it.Text(), beginPrefix):
				orphaned = true
			case st.Held != nil && it.Role == openresponses.RoleUser && (st.talked || strings.HasPrefix(it.Text(), noVerdict)):
				release(i, nil)
			case st.Held != nil && st.Begun && it.Role == openresponses.RoleAssistant:
				st.talked = true
			case it.Role == openresponses.RoleUser && strings.HasPrefix(it.Text(), nudgePrefix):
				st.Nudges++
			case st.Node != nil && opens(it, st.Node):
				st.Begun = true
			case it.Role == openresponses.RoleAssistant && st.Done && st.Ended:
				st.Reported = true
			}
		case *openresponses.FunctionCall:
			// Only the program makes these IDs. Its start and an earlier
			// run's end are the two that stand without a run.
			orphaned = orphaned || !st.Started && it.Name != startTool && it.Name != endTool && strings.HasPrefix(it.CallID, g.idPrefix())
			calls[it.CallID] = it
			at[it.CallID] = i
		case *openresponses.FunctionCallOutput:
			c := calls[it.CallID]
			// An ending counts however the call went: it is what was
			// found, and asking again would not make it more so.
			if c != nil && c.Name == endTool {
				var e Ending
				_ = decode([]byte(c.Arguments), &e)
				switch {
				case st.abandoned != nil && it.CallID == g.endID(st.abandoned.Instance):
					st.abandoned = nil
				case st.Started && it.CallID == g.endID(st.Instance):
					st.Ended = true
					// A step that failed with nobody to ask is over only
					// by saying so.
					if e.Status == Failed && failed == nil {
						failed = fmt.Errorf("%w: %s", ErrStepFailed, e.Reason)
					}
				}
				continue
			}
			if failed != nil || st.Done {
				continue
			}
			if !st.Started {
				var p startPayload
				if c == nil || c.Name != startTool || decode([]byte(c.Arguments), &p) != nil || p.Workflow != g.Name {
					continue
				}
				var res Result
				switch {
				case json.Unmarshal([]byte(it.Output.String()), &res) != nil || res.Outcome != OK:
					failed = fmt.Errorf("%w: start: %s", ErrStepFailed, it.Output.String())
				case p.Digest != digest || p.Format != RecordFormat:
					failed = ErrChanged
				default:
					instance := strings.TrimSuffix(strings.TrimPrefix(it.CallID, g.idPrefix()), "_start")
					st = State{Started: true, Instance: instance, Inputs: p.Inputs, Node: &g.Nodes[0], Visit: 1, Attempt: 1, abandoned: st.abandoned}
					visits[st.Node.ID], since = 1, i
				}
				continue
			}
			n := st.Node
			res, invalid := parseResult(n, it.Output.String())
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
			case st.Held != nil:
				// Nothing but a verdict, or two failures to give one,
				// moves a held answer on.
				if c == nil || c.Name != judgeTool || at[it.CallID] <= since {
					continue
				}
				if v, ok := verdictOf(n, c, it); ok && st.Begun {
					release(i, &v)
				} else if st.refused++; st.refused >= 2 {
					release(i, nil)
				}
				continue
			case n.Kind == Person && c != nil && c.Name == judgeTool:
				// A verdict advises; the question's own answer finishes
				// the step. The first one given for this visit counts.
				if v, ok := verdictOf(n, c, it); ok && st.Advice == nil && st.Begun && at[it.CallID] > since {
					st.Advice = &v
				}
				continue
			case n.Kind == Agent:
				// A finish that was rejected or is for another node is
				// not progress; the model saw the error and goes on.
				if invalid != nil || c == nil || c.Name != finishTool || finishStep(c.Arguments) != n.ID || at[it.CallID] < since {
					continue
				}
			default:
				if it.CallID != g.callID(st) {
					continue
				}
				if invalid != nil {
					st.Failure = it.Output.String()
					if json.Valid([]byte(st.Failure)) {
						st.Failure = invalid.Error() + ": " + st.Failure
					}
					continue
				}
			}
			// A person's answer to a question judged unseen waits for
			// the judge. An answer from anything else does not.
			if n.Kind == Person && res.By == "" && askedUnseen(c) {
				since, st.Nudges, st.Begun, st.Held = i, 0, false, &res
				continue
			}
			finish(i, Step{Result: res, Advice: st.Advice})
		}
	}
	st.unseen = unseen
	// Starting again would run every step a second time.
	if orphaned && !st.Started && failed == nil {
		return st, fmt.Errorf("%w: the transcript holds a run of %q without its start", ErrPositionLost, g.Name)
	}
	return st, failed
}

// verdictOf reads the verdict a flow_judge call on n produced.
func verdictOf(n *Node, c *openresponses.FunctionCall, out *openresponses.FunctionCallOutput) (v Verdict, ok bool) {
	ok = finishStep(c.Arguments) == n.ID && decode([]byte(out.Output.String()), &v) == nil &&
		(v.Outcome == HandBack || n.Judge != nil && n.judged(v) == nil)
	return v, ok
}

// askedUnseen says the question was put with its judge held back.
func askedUnseen(c *openresponses.FunctionCall) bool {
	var q Question
	return c != nil && c.Name == askTool && decode([]byte(c.Arguments), &q) == nil && q.Mode == Shadow.String()
}

// without drops the stretches of t a judge spent after a person had
// answered, so what it said reaches no later step.
func without(t openresponses.Items, unseen [][2]int) openresponses.Items {
	if len(unseen) == 0 {
		return t
	}
	out := make(openresponses.Items, 0, len(t))
	dropped := map[string]bool{}
	for i, item := range t {
		in := slices.ContainsFunc(unseen, func(s [2]int) bool { return i >= s[0] && i <= s[1] })
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			dropped[it.CallID] = in
		case *openresponses.FunctionCallOutput:
			// A call's output goes with it, wherever it landed.
			in = in || dropped[it.CallID]
		}
		if !in {
			out = append(out, item)
		}
	}
	return out
}

// visitTag tells one visit's messages from every other's. They must not
// read alike: agentturn quarantines by content, so after a resume a
// message like one that was once left out is left out too.
func visitTag(st State) string { return fmt.Sprintf(" (run %s, visit %d)", st.Instance, st.Visit) }

// beginning starts the message that opens a visit of an Agent node.
func beginning(id string) string { return beginPrefix + strconv.Quote(id) + "." }

// opens says m is the message that opens a visit of n.
func opens(m *openresponses.Message, n *Node) bool {
	return (n.Kind == Agent || n.Judge != nil) && m.Role == openresponses.RoleUser && strings.HasPrefix(m.Text(), beginning(n.ID))
}

func parseResult(n *Node, output string) (Result, error) {
	var res Result
	if err := decode([]byte(output), &res); err != nil {
		return Result{}, err
	}
	return res, n.check(res)
}

func finishStep(args string) string {
	var a finishArgs
	_ = json.Unmarshal([]byte(args), &a)
	return a.Step
}
