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

// Child is a run the program holds a call open for while it works in a
// child session of its own: a fresh call to a workflow, a fresh model
// step, or a judge's look at a question. The host's opener, given with
// [Children], makes the agent it runs in.
type Child struct {
	// Run is the child run's key. Spawn is the spawn ID of the child's
	// session: Run, or for a kept child the key of the run that first
	// started it. Its session is the one the spawn names under the
	// caller's, as agentturn/session's Recorder.StartChild makes it.
	Run, Spawn string
	// Caller is the ID of the call that holds the run, and Parent the key
	// of the run that made it.
	Caller, Parent string
	// Workflow is what runs in the child: a workflow, or for a step one
	// made from it, named <workflow>:<step>, or <workflow>:<step>:judge
	// for a judge's.
	Workflow string
	// Step is the step a fresh model step or a judge works on, and Judge
	// says it is a judge's look.
	Step  string
	Judge bool
	// Kept is the name of a child kept across the visits and steps of the
	// calling run, empty for one used once.
	Kept string
	// Depth is how deeply the run is nested, and Path the way to its call
	// from the run nobody called.
	Depth int
	Path  []Place
	// Settings are the child's own.
	Settings ChildSettings

	parent   *flow
	g        *Graph
	maxDepth int
	// maxSteps is what the caller's tree had left of its steps, and
	// policies are those of the steps and calls the child's work is
	// nested in, which hold its tools too.
	maxSteps int
	policies []ruled
	made     **flow
}

// ChildSettings are what a step says of the child it runs in.
type ChildSettings struct {
	// Dir is the child's working directory, below the workspace's root,
	// empty for the root. script.Sub makes a workspace of it.
	Dir string
	// Model names the model the child asks, empty for the caller's, and
	// Thinking how hard it reasons, for a fresh model step.
	Model    string
	Thinking string
	// Instructions is added to the child's instructions.
	Instructions string
	// Tools names the tools its step may call.
	Tools []string
}

// Apply makes cfg follow the child's workflow, as [Apply] makes a config
// follow a graph, with the options of the flow that called it and opts
// added: a [History] for the child's own record, say. An opener calls it
// on the config it builds.
func (c Child) Apply(cfg agentturn.Config, opts ...Option) (agentturn.Config, error) {
	if c.parent == nil || c.g == nil {
		return cfg, errors.New("agentflow: a child is the program's to make")
	}
	p := c.parent
	f := &flow{g: c.g, maxDepth: p.maxDepth, maxSteps: p.maxSteps, unattended: p.unattended, reply: p.reply, delegations: p.delegations,
		modes: p.modes, judgeName: p.judgeName, children: p.children, models: p.models, idle: p.idle, matchers: p.matchers, policyOpts: p.policyOpts}
	own := c
	f.child = &own
	for _, opt := range opts {
		opt(f)
	}
	out, err := f.apply(cfg)
	if err == nil && c.made != nil {
		*c.made = f
	}
	return out, err
}

// Children gives the opener of the agents children run in. open is
// called each time the program works on a child, after a restart
// included. It returns the context the child's runs take and an agent
// for the child's session, whose config it passes through c.Apply: with
// agentturn/session, the context of Recorder.ContinueChild(ctx, c.Spawn),
// or of StartChild when the parent's session has no such spawn yet, and
// an agent seeded with what the child's session holds. A kept child's
// session holds its earlier runs. A graph with a fresh step, a fresh call
// or a step the host lets a judge answer needs it.
func Children(open func(ctx context.Context, c Child) (context.Context, *agentturn.Agent, error)) Option {
	return func(f *flow) { f.children = open }
}

// Models names the models a host serves to children, so a step that asks
// for another is refused by [Apply].
func Models(names ...string) Option {
	return func(f *flow) { f.models = append([]string{}, names...) }
}

// abandonText is what the program tells a child whose caller has been
// abandoned: every run under way in it is abandoned too, and none starts.
const abandonText = "agentflow: abandon"

