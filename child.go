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
	// Seeded says the child's model begins with what its caller's saw:
	// the opener forks the child's session from its caller's, before the
	// call Caller names, as agentturn/session's Recorder.ForkChild does at
	// EntryBefore of it. What the child's session then opens with is no
	// part of the child's record: the opener passes c.Apply a [History] of
	// the items the child's session wrote itself, and an [Inherited] of
	// those it opened with. Apply refuses a seeded child without both.
	Seeded bool
	// Seed is for an opener with no session to fork, one that keeps its
	// agents in memory: what the caller's agent held before the call,
	// which it seeds the child's agent with and gives back as Inherited.
	// An opener that forks a session leaves it alone.
	Seed openresponses.Items

	parent   *flow
	g        *Graph
	maxDepth int
	// maxSteps is what the caller's tree had left of its steps, and
	// policies are those of the steps and calls the child's work is
	// nested in, which hold its tools too.
	maxSteps int
	policies []ruled
	// documents are the texts the call passed in of the documents its step
	// reads.
	documents map[string]string
	// files are what the call passed in for its step to be given, and
	// question, for a judge, the question's text as the person is asked it.
	files    []fileRef
	question string
	made     **flow
}

// ChildSettings are what a step says of the child it runs in.
type ChildSettings struct {
	// Dir is the child's working directory, below the workspace's root,
	// empty for the root: the one its call recorded, resolved from the
	// record when the call was made. script.Sub makes a workspace of it.
	Dir string
	// Model names the model the child asks, empty for the caller's, and
	// Thinking how hard it reasons, for a fresh model step.
	Model    string
	Thinking string
	// Instructions is added to the child's instructions.
	Instructions string
	// Tools names the tools its step may call. AllTools says it may call
	// every tool the host offers but ExcludeTools, which it is never
	// offered.
	Tools        []string
	AllTools     bool
	ExcludeTools []string
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
		modes: p.modes, judgeName: p.judgeName, children: p.children, models: p.models, idle: p.idle, matchers: p.matchers, policyOpts: p.policyOpts,
		prices: p.prices, fileTool: p.fileTool, fileField: p.fileField}
	own := c
	f.child = &own
	for _, opt := range opts {
		opt(f)
	}
	if c.Seeded && (f.history == nil || f.inherited == nil) {
		return cfg, fmt.Errorf("agentflow: child %s of %s begins with its caller's context, so its session holds items that are not its record: give Child.Apply a History of the items the child's session wrote itself and an Inherited of those it opened with", c.Run, c.Caller)
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
// or when the parent's session has no such spawn yet, of StartChild, or
// for a seeded child, see [Child.Seeded], of ForkChild at the entry
// before the call c.Caller names; and an agent seeded with what the
// child's session holds. A kept child's session holds its earlier runs.
// A graph with a call to a workflow, a fresh step or a step the host lets
// a judge answer needs it.
//
// c.Apply checks the child's tools against its steps, as Apply does a
// root's: an opener returns its error as it is, or wrapped with %w, and a
// child whose config does not offer what its steps call, or not as they
// call it, fails the call that waits on it, which the step asks to retry.
// So does an error that wraps [ErrChildRefused]. Any other error is the
// host's, and comes back from [Drive].
func Children(open func(ctx context.Context, c Child) (context.Context, *agentturn.Agent, error)) Option {
	return func(f *flow) { f.children = open }
}

// ErrChildRefused is an opener's error that says the child cannot be
// opened as its call asks, such as in a directory that does not exist:
// something the step's inputs or the workspace got wrong, which a person
// may put right and retry, not a fault of the host's. An opener wraps it
// with %w; package script's Sub does for a dir it cannot work in. The call that
// waits on the child fails with the error's words, so its step asks to
// retry, or goes on where it routes on failed, and a judge's look gives no
// verdict, so the person is asked.
var ErrChildRefused = errors.New("agentflow: the child cannot be opened as its call asks")

// Models names the models a host serves to children, so a step that asks
// for another is refused by [Apply].
func Models(names ...string) Option {
	return func(f *flow) { f.models = append([]string{}, names...) }
}

// abandonText is what the program tells a child whose caller has been
// abandoned: every run under way in it is abandoned too, and none starts.
// A reason may follow, after ": ", which each run's end records.
const abandonText = "agentflow: abandon"

// abandonMessage tells a child to abandon its runs, for why, if any.
func abandonMessage(why string) openresponses.Item {
	if why == "" {
		return openresponses.DeveloperText(abandonText)
	}
	return openresponses.DeveloperText(abandonText + ": " + why)
}

// abandonedFor reads an abandon message, and the reason it gives.
func abandonedFor(text string) (why string, ok bool) {
	if text == abandonText {
		return "", true
	}
	return strings.CutPrefix(text, abandonText+": ")
}

// abandonedText is the output a host gives a call pending as it abandons
// the runs under way, before it asks for the next: agentflow's own words,
// so replay knows a cleanup question refused so was cut short, not failed.
const abandonedText = "agentflow: abandoned"

// heldChild is the call of pending that waits on a child, if one does.
func heldChild(pending []agentturn.PendingCall) (agentturn.PendingCall, callArgs, bool) {
	for _, p := range pending {
		c := p.Call
		if c == nil || !strings.HasPrefix(c.CallID, reserved) || c.Name != callTool && c.Name != judgeTool {
			continue
		}
		var a callArgs
		if decode([]byte(c.Arguments), &a) == nil && a.held() {
			return p, a, true
		}
	}
	return agentturn.PendingCall{}, callArgs{}, false
}

// childFor is the child a held call of st's run waits on.
func (f *flow) childFor(ctx context.Context, view agentturn.Transcript, st State, call *openresponses.FunctionCall, a callArgs) (Child, error) {
	n := st.Node
	if n == nil {
		return Child{}, errors.New("agentflow: a call waits on a child, and its run is at no step")
	}
	g, err := st.g.find(a.Workflow)
	if err != nil {
		return Child{}, err
	}
	c := Child{Run: a.Run, Spawn: a.Spawn, Caller: call.CallID, Parent: st.Instance, Workflow: g.Name, Kept: a.Child,
		Depth: st.Depth + 1, Path: slices.Clone(st.Path), parent: f, g: g, maxDepth: st.maxDepth, maxSteps: a.MaxSteps, documents: a.Documents, files: a.Files, question: a.Question}
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
		c.Settings = ChildSettings{Dir: a.Dir, Model: n.Model, Thinking: cmp.Or(n.Thinking, st.g.Thinking), Instructions: n.Instructions, Tools: n.Tools,
			AllTools: n.AllTools, ExcludeTools: st.g.excluded(n)}
		if st.Note != "" {
			c.Settings.Instructions = strings.TrimSpace(c.Settings.Instructions + "\n\nNote from the operator: " + st.Note)
		}
		// The step's own policy is its node's in the child, unless it takes
		// rules from the record, which only this run can resolve.
		c.policies = f.chain(st)
		if n.Policy.fromData() {
			c.policies = f.policies(st)
		}
	default:
		c.Settings = ChildSettings{Dir: a.Dir, Model: n.Model, Instructions: n.Instructions}
		c.policies = f.policies(st)
		if a.Context == ContextSession {
			c.Seeded = true
			if c.Seed, err = seedFor(view, call.CallID); err != nil {
				return Child{}, err
			}
		}
	}
	return c, nil
}

