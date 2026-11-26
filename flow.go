package agentflow

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// Question is what a Person node asks.
type Question struct {
	// Workflow and Run name the run that asks, and Path the way to it
	// from the run nobody called. A question of a record of format 1
	// names none of them.
	Workflow string   `json:"workflow,omitempty"`
	Run      string   `json:"run,omitempty"`
	Path     []Place  `json:"path,omitempty"`
	Step     string   `json:"step"`
	Question string   `json:"question"`
	Outcomes []string `json:"outcomes"`
	// With is the node's inputs, to show beside the question.
	With map[string]any `json:"with,omitempty"`
	// Form is the schema of the data each outcome wants.
	Form map[string]any `json:"form,omitempty"`
	// Advice is what a judge said, to show beside the question.
	Advice *Verdict `json:"advice,omitempty"`
	// Mode is how far the host let a judge answer this, empty for not at
	// all: "shadow", "advise" or "decide". Under shadow there is no
	// verdict yet and none to show.
	Mode string `json:"mode,omitempty"`
	// Version names the step's definition, so that answers to the same
	// question can be told from answers to a reworded one.
	Version string `json:"version,omitempty"`
	// SentBack is how many times running a judge has sent the work back
	// here.
	SentBack int `json:"sent_back,omitempty"`
	// Retry says the question is whether to run a failed step again.
	Retry bool `json:"retry,omitempty"`
	// Answer is empty as asked. [Answer] fills it in.
	Answer *Result `json:"answer,omitempty"`
}

type finishArgs struct {
	Run     string `json:"run,omitempty"`
	Step    string `json:"step"`
	Outcome string `json:"outcome"`
	Data    any    `json:"data,omitempty"`
}

// Option configures [Apply].
type Option func(*flow)

// Unattended says nobody is there to ask. A Person node takes its
// unattended outcome, one without waits as it would otherwise, and a
// failed step ends the workflow.
func Unattended() Option { return func(f *flow) { f.unattended = true } }

// Reply answers questions by the host's own rules, before anyone is
// asked. It returns false to leave a question to the person.
func Reply(fn func(context.Context, Question) (Result, bool)) Option {
	return func(f *flow) { f.reply = fn }
}

// MaxDepth bounds how deeply calls nest, under the bound the workflow
// sets for itself; the smaller wins. The run's start records the bound,
// so a resume keeps it.
func MaxDepth(n int) Option { return func(f *flow) { f.maxDepth = n } }

type flow struct {
	g          *Graph
	maxDepth   int
	model      openresponses.Streamer
	modelName  string
	unattended bool
	reply      func(context.Context, Question) (Result, bool)

	history     func(context.Context) (agentturn.Transcript, error)
	delegations []delegation
	modes       map[string]Mode
	judgeName   string
}

// programPrefix starts the model name of a turn the program takes, so a
// record never passes a program's call off as a model's.
const programPrefix = "agentflow/"