// heldChild is the call of pending that waits on a child, if one does.
func heldChild(pending []agentturn.PendingCall) (agentturn.PendingCall, callArgs, bool) {
	for _, p := range pending {
		c := p.Call
		if c == nil || !strings.HasPrefix(c.CallID, reserved) || c.Name != callTool && c.Name != judgeTool {
			continue
		}
		var a callArgs
		if decode([]byte(c.Arguments), &a) == nil && a.Context == ContextFresh {
			return p, a, true
		}
	}
	return agentturn.PendingCall{}, callArgs{}, false
}

// childFor is the child a held call of st's run waits on.
func (f *flow) childFor(st State, call *openresponses.FunctionCall, a callArgs) (Child, error) {
	n := st.Node
	if n == nil {
		return Child{}, errors.New("agentflow: a call waits on a child, and its run is at no step")
	}
	g, err := st.g.find(a.Workflow)
	if err != nil {
		return Child{}, err
	}
	c := Child{Run: a.Run, Spawn: a.Spawn, Caller: call.CallID, Parent: st.Instance, Workflow: g.Name, Kept: a.Child,
		Depth: st.Depth + 1, Path: slices.Clone(st.Path), parent: f, g: g, maxDepth: st.maxDepth, maxSteps: a.MaxSteps}
	switch {
	case call.Name == judgeTool:
		c.Step, c.Judge = n.ID, true
		c.Settings.Tools = n.Judge.Tools
		if f.judgeName != f.modelName {
			c.Settings.Model = f.judgeName
		}
		c.policies = f.chain(st)
	case n.Kind == Agent:
		c.Step = n.ID
		c.Settings = ChildSettings{Dir: n.Dir, Model: n.Model, Thinking: cmp.Or(n.Thinking, st.g.Thinking), Instructions: n.Instructions, Tools: n.Tools}
		// The step's own policy is its node's in the child.
		c.policies = f.chain(st)
	default:
		c.Settings = ChildSettings{Dir: n.Dir, Model: n.Model, Instructions: n.Instructions}
		c.policies = f.policies(st)
	}
	return c, nil
}

// open asks the host for the child's agent, the context its runs take,
// and the flow it follows.
func (f *flow) open(ctx context.Context, c Child) (context.Context, *agentturn.Agent, *flow, error) {
	if f.children == nil {
		return nil, nil, nil, errors.New("agentflow: a call waits on a child, and the host gave no Children to open it")
	}
	var made *flow
	c.made = &made
	cctx, a, err := f.children(ctx, c)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("agentflow: open child %s of %s: %w", c.Run, c.Caller, err)
	}
	if a == nil || cctx == nil || made == nil {
		return nil, nil, nil, errors.New("agentflow: the opener gave no agent and context, or did not pass the child's config through Child.Apply")
	}
	return cctx, a, made, nil
}

// over is how the child's run ended, nil while it has not.
func (f *flow) over(ctx context.Context, a *agentturn.Agent, run string) (*State, error) {
	st, _, err := f.read(ctx, a.State().Transcript)
	if st.Instance != run || !st.Ended {
		return nil, nil
	}
	return &st, err
}

// started says the child's session holds the run.
func (f *flow) started(ctx context.Context, a *agentturn.Agent, run string) bool {
	st, _, _ := f.read(ctx, a.State().Transcript)
	return st.Instance == run && st.Started
}

