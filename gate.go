package agentflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// makes holds a call that waits on a child to the one the program makes
// for st: the step's call, or with judge a judge's look. The run's key is
// the call's own, a new one each time; everything else is the program's
// to say, and a call that says otherwise is not its.
func (f *flow) makes(st State, c *openresponses.FunctionCall, judge bool) error {
	n := st.Node
	id, tool, what := st.callID(), callTool, fmt.Sprintf("step %q runs in a child", n.ID)
	if judge {
		id, tool, what = st.judgeID(), judgeTool, fmt.Sprintf("a judge looks at step %q in a child", n.ID)
	}
	var a callArgs
	if c.Name != tool || c.CallID != id || decode([]byte(c.Arguments), &a) != nil || !keyPattern.MatchString(a.Run) {
		return fmt.Errorf("%s, and this is not its call", what)
	}
	var want callArgs
	if judge {
		w, err := f.judgeCall(st, a.Run)
		if err != nil {
			return err
		}
		want = w
	} else {
		with, err := st.resolveAll(n.With)
		if err != nil {
			return err
		}
		want = st.childCall(with, a.Run)
	}
	if !sameJSON(a, want) {
		return fmt.Errorf("%s, and this is not the call the workflow makes for it", what)
	}
	return nil
}

// finishesBefore says a finish of st's step waits for its result ahead of
// the call id: the same response made both, the finish first.
func finishesBefore(t agentturn.Transcript, st State, id string) bool {
	waiting := false
	for _, item := range t {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			if it.CallID == id {
				return waiting
			}
			if (it.Name == finishTool || it.Name == stuckTool) && runOf(it.Arguments) == st.Instance && finishStep(it.Arguments) == st.Node.ID {
				waiting = true
			}
		case *openresponses.FunctionCallOutput:
			// A result ends the response before it: a finish there was
			// refused, or the step would be over.
			waiting = false
		}
	}
	return false
}

// What the gate says to a flow_end or a flow_shown that is not the
// program's. Replay reads neither call where its output holds the
// refusal: a call the gate turned away was nobody's to make.
const (
	endRefusal   = "only the workflow ends a run"
	shownRefusal = "only the workflow shows a step's result"
)

// turnedAway says out is the gate's refusal, for reason.
func turnedAway(out *openresponses.FunctionCallOutput, reason string) bool {
	return strings.Contains(out.Output.String(), "agentflow: "+reason)
}