// Apply returns cfg made to follow g. cfg's Model and ModelName serve
// the Agent nodes, and its tools serve both kinds that call tools. cfg's
// own BeforeModelCall and BeforeToolCall still run, and may refuse what
// the workflow asks for.
func Apply(g *Graph, cfg agentturn.Config, opts ...Option) (agentturn.Config, error) {
	if err := g.Validate(); err != nil {
		return cfg, err
	}
	closure := g.closure()
	for _, x := range closure {
		for _, n := range x.Nodes {
			if n.Flow != "" && n.call == nil {
				return cfg, fmt.Errorf("agentflow: %s: node %q calls %s, and the graph is not linked; call Link", x.Name, n.ID, n.Flow)
			}
		}
	}
	f := &flow{g: g, model: cfg.Model, modelName: cfg.ModelName}
	for _, opt := range opts {
		opt(f)
	}
	if f.modelName == "" {
		f.modelName = cfg.Request.Model
	}
	if f.modelName == "" || strings.HasPrefix(f.modelName, programPrefix) {
		return cfg, errors.New("agentflow: the config names no model")
	}
	if err := f.delegate(); err != nil {
		return cfg, err
	}
	if f.judgeName == "" {
		f.judgeName = f.modelName
	}
	if strings.HasPrefix(f.judgeName, programPrefix) {
		return cfg, errors.New("agentflow: the judge needs a model")
	}

	if f.maxDepth < 0 {
		return cfg, errors.New("agentflow: max depth must not be negative")
	}
	own := []agenttool.Tool{f.startTool(), f.askTool(), f.finishTool(), f.judgeTool(), f.endTool(), f.callTool()}
	if p := cfg.ToolProvider; p != nil {
		cfg.ToolProvider = func(ctx context.Context) []agenttool.Tool {
			return append(slices.Clone(p(ctx)), own...)
		}
	} else {
		cfg.Tools = append(slices.Clone(cfg.Tools), own...)
	}

	scripts := false
	for _, x := range closure {
		scripts = scripts || slices.ContainsFunc(x.Nodes, func(n Node) bool { return n.Script != "" })
	}
	if cfg.ToolProvider == nil && scripts &&
		!slices.ContainsFunc(cfg.Tools, func(t agenttool.Tool) bool { return t.Name() == ScriptTool }) {
		return cfg, fmt.Errorf("agentflow: the graph has script steps and the config no %q tool", ScriptTool)
	}

	cfg.Model = f
	if transform := cfg.Transform; transform != nil {
		cfg.Transform = f.transform(transform)
	}
	cfg.BeforeTurn = agentturn.ChainBeforeTurn(f.begin, cfg.BeforeTurn)
	userModel := cfg.BeforeModelCall
	cfg.BeforeModelCall = func(ctx context.Context, req *openresponses.Request) error {
		if userModel != nil {
			if err := userModel(ctx, req); err != nil {
				return err
			}
		}
		return f.prepare(ctx, req)
	}
	userTool := cfg.BeforeToolCall
	cfg.BeforeToolCall = func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		d, err := f.gate(ctx, info)
		if d != nil || err != nil || userTool == nil {
			return d, err
		}
		return userTool(ctx, info)
	}
	return cfg, nil
}

// ErrPositionLost is a transcript that no longer says where the workflow
// stands: a transform, a compaction say, dropped part of a run, or the
// transcript was rebuilt from a session without it.
var ErrPositionLost = errors.New("agentflow: what the workflow's position is read from is gone")

func (f *flow) samePosition(before, after agentturn.Transcript) error {
	b, err := f.g.Replay(before)
	if err != nil {
		return err
	}
	a, err := f.g.Replay(after)
	if err != nil {
		return err
	}
	if a.Instance != b.Instance || a.Node != b.Node || a.Visit != b.Visit || a.Attempt != b.Attempt ||
		a.Done != b.Done || a.Ended != b.Ended || a.Reported != b.Reported || len(a.Steps) != len(b.Steps) || (a.Advice == nil) != (b.Advice == nil) || (a.Held == nil) != (b.Held == nil) ||
		a.Callee != b.Callee || (a.Returned == nil) != (b.Returned == nil) || len(a.Path) != len(b.Path) {
		return ErrPositionLost
	}
	return nil
}

// begin opens a visit of an Agent node with a message in the transcript.
// Without it a model reads its last step's flow_finish under a tool that
// now accepts only this step's, and takes that call for a mistake. It is
// also where a fresh context starts. It is a user message, since a
// provider may move a leading developer message out of the conversation,
// and a hook's, so no record takes it for the person's.
func (f *flow) begin(ctx context.Context, info agentturn.TurnStartInfo) (openresponses.Items, error) {
	st, _, err := f.read(ctx, info.Transcript)
	if err != nil || st.Node == nil || st.Failure != "" || st.Begun {
		// An error here is the same one prepare reports.
		return nil, nil
	}
	switch {
	case f.wants(st):
		return openresponses.Items{openresponses.UserText(beginning(st.Node.ID) + " Judge it now." + visitTag(st))}, nil
	case st.Node.Kind != Agent:
		return nil, nil
	}
	text := beginning(st.Node.ID)
	if st.Node.Context != ContextFresh && len(st.Steps) > 0 {
		text += " The steps before it are finished and their results accepted, the " + finishTool + " calls included."
	}
	return openresponses.Items{openresponses.UserText(text + " Do this step now." + visitTag(st))}, nil
}

