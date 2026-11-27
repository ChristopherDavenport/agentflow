package agentflow

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// begin opens a visit of an Agent node with a message in the transcript.
// Without it a model reads its last step's flow_finish under a tool that
// now accepts only this step's, and takes that call for a mistake. It is
// also where a fresh context starts. It is a user message, since a
// provider may move a leading developer message out of the conversation,
// and a hook's, so no record takes it for the person's.
func (f *flow) begin(ctx context.Context, info agentturn.TurnStartInfo) (openresponses.Items, error) {
	st, _, err := f.read(ctx, info.Transcript)
	if err != nil || st.Node == nil || st.Failure != "" || st.Reached != nil || st.Begun {
		// An error here is the same one prepare reports.
		return nil, nil
	}
	switch {
	case st.Node.Kind != Agent || st.Node.fresh():
		return nil, nil
	}
	text := beginning(st.Node.ID)
	if st.Node.Verdict {
		return openresponses.Items{openresponses.UserText(text + " Judge it now." + visitTag(st))}, nil
	}
	text += noteText(st)
	if len(st.Steps) > 0 {
		text += " The steps before it are finished and their results accepted, the " + finishTool + " calls included."
	}
	return openresponses.Items{openresponses.UserText(text + " Do this step now." + visitTag(st))}, nil
}

// overdue is the ending that has yet to be written, nil when none has.
func (f *flow) overdue(st State, failed error) *Ending {
	switch {
	case st.endNow:
		return ending(st, failed)
	case !st.Started || st.Ended || errors.Is(failed, ErrChanged):
		return nil
	case failed != nil || st.Done:
		return ending(st, failed)
	case st.Failure != "" && f.unattended && f.reply == nil:
		e := ending(st, nil)
		e.Status, e.Reason, e.Step = Failed, st.Node.ID+": "+st.Failure, st.Node.ID
		return e
	case st.Reached != nil && f.unattended && f.reply == nil:
		e := ending(st, nil)
		e.Status, e.Reason, e.Step, e.Limit = Failed, st.Reached.Reason, st.Node.ID, st.Reached.Limit
		return e
	}
	return nil
}

// modelTurn says the model takes the next turn.
func modelTurn(st State) bool {
	return st.Done && st.Reported || st.conversing() || st.Node != nil && st.Node.Kind == Agent && !st.Node.fresh() && st.Failure == "" && st.Reached == nil
}

// conversing says an operator abandoned the run nobody called, and the
// program has said so: the conversation is the model's, with its tools.
func (st State) conversing() bool { return !st.Started && st.released != "" && st.Reported }

// clockDue says the program reads the clock before st's model step
// works, and expired that the step's time has run out.
//
// A call with a timeout reads the clock before it calls, so the deadline
// holds the run it calls from its start.
func (f *flow) clockDue(st State) bool {
	if n := st.Node; n != nil && n.Flow != "" {
		return n.Timeout > 0 && !st.clocked && st.Failure == "" && st.Reached == nil && !st.Done
	}
	return modelTurn(st) && st.Node != nil && !st.clocked && f.timeout(st) > 0
}

// due is the time st's step must be done by, zero for none.
func (st State) due() time.Time { return st.Deadline }

func (f *flow) expired(st State) bool {
	return modelTurn(st) && st.Node != nil && !st.Deadline.IsZero() && !time.Now().Before(st.Deadline)
}

// timeout is how long st's step may take: its own timeout, or for a model
// step with nobody there, the host's default.
//
// A script, tool or model step that sets none takes its workflow's.
func (f *flow) timeout(st State) time.Duration {
	n := st.Node
	switch {
	case n == nil:
		return 0
	case n.Timeout > 0:
		return n.Timeout
	case st.g != nil && st.g.Timeout > 0 && (n.Kind == Agent && !n.Verdict || n.Kind == Run && n.Flow == ""):
		return st.g.Timeout
	case n.Kind == Agent && f.unattended:
		if f.idle != nil {
			return *f.idle
		}
		return DefaultUnattendedTimeout
	}
	return 0
}