// seedFor is what an opener with no session seeds a child of the call id
// with: what the caller's agent, whose transcript is view, held before
// the call. The call's own turn is the program's, and is left out, so the
// child reads no call without a result. What no model may read of it the
// child's view leaves out, as the caller's did.
func seedFor(view agentturn.Transcript, id string) (openresponses.Items, error) {
	at := slices.IndexFunc(view, func(item openresponses.Item) bool {
		c, ok := item.(*openresponses.FunctionCall)
		return ok && c.CallID == id
	})
	if at < 0 {
		return nil, fmt.Errorf("agentflow: the call %s is not in its session's transcript", id)
	}
	return slices.Clone(openresponses.Items(view[:at])), nil
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

// past is the state at its end of a run the child's session holds and
// is no longer at, nil for none: a kept child's earlier visit, or a run
// abandoned there.
func (f *flow) past(ctx context.Context, a *agentturn.Agent, run string) (*State, error) {
	all, err := f.record(ctx, a.State().Transcript)
	if err != nil {
		return nil, err
	}
	runs, err := Runs(all, nil)
	if err != nil {
		return nil, err
	}
	for _, in := range runs {
		if in.ID == run && in.End >= 0 {
			st := in.State
			st.Summary = in.Summary
			return &st, nil
		}
	}
	return nil, nil
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
func (f *flow) settle(ctx context.Context, view agentturn.Transcript, st State, p agentturn.PendingCall, a callArgs, answers []agentturn.Answer) (*agentturn.Answer, *agentturn.RunEnd, State, error) {
	due := callDeadline(st, p)
	c, err := f.childFor(ctx, view, st, p.Call, a)
	if err != nil {
		return nil, nil, st, err
	}
	cctx, child, cf, err := f.open(ctx, c)
	late := !due.IsZero() && !time.Now().Before(due)
	if unfit := (*unfitChild)(nil); late && (errors.Is(err, ErrChildRefused) || errors.As(err, &unfit)) {
		// Its time ran out over a child that cannot be opened: there is
		// nothing there to read or to abandon, and the call times out.
		ans, err := f.expireChild(ctx, view, st, p, a)
		return ans, nil, st, err
	}
	if unfit := (*unfitChild)(nil); errors.As(err, &unfit) {
		// The child cannot run its steps on the tools it was given: the call
		// that waits on it fails, as the step's own call would.
		ans, err := failCall(st.Node, p, a, "agentflow: the child cannot run on the tools it was given: "+strings.TrimPrefix(unfit.Error(), "agentflow: "),
			"agentflow: the child's config does not fit its steps")
		return ans, nil, st, err
	}
	if errors.Is(err, ErrChildRefused) {
		// The child cannot be opened where it was asked to be: its step
		// fails, and may be retried once that is put right.
		ans, err := failCall(st.Node, p, a, err.Error(), "agentflow: the child cannot be opened as its call asks")
		return ans, nil, st, err
	}
	if err != nil {
		return nil, nil, st, err
	}
	// A call with a timeout holds its child to it: past it, the child's runs
	// are abandoned and the call fails. A child whose run ended in time is
	// not held to it, however late its caller comes to read the end.
	if late {
		if done, _ := cf.over(cctx, child, a.Run); done == nil {
			ans, err := f.expireChild(ctx, view, st, p, a)
			return ans, nil, st, err
		}
	}
	var end *agentturn.RunEnd
	var cst State
	// The child's runs take the call's deadline; reading the child's
	// record afterwards does not.
	run := cctx
	if !due.IsZero() {
		var stop context.CancelFunc
		run, stop = context.WithDeadline(cctx, due)
		defer stop()
	}
	// A run the child's session has ended and moved on from, abandoned
	// there before this session heard of it, is read where it ended: its
	// key is not started a second time.
	if done, _ := cf.over(cctx, child, a.Run); done == nil {
		past, err := cf.past(cctx, child, a.Run)
		if err != nil {
			return nil, nil, st, err
		}
		if past != nil {
			ans, err := letGo(st, p, a, *past, nil)
			return ans, nil, st, err
		}
	}
	switch done, _ := cf.over(cctx, child, a.Run); {
	case done != nil:
	case len(answers) > 0:
		end, cst, err = cf.resume(run, child, answers)
	case len(child.State().Pending) > 0:
		end, cst, err = cf.drive(run, child, &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: child.State().Pending})
	case cf.started(cctx, child, a.Run):
		// Cut off part way: it goes on where it stood.
		if end, err = child.Continue(run); end != nil {
			end, cst, err = cf.drive(run, child, end)
		}
	default:
		var item openresponses.Item
		if item, err = cf.g.invocation(inputsOrNil(a.With)); err == nil {
			trigger := agentturn.ContextWithTrigger(run, agentturn.Trigger{Kind: TriggerKind, Ref: c.Caller})
			if end, err = child.Prompt(trigger, item); end != nil {
				end, cst, err = cf.drive(run, child, end)
			}
		}
	}
	if !due.IsZero() && ctx.Err() == nil && errors.Is(run.Err(), context.DeadlineExceeded) {
		if done, _ := cf.over(cctx, child, a.Run); done == nil {
			ans, err := f.expireChild(ctx, view, st, p, a)
			return ans, nil, st, err
		}
	}
	done, readErr := cf.over(cctx, child, a.Run)
	switch {
	case done != nil:
		ans, err := letGo(st, p, a, *done, readErr)
		return ans, nil, st, err
	case c.Judge && errors.Is(err, ErrStalled):
		// A judge that will not give a verdict has given none, and its look
		// still counts. Its run is ended where it stalled, so the child's
		// record does not read as a look still under way.
		stalled := err
		var looked State
		if looked.Entries, looked.Granted, looked.Summary, err = f.abandonChild(ctx, view, st, p, a, "the judge gave no verdict"); err != nil {
			return nil, nil, st, err
		}
		ans, err := letGo(st, p, a, looked, stalled)
		return ans, nil, st, err
	case err != nil:
		return nil, end, cst, err
	case end != nil && end.Reason == agentturn.ReasonInputRequired:
		return nil, end, cst, nil
	}
	// The child's agent stopped for a reason of its own, such as its turns
	// or its output running out: the error says which, since the call it
	// leaves held cannot.
	why := ""
	if end != nil {
		why = ": its agent's run ended " + string(end.Reason)
		if end.Cause != "" {
			why += ", " + string(end.Cause)
		}
	}
	return nil, end, cst, fmt.Errorf("agentflow: child %s stopped without ending its run%s", c.Run, why)
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
			r.Documents = editedDocuments(n, done.Documents)
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
	a.Entries, a.Granted, a.Summary = done.Entries, done.Granted, done.Summary
	args, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	ans := agentturn.ApproveWith(p.Call.CallID, args).WithBy("policy").WithReason("agentflow: the child's run has ended")
	return &ans, nil
}

// failCall is the answer that lets a held call go when its child cannot
// run at all, for why: the outcome failed where a call to a workflow
// routes on it, else a failure the step asks to retry, and for a judge's
// look no verdict, so the person is asked. No step of the child ran, so
// there is nothing to count. reason is the decision's.
func failCall(n *Node, p agentturn.PendingCall, a callArgs, why, reason string) (*agentturn.Answer, error) {
	switch {
	case p.Call.Name == judgeTool:
		a.Verdict = &Verdict{Outcome: HandBack, Reason: why, None: true}
	case n != nil && n.Flow != "" && n.routesFailure():
		a.Result = &Result{Outcome: Failed, Data: map[string]any{"reason": why, "workflow": a.Workflow}}
	default:
		a.Failure = why
	}
	args, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	ans := agentturn.ApproveWith(p.Call.CallID, args).WithBy("policy").WithReason(reason)
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
	ans, end, cst, err := f.settle(ctx, a.State().Transcript, st, p, args, answers)
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
	outs, err := f.setAside(ctx, a, abandonedText, "")
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

// setAside abandons the children a's pending calls wait on, and returns
// outputs, of text, for every pending call, to open the next prompt with.
//
// why is what the children's runs are abandoned for, which their ends
// record: an operator's override, say; empty for a new invocation.
func (f *flow) setAside(ctx context.Context, a *agentturn.Agent, text, why string) (openresponses.Items, error) {
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
			// What the child's run spent counts toward this tree however the
			// call goes, so the call is answered with it: an override cannot
			// buy steps by throwing a child's away.
			let := callFailure{Failure: text}
			var err error
			if let.Entries, let.Granted, let.Summary, err = f.abandonChild(ctx, a.State().Transcript, st, p, args, why); err != nil {
				return nil, err
			}
			b, err := json.Marshal(let)
			if err != nil {
				return nil, err
			}
			outs = append(outs, openresponses.NewFunctionCallOutput(p.Call.CallID, string(b)))
			continue
		}
		outs = append(outs, openresponses.NewFunctionCallOutput(p.Call.CallID, text))
	}
	return outs, nil
}