// since returns the input from the message that opened this visit of
// st's node.
func since(input openresponses.Items, st State) (openresponses.Items, error) {
	for i := len(input) - 1; i >= 0; i-- {
		if m, ok := input[i].(*openresponses.Message); ok && opens(m, &st) {
			return slices.Clone(input[i:]), nil
		}
	}
	// Sending everything would show a step what it must not see.
	return nil, fmt.Errorf("%w: the start of step %q", ErrPositionLost, st.Node.ID)
}

// overdue is the ending that has yet to be written, nil when none has.
func (f *flow) overdue(st State, failed error) *Ending {
	switch {
	case len(st.abandoned) > 0 && !st.Started:
		return st.abandoned[0]
	case !st.Started || st.Ended || errors.Is(failed, ErrChanged):
		return nil
	case failed != nil || st.Done:
		return ending(st, failed)
	case st.Failure != "" && f.unattended && f.reply == nil:
		e := ending(st, nil)
		e.Status, e.Reason, e.Step = Failed, st.Node.ID+": "+st.Failure, st.Node.ID
		return e
	}
	return nil
}

// modelTurn says the model takes the next turn.
func modelTurn(st State) bool {
	return st.Done && st.Reported || st.Node != nil && st.Node.Kind == Agent && st.Failure == ""
}

// prepare shapes the request for whoever takes this turn. It runs
// before the turn is recorded, so the record holds what was sent.
func (f *flow) prepare(ctx context.Context, req *openresponses.Request) error {
	st, all, err := f.read(ctx, req.Input)
	if f.overdue(st, err) != nil {
		req.Model = programPrefix + f.g.Name
		return nil
	}
	if err != nil {
		return err
	}
	if f.judging(st) {
		return f.prepareJudge(req, st, all)
	}
	if !modelTurn(st) {
		req.Model = programPrefix + f.g.Name
		return nil
	}
	req.Model = f.modelName
	req.Input = f.visible(req.Input, all, st)
	keep := func(name string) bool { return !strings.HasPrefix(name, reserved) && name != ScriptTool }
	if n := st.Node; n != nil {
		if n.Context == ContextFresh {
			// From the record, which a compaction does not reach.
			from := req.Input
			if f.history != nil {
				from = openresponses.Items(all)
			}
			if req.Input, err = since(from, st); err != nil {
				return err
			}
		}
		step, err := f.instructions(st)
		if err != nil {
			return err
		}
		req.Instructions = strings.TrimSpace(req.Instructions + "\n\n" + step)
		keep = func(name string) bool { return name == finishTool || slices.Contains(n.Tools, name) }
		if req.Tools, err = stepTools(req.Tools, finishTool, st, nil); err != nil {
			return err
		}
	}
	req.Tools = slices.DeleteFunc(slices.Clone(req.Tools), func(t openresponses.Tool) bool {
		fn, ok := t.(*openresponses.FunctionTool)
		return !ok || !keep(fn.Name)
	})
	return nil
}

