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
// and one without fails.
var ErrStalled = errors.New("agentflow: the model stopped without finishing its step")

// TriggerKind is the trigger of what a workflow says on its own.
const TriggerKind = "workflow"

// Drive takes a run that has ended and sees it through what a config
// alone cannot: a model that stops without finishing its step is
// reminded once. The reminder is a user message, which models heed far
// more than a developer one, sent under a trigger of kind [TriggerKind]
// so neither the loop nor a record takes it for the person's. It returns the last run's end and where
// the workflow stands. A run waiting on a question is returned as it
// is, for [Answer] and Resume. Of the options it reads [History], which
// a host that gave [Apply] one gives here too.
func Drive(ctx context.Context, g *Graph, a *agentturn.Agent, end *agentturn.RunEnd, opts ...Option) (*agentturn.RunEnd, State, error) {
	f := &flow{g: g}
	for _, opt := range opts {
		opt(f)
	}
	for {
		st, _, err := f.read(ctx, a.State().Transcript)
		switch {
		case end == nil:
			return nil, st, errors.New("agentflow: no run to drive")
		case end.Err != nil:
			return end, st, end.Err
		case err != nil:
			return end, st, err
		// A judge that was opened and gave no verdict has stopped too:
		// with no judge the question would be waiting, not done.
		case end.Reason != agentturn.ReasonDone || st.Node == nil ||
			st.Node.Kind != Agent && !(st.Node.Kind == Person && st.Begun && st.Advice == nil):
			return end, st, nil
		case st.Nudges > 0:
			return end, st, fmt.Errorf("%w: %s", ErrStalled, st.Node.ID)
		}
		call := finishTool
		if st.Node.Kind == Person {
			call = judgeTool
		}
		nudge := fmt.Sprintf("%s Text is not read. Do the step %q, then call %s.%s", nudgePrefix, st.Node.ID, call, visitTag(st))
		// A judge nobody is waiting on is not reminded: the person has
		// answered, and the workflow goes on.
		if st.Held != nil {
			nudge = noVerdict + visitTag(st)
		}
		from := agentturn.ContextWithTrigger(ctx, agentturn.Trigger{Kind: TriggerKind, Ref: g.Name})
		next, err := a.Prompt(from, openresponses.UserText(nudge))
		if next == nil {
			return end, st, err
		}
		end = next
	}
}