// abandonChild abandons the runs of the child p waits on, for why, each
// after its cleanup, and says what the child's run spent: its tree's
// steps, what a person granted there, and its summary. A child that never
// started spent nothing.
//
// Nor did one that cannot be opened as its call asks, refused by the host
// or unfit for its tools: it never ran there, nothing in it can be
// abandoned, and setting its call aside, or timing it out, must not fail
// for that.
func (f *flow) abandonChild(ctx context.Context, view agentturn.Transcript, st State, p agentturn.PendingCall, args callArgs, why string) (entries, granted int, summary *Summary, err error) {
	c, err := f.childFor(ctx, view, st, p.Call, args)
	if err != nil {
		return 0, 0, nil, err
	}
	cctx, child, cf, err := f.open(ctx, c)
	if unfit := (*unfitChild)(nil); errors.Is(err, ErrChildRefused) || errors.As(err, &unfit) {
		return 0, 0, nil, nil
	}
	if err != nil {
		return 0, 0, nil, err
	}
	if !cf.started(cctx, child, args.Run) {
		// Not under way there: it never started, and spent nothing, or an
		// abandon cut off before this session heard of it has ended it
		// already, and what it spent is on its record.
		return cf.spentBy(cctx, child, args.Run)
	}
	inner, err := cf.setAside(cctx, child, abandonedText, why)
	if err != nil {
		return 0, 0, nil, err
	}
	// The abandon is the program's word to the child, not a person's.
	from := agentturn.ContextWithTrigger(cctx, agentturn.Trigger{Kind: TriggerKind, Ref: c.Caller})
	end, err := child.Prompt(from, append(inner, abandonMessage(why))...)
	if end == nil {
		return 0, 0, nil, err
	}
	// A cleanup that waits on a question in the child is cut short: the
	// person is asked nothing in a session abandoned from above, and the
	// child's runs end.
	if end.Reason == agentturn.ReasonInputRequired {
		var refused openresponses.Items
		for _, p := range end.Pending {
			if p.Call != nil {
				refused = append(refused, openresponses.NewFunctionCallOutput(p.Call.CallID, abandonedText))
			}
		}
		if end, err := child.Prompt(from, append(refused, abandonMessage(why))...); end == nil {
			return 0, 0, nil, err
		}
	}
	return cf.spentBy(cctx, child, args.Run)
}