// prepareJudge shapes a judge's request, which never reads the
// conversation that led to the question.
func (f *flow) prepareJudge(req *openresponses.Request, st State, all agentturn.Transcript) (err error) {
	n := st.Node
	req.Model = f.judgeName
	from := req.Input
	if f.history != nil {
		from = openresponses.Items(all)
	}
	if req.Input, err = since(from, st); err != nil {
		return err
	}
	ask, err := f.judgeInstructions(st)
	if err != nil {
		return err
	}
	req.Instructions = strings.TrimSpace(req.Instructions + "\n\n" + ask)
	// The verdict takes the shape of what the judge may choose.
	allowed := *n
	allowed.Outcomes = append(slices.Clone(n.Judge.Outcomes), HandBack)
	judged := st
	judged.Node = &allowed
	if req.Tools, err = stepTools(req.Tools, judgeTool, judged, map[string]any{"reason": map[string]any{"type": "string"}}); err != nil {
		return err
	}
	req.Tools = slices.DeleteFunc(slices.Clone(req.Tools), func(t openresponses.Tool) bool {
		fn, ok := t.(*openresponses.FunctionTool)
		return !ok || fn.Name != judgeTool && !slices.Contains(n.Judge.Tools, fn.Name)
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
	required := []string{"step", "outcome"}
	if st.Format != 1 {
		// A finish names its run, so it is matched by run and step and
		// never by the step's name alone.
		data["run"] = map[string]any{"type": "string", "enum": []string{st.Instance}}
		required = append([]string{"run"}, required...)
	}
	for _, outcome := range n.outcomes() {
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
	if len(shapes) == len(n.outcomes()) {
		required = append(required, "data")
	}
	for name, schema := range more {
		data[name] = schema
		required = append(required, name)
	}
	data["step"] = map[string]any{"type": "string", "enum": []string{n.ID}}
	data["outcome"] = map[string]any{"type": "string", "enum": n.outcomes()}
	params, err := json.Marshal(map[string]any{"type": "object", "properties": data, "required": required})
	if err != nil {
		return nil, err
	}
	out := slices.Clone(tools)
	for i, t := range out {
		if fn, ok := t.(*openresponses.FunctionTool); ok && fn.Name == finish {
			own := *fn
			own.Parameters, own.Strict = params, nil
			out[i] = &own
		}
	}
	return out, nil
}

// instructions tells the model its step, with the step's inputs and the
// data each outcome wants.
func (f *flow) instructions(st State) (string, error) {
	n := st.Node
	var b strings.Builder
	fmt.Fprintf(&b, "You are on step %q of workflow %q.\n%s", n.ID, st.g.Name, n.Prompt)
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
		finishTool, n.ID, runClause(st), strings.Join(n.outcomes(), ", "))
	for _, outcome := range n.outcomes() {
		if s := n.Data[outcome]; s != nil {
			j, err := json.Marshal(s)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "\nFor outcome %s, data must match this schema: %s", outcome, j)
		}
	}
	return b.String(), nil
}

// call is the call a program step makes, and what a person is asked.
func (f *flow) call(st State) (name string, args any, err error) {
	n := st.Node
	with, err := st.resolveAll(n.With)
	if err != nil {
		return "", nil, fmt.Errorf("agentflow: step %q: %w", n.ID, err)
	}
	switch {
	case n.Kind == Person:
		q := Question{Step: n.ID, Question: n.Prompt, Outcomes: n.outcomes(), With: with, Advice: st.Advice, Version: n.version()}
		q.where(st)
		if mode := f.mode(st); mode != 0 {
			q.SentBack = st.g.sentBack(st, n)
			q.Mode = mode.String()
		}
		for outcome, s := range n.Data {
			if q.Form == nil {
				q.Form = map[string]any{}
			}
			q.Form[outcome] = s
		}
		return askTool, q, nil
	case n.Script != "":
		sc := ScriptCall{Run: n.Script, With: with, Outcomes: n.Outcomes}
		if skill := skillOf(st.g.Name); skill != skillOf(f.g.Name) {
			sc.Skill = skill
		}
		return ScriptTool, sc, nil
	case n.Flow != "":
		c := st.g.Callees[n.Flow]
		return callTool, callArgs{Workflow: c.Name, Digest: c.OwnDigest(), Run: st.Returned.Instance, With: with}, nil
	}
	merged := map[string]any{}
	if with == nil {
		if n.Args != nil {
			return n.Tool, n.Args, nil
		}
		return n.Tool, merged, nil
	}
	if n.Args != nil {
		if err := decode(n.Args, &merged); err != nil {
			return "", nil, err
		}
	}
	maps.Copy(merged, with)
	return n.Tool, merged, nil
}

// CreateStream takes a program turn itself and passes a model's on.
func (f *flow) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if !strings.HasPrefix(req.Model, programPrefix) {
		return f.model.CreateStream(ctx, req, sink)
	}
	st, _, err := f.read(ctx, req.Input)
	over := f.overdue(st, err)
	if err != nil && over == nil {
		return err
	}
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	var id, name string
	var args any
	switch {
	case over != nil:
		id, name, args = over.id, endTool, over
	case st.Done:
		if err := f.report(e, st); err != nil {
			return err
		}
		return e.Complete()
	case !st.Started:
		key, err := newKey()
		if err != nil {
			return err
		}
		p, err := f.rootStart(key, st.Inputs)
		if err != nil {
			return err
		}
		id, name, args = keyPrefix(key)+"start", startTool, p
	case st.Failure != "":
		id, name = st.retryID(), askTool
		q := Question{Step: st.Node.ID, Retry: true, Outcomes: []string{retryOutcome, stopOutcome},
			Question: fmt.Sprintf("Step %q failed: %s\nRun it again?", st.Node.ID, st.Failure)}
		q.where(st)
		args = q
	case st.Node.Flow != "" && st.Callee == "":
		key, err := newKey()
		if err != nil {
			return err
		}
		with, err := st.resolveAll(st.Node.With)
		if err != nil {
			return fmt.Errorf("agentflow: step %q: %w", st.Node.ID, err)
		}
		c := st.g.Callees[st.Node.Flow]
		id, name = keyPrefix(key)+"start", startTool
		args = startPayload{Workflow: c.Name, Digest: c.OwnDigest(), Inputs: with, Format: RecordFormat,
			Run: key, Parent: st.Instance, Caller: st.callID(), Depth: st.Depth + 1}
	default:
		id = st.callID()
		if name, args, err = f.call(st); err != nil {
			return err
		}
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
		return nil, nil
	}
	t, ok := agentturn.TranscriptFromContext(ctx)
	if !ok {
		return nil, errors.New("agentflow: no transcript on the context")
	}
	st, _, err := f.read(ctx, t)
	name, id := info.Call.Name, info.Call.CallID
	if name == endTool {
		if over := f.overdue(st, err); over == nil || id != over.id {
			return block("only the workflow ends a run")
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	n := st.Node
	switch {
	case st.Done && st.Reported:
		if strings.HasPrefix(name, reserved) || name == ScriptTool {
			return block("only a running workflow calls " + name)
		}
	case st.Done:
		return block("the workflow has finished")
	case !st.Started:
		if name != startTool || !strings.HasPrefix(id, reserved) {
			return block("the workflow has not started")
		}
	case st.Failure != "":
		if name != askTool || id != st.retryID() {
			return block(fmt.Sprintf("step %q failed", n.ID))
		}
		return f.ask(ctx, info, st)
	case n.Flow != "" && st.Callee == "":
		if err := callStart(st, info.Call); err != nil {
			return block(err.Error())
		}
	case n.Flow != "":
		if err := callReturn(st, info.Call); err != nil {
			return block(err.Error())
		}
	case n.Kind == Agent:
		if name == finishTool {
			if step := finishStep(info.Call.Arguments); step != n.ID {
				return block(fmt.Sprintf("you are on step %q, not %q", n.ID, step))
			}
			if run := runOf(info.Call.Arguments); st.Format != 1 && run != st.Instance {
				return block(fmt.Sprintf("you are on step %q of run %q, not of run %q", n.ID, st.Instance, run))
			}
			return nil, nil
		}
		if !slices.Contains(n.Tools, name) {
			return block(fmt.Sprintf("step %q does not allow %s", n.ID, name))
		}
	case f.judging(st):
		if name == judgeTool {
			if step := finishStep(info.Call.Arguments); step != n.ID {
				return block(fmt.Sprintf("you are judging step %q, not %q", n.ID, step))
			}
			if run := runOf(info.Call.Arguments); st.Format != 1 && run != st.Instance {
				return block(fmt.Sprintf("you are judging step %q of run %q, not of run %q", n.ID, st.Instance, run))
			}
			return nil, nil
		}
		if !slices.Contains(n.Judge.Tools, name) {
			return block(fmt.Sprintf("the judge of step %q cannot call %s", n.ID, name))
		}
	case id != st.callID():
		return block(fmt.Sprintf("step %q takes no other calls", n.ID))
	case n.Kind == Person:
		return f.ask(ctx, info, st)
	}
	return nil, nil
}

// validAnswer holds an answer to what its question may be answered with.
func validAnswer(n *Node, retry bool, res Result) error {
	if !retry {
		return n.check(res)
	}
	if res.Outcome != retryOutcome && res.Outcome != stopOutcome {
		return fmt.Errorf("outcome %q: want %s or %s", res.Outcome, retryOutcome, stopOutcome)
	}
	return nil
}

// ask decides who answers a question: the host's rules, then a judge
// the host let decide, then the node's default when nobody is there,
// then the person.
func (f *flow) ask(ctx context.Context, info agentturn.ToolCallInfo, st State) (*agentturn.ToolDecision, error) {
	n := st.Node
	answer := func(q Question, res Result, why string) (*agentturn.ToolDecision, error) {
		q.Answer = &res
		args, err := json.Marshal(q)
		if err != nil {
			return nil, err
		}
		return &agentturn.ToolDecision{Action: agentturn.Allow, Args: args, By: "policy", Reason: "agentflow: " + why}, nil
	}
	var q Question
	if err := decode(info.Args, &q); err != nil {
		return nil, fmt.Errorf("agentflow: question: %w", err)
	}
	if f.reply != nil {
		if res, ok := f.reply(ctx, q); ok && validAnswer(n, q.Retry, res) == nil {
			res.By = "rules"
			return answer(q, res, "answered by the host's rules")
		}
	}
	// The verdict is read from the transcript, not from the question.
	why := q.Question
	if v := st.Advice; !q.Retry && v != nil && f.mode(st) == Decide && v.Outcome != HandBack {
		if st.g.spent(st, *v) {
			why = fmt.Sprintf("the judge has sent this back %d times: %s", st.g.sentBack(st, n), q.Question)
		} else {
			return answer(q, Result{Outcome: v.Outcome, Data: v.Data, By: ByJudge, Reason: v.Reason}, "decided by the judge")
		}
	}
	// With nobody there, the tool takes the node's default, and refuses
	// a retry, which ends the workflow.
	if f.unattended && (q.Retry || n.Unattended != "") {
		return nil, nil
	}
	return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: "agentflow: " + why}, nil
}

func safeReplay(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe }

func (f *flow) startTool() agenttool.Tool {
	return agenttool.New(startTool, "Start a workflow.", func(ctx context.Context, p startPayload) (Result, error) {
		if err := exact(ctx, &p); err != nil {
			return Result{}, err
		}
		g := f.g.closure()[p.Workflow]
		if g == nil {
			return Result{}, fmt.Errorf("agentflow: no workflow %q", p.Workflow)
		}
		if err := checkData(g.Inputs, p.Inputs); err != nil {
			return Result{}, fmt.Errorf("agentflow: inputs of %s: %w", g.Name, err)
		}
		return Result{Outcome: OK}, nil
	}, agenttool.WithReplay(safeReplay))
}

// endTool writes down that a run is over. Its arguments are the record.
func (f *flow) endTool() agenttool.Tool {
	return agenttool.New(endTool, "End a workflow run.", func(context.Context, Ending) (Result, error) {
		return Result{Outcome: OK}, nil
	}, agenttool.WithReplay(safeReplay))
}

// withoutStarts keeps flow_start from the model: it holds the whole
// definition, a judge's criteria among it.
func withoutStarts(t openresponses.Items) openresponses.Items {
	dropped := map[string]bool{}
	var out openresponses.Items
	for _, item := range t {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			if it.Name == startTool && strings.HasPrefix(it.CallID, reserved) {
				dropped[it.CallID] = true
				continue
			}
		case *openresponses.FunctionCallOutput:
			if dropped[it.CallID] {
				continue
			}
		}
		out = append(out, item)
	}
	if len(dropped) == 0 {
		return t
	}
	return out
}

