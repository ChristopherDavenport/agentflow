package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// ErrRecordBehind is a record [History] returned that does not hold what
// the transcript beside it does: a recorder that has stopped, or another
// conversation's. Position read from it would run steps again.
var ErrRecordBehind = errors.New("agentflow: the record does not hold the transcript's calls")

// History gives the workflow the record: every item that has happened,
// oldest first, such as the item entries of a session's path. Position,
// step inputs and the gate are then read from it, and the transcript is
// only what a model is shown, so a compaction or a session resumed with
// items left out loses the workflow nothing.
//
// It must be current when called: it is, for a recorder attached to the
// agent and a store that reads what was written. It is called several
// times a turn. [Drive] takes the same option.
func History(read func(context.Context) (agentturn.Transcript, error)) Option {
	return func(f *flow) { f.history = read }
}

// record is what position is read from: the history, held to be at least
// what local holds, or without one local itself.
func (f *flow) record(ctx context.Context, local agentturn.Transcript) (agentturn.Transcript, error) {
	if f.history == nil {
		return local, nil
	}
	all, err := f.history(ctx)
	if err != nil {
		return nil, fmt.Errorf("agentflow: history: %w", err)
	}
	calls, outputs := map[string]bool{}, map[string]bool{}
	for _, item := range all {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			calls[it.CallID] = true
		case *openresponses.FunctionCallOutput:
			outputs[it.CallID] = true
		}
	}
	for _, item := range local {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			if !calls[it.CallID] {
				return nil, fmt.Errorf("%w: call %s", ErrRecordBehind, it.CallID)
			}
		case *openresponses.FunctionCallOutput:
			if !outputs[it.CallID] {
				return nil, fmt.Errorf("%w: the output of %s", ErrRecordBehind, it.CallID)
			}
		}
	}
	return all, nil
}

// read is where the workflow stands, and the record it was read from.
func (f *flow) read(ctx context.Context, local agentturn.Transcript) (State, agentturn.Transcript, error) {
	all, err := f.record(ctx, local)
	if err != nil {
		return State{}, nil, err
	}
	st, err := f.g.Replay(all)
	return st, all, err
}

// visible drops from input what no model may read: flow_start, and what
// a judge did after a person had answered.
func (f *flow) visible(input openresponses.Items, record agentturn.Transcript, st State) openresponses.Items {
	if f.history == nil {
		// The input is what was replayed, so its places are the state's.
		return withoutStarts(without(input, st.unseen))
	}
	// The input is a view of the record, so items are known by what they
	// are rather than where.
	shown := map[string]bool{}
	for _, item := range withoutStarts(without(openresponses.Items(record), st.unseen)) {
		shown[itemKey(item)] = true
	}
	hidden := map[string]bool{}
	for _, item := range record {
		if k := itemKey(item); !shown[k] {
			hidden[k] = true
		}
	}
	if len(hidden) == 0 {
		return input
	}
	out := make(openresponses.Items, 0, len(input))
	for _, item := range input {
		if !hidden[itemKey(item)] {
			out = append(out, item)
		}
	}
	return out
}

// itemKey names an item the same wherever it is read from.
func itemKey(item openresponses.Item) string {
	switch it := item.(type) {
	case *openresponses.FunctionCall:
		return "call " + it.CallID
	case *openresponses.FunctionCallOutput:
		return "output " + it.CallID
	case *openresponses.Message:
		if it.ID != "" {
			return "message " + it.ID
		}
		return "said " + string(it.Role) + " " + it.Text()
	case *openresponses.ReasoningItem:
		if it.ID != "" {
			return "reasoning " + it.ID
		}
	}
	b, _ := json.Marshal(item)
	return item.ItemType() + " " + string(b)
}

// ownInput says the step's model reads only from its opening message, so
// there is nothing of the conversation to shorten.
func (f *flow) ownInput(st State) bool {
	return f.judging(st) || modelTurn(st) && st.Node != nil && st.Node.Context == ContextFresh
}

// transform runs the host's. With a history it is given only what a
// model may read, so nothing it summarises can hand back what was kept
// from one. Without, what it returns must still say where the workflow
// stands.
func (f *flow) transform(host func(context.Context, agentturn.Transcript) (agentturn.Transcript, error)) func(context.Context, agentturn.Transcript) (agentturn.Transcript, error) {
	return func(ctx context.Context, t agentturn.Transcript) (agentturn.Transcript, error) {
		if f.history == nil {
			out, err := host(ctx, t)
			if err != nil {
				return nil, err
			}
			return out, f.samePosition(t, out)
		}
		st, all, err := f.read(ctx, t)
		if all == nil {
			return nil, err
		}
		// A failed run's error is the request's to report.
		if f.ownInput(st) {
			return t, nil
		}
		return host(ctx, agentturn.Transcript(f.visible(openresponses.Items(t), all, st)))
	}
}

// SummaryFilter keeps the workflow's own messages out of what is
// summarised: the invocation, each step's opening message and the
// reminders. A summary that took one in would tell a later step, in the
// person's voice, that an earlier one is under way. Give it to the
// compactor that writes the summary, as compact.WithFilter takes it. It
// does [agentturn.DefaultFilter]'s work too, which that option
// replaces.
//
// It shapes only what the summarizer reads. The request keeps its
// messages and its hash.
func SummaryFilter(t agentturn.Transcript) agentturn.Transcript {
	return slices.DeleteFunc(slices.Clone(agentturn.DefaultFilter(t)), notice)
}

// notice says item is something the workflow says to the model.
func notice(item openresponses.Item) bool {
	m, ok := item.(*openresponses.Message)
	if !ok {
		return false
	}
	prefixes := []string{beginPrefix, nudgePrefix, noVerdict}
	switch m.Role {
	case openresponses.RoleDeveloper:
		prefixes = []string{invocationPrefix}
	case openresponses.RoleUser:
	default:
		return false
	}
	return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(m.Text(), p) })
}
