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
	Workflow string  `json:"workflow,omitempty"`
	Run      string  `json:"run,omitempty"`
	Path     []Place `json:"path,omitempty"`
	Step     string  `json:"step"`
	Question string  `json:"question"`
	// Template is the question as the step writes it, before the values it
	// names were filled in, when it names any: answers are joined across
	// sessions by Version, which is read from it, never from Question.
	Template string   `json:"template,omitempty"`
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
	// Note is what an operator said with the override that brought the
	// run here.
	Note string `json:"note,omitempty"`
	// Answer is empty as asked. [Answer] fills it in.
	Answer *Result `json:"answer,omitempty"`

	// Description is the step's, for the person: [Graph.Question] reads
	// it from the definition. It is never in the question's arguments,
	// which a model in the same context reads.
	Description string `json:"-"`
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
// asked. It returns false to leave a question to the person. It may be
// called more than once for a question: before a judge would look, so a
// question the rules answer is not judged, and again when the question is
// put, whose answer is the one taken. It should answer a question the
// same way each time; if it does not, the later answer stands.
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
	inherited   func(context.Context) (agentturn.Transcript, error)
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
	// fileTool and fileField name the host's tool that reads a workspace
	// file for a step's files, and its path argument.
	fileTool, fileField string
	// book counts what each run's model requests use, and prices prices
	// it, for the summary a run's end records.
	book   *usageBook
	prices func(model string, u openresponses.Usage) (float64, bool)
	// responses reads what the session's responses used, when the host
	// gives it; without, the book counts them as they are served.
	responses func(ctx context.Context) ([]ResponseUsage, error)
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
	if g.Hidden && f.child == nil {
		return cfg, fmt.Errorf("agentflow: %s is hidden: only a workflow that calls it runs it, so it is not applied on its own", g.Name)
	}
	closure := g.closure()
	needs := false
	for _, x := range closure {
		for _, n := range x.Nodes {
			if n.Flow != "" && n.call == nil {
				return cfg, fmt.Errorf("agentflow: %s: node %q calls %s, and the graph is not linked; call Link", x.Name, n.ID, n.Flow)
			}
			needs = needs || n.inChild()
			for _, v := range n.Files {
				if p, ok := v.Literal.(string); ok && strings.HasPrefix(p, skillPrefix) {
					if _, read := n.SkillFiles[strings.TrimPrefix(p, skillPrefix)]; !read {
						return cfg, fmt.Errorf("agentflow: %s: node %q is given the file %s of the skill, which was never read: load the workflow with workflow.Load", x.Name, n.ID, p)
					}
				}
			}
			for _, sk := range n.Skills {
				if strings.TrimSpace(sk.Instructions) == "" {
					return cfg, fmt.Errorf("agentflow: %s: node %q names the skill %s, whose instructions were never read: load the workflow with workflow.LoadWith", x.Name, n.ID, sk.Name)
				}
			}
			if n.Model != "" && f.models != nil && !slices.Contains(f.models, n.Model) {
				return cfg, fmt.Errorf("agentflow: %s: node %q asks for model %q, which the host does not serve%s: want one of %s", x.Name, n.ID, n.Model, Suggest(n.Model, f.models), strings.Join(f.models, ", "))
			}
		}
		if x.Model != "" && f.models != nil && !slices.Contains(f.models, x.Model) {
			return cfg, fmt.Errorf("agentflow: %s asks for model %q, which the host does not serve%s: want one of %s", x.Name, x.Model, Suggest(x.Model, f.models), strings.Join(f.models, ", "))
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
	own := append([]agenttool.Tool{f.startTool(), f.askTool(), f.finishTool(), f.judgeTool(), f.endTool(), f.callTool(), f.clockTool(), f.stuckTool(), f.overrideTool(), f.filesTool(), f.shownTool(), f.limitsTool()}, f.docTools()...)
	if p := cfg.ToolProvider; p != nil {
		cfg.ToolProvider = func(ctx context.Context) []agenttool.Tool {
			return append(f.timed(p(ctx)), own...)
		}
	} else {
		cfg.Tools = append(f.timed(cfg.Tools), own...)
	}

	// The tools are checked for what runs on them: the graph and what it
	// calls in this context. A workflow a fresh call runs works on its
	// child's tools, which its child's Apply checks.
	local := map[string]*Graph{g.Name: g}
	scripts := false
	for _, x := range local {
		scripts = scripts || slices.ContainsFunc(x.Nodes, func(n Node) bool { return n.Script != "" })
	}
	if cfg.ToolProvider == nil && scripts &&
		!slices.ContainsFunc(cfg.Tools, func(t agenttool.Tool) bool { return t.Name() == ScriptTool }) {
		return cfg, f.unfit(fmt.Errorf("agentflow: the graph has script steps and the config no %q tool", ScriptTool))
	}
	if err := checkTools(local, cfg); err != nil {
		return cfg, f.unfit(err)
	}
	if err := f.checkFileReader(local, cfg); err != nil {
		return cfg, f.unfit(err)
	}
	if err := checkArgs(g, cfg); err != nil {
		return cfg, f.unfit(err)
	}

	cfg.Model = f
	f.due = &deadline{}
	if f.book == nil {
		f.book = &usageBook{}
	}
	if transform := cfg.Transform; transform != nil {
		// What a transform returns is a view, and position is not read
		// from a view.
		if f.history == nil {
			return cfg, errors.New("agentflow: the config has a Transform, which needs the record: give History")
		}
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

// unfitChild is a child's config that does not offer what the child's
// steps call, or not as they call it, found as its opener builds it. The
// call that waits on the child fails with it, as the step's own call
// would, rather than the host's run.
type unfitChild struct{ err error }

func (e *unfitChild) Error() string { return e.err.Error() }

func (e *unfitChild) Unwrap() error { return e.err }

// unfit marks err, a check of the config's tools, as a child's, whose call
// it fails. A root's is the host's error at Apply.
func (f *flow) unfit(err error) error {
	if f.child == nil {
		return err
	}
	return &unfitChild{err}
}

// checkTools holds the tools steps name to those the config offers,
// where it says what they are: a tool a program step calls, and those a
// model step working in this session may. local is the workflows that run
// on the config, which is the graph alone: a child's tools
// are its opener's, which the child's own Apply checks, and a config with
// a ToolProvider names its tools only as it runs.
func checkTools(local map[string]*Graph, cfg agentturn.Config) error {
	if cfg.ToolProvider != nil {
		return nil
	}
	var offered []string
	for _, t := range cfg.Tools {
		if !strings.HasPrefix(t.Name(), reserved) {
			offered = append(offered, t.Name())
		}
	}
	var errs []error
	missing := func(x *Graph, n *Node, tool string) {
		if !slices.Contains(offered, tool) {
			errs = append(errs, fmt.Errorf("agentflow: %s: node %q names the tool %s, which the config does not offer%s", x.Name, n.ID, tool, Suggest(tool, offered)))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(local)) {
		x := local[name]
		for i := range x.Nodes {
			n := &x.Nodes[i]
			switch {
			case n.Kind == Run && n.Tool != "":
				missing(x, n, n.Tool)
			case n.Kind == Agent && !n.fresh():
				for _, tool := range n.Tools {
					missing(x, n, tool)
				}
				for _, tool := range n.ExcludeTools {
					missing(x, n, tool)
				}
			}
		}
		for _, tool := range x.ExcludeTools {
			if !slices.Contains(offered, tool) {
				errs = append(errs, fmt.Errorf("agentflow: %s excludes the tool %s, which the config does not offer%s", x.Name, tool, Suggest(tool, offered)))
			}
		}
	}
	return errors.Join(errs...)
}

// ErrPositionLost is a transcript that no longer says where the workflow
// stands: it was rebuilt from a session without part of a run, and no
// [History] gives the record.
var ErrPositionLost = errors.New("agentflow: what the workflow's position is read from is gone")

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
		text, err := st.rendered(n.Prompt)
		if err != nil {
			return "", nil, fmt.Errorf("agentflow: step %q: question: %w", n.ID, err)
		}
		q := Question{Step: n.ID, Question: text, Outcomes: n.outcomes(), With: with, Advice: st.Advice, Version: n.version(), Note: st.Note}
		if templated(n.Prompt) {
			q.Template = n.Prompt
		}
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
		if n.OnTimeout.Outcome != "" && !st.Deadline.IsZero() {
			due := st.Deadline
			q.Deadline, q.OnTimeout = &due, n.OnTimeout.Outcome
		}
		for outcome, s := range form {
			if q.Form == nil {
				q.Form = map[string]any{}
			}
			q.Form[outcome] = s
		}
		return askTool, q, nil
	case n.Script != "":
		sc := ScriptCall{Run: n.Script, With: with, Outcomes: n.Outcomes, Dir: st.dirArg(n)}
		if skill := skillOf(st.g.Name); skill != skillOf(f.rootName()) {
			sc.Skill = skill
		}
		return ScriptTool, sc, nil
	case n.inChild():
		key, err := newKey()
		if err != nil {
			return "", nil, err
		}
		return callTool, st.childCall(with, key), nil
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

// childCall is the call st's step makes into a child, for the run key:
// the workflow it calls, or the one made of a fresh step, as pinned, with
// what the child is given.
func (st State) childCall(with map[string]any, key string) callArgs {
	n := st.Node
	c := st.g.Callees[n.Flow]
	if n.Kind == Agent {
		c = st.g.stepGraph(n)
	}
	a := callArgs{Workflow: c.Name, Digest: c.OwnDigest(), Run: key, With: with, Context: n.childContext(), Child: n.Child, Spawn: key, MaxSteps: st.left(), Dir: st.dirArg(n)}
	if n.Kind == Agent {
		a.Documents, a.Files = st.passedDocuments(n), st.filesFor(n)
	}
	if spawn := st.kept[n.Child]; n.Child != "" && spawn != "" {
		a.Spawn = spawn
	}
	return a
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
	// a retry, which ends the workflow. A cleanup's question with no
	// default cannot be answered either, which fails its step and ends the
	// cleanup: a cleanup never holds up the abandon it is part of.
	if f.unattended && (q.Retry || q.Limit != "" || n.Unattended.Outcome != "" || st.Cleaning) {
		return nil, nil
	}
	// A question asked after its time ran out, on a resume that came late,
	// takes its timeout outcome at once.
	if res, err := f.answerByTimeout(q); err != nil || res != nil {
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
		// The time it started is a result, so a summary reads it back.
		return Result{Outcome: OK, Data: map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano)}}, nil
	}, agenttool.WithReplay(safeReplay))
}

// endTool writes down that a run is over. Its arguments are the record.
func (f *flow) endTool() agenttool.Tool {
	return agenttool.New(endTool, "End a workflow run.", func(context.Context, Ending) (Result, error) {
		return Result{Outcome: OK}, nil
	}, agenttool.WithReplay(safeReplay))
}

// askTool returns the answer an approval or a rule carried, or the
// node's default when the question was not deferred because nobody is
// there to ask.
func (f *flow) askTool() agenttool.Tool {
	return agenttool.New(askTool, "Ask the person a workflow question.", func(ctx context.Context, q Question) (Result, error) {
		if err := exact(ctx, &q); err != nil {
			return Result{}, err
		}
		n, err := f.g.asker(q)
		if err != nil {
			return Result{}, err
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
		case f.unattended && !q.Retry && q.Limit == "" && n.Unattended.Outcome != "":
			return Result{Outcome: n.Unattended.Outcome, Data: n.Unattended.Data, By: "unattended default"}, nil
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
	q, n, err := g.asked(pending)
	if err != nil {
		return agentturn.Answer{}, err
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

// Question reads a pending question as a front shows it: its arguments,
// with the step's description from the definition its run pinned.
func (g *Graph) Question(pending agentturn.PendingCall) (Question, error) {
	q, n, err := g.asked(pending)
	if err != nil {
		return Question{}, err
	}
	q.Description = n.Description
	return q, nil
}

// asked reads a pending question and the step that asks it, held to the
// definition of a workflow g pinned.
func (g *Graph) asked(pending agentturn.PendingCall) (Question, *Node, error) {
	var q Question
	if pending.Call == nil || pending.Call.Name != askTool {
		return Question{}, nil, errors.New("agentflow: not a workflow question")
	}
	if err := decode([]byte(pending.Call.Arguments), &q); err != nil {
		return Question{}, nil, fmt.Errorf("agentflow: question: %w", err)
	}
	n, err := g.asker(q)
	if err != nil {
		return Question{}, nil, err
	}
	return q, n, nil
}

// asker is the step that asks q: one of g's, or of a workflow g calls.
func (g *Graph) asker(q Question) (*Node, error) {
	x, err := g.find(cmp.Or(q.Workflow, g.Name))
	if err != nil {
		return nil, err
	}
	n := x.node(q.Step)
	if n == nil {
		return nil, fmt.Errorf("agentflow: no step %q", q.Step)
	}
	return n, nil
}