// askTool returns the answer an approval or a rule carried, or the
// node's default when the question was not deferred because nobody is
// there to ask.
func (f *flow) askTool() agenttool.Tool {
	return agenttool.New(askTool, "Ask the person a workflow question.", func(ctx context.Context, q Question) (Result, error) {
		if err := exact(ctx, &q); err != nil {
			return Result{}, err
		}
		g := f.g.closure()[cmp.Or(q.Workflow, f.g.Name)]
		if g == nil {
			return Result{}, fmt.Errorf("agentflow: no workflow %q", q.Workflow)
		}
		n := g.node(q.Step)
		if n == nil {
			return Result{}, fmt.Errorf("agentflow: no step %q", q.Step)
		}
		if !q.Retry && n.Kind != Person {
			return Result{}, fmt.Errorf("agentflow: step %q asks nothing", q.Step)
		}
		switch {
		case q.Answer != nil:
			if err := validAnswer(n, q.Retry, *q.Answer); err != nil {
				return Result{}, fmt.Errorf("agentflow: %w", err)
			}
			return *q.Answer, nil
		case f.unattended && !q.Retry && n.Unattended != "":
			return Result{Outcome: n.Unattended, Data: n.UnattendedData, By: "unattended default"}, nil
		}
		return Result{}, errors.New("agentflow: this question needs an answer")
	}, agenttool.WithReplay(safeReplay))
}

