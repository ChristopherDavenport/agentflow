package agentflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// ErrStalled is a model that stopped, and stopped again when reminded,
// without finishing its step. The step is still current: a host with a
// person shows them what the model said and prompts with their reply,
// and one without fails. A judge that stalls has given no verdict.
var ErrStalled = errors.New("agentflow: the model stopped without finishing its step")

// TriggerKind is the trigger of what a workflow says on its own.
const TriggerKind = "workflow"

// Drive takes a run that has ended and sees it through what a config
// alone cannot. A model that stops without finishing its step is
// reminded once. The reminder is a user message, which models heed far
// more than a developer one, sent under a trigger of kind [TriggerKind]
// so neither the loop nor a record takes it for the person's. A call the
// program holds on a child is worked on: the child is opened with the
// host's [Children], driven the same way, and the call let go with what
// came of it.
//
// A nil end drives an agent as it stands, one restored with calls
// pending, say.
//
// It returns the end and state of the run that waits or stopped: a's, or
// a child's waiting on a question, whose pending calls are answered with
// [Resume]. It uses the options a was given by [Apply], with opts added;
// of them it reads [History], which a host that gave Apply one gives
// here too.
func Drive(ctx context.Context, g *Graph, a *agentturn.Agent, end *agentturn.RunEnd, opts ...Option) (*agentturn.RunEnd, State, error) {
	f, err := flowOf(g, a, opts)
	if err != nil {
		return end, State{}, err
	}
	return f.drive(ctx, a, end)
}

// flowOf is the flow a follows for g, as Apply made it, with opts added.
func flowOf(g *Graph, a *agentturn.Agent, opts []Option) (*flow, error) {
	var f *flow
	if made, ok := a.Config().Model.(*flow); ok && made.g == g {
		own := *made
		f = &own
	} else {
		f = &flow{g: g}
	}
	for _, opt := range opts {
		opt(f)
	}
	if f.modes == nil {
		if err := f.delegate(); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (f *flow) drive(ctx context.Context, a *agentturn.Agent, end *agentturn.RunEnd) (*agentturn.RunEnd, State, error) {
	if pending := a.State().Pending; end == nil && len(pending) > 0 {
		end = &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: pending}
	}
	for {
		st, _, err := f.read(ctx, a.State().Transcript)
		switch {
		case end == nil:
			return nil, st, errors.New("agentflow: no run to drive")
		case errors.Is(end.Err, ErrStepTimeout) && err == nil:
			// The model was cut off at its step's deadline: the program
			// records the step's failure, and asks about it.
			next, err := a.Continue(ctx)
			if next == nil {
				return end, st, err
			}
			end = next
			continue
		case end.Err != nil:
			return end, st, end.Err
		case err != nil:
			return end, st, err
		case end.Reason == agentturn.ReasonInputRequired && f.callExpired(st) != nil:
			// A call the waiting run is nested in has run out of time: what
			// waits is set aside, and the program says so, which abandons the
			// runs the call waits on.
			outs, err := f.setAside(ctx, a, timedOutText, f.callExpired(st).why())
			if err != nil {
				return end, st, err
			}
			next, err := a.Prompt(ctx, outs...)
			if next == nil {
				return end, st, err
			}
			end = next
			continue
		case end.Reason == agentturn.ReasonInputRequired:
			// A question whose time has run out takes its timeout outcome.
			expired, err := f.expire(end.Pending)
			if err != nil {
				return end, st, err
			}
			if len(expired) > 0 {
				next, err := a.Resume(ctx, expired...)
				if next == nil {
					return end, st, err
				}
				end = next
				continue
			}
			p, args, ok := heldChild(end.Pending)
			if !ok {
				return end, st, nil
			}
			ans, wait, wst, err := f.settle(ctx, st, p, args, nil)
			if ans == nil {
				return wait, wst, err
			}
			next, err := a.Resume(ctx, *ans)
			if next == nil {
				return end, st, err
			}
			end = next
			continue
		case end.Reason != agentturn.ReasonDone || st.Node == nil || st.Node.Kind != Agent || st.Node.fresh():
			return end, st, nil
		case st.Nudges > 0:
			return end, st, fmt.Errorf("%w: %s", ErrStalled, st.Node.ID)
		}
		call := finishTool
		if st.Node.Verdict {
			call = judgeTool
		}
		nudge := fmt.Sprintf("%s Text is not read. Do the step %q, then call %s.%s", nudgePrefix, st.Node.ID, call, visitTag(st))
		from := agentturn.ContextWithTrigger(ctx, agentturn.Trigger{Kind: TriggerKind, Ref: f.g.Name})
		next, err := a.Prompt(from, openresponses.UserText(nudge))
		if next == nil {
			return end, st, err
		}
		end = next
	}
}
