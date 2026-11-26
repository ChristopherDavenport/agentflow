package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// Override is an operator's request to change where a run stands: run
// its current step again, jump to another step with a note, or abandon
// it. It reaches the program as a developer message, which no model
// writes, and the program applies it with a call, flow_override, whose
// result says whether it was applied; replay reads both.
type Override struct {
	// Run is the key of the run to change. Retry and jump change the
	// innermost run under way; abandon may name any run under way, and
	// abandons it and every run it called.
	Run    string `json:"run"`
	Action string `json:"action"`
	// Step is where a jump goes, and Note what the operator says of it,
	// which the step is shown: a model step in its opening message, a
	// person in the question.
	Step string `json:"step,omitempty"`
	Note string `json:"note,omitempty"`
}

// The actions of an [Override].
const (
	OverrideRetry   = "retry"
	OverrideJump    = "jump"
	OverrideAbandon = "abandon"
)

const (
	overridePrefix = "agentflow: override "
	overrideTool   = "flow_override"
)

// overrideActions are the actions an override may take.
var overrideActions = []string{OverrideRetry, OverrideJump, OverrideAbandon}

// check holds an override to its own shape.
func (o Override) check() error {
	switch {
	case !keyPattern.MatchString(o.Run):
		return fmt.Errorf("agentflow: override: run %q is not a run's key", o.Run)
	case !slices.Contains(overrideActions, o.Action):
		return fmt.Errorf("agentflow: override: action %q%s: want one of %s", o.Action, suggest(o.Action, overrideActions), strings.Join(overrideActions, ", "))
	case o.Action == OverrideJump && o.Step == "":
		return errors.New("agentflow: override: a jump names the step it goes to")
	case o.Action != OverrideJump && o.Step != "":
		return errors.New("agentflow: override: only a jump names a step")
	}
	return nil
}

// OverrideRequest is the item that asks the program to apply o: a
// developer message, which no model can write.
func OverrideRequest(o Override) (openresponses.Item, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(o)
	if err != nil {
		return nil, err
	}
	return openresponses.DeveloperText(overridePrefix + string(b)), nil
}

// requested reads an override request from a developer message.
func requested(text string) (Override, bool) {
	rest, ok := strings.CutPrefix(text, overridePrefix)
	if !ok {
		return Override{}, false
	}
	var o Override
	if decode([]byte(rest), &o) != nil || o.check() != nil {
		return Override{}, false
	}
	return o, true
}

// target is the frame under way an override names, and why it cannot be
// applied, if it cannot.
func (r *replay) target(o Override) (*frame, error) {
	k := slices.IndexFunc(r.stack, func(f *frame) bool { return f.st.Instance == o.Run })
	if k < 0 {
		return nil, fmt.Errorf("run %q is not under way here", o.Run)
	}
	f := r.stack[k]
	switch {
	case r.abort != nil:
		return nil, errors.New("the runs here are being abandoned")
	case o.Action == OverrideAbandon:
		return f, nil
	case f != r.top():
		return nil, fmt.Errorf("run %q waits on run %q, which it called: override that one, or abandon", o.Run, r.top().st.Instance)
	case f.failed != nil || f.st.Done || f.st.Node == nil:
		return nil, fmt.Errorf("run %q is at no step", o.Run)
	}
	if o.Action == OverrideJump {
		n := f.st.g.node(o.Step)
		switch {
		case n == nil:
			return nil, fmt.Errorf("workflow %s has no step %q%s", f.st.Workflow, o.Step, suggest(o.Step, f.st.g.nodeIDs()))
		case f.st.g.cleans(n.ID):
			return nil, fmt.Errorf("step %q is on the cleanup path, which only an abandon starts", o.Step)
		}
		// A jump goes only where the step's inputs are on the record.
		if _, err := f.st.resolveAll(n.With); err != nil {
			return nil, fmt.Errorf("step %q cannot run from here: %w", o.Step, err)
		}
		if c := n.Choices; c != nil {
			if _, _, err := f.st.resolve(c.From); err != nil {
				return nil, fmt.Errorf("step %q cannot run from here: %w", o.Step, err)
			}
		}
	}
	return f, nil
}

// overridden applies the override flow_override's result says was, at
// item i.
func (r *replay) overridden(i int, c *openresponses.FunctionCall, out *openresponses.FunctionCallOutput) {
	r.overrides++
	o := r.request
	r.request = nil
	var res Result
	if o == nil || decode([]byte(out.Output.String()), &res) != nil || res.Outcome != OK {
		r.reask()
		return
	}
	f, err := r.target(*o)
	if err != nil {
		return
	}
	st := &f.st
	st.reask = false
	switch o.Action {
	case OverrideAbandon:
		var next *frame
		if f == r.root {
			// Nothing starts until the workflow is invoked again, and once
			// the program has said so the conversation is the model's, as
			// after a run that finished.
			next = r.newRoot(nil)
			next.st.Left, next.st.released = true, f.st.Instance
		}
		r.abandon(i, f, next, overriddenFor(o.Action))
		return
	case OverrideRetry:
		n := st.Node
		st.Attempt++
		if n.Kind == Agent {
			f.since, st.Nudges, st.Begun, st.refused = i, 0, false, 0
		}
		r.entries++
		st.treeSteps++
	case OverrideJump:
		f.since, st.Nudges, st.Begun, st.refused = i, 0, false, 0
		f.came = "an operator jumped to it"
	}
	st.Failure, st.Reached, st.Held, st.Advice = "", nil, nil, nil
	st.Callee, st.Returned, st.calleeInputs = "", nil, nil
	st.clocked, st.Deadline, st.asked, st.filed = false, time.Time{}, 0, false
	if o.Action == OverrideJump {
		r.enter(f, i, st.g.node(o.Step))
	} else {
		r.bounds(f)
	}
	st.Note = o.Note
}