// unquote reads data a model sent as a string holding JSON, a common
// mistake with nested data, where the schema wants an object or an
// array. Anything else is returned as it is, to be held to the schema.
func unquote(s *agenttool.Schema, data any) any {
	text, ok := data.(string)
	if !ok || s == nil || s.Type != "object" && s.Type != "array" {
		return data
	}
	var v any
	if !json.Valid([]byte(text)) || decode([]byte(text), &v) != nil {
		return data
	}
	return v
}

func (f *flow) finishTool() agenttool.Tool {
	return agenttool.New(finishTool, "Finish the current workflow step.", func(ctx context.Context, a finishArgs) (Result, error) {
		if err := exact(ctx, &a); err != nil {
			return Result{}, err
		}
		st, err := f.current(ctx)
		if err != nil {
			return Result{}, err
		}
		n := st.Node
		if n == nil || n.Kind != Agent || n.ID != a.Step || st.Format != 1 && a.Run != st.Instance {
			return Result{}, fmt.Errorf("agentflow: %q is not a step you can finish", a.Step)
		}
		res := Result{Outcome: a.Outcome, Data: unquote(n.Data[a.Outcome], a.Data)}
		if err := n.check(res); err != nil {
			if s, ok := res.Data.(string); ok && n.Data[a.Outcome] != nil && strings.HasPrefix(strings.TrimSpace(s), "{") {
				return Result{}, errors.New("agentflow: data is a string that is not valid JSON; send data as a JSON object, not as a string")
			}
			return Result{}, fmt.Errorf("agentflow: %w", err)
		}
		return res, nil
	})
}