// prepare shapes the request for whoever takes this turn. It runs
// before the turn is recorded, so the record holds what was sent.
func (f *flow) prepare(ctx context.Context, req *openresponses.Request) error {
	st, all, err := f.read(ctx, req.Input)
	nt, err := f.next(ctx, st, err)
	if err != nil {
		return err
	}
	if nt.by != theModel {
		req.Model = programPrefix + f.g.Name
		return nil
	}
	if f.due != nil {
		f.due.set(st.due(), st.Instance)
	}
	req.Model = f.modelName
	view, err := f.visible(ctx, req.Input, all, st)
	if err != nil {
		return err
	}
	req.Input = keepFiles(view, all, st)
	if c := f.child; c != nil && c.Settings.Instructions != "" {
		req.Instructions = strings.TrimSpace(req.Instructions + "\n\n" + c.Settings.Instructions)
	}
	keep := func(name string) bool { return !strings.HasPrefix(name, reserved) && name != ScriptTool }
	if n := st.Node; n != nil {
		step, err := f.instructions(st)
		if err != nil {
			return err
		}
		req.Instructions = strings.TrimSpace(req.Instructions + "\n\n" + step)
		// The step's model and thinking are in the request, so the record
		// and its hash hold them.
		req.Model = cmp.Or(n.Model, st.g.Model, f.modelName)
		if thinking := cmp.Or(n.Thinking, st.g.Thinking); thinking != "" {
			req.Reasoning.SetExtendedEffort("")
			if thinking == openresponses.EffortMax {
				req.Reasoning.SetExtendedEffort(thinking)
			} else {
				req.Reasoning.Effort = openresponses.ReasoningEffort(thinking)
			}
		}
		finish, more := finishTool, map[string]any(nil)
		if n.Verdict {
			// The verdict takes the shape of what the judge may choose.
			finish, more = judgeTool, map[string]any{"reason": map[string]any{"type": "string"}}
		}
		keep = func(name string) bool {
			return name == finish || name == stuckTool && n.canStick() || st.g.offers(n, name) ||
				name == docReadTool && n.documented() || name == docWriteTool && len(n.Edits) > 0
		}
		if req.Tools, err = stepTools(req.Tools, finish, st, more); err != nil {
			return err
		}
	}
	req.Tools = slices.DeleteFunc(slices.Clone(req.Tools), func(t openresponses.Tool) bool {
		fn, ok := t.(*openresponses.FunctionTool)
		return !ok || !keep(fn.Name)
	})
	return nil
}

// stepTools gives flow_finish the shape the step wants, so the model is
// told by the tool itself and not only in prose. The request is what is
// recorded, so the record holds what the model was offered.
func stepTools(tools openresponses.Tools, finish string, st State, more map[string]any) (openresponses.Tools, error) {
	n := st.Node
	data := map[string]any{}
	var shapes []any
	// A finish names its run, so it is matched by run and step and never
	// by the step's name alone.
	required := []string{"run", "step", "outcome"}
	data["run"] = map[string]any{"type": "string", "enum": []string{st.Instance}}
	for _, outcome := range n.finishable() {
		if s := n.Data[outcome]; s != nil {
			shapes = append(shapes, s)
		}
	}
	switch {
	case len(shapes) == 1:
		data["data"] = shapes[0]
	case len(shapes) > 1:
		data["data"] = map[string]any{"anyOf": shapes}
	default:
		data["data"] = map[string]any{}
	}
	if len(shapes) == len(n.finishable()) {
		required = append(required, "data")
	}
	for name, schema := range more {
		data[name] = schema
		required = append(required, name)
	}
	data["step"] = map[string]any{"type": "string", "enum": []string{n.ID}}
	data["outcome"] = map[string]any{"type": "string", "enum": n.finishable()}
	params, err := json.Marshal(map[string]any{"type": "object", "properties": data, "required": required})
	if err != nil {
		return nil, err
	}
	stuck, err := json.Marshal(map[string]any{"type": "object", "required": []string{"run", "step", "reason"}, "properties": map[string]any{
		"run":    data["run"],
		"step":   data["step"],
		"reason": map[string]any{"type": "string", "description": "What stops you, for whoever reads it."},
	}})
	if err != nil {
		return nil, err
	}
	var read, write json.RawMessage
	if n.documented() {
		if read, write, err = docSchemas(st); err != nil {
			return nil, err
		}
	}
	out := slices.Clone(tools)
	for i, t := range out {
		fn, ok := t.(*openresponses.FunctionTool)
		if !ok {
			continue
		}
		own := *fn
		own.Strict = nil
		switch {
		case fn.Name == finish:
			own.Parameters = params
		case fn.Name == stuckTool:
			own.Parameters = stuck
		case fn.Name == docReadTool && read != nil:
			own.Parameters = read
		case fn.Name == docWriteTool && write != nil:
			own.Parameters = write
		default:
			continue
		}
		out[i] = &own
	}
	return out, nil
}