// settle works on the child a held call waits on, with answers for what
// waits in it, until the child's run ends or waits. It returns the
// answer that lets the call go, or the child's end and state while it
// waits.
func (f *flow) settle(ctx context.Context, st State, p agentturn.PendingCall, a callArgs, answers []agentturn.Answer) (*agentturn.Answer, *agentturn.RunEnd, State, error) {
	c, err := f.childFor(st, p.Call, a)
	if err != nil {
		return nil, nil, st, err
	}
	cctx, child, cf, err := f.open(ctx, c)
	if err != nil {
		return nil, nil, st, err
	}
	var end *agentturn.RunEnd
	var cst State
	switch done, _ := cf.over(cctx, child, a.Run); {
	case done != nil:
	case len(answers) > 0:
		end, cst, err = cf.resume(cctx, child, answers)
	case len(child.State().Pending) > 0:
		end, cst, err = cf.drive(cctx, child, &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: child.State().Pending})
	case cf.started(cctx, child, a.Run):
		// Cut off part way: it goes on where it stood.
		if end, err = child.Continue(cctx); end != nil {
			end, cst, err = cf.drive(cctx, child, end)
		}
	default:
		var item openresponses.Item
		if item, err = cf.g.Invocation(inputsOrNil(a.With)); err == nil {
			trigger := agentturn.ContextWithTrigger(cctx, agentturn.Trigger{Kind: TriggerKind, Ref: c.Caller})
			if end, err = child.Prompt(trigger, item); end != nil {
				end, cst, err = cf.drive(cctx, child, end)
			}
		}
	}
	done, readErr := cf.over(cctx, child, a.Run)
	switch {
	case done != nil:
		ans, err := letGo(st, p, a, *done, readErr)
		return ans, nil, st, err
	case c.Judge && errors.Is(err, ErrStalled):
		// A judge that will not give a verdict has given none, and its look
		// still counts.
		looked, _, _ := cf.read(cctx, child.State().Transcript)
		ans, err := letGo(st, p, a, State{Entries: looked.Entries, Granted: looked.Granted}, err)
		return ans, nil, st, err
	case err != nil:
		return nil, end, cst, err
	case end != nil && end.Reason == agentturn.ReasonInputRequired:
		return nil, end, cst, nil
	}
	return nil, end, cst, fmt.Errorf("agentflow: child %s stopped without ending its run", c.Run)
}

func inputsOrNil(with map[string]any) any {
	if len(with) == 0 {
		return nil
	}
	return with
}

// letGo is the answer that lets a held call go, with what came of the
// child added to its arguments: done is the child's state at its end, and
// failed how it failed.
func letGo(st State, p agentturn.PendingCall, a callArgs, done State, failed error) (*agentturn.Answer, error) {
	n := st.Node
	switch {
	case p.Call.Name == judgeTool:
		if last := len(done.Steps) - 1; done.Done && last >= 0 {
			r := done.Steps[last].Result
			a.Verdict = &Verdict{Outcome: r.Outcome, Reason: r.Reason, Data: r.Data}
			if r.Outcome == HandBack {
				a.Verdict.Data = nil
			}
		}
	case treeWide(done.limit):
		// The child's tree came to a bound of the whole tree, which fails
		// the caller too.
		a.Result = &Result{Outcome: Failed, Limit: done.limit, Reason: fmt.Sprint(failed)}
	case n.Kind == Agent:
		last := len(done.Steps) - 1
		switch {
		case !done.Done || last < 0 || failed != nil:
			a.Failure = fmt.Sprintf("agentflow: step %q did not finish in its child: %v", n.ID, failed)
		case done.Steps[last].Result.Outcome == Stuck && !n.routesStuck():
			a.Failure = "agentflow: the model reported it is stuck: " + done.Steps[last].Result.Reason
		default:
			r := done.Steps[last].Result
			a.Result = &r
		}
	default:
		r, err := returned(n, a.Workflow, ending(done, failed))
		if err != nil {
			a.Failure = err.Error()
		} else {
			a.Result = &r
		}
	}
	// What the child's runs spent counts toward the caller's tree, however
	// they came out: a failed child's steps and a judge's look included.
	a.Entries, a.Granted = done.Entries, done.Granted
	args, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	ans := agentturn.ApproveWith(p.Call.CallID, args).WithBy("policy").WithReason("agentflow: the child's run has ended")
	return &ans, nil
}