// expireChild lets go a call into a child whose time ran out: the child's
// runs are abandoned, each after its cleanup, and the call ends with the
// outcome failed where its step routes on it, or as a failure the step
// asks to retry. What the child spent still counts toward the caller's
// tree.
func (f *flow) expireChild(ctx context.Context, view agentturn.Transcript, st State, p agentturn.PendingCall, a callArgs) (*agentturn.Answer, error) {
	n := st.Node
	c := callClock{workflow: st.Workflow, step: n.ID, timeout: n.Timeout}
	why := c.why()
	var err error
	if a.Entries, a.Granted, a.Summary, err = f.abandonChild(ctx, view, st, p, a, why); err != nil {
		return nil, err
	}
	if n.routesFailure() {
		a.Result = &Result{Outcome: Failed, Data: map[string]any{"reason": why, "workflow": a.Workflow}}
	} else {
		a.Failure = "agentflow: " + why
	}
	args, err := json.Marshal(a)
	if err != nil {
		return nil, err
	}
	ans := agentturn.ApproveWith(p.Call.CallID, args).WithBy("policy").WithReason("agentflow: the call ran past its timeout, at " + st.Deadline.Format(time.RFC3339))
	return &ans, nil
}

// callDeadline is when the call p of st's step must have come back by,
// zero for none: a fresh call's to a workflow with a timeout.
func callDeadline(st State, p agentturn.PendingCall) time.Time {
	if n := st.Node; n == nil || n.Flow == "" || p.Call == nil || p.Call.Name != callTool {
		return time.Time{}
	}
	return st.Deadline
}

// spentBy is what run spent in a's session, as its record says once its
// end is written: the steps its tree entered, what a person granted there,
// and its summary. A kept child's session holds a run for each visit, and
// only run's are counted.
func (f *flow) spentBy(ctx context.Context, a *agentturn.Agent, run string) (entries, granted int, summary *Summary, err error) {
	all, err := f.record(ctx, a.State().Transcript)
	if err != nil {
		return 0, 0, nil, err
	}
	runs, err := Runs(all, nil)
	if err != nil {
		return 0, 0, nil, err
	}
	for _, in := range runs {
		if in.ID == run {
			return in.State.Entries, in.State.Granted, in.Summary, nil
		}
	}
	return 0, 0, nil, nil
}
