package agentflow

import (
	"cmp"
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

// SummaryFilter shapes what a summarizer reads of a workflow. Give it
// to the compactor that writes the summary, as compact.WithFilter takes
// it. It does [agentturn.DefaultFilter]'s work too, which that option
// replaces.
//
// The workflow's own messages, the invocation, each step's opening
// message and the reminders, are left out: a summary that took one in
// would tell a later step that an earlier one is under way. A call the
// model did not make, the program's and a judge's, is replaced with its
// result by a note saying whose it was, since a summarizer cannot tell
// and writes that the assistant did it.
//
// It shapes only what the summarizer reads. The request keeps its items
// and its hash.
func SummaryFilter(t agentturn.Transcript) agentturn.Transcript {
	items := agentturn.DefaultFilter(t)
	outputs := map[string]string{}
	others := map[string]bool{}
	for _, item := range items {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			others[it.CallID] = others[it.CallID] || notTheModels(it)
		case *openresponses.FunctionCallOutput:
			outputs[it.CallID] = it.Output.String()
		}
	}
	var out agentturn.Transcript
	noted := map[string]bool{}
	for _, item := range items {
		switch it := item.(type) {
		case *openresponses.Message:
			if notice(it) {
				continue
			}
		case *openresponses.FunctionCall:
			if others[it.CallID] {
				result, answered := outputs[it.CallID]
				noted[it.CallID] = true
				out = append(out, openresponses.DeveloperText(note(it, result, answered)))
				continue
			}
		case *openresponses.FunctionCallOutput:
			switch {
			case noted[it.CallID]:
				continue
			// A program's result whose call is not among these.
			case strings.HasPrefix(it.CallID, reserved):
				out = append(out, openresponses.DeveloperText(recordPrefix+"a step the workflow ran, not the assistant, gave: "+it.Output.String()))
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

// recordPrefix starts a note [SummaryFilter] writes in place of a call.
const recordPrefix = "Workflow record: "

// notTheModels says the working model did not make c: the program makes
// IDs with the reserved prefix, and only a judge calls flow_judge.
func notTheModels(c *openresponses.FunctionCall) bool {
	return strings.HasPrefix(c.CallID, reserved) || c.Name == judgeTool
}

// note says what a call the model did not make did, and who made it.
func note(c *openresponses.FunctionCall, result string, answered bool) string {
	if !answered {
		result = "no result yet"
	}
	var res Result
	_ = decode([]byte(result), &res)
	switch c.Name {
	case judgeTool:
		var a judgeArgs
		_ = decode([]byte(c.Arguments), &a)
		return fmt.Sprintf("%sa separate judge model, not the assistant, gave a verdict on step %q: %s, giving the reason %q", recordPrefix, a.Step, a.Outcome, a.Reason)
	case askTool:
		var q Question
		_ = decode([]byte(c.Arguments), &q)
		if res.Outcome == "" {
			return fmt.Sprintf("%sthe workflow, not the assistant, asked %q (step %q): %s", recordPrefix, q.Question, q.Step, result)
		}
		who := map[string]string{"": "the person", ByJudge: "a judge model", "rules": "the host's rules", "unattended default": "the unattended default"}[res.By]
		said := fmt.Sprintf("%sthe workflow, not the assistant, asked %q (step %q). %s answered %s", recordPrefix, q.Question, q.Step, cmp.Or(who, res.By), res.Outcome)
		if res.Reason != "" {
			said += fmt.Sprintf(", giving the reason %q", res.Reason)
		}
		return said
	case endTool:
		var e Ending
		_ = decode([]byte(c.Arguments), &e)
		return fmt.Sprintf("%sthe workflow run ended: %s. %s", recordPrefix, e.Status, e.Reason)
	case ScriptTool:
		var sc ScriptCall
		_ = decode([]byte(c.Arguments), &sc)
		return fmt.Sprintf("%sthe workflow, not the assistant, ran the script %s. Result: %s", recordPrefix, sc.Run, result)
	}
	return fmt.Sprintf("%sthe workflow, not the assistant, called %s. Result: %s", recordPrefix, c.Name, result)
}

// notice says item is something the workflow says to the model.
func notice(m *openresponses.Message) bool {
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