// instructions tells the model its step, with the step's inputs and the
// data each outcome wants.
func (f *flow) instructions(st State) (string, error) {
	n := st.Node
	if n.Verdict {
		return f.judgeInstructions(st)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "You are on step %q of workflow %q.\n%s", n.ID, st.g.Name, n.Prompt)
	skillsText(&b, n.Skills)
	documentsText(&b, st)
	if st.hasFiles() {
		fmt.Fprintf(&b, "\n\nThe step's files were given to you, in order, in the result of the workflow's %s call.", filesTool)
	}
	with, err := st.resolveAll(n.With)
	if err != nil {
		return "", fmt.Errorf("agentflow: step %q: %w", n.ID, err)
	}
	if with != nil {
		j, err := json.Marshal(with)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "\n\nInputs: %s", j)
	}
	fmt.Fprintf(&b, "\n\nWhen the step is done, call %s with step %q%s and one outcome of: %s.",
		finishTool, n.ID, runClause(st), strings.Join(n.finishable(), ", "))
	if err := n.dataText(&b, n.finishable()); err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "\nIf you cannot do the step, call %s with step %q%s and your reason, rather than finish it with an outcome that is not so.",
		stuckTool, n.ID, runClause(st))
	f.limitsText(&b, st)
	return b.String(), nil
}

// dataText adds the schema each of outcomes holds its data to.
func (n *Node) dataText(b *strings.Builder, outcomes []string) error {
	for _, outcome := range outcomes {
		if s := n.Data[outcome]; s != nil {
			j, err := json.Marshal(s)
			if err != nil {
				return err
			}
			fmt.Fprintf(b, "\nFor outcome %s, data must match this schema: %s", outcome, j)
		}
	}
	return nil
}

// skillsText adds the instructions of the skills a step names. Their
// files are not served: a step reads them only with a tool the host
// offers it.
func skillsText(b *strings.Builder, skills []StepSkill) {
	if len(skills) == 0 {
		return
	}
	b.WriteString("\n\nSkills for this step, to follow as you do it:")
	for _, sk := range skills {
		fmt.Fprintf(b, "\n<skill name=%q>\n%s\n</skill>", sk.Name, strings.TrimSpace(sk.Instructions))
	}
}

// limitsText says what limits st's step works under: its policies and
// its time.
func (f *flow) limitsText(b *strings.Builder, st State) {
	if limits := describe(f.policies(st)); limits != "" {
		fmt.Fprintf(b, "\n\n%s", limits)
	}
	if t := f.timeout(st); t > 0 {
		fmt.Fprintf(b, "\n\nThe step has %s; past that it is stopped.", t)
	}
}

// serve passes a model's turn to the model, cut off at the deadline of
// its step. A stream cut there ends in [ErrStepTimeout], which no retry
// mends, and the program records the step's failure on its next turn.
func (f *flow) serve(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	sink = f.counting(sink, req.Model)
	var due time.Time
	if f.due != nil {
		due = f.due.get()
	}
	if due.IsZero() {
		return f.model.CreateStream(ctx, req, sink)
	}
	if !time.Now().Before(due) {
		return fmt.Errorf("%w, at %s", ErrStepTimeout, due.Format(time.RFC3339))
	}
	cut, stop := context.WithDeadline(ctx, due)
	defer stop()
	err := f.model.CreateStream(cut, req, sink)
	if err != nil && ctx.Err() == nil && errors.Is(cut.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w, at %s", ErrStepTimeout, due.Format(time.RFC3339))
	}
	return err
}

// Who takes a turn, and for the program, how.
const (
	theModel = iota // the model works, or converses
	toCall          // the program makes a call
	toReport        // the program says how the run finished
	toSay           // the program says a thing of its own
)

// turn is the next turn the record calls for: the model's, or the
// program's with the call it makes or what it says. It is the one place
// that decides it, for the request that is prepared and the turn that is
// taken.
type turn struct {
	by int
	// call makes the program's call. A start and a call into a child take
	// a new key each time, and a step's inputs may not resolve, so it is
	// made only for the turn taken.
	call func() (id, tool string, args any, err error)
	text string
}