// resume answers calls the tree waits on: those of a's own run, or those
// of the child a's held call waits on, and drives on.
func (f *flow) resume(ctx context.Context, a *agentturn.Agent, answers []agentturn.Answer) (*agentturn.RunEnd, State, error) {
	pending := a.State().Pending
	mine := func(ans agentturn.Answer) bool {
		return slices.ContainsFunc(pending, func(p agentturn.PendingCall) bool { return p.Call != nil && p.Call.CallID == ans.CallID })
	}
	if len(answers) > 0 && !slices.ContainsFunc(answers, func(ans agentturn.Answer) bool { return !mine(ans) }) {
		end, err := a.Resume(ctx, answers...)
		if end == nil {
			st, _, _ := f.read(ctx, a.State().Transcript)
			return nil, st, err
		}
		return f.drive(ctx, a, end)
	}
	st, _, err := f.read(ctx, a.State().Transcript)
	if err != nil {
		return nil, st, err
	}
	p, args, ok := heldChild(pending)
	if !ok {
		return nil, st, fmt.Errorf("%w: no call of the run, or of a child it waits on, is pending", agentturn.ErrNotPending)
	}
	ans, end, cst, err := f.settle(ctx, st, p, args, answers)
	if ans == nil {
		return end, cst, err
	}
	next, err := a.Resume(ctx, *ans)
	if next == nil {
		return nil, st, err
	}
	return f.drive(ctx, a, next)
}

// Resume answers calls that runs of the tree wait on, wherever they
// wait, and drives on as [Drive] does: an answer to a question asked in a
// child is given there, and the call that waits on the child is let go
// once the child's run is done. It returns as Drive does, with the end
// and state of the run that waits next.
func Resume(ctx context.Context, g *Graph, a *agentturn.Agent, answers []agentturn.Answer, opts ...Option) (*agentturn.RunEnd, State, error) {
	f, err := flowOf(g, a, opts)
	if err != nil {
		return nil, State{}, err
	}
	return f.resume(ctx, a, answers)
}

// Invoke asks for a new run of g over the runs under way, as a person
// asks with [Graph.Invocation]: every unfinished run of the tree is
// abandoned, innermost first, a child's in its own session, before the
// new one starts. A call held on a child is refused as abandoned. It
// then drives the new run as [Drive] does.
func Invoke(ctx context.Context, g *Graph, a *agentturn.Agent, inputs any, opts ...Option) (*agentturn.RunEnd, State, error) {
	f, err := flowOf(g, a, opts)
	if err != nil {
		return nil, State{}, err
	}
	item, err := g.Invocation(inputs)
	if err != nil {
		return nil, State{}, err
	}
	outs, err := f.abandon(ctx, a)
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

// abandon abandons the children a's pending calls wait on, and returns
// outputs refusing every pending call, to open the next prompt with.
func (f *flow) abandon(ctx context.Context, a *agentturn.Agent) (openresponses.Items, error) {
	pending := a.State().Pending
	if len(pending) == 0 {
		return nil, nil
	}
	st, _, err := f.read(ctx, a.State().Transcript)
	if err != nil {
		return nil, err
	}
	var outs openresponses.Items
	for _, p := range pending {
		if p.Call == nil {
			continue
		}
		if _, args, ok := heldChild([]agentturn.PendingCall{p}); ok {
			c, err := f.childFor(st, p.Call, args)
			if err != nil {
				return nil, err
			}
			cctx, child, cf, err := f.open(ctx, c)
			if err != nil {
				return nil, err
			}
			if cf.started(cctx, child, args.Run) {
				inner, err := cf.abandon(cctx, child)
				if err != nil {
					return nil, err
				}
				if end, err := child.Prompt(cctx, append(inner, openresponses.DeveloperText(abandonText))...); end == nil {
					return nil, err
				}
			}
		}
		outs = append(outs, openresponses.NewFunctionCallOutput(p.Call.CallID, "agentflow: abandoned"))
	}
	return outs, nil
}
