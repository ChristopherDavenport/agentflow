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
	"time"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// Question is what a Person node asks.
type Question struct {
	// Workflow and Run name the run that asks, and Path the way to it
	// from the run nobody called.
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
	// Limit names the bound the run has reached, when the question is
	// whether to grant more of it: grant, with the amount, or stop.
	Limit string `json:"limit,omitempty"`
	// Choices is what an answer may pick from, as it was resolved when
	// the question was asked. A pick from anything else is refused.
	Choices *Offered `json:"choices,omitempty"`
	// Text says an answer in the person's own words takes its outcome,
	// the words in its data's member.
	Text *FreeText `json:"text,omitempty"`
	// Deadline is when the question takes OnTimeout if nobody has
	// answered, as the program's clock recorded it; nil for never.
	Deadline  *time.Time `json:"deadline,omitempty"`
	OnTimeout string     `json:"on_timeout,omitempty"`
	// Answer is empty as asked. [Answer] fills it in.
	Answer *Result `json:"answer,omitempty"`
}

type finishArgs struct {
	Run     string `json:"run"`
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

// MaxSteps bounds the steps a tree of runs may enter, under the bound the
// workflow sets for itself; the smaller wins. The run's start records
// the bound, so a resume keeps it.
func MaxSteps(n int) Option { return func(f *flow) { f.maxSteps = n } }

// DefaultUnattendedTimeout bounds a model step that sets no timeout of
// its own when nobody is there to see it stall.
const DefaultUnattendedTimeout = 30 * time.Minute

// UnattendedTimeout sets the timeout of a model step that sets none,
// when [Unattended]: [DefaultUnattendedTimeout] if not given, none for 0.
func UnattendedTimeout(d time.Duration) Option { return func(f *flow) { f.idle = &d } }

type flow struct {
	g        *Graph
	maxDepth int
	maxSteps int
	// idle is the timeout of an unattended model step that sets none.
	idle *time.Duration
	// matchers and policyOpts are the host's, for agentpolicy to read a
	// step's policy with, and engines the policies built with them.
	matchers   func(dir string) map[string]agentpolicy.ToolMatcher
	policyOpts []agentpolicy.Option
	engines    *engines
	model      openresponses.Streamer
	modelName  string
	unattended bool
	reply      func(context.Context, Question) (Result, bool)

	history     func(context.Context) (agentturn.Transcript, error)
	delegations []delegation
	modes       map[string]Mode
	judgeName   string

	// children opens the agents children run in, and models names the
	// models a child may ask for, nil for any.
	children func(context.Context, Child) (context.Context, *agentturn.Agent, error)
	models   []string
	// child is set for a flow run in a child: what its start records,
	// and the instructions its settings add.
	child *Child
	// due is the deadline of the model step the last request was shaped
	// for, so the model's stream is cut there.
	due *deadline
}

// programPrefix starts the model name of a turn the program takes, so a
// record never passes a program's call off as a model's.
const programPrefix = "agentflow/"

// Apply returns cfg made to follow g. cfg's Model and ModelName serve
// the Agent nodes, and its tools serve both kinds that call tools. cfg's
// own BeforeModelCall and BeforeToolCall still run, and may refuse what
// the workflow asks for.
func Apply(g *Graph, cfg agentturn.Config, opts ...Option) (agentturn.Config, error) {
	f := &flow{g: g}
	for _, opt := range opts {
		opt(f)
	}
	return f.apply(cfg)
}

// apply makes cfg follow f's graph.
func (f *flow) apply(cfg agentturn.Config) (agentturn.Config, error) {
	g := f.g
	if err := g.Validate(); err != nil {
		return cfg, err
	}
	closure := g.closure()
	needs := false
	for _, x := range closure {
		for _, n := range x.Nodes {
			if n.Flow != "" && n.call == nil {
				return cfg, fmt.Errorf("agentflow: %s: node %q calls %s, and the graph is not linked; call Link", x.Name, n.ID, n.Flow)
			}
			needs = needs || n.fresh()
			if n.Model != "" && f.models != nil && !slices.Contains(f.models, n.Model) {
				return cfg, fmt.Errorf("agentflow: %s: node %q asks for model %q, which the host does not serve: want one of %s", x.Name, n.ID, n.Model, strings.Join(f.models, ", "))
			}
		}
		if x.Model != "" && f.models != nil && !slices.Contains(f.models, x.Model) {
			return cfg, fmt.Errorf("agentflow: %s asks for model %q, which the host does not serve: want one of %s", x.Name, x.Model, strings.Join(f.models, ", "))
		}
	}
	if f.maxSteps < 0 {
		return cfg, errors.New("agentflow: max steps must not be negative")
	}
	if f.idle != nil && (*f.idle < 0 || *f.idle > MaxTimeout) {
		return cfg, fmt.Errorf("agentflow: an unattended timeout must be 0 to %s", MaxTimeout)
	}
	if err := f.buildPolicies(); err != nil {
		return cfg, err
	}
	f.model, f.modelName = cfg.Model, cfg.ModelName
	if f.modelName == "" {
		f.modelName = cfg.Request.Model
	}
	if f.modelName == "" || strings.HasPrefix(f.modelName, programPrefix) {
		return cfg, errors.New("agentflow: the config names no model")
	}
	// A child keeps its caller's delegations, resolved where they were
	// given.
	if f.child == nil {
		if err := f.delegate(); err != nil {
			return cfg, err
		}
	}
	if f.judgeName == "" {
		f.judgeName = f.modelName
	}
	if strings.HasPrefix(f.judgeName, programPrefix) {
		return cfg, errors.New("agentflow: the judge needs a model")
	}
	if f.models != nil && f.judgeName != f.modelName && !slices.Contains(f.models, f.judgeName) {
		return cfg, fmt.Errorf("agentflow: the judge's model %q is not one the host serves: want one of %s", f.judgeName, strings.Join(f.models, ", "))
	}

	if f.maxDepth < 0 {
		return cfg, errors.New("agentflow: max depth must not be negative")
	}
	// A judge looks in a child of its own, as a fresh step works in one.
	if (needs || len(f.modes) > 0) && f.children == nil {
		return cfg, errors.New("agentflow: the graph runs work in children, a fresh step or call or a judge, and the host gave no Children to open them")
	}
	own := []agenttool.Tool{f.startTool(), f.askTool(), f.finishTool(), f.judgeTool(), f.endTool(), f.callTool(), f.clockTool(), f.stuckTool()}
	if p := cfg.ToolProvider; p != nil {
		cfg.ToolProvider = func(ctx context.Context) []agenttool.Tool {
			return append(f.timed(p(ctx)), own...)
		}
	} else {
		cfg.Tools = append(f.timed(cfg.Tools), own...)
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
	f.due = &deadline{}
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
	if len(st.Steps) > 0 {
		text += " The steps before it are finished and their results accepted, the " + finishTool + " calls included."
	}
	return openresponses.Items{openresponses.UserText(text + " Do this step now." + visitTag(st))}, nil
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
	case st.Reached != nil && f.unattended && f.reply == nil:
		e := ending(st, nil)
		e.Status, e.Reason, e.Step, e.Limit = Failed, st.Reached.Reason, st.Node.ID, st.Reached.Limit
		return e
	}
	return nil
}

// modelTurn says the model takes the next turn.
func modelTurn(st State) bool {
	return st.Done && st.Reported || st.Node != nil && st.Node.Kind == Agent && !st.Node.fresh() && st.Failure == "" && st.Reached == nil
}

// clockDue says the program reads the clock before st's model step
// works, and expired that the step's time has run out.
func (f *flow) clockDue(st State) bool {
	return modelTurn(st) && st.Node != nil && !st.clocked && f.timeout(st) > 0
}

func (f *flow) expired(st State) bool {
	return modelTurn(st) && st.Node != nil && !st.Deadline.IsZero() && !time.Now().Before(st.Deadline)
}

// timeout is how long st's step may take: its own timeout, or for a model
// step with nobody there, the host's default.
func (f *flow) timeout(st State) time.Duration {
	n := st.Node
	switch {
	case n == nil:
		return 0
	case n.Timeout > 0:
		return n.Timeout
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
	if f.overdue(st, err) != nil {
		req.Model = programPrefix + f.g.Name
		return nil
	}
	if err != nil {
		return err
	}
	if !modelTurn(st) || f.clockDue(st) || f.expired(st) {
		req.Model = programPrefix + f.g.Name
		return nil
	}
	if f.due != nil {
		f.due.set(st.Deadline)
	}
	req.Model = f.modelName
	req.Input = f.visible(req.Input, all, st)
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
			return name == finish || name == stuckTool && n.canStick() || slices.Contains(n.Tools, name)
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
	out := slices.Clone(tools)
	for i, t := range out {
		if fn, ok := t.(*openresponses.FunctionTool); ok && (fn.Name == finish || fn.Name == stuckTool) {
			own := *fn
			own.Parameters, own.Strict = params, nil
			if fn.Name == stuckTool {
				own.Parameters = stuck
			}
			out[i] = &own
		}
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
	for _, outcome := range n.finishable() {
		if s := n.Data[outcome]; s != nil {
			j, err := json.Marshal(s)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&b, "\nFor outcome %s, data must match this schema: %s", outcome, j)
		}
	}
	fmt.Fprintf(&b, "\nIf you cannot do the step, call %s with step %q%s and your reason, rather than finish it with an outcome that is not so.",
		stuckTool, n.ID, runClause(st))
	f.limitsText(&b, st)
	return b.String(), nil
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

// rootName is the workflow the tree's run nobody called is of: a child's
// path starts there.
func (f *flow) rootName() string {
	if c := f.child; c != nil && len(c.Path) > 0 {
		return c.Path[0].Workflow
	}
	return f.g.Name
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
		form := n.schemas()
		o, err := st.offer(n)
		if err != nil {
			return "", nil, fmt.Errorf("agentflow: step %q: %w", n.ID, err)
		}
		if o != nil {
			q.Choices = o
			form = maps.Clone(form)
			form[o.Outcome] = o.form(n.Data[o.Outcome])
		}
		if t := n.Text; t != nil {
			q.Text = &FreeText{Outcome: t.Outcome, As: t.member()}
		}
		if n.OnTimeout != "" && !st.Deadline.IsZero() {
			due := st.Deadline
			q.Deadline, q.OnTimeout = &due, n.OnTimeout
		}
		for outcome, s := range form {
			if q.Form == nil {
				q.Form = map[string]any{}
			}
			q.Form[outcome] = s
		}
		return askTool, q, nil
	case n.Script != "":
		sc := ScriptCall{Run: n.Script, With: with, Outcomes: n.Outcomes}
		if skill := skillOf(st.g.Name); skill != skillOf(f.rootName()) {
			sc.Skill = skill
		}
		return ScriptTool, sc, nil
	case n.Flow != "" && !n.fresh():
		c := st.g.Callees[n.Flow]
		return callTool, callArgs{Workflow: c.Name, Digest: c.OwnDigest(), Run: st.Returned.Instance, With: with}, nil
	case n.fresh():
		c := st.g.Callees[n.Flow]
		if n.Kind == Agent {
			c = st.g.stepGraph(n)
		}
		key, err := newKey()
		if err != nil {
			return "", nil, err
		}
		a := callArgs{Workflow: c.Name, Digest: c.OwnDigest(), Run: key, With: with, Context: ContextFresh, Child: n.Child, Spawn: key, MaxSteps: st.left()}
		if spawn := st.kept[n.Child]; n.Child != "" && spawn != "" {
			a.Spawn = spawn
		}
		return callTool, a, nil
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

// serve passes a model's turn to the model, cut off at the deadline of
// its step. A stream cut there ends in [ErrStepTimeout], which no retry
// mends, and the program records the step's failure on its next turn.
func (f *flow) serve(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
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

// CreateStream takes a program turn itself and passes a model's on.
func (f *flow) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if !strings.HasPrefix(req.Model, programPrefix) {
		return f.serve(ctx, req, sink)
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
	case !st.Started && st.Left:
		msg, err := e.Message("")
		if err != nil {
			return err
		}
		if err := msg.Text("The workflow's runs here were abandoned with the run that called them."); err != nil {
			return err
		}
		if err := msg.Close(); err != nil {
			return err
		}
		return e.Complete()
	case !st.Started:
		p, err := f.rootStart(st.Inputs)
		if err != nil {
			return err
		}
		id, name, args = keyPrefix(p.Run)+"start", startTool, p
	case st.Reached != nil:
		id, name, args = st.limitID(), askTool, limitQuestion(st)
	case st.Failure != "":
		id, name = st.retryID(), askTool
		q := Question{Step: st.Node.ID, Retry: true, Outcomes: []string{retryOutcome, stopOutcome},
			Question: fmt.Sprintf("Step %q failed: %s\nRun it again?", st.Node.ID, st.Failure)}
		q.where(st)
		args = q
	case f.expired(st):
		name = clockTool
		id, args = f.clockCall(st, true)
	case f.clockDue(st):
		name = clockTool
		id, args = f.clockCall(st, false)
	case f.judging(st):
		id, name = st.judgeID(), judgeTool
		if args, err = f.judgeCall(st); err != nil {
			return err
		}
	case f.askClockDue(st):
		name = clockTool
		id, args = f.clockQuestion(st)
	case st.Node.Flow != "" && !st.Node.fresh() && st.Callee == "":
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
	case f.judging(st):
		// The judge looks in a child, and the call waits on it.
		if err := checkJudgeCall(st, info.Call); err != nil {
			return block(err.Error())
		}
		return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: fmt.Sprintf("agentflow: a judge looks at step %q in a child", n.ID)}, nil
	case n.fresh():
		// The work is done in a child, and the call waits on it.
		if err := callFresh(st, info.Call); err != nil {
			return block(err.Error())
		}
		return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: fmt.Sprintf("agentflow: step %q runs in a child", n.ID)}, nil
	case n.Flow != "" && st.Callee == "":
		if err := callStart(st, info.Call); err != nil {
			return block(err.Error())
		}
	case n.Flow != "":
		if err := callReturn(st, info.Call); err != nil {
			return block(err.Error())
		}
	case n.Kind == Agent:
		finish, verb := finishTool, "on"
		if n.Verdict {
			finish, verb = judgeTool, "judging"
		}
		if name == clockTool {
			if id == st.clockID() && f.clockDue(st) || id == st.timeoutID() && f.expired(st) {
				return nil, nil
			}
			return block("only the workflow reads the clock")
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
		if !slices.Contains(n.Tools, name) {
			return block(fmt.Sprintf("step %q does not allow %s", n.ID, name))
		}
		if why := f.police(ctx, st, info); why != "" {
			return block(fmt.Sprintf("step %q refuses this call: %s", n.ID, why))
		}
	case n.Kind == Person && name == clockTool:
		if id == st.clockID() && f.askClockDue(st) {
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

// validAnswer holds an answer to what its question may be answered with.
func validAnswer(n *Node, q Question, res Result) error {
	switch {
	case q.Limit != "":
		switch {
		case res.Outcome == stopOutcome:
		case res.Outcome != grantOutcome:
			return fmt.Errorf("outcome %q: want %s or %s", res.Outcome, grantOutcome, stopOutcome)
		case grantOf(numbers(res.Data)) == 0:
			return fmt.Errorf("a grant needs an amount, a whole number from 1 to %d", maxGrant)
		}
		return nil
	case !q.Retry:
		if err := n.check(res); err != nil {
			return err
		}
		return q.holds(res)
	}
	if res.Outcome != retryOutcome && res.Outcome != stopOutcome {
		return fmt.Errorf("outcome %q: want %s or %s", res.Outcome, retryOutcome, stopOutcome)
	}
	return nil
}

// numbers reads data as the record will, numbers kept as written.
func numbers(data any) any {
	b, err := json.Marshal(data)
	if err != nil {
		return data
	}
	var out any
	if decode(b, &out) != nil {
		return data
	}
	return out
}

// limitQuestion asks whether to grant more of the bound st has reached.
func limitQuestion(st State) Question {
	what := map[string]string{"visits": "more visits of the step", "depth": "more levels of nesting", "steps": "more steps for the tree", "repeat": "more repeats of the step"}[st.Reached.Limit]
	q := Question{Step: st.Node.ID, Limit: st.Reached.Limit, Outcomes: []string{grantOutcome, stopOutcome},
		Question: fmt.Sprintf("%s.\nGrant %s, or stop?", st.Reached.Reason, what),
		Form: map[string]any{grantOutcome: map[string]any{"type": "object", "required": []string{"amount"}, "properties": map[string]any{
			"amount": map[string]any{"type": "integer", "description": fmt.Sprintf("How many %s, 1 to %d.", what, maxGrant)}}}}}
	q.where(st)
	return q
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
		if res, ok := f.reply(ctx, q); ok && validAnswer(n, q, res) == nil {
			res.By = "rules"
			return answer(q, res, "answered by the host's rules")
		}
	}
	// The verdict is read from the transcript, not from the question.
	why := q.Question
	if v := st.Advice; !q.Retry && q.Limit == "" && v != nil && f.mode(st) == Decide && v.Outcome != HandBack {
		if st.g.spent(st, *v) {
			why = fmt.Sprintf("the judge has sent this back %d times: %s", st.g.sentBack(st, n), q.Question)
		} else {
			return answer(q, Result{Outcome: v.Outcome, Data: v.Data, By: ByJudge, Reason: v.Reason}, "decided by the judge")
		}
	}
	// With nobody there, the tool takes the node's default, and refuses
	// a retry, which ends the workflow.
	if f.unattended && (q.Retry || q.Limit != "" || n.Unattended != "") {
		return nil, nil
	}
	// A question asked after its time ran out, on a resume that came late,
	// takes its timeout outcome at once.
	if res, err := f.answerByTimeout(ctx, q); err != nil || res != nil {
		if err != nil {
			return nil, err
		}
		return answer(q, *res, "the question's time ran out")
	}
	return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: "agentflow: " + why}, nil
}

func safeReplay(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe }

func (f *flow) startTool() agenttool.Tool {
	return agenttool.New(startTool, "Start a workflow.", func(ctx context.Context, p startPayload) (Result, error) {
		if err := exact(ctx, &p); err != nil {
			return Result{}, err
		}
		g, err := f.g.find(p.Workflow)
		if err != nil {
			return Result{}, err
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

// withoutHidden keeps from the model flow_start, which holds the whole
// definition, a judge's criteria among it, and a judge's look at an
// answer it judged unseen.
func withoutHidden(t openresponses.Items) openresponses.Items {
	dropped := map[string]bool{}
	var out openresponses.Items
	for _, item := range t {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			if hidden(it) {
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

// hidden says no working model may read c.
func hidden(c *openresponses.FunctionCall) bool {
	if !strings.HasPrefix(c.CallID, reserved) {
		return false
	}
	if c.Name == startTool {
		return true
	}
	var a callArgs
	return c.Name == judgeTool && decode([]byte(c.Arguments), &a) == nil && a.Mode == Shadow.String()
}

// askTool returns the answer an approval or a rule carried, or the
// node's default when the question was not deferred because nobody is
// there to ask.
func (f *flow) askTool() agenttool.Tool {
	return agenttool.New(askTool, "Ask the person a workflow question.", func(ctx context.Context, q Question) (Result, error) {
		if err := exact(ctx, &q); err != nil {
			return Result{}, err
		}
		g, err := f.g.find(cmp.Or(q.Workflow, f.g.Name))
		if err != nil {
			return Result{}, err
		}
		n := g.node(q.Step)
		if n == nil {
			return Result{}, fmt.Errorf("agentflow: no step %q", q.Step)
		}
		if !q.Retry && q.Limit == "" && n.Kind != Person {
			return Result{}, fmt.Errorf("agentflow: step %q asks nothing", q.Step)
		}
		switch {
		case q.Answer != nil:
			if err := validAnswer(n, q, *q.Answer); err != nil {
				return Result{}, fmt.Errorf("agentflow: %w", err)
			}
			return *q.Answer, nil
		case f.unattended && !q.Retry && q.Limit == "" && n.Unattended != "":
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
		if n == nil || n.Kind != Agent || n.Verdict || n.fresh() || n.ID != a.Step || a.Run != st.Instance {
			return Result{}, fmt.Errorf("agentflow: %q is not a step you can finish", a.Step)
		}
		if a.Outcome == Stuck {
			return Result{}, fmt.Errorf("agentflow: to say you are stuck, call %s with your reason", stuckTool)
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
	asker, err := g.find(cmp.Or(q.Workflow, g.Name))
	if err != nil {
		return agentturn.Answer{}, err
	}
	n := asker.node(q.Step)
	if n == nil {
		return agentturn.Answer{}, fmt.Errorf("agentflow: no step %q", q.Step)
	}
	switch {
	case res.By == ByTimeout:
		return agentturn.Answer{}, errors.New("agentflow: only the question's timeout answers by its timeout")
	case q.overdue(time.Now()):
		return agentturn.Answer{}, fmt.Errorf("%w at %s: drive the run, and it takes %q", ErrExpired, q.Deadline.Format(time.RFC3339), q.OnTimeout)
	}
	if err := validAnswer(n, q, res); err != nil {
		return agentturn.Answer{}, fmt.Errorf("agentflow: %w", err)
	}
	q.Answer = &res
	args, err := json.Marshal(q)
	if err != nil {
		return agentturn.Answer{}, err
	}
	return agentturn.ApproveWith(pending.Call.CallID, args).WithBy("human"), nil
}