// next is the turn where the workflow stands at st calls for; failed is
// the error the record was read with, which only an ending answers.
func (f *flow) next(ctx context.Context, st State, failed error) (turn, error) {
	call := func(id, tool string, args any) (turn, error) {
		return turn{by: toCall, call: func() (string, string, any, error) { return id, tool, args, nil }}, nil
	}
	over := f.overdue(st, failed)
	switch {
	case st.show != nil:
		// What a model sees of a result comes before anything a model
		// could read it in.
		return call(st.show.id(), shownTool, st.show.args())
	case over != nil:
		return call(over.id, endTool, over)
	case failed != nil:
		return turn{}, failed
	case st.request != nil:
		return call(st.requestID, overrideTool, *st.request)
	case modelTurn(st) && !f.clockDue(st) && !f.expired(st) && !f.filesDue(st) && !f.limitsDue(st):
		return turn{by: theModel}, nil
	case st.Done:
		return turn{by: toReport}, nil
	case !st.Started && st.Left:
		text := "The workflow's runs here were abandoned, and none starts until it is invoked again."
		if st.released != "" {
			text = fmt.Sprintf("Workflow %s: an operator abandoned run %s. Nothing of it is under way, and invoking the workflow starts a new run.", st.g.Name, st.released)
		}
		return turn{by: toSay, text: text}, nil
	case !st.Started:
		return turn{by: toCall, call: func() (string, string, any, error) {
			p, err := f.rootStart(st.Inputs)
			return keyPrefix(p.Run) + "start", startTool, p, err
		}}, nil
	case st.Reached != nil:
		return call(st.limitID(), askTool, limitQuestion(st))
	case st.Failure != "":
		q := Question{Step: st.Node.ID, Retry: true, Outcomes: []string{retryOutcome, stopOutcome},
			Question: fmt.Sprintf("Step %q failed: %s\nRun it again?", st.Node.ID, st.Failure)}
		q.where(st)
		return call(st.retryID(), askTool, q)
	case f.limitsDue(st):
		return call(st.limitsID(), limitsTool, limitsArgs{Run: st.Instance, Step: st.Node.ID, Reason: f.policyErr(st).Error()})
	case f.expired(st):
		id, args := f.clockCall(st, true)
		return call(id, clockTool, args)
	case f.clockDue(st):
		id, args := f.clockCall(st, false)
		return call(id, clockTool, args)
	case f.filesDue(st):
		return call(st.filesID(), filesTool, filesArgs{Run: st.Instance, Step: st.Node.ID, Files: st.fileList()})
	}
	// Whether a judge looks asks the host's rules, so it is asked only
	// for the turn taken.
	return turn{by: toCall, call: func() (string, string, any, error) {
		switch {
		case f.judging(ctx, st):
			key, err := newKey()
			if err != nil {
				return "", "", nil, err
			}
			args, err := f.judgeCall(st, key)
			return st.judgeID(), judgeTool, args, err
		case f.askClockDue(ctx, st):
			id, args := f.clockCall(st, false)
			return id, clockTool, args, nil
		}
		name, args, err := f.call(st)
		return st.callID(), name, args, err
	}}, nil
}

// CreateStream takes a program turn itself and passes a model's on.
func (f *flow) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if !strings.HasPrefix(req.Model, programPrefix) {
		return f.serve(ctx, req, sink)
	}
	st, all, err := f.read(ctx, req.Input)
	nt, err := f.next(ctx, st, err)
	if err != nil {
		return err
	}
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	switch nt.by {
	case theModel:
		return errors.New("agentflow: the turn is the model's, and the program was asked to take it")
	case toReport:
		if err := f.report(e, st); err != nil {
			return err
		}
		return e.Complete()
	case toSay:
		msg, err := e.Message("")
		if err != nil {
			return err
		}
		if err := msg.Text(nt.text); err != nil {
			return err
		}
		if err := msg.Close(); err != nil {
			return err
		}
		return e.Complete()
	}
	id, name, args, err := nt.call()
	if err != nil {
		return err
	}
	switch a := args.(type) {
	case *Ending:
		a.Summary = f.summarize(ctx, st, all)
	case startPayload:
		f.book.started(a.Run)
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	call, err := e.FunctionCall(id, name)
	if err != nil {
		return err
	}
	if err := call.Arguments(string(encoded)); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

func (f *flow) report(e *openresponses.Emitter, st State) error {
	msg, err := e.Message("")
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow %s finished.", st.g.Name)
	for _, s := range st.Steps {
		fmt.Fprintf(&b, "\n- %s: %s", s.Node, s.Result.Outcome)
		if s.Result.By != "" {
			fmt.Fprintf(&b, " (by %s)", s.Result.By)
		}
	}
	if st.Outputs != nil {
		j, err := json.Marshal(st.Outputs)
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "\n\nOutputs: %s", j)
	}
	if err := msg.Text(b.String()); err != nil {
		return err
	}
	return msg.Close()
}