// gate keeps each turn to what the workflow is at. It returns nil to
// leave the call to the config's own hook.
func (f *flow) gate(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	block := func(reason string) (*agentturn.ToolDecision, error) {
		return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "agentflow: " + reason}, nil
	}
	if info.Parent != "" {
		if info.Call.Name == ScriptTool || strings.HasPrefix(info.Call.Name, reserved) {
			return block("only the workflow calls " + info.Call.Name)
		}
		// A read the program makes for a step's files is held to the step's
		// policy, as the step's own would be.
		if readsFiles(info) {
			if st, err := f.current(ctx); err == nil && st.Node != nil {
				if why := f.police(ctx, st, info); why != "" {
					return block(fmt.Sprintf("step %q may not be given this file: %s", st.Node.ID, why))
				}
			}
			return nil, nil
		}
		// A call a model's tool makes is the model's work too, and is held
		// to the step's policy as the tool's own call was: a tool the step
		// offers does not carry the model past what the step refuses. The
		// step's list of tools is not asked, since it says what the model
		// is offered and a tool's own calls are how it is built. What a
		// program step's tool calls is the workflow's own, as the step is.
		if !strings.HasPrefix(info.Parent, reserved) {
			if st, err := f.current(ctx); err == nil && st.Started && !st.Done && st.Node != nil && st.Node.Kind == Agent && st.Failure == "" && st.Reached == nil {
				if why := f.police(ctx, st, info); why != "" {
					return block(fmt.Sprintf("step %q refuses this call: %s", st.Node.ID, why))
				}
			}
		}
		return nil, nil
	}
	t, ok := agentturn.TranscriptFromContext(ctx)
	if !ok {
		return nil, errors.New("agentflow: no transcript on the context")
	}
	st, _, err := f.read(ctx, t)
	name, id := info.Call.Name, info.Call.CallID
	if name == shownTool {
		if st.show == nil || id != st.show.id() {
			return block(shownRefusal)
		}
		return nil, nil
	}
	if name == endTool {
		if over := f.overdue(st, err); over == nil || id != over.id {
			return block(endRefusal)
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if name == overrideTool {
		if st.request == nil || id != st.requestID {
			return block("only the workflow applies an operator's override, when one asks")
		}
		return nil, nil
	}
	n := st.Node
	switch {
	case st.Done && st.Reported, st.conversing():
		if strings.HasPrefix(name, reserved) || name == ScriptTool {
			return block("only a running workflow calls " + name)
		}
	case st.Done:
		return block("the workflow has finished")
	case !st.Started:
		if name != startTool || !strings.HasPrefix(id, reserved) {
			return block("the workflow has not started")
		}
	case st.Reached != nil:
		if name != askTool || id != st.limitID() {
			return block(fmt.Sprintf("step %q has reached its bound on %s", n.ID, st.Reached.Limit))
		}
		return f.ask(ctx, info, st)
	case st.Failure != "":
		if name != askTool || id != st.retryID() {
			return block(fmt.Sprintf("step %q failed", n.ID))
		}
		return f.ask(ctx, info, st)
	case n.Flow != "" && name == clockTool:
		if id == st.clockID() && f.clockDue(st) {
			return nil, nil
		}
		return block("only the workflow reads the clock")
	case f.judging(ctx, st):
		// The judge looks in a child, and the call waits on it.
		if err := f.makes(st, info.Call, true); err != nil {
			return block(err.Error())
		}
		return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: fmt.Sprintf("agentflow: a judge looks at step %q in a child", n.ID)}, nil
	case id == st.callID() && n.inChild() && f.policyErr(st) != nil:
		// A policy read from the record that does not resolve fails the
		// call, which asks to retry: a child is never opened under it.
		return block(fmt.Sprintf("step %q: %v", n.ID, f.policyErr(st)))
	case n.Dir.set() && id == st.callID() && (n.inChild() || n.Script != ""):
		// The directory is read from the record as the call is made: one
		// that is no path below the root fails the step, which asks to
		// retry. A child works in the one its call recorded.
		if _, err := st.dirOf(n); err != nil {
			return block(fmt.Sprintf("step %q: %v", n.ID, err))
		}
		if n.Script != "" {
			return nil, nil
		}
		if err := f.makes(st, info.Call, false); err != nil {
			return block(err.Error())
		}
		return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: fmt.Sprintf("agentflow: step %q runs in a child", n.ID)}, nil
	case n.inChild():
		// The work is done in a child, and the call waits on it.
		if err := f.makes(st, info.Call, false); err != nil {
			return block(err.Error())
		}
		return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: fmt.Sprintf("agentflow: step %q runs in a child", n.ID)}, nil
	case n.Kind == Agent:
		finish, verb := finishTool, "on"
		if n.Verdict {
			finish, verb = judgeTool, "judging"
		}
		if name == filesTool {
			if id == st.filesID() && f.filesDue(st) {
				return nil, nil
			}
			return block("only the workflow gives a step its files")
		}
		if name == limitsTool {
			if id == st.limitsID() && f.limitsDue(st) {
				return nil, nil
			}
			return block("only the workflow says a step's policy cannot be read")
		}
		if name == clockTool {
			if id == st.clockID() && f.clockDue(st) || id == st.timeoutID() && f.expired(st) {
				return nil, nil
			}
			return block("only the workflow reads the clock")
		}
		if name == docReadTool || name == docWriteTool {
			if err := checkDocCall(st, name, info.Args); err != nil {
				return block(err.Error())
			}
			// A write after the step's finish in one response would be
			// answered as written and then not read, the step being over.
			if name == docWriteTool && finishesBefore(t, st, id) {
				return block(fmt.Sprintf("this write came after the step's finish in the same response, and was not kept: write a document before you finish step %q", n.ID))
			}
			return nil, nil
		}
		if name == stuckTool && n.canStick() {
			name = finish
		}
		if name == finish {
			if step := finishStep(info.Call.Arguments); step != n.ID {
				return block(fmt.Sprintf("you are %s step %q, not %q", verb, n.ID, step))
			}
			if run := runOf(info.Call.Arguments); run != st.Instance {
				return block(fmt.Sprintf("you are %s step %q of run %q, not of run %q", verb, n.ID, st.Instance, run))
			}
			return nil, nil
		}
		if !st.g.offers(n, name) {
			return block(fmt.Sprintf("step %q does not allow %s", n.ID, name))
		}
		if why := f.police(ctx, st, info); why != "" {
			return block(fmt.Sprintf("step %q refuses this call: %s", n.ID, why))
		}
	case n.Kind == Person && name == clockTool:
		if id == st.clockID() && f.askClockDue(ctx, st) {
			return nil, nil
		}
		return block("only the workflow reads the clock")
	case id != st.callID():
		return block(fmt.Sprintf("step %q takes no other calls", n.ID))
	case n.Kind == Person:
		return f.ask(ctx, info, st)
	}
	return nil, nil
}