// reask puts again the question the host set aside for an override that
// was refused, under a new ID, as the call it was could not be answered.
func (r *replay) reask() {
	f := r.top()
	st := &f.st
	if !st.reask {
		return
	}
	st.reask = false
	switch {
	case st.Reached != nil:
		st.asked++
	case st.Failure != "":
		st.reasked++
	case st.Node != nil && st.Node.Kind == Person:
		st.Attempt++
		st.clocked, st.Deadline = false, time.Time{}
	}
}

// overriddenText is the output of a call the host set aside to ask for
// an override.
const overriddenText = "agentflow: overridden"

// overriddenFor is the reason the end of a run an override abandoned
// records, the children set aside for one included.
func overriddenFor(action string) string { return "overridden: " + action }

// overrideID is the ID of the call that applies the override pending.
func (r *replay) overrideID() string {
	return r.top().st.prefix + "override" + strconv.Itoa(r.overrides+1)
}

// overrideTool applies an operator's override, or says why it cannot.
// Its arguments are the request, and replay applies it from its result.
func (f *flow) overrideTool() agenttool.Tool {
	return agenttool.New(overrideTool, "Apply an operator's override.", func(ctx context.Context, o Override) (Result, error) {
		if err := exact(ctx, &o); err != nil {
			return Result{}, err
		}
		st, err := f.current(ctx)
		if err != nil {
			return Result{}, err
		}
		switch {
		case st.request == nil || *st.request != o:
			return Result{}, errors.New("agentflow: no such override was asked for")
		case st.requestErr != nil:
			return Result{}, fmt.Errorf("agentflow: override refused: %w", st.requestErr)
		}
		return Result{Outcome: OK, Reason: "applied by the operator's request"}, nil
	}, agenttool.WithReplay(safeReplay))
}

// noteText is what an override's note tells the step it brought the run
// to, "" for none.
func noteText(st State) string {
	if st.Note == "" {
		return ""
	}
	return " Note from the operator: " + strconv.Quote(st.Note) + "."
}

// OverrideRun asks for o, wherever the run it names is under way: in a's
// session, or in a child a call there waits on, at any depth. Pending
// calls are refused and the children they wait on abandoned, as for an
// invocation, so the run stands still while the override is applied;
// then the program applies it, or refuses it with its reason, and drives
// on as [Drive] does.
func OverrideRun(ctx context.Context, g *Graph, a *agentturn.Agent, o Override, opts ...Option) (*agentturn.RunEnd, State, error) {
	f, err := flowOf(g, a, opts)
	if err != nil {
		return nil, State{}, err
	}
	return f.overrule(ctx, a, o)
}

func (f *flow) overrule(ctx context.Context, a *agentturn.Agent, o Override) (*agentturn.RunEnd, State, error) {
	item, err := OverrideRequest(o)
	if err != nil {
		return nil, State{}, err
	}
	all, err := f.record(ctx, a.State().Transcript)
	if err != nil {
		return nil, State{}, err
	}
	r := f.g.replay(all)
	if slices.ContainsFunc(r.stack, func(fr *frame) bool { return fr.st.Instance == o.Run }) {
		outs, err := f.setAside(ctx, a, overriddenText, overriddenFor(o.Action))
		if err != nil {
			return nil, State{}, err
		}
		end, err := a.Prompt(ctx, append(outs, item)...)
		if end == nil {
			st, _, _ := f.read(ctx, a.State().Transcript)
			return nil, st, err
		}
		return f.drive(ctx, a, end)
	}
	st := r.state(r.top())
	p, args, ok := heldChild(a.State().Pending)
	if !ok {
		return nil, st, fmt.Errorf("agentflow: run %q is not under way here, nor in a child a call waits on", o.Run)
	}
	c, err := f.childFor(st, p.Call, args)
	if err != nil {
		return nil, st, err
	}
	cctx, child, cf, err := f.open(ctx, c)
	if err != nil {
		return nil, st, err
	}
	end, cst, err := cf.overrule(cctx, child, o)
	if err != nil || end == nil || end.Reason == agentturn.ReasonInputRequired {
		return end, cst, err
	}
	// The child's run went on to its end: the call that waits on it goes.
	return f.drive(ctx, a, nil)
}