// Answer answers a pending question on a person's behalf. An answer the
// node does not accept is refused here, and the question stays pending.
// It approves
// the question with the answer in its arguments, so the record reads as
// a call a person let proceed. An output supplied in its place would be
// recorded as a rejection.
func (g *Graph) Answer(pending agentturn.PendingCall, res Result) (agentturn.Answer, error) {
	var q Question
	if pending.Call == nil || pending.Call.Name != askTool {
		return agentturn.Answer{}, errors.New("agentflow: not a workflow question")
	}
	if err := decode([]byte(pending.Call.Arguments), &q); err != nil {
		return agentturn.Answer{}, fmt.Errorf("agentflow: question: %w", err)
	}
	// The question's run is of a workflow g pinned, so the answer is
	// held to that one's definition.
	asker := g.closure()[cmp.Or(q.Workflow, g.Name)]
	if asker == nil {
		return agentturn.Answer{}, fmt.Errorf("agentflow: no workflow %q", q.Workflow)
	}
	n := asker.node(q.Step)
	if n == nil {
		return agentturn.Answer{}, fmt.Errorf("agentflow: no step %q", q.Step)
	}
	if err := validAnswer(n, q.Retry, res); err != nil {
		return agentturn.Answer{}, fmt.Errorf("agentflow: %w", err)
	}
	q.Answer = &res
	args, err := json.Marshal(q)
	if err != nil {
		return agentturn.Answer{}, err
	}
	return agentturn.ApproveWith(pending.Call.CallID, args).WithBy("human"), nil
}
