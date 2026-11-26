package agentflow

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

const (
	reserved   = "flow_"
	startTool  = "flow_start"
	askTool    = "flow_ask"
	finishTool = "flow_finish"
	judgeTool  = "flow_judge"
	endTool    = "flow_end"
	callTool   = "flow_call"

	// The outcomes of the question a failed step raises.
	retryOutcome = "retry"
	stopOutcome  = "stop"

	invocationPrefix = "agentflow: start "
	nudgePrefix      = "agentflow: the step is not finished."
	beginPrefix      = "agentflow: begin step "
)

var (
	// ErrStepFailed is a run or person step whose output is not a
	// [Result] with a declared outcome, and that is not to be retried.
	ErrStepFailed = errors.New("agentflow: step failed")
	// ErrVisits is a node reached more often than it allows.
	ErrVisits = errors.New("agentflow: visit limit reached")
	// ErrChanged is a transcript whose run started with another
	// definition, of the workflow or of one it can call.
	ErrChanged = errors.New("agentflow: the workflow changed since it started")
	// ErrDepth is a call nested deeper than the run allows. It fails
	// every run it is nested in, since calling again would go as deep.
	ErrDepth = errors.New("agentflow: calls nested too deeply")
)

// Result is the output of every step. A tool a Run node calls returns
// one. Anything else, an error or a refusal among them, fails the step:
// a transcript carries no failure flag, so success has to be said.
type Result struct {
	Outcome string `json:"outcome"`
	Data    any    `json:"data,omitempty"`
	// By says who chose, when it was not whoever the node names.
	By string `json:"by,omitempty"`
	// Reason is why, in the words of whoever chose. It is for whoever
	// reads the record: no step reads it as data.
	Reason string `json:"reason,omitempty"`
}

// Step is one finished node.
type Step struct {
	Node   string
	Visit  int
	Result Result
	// Advice is what a judge said of this question, if one was asked.
	Advice *Verdict
	// Blind says whoever answered could not have seen Advice, which was
	// asked for afterwards.
	Blind bool
}

// Place is one run on the way from a run's root to where it stands.
type Place struct {
	Workflow string `json:"workflow"`
	Run      string `json:"run"`
	Step     string `json:"step,omitempty"`
}

// State is where one run of a workflow stands. Runs can share a
// transcript, one nested in another; [Graph.Replay] gives the innermost
// under way.
type State struct {
	// Started says the transcript holds an instance, named by Instance,
	// its key.
	Started  bool
	Instance string
	// Workflow is the workflow the run is of. Parent is the key of the
	// run that called it, Caller the ID of the calling call, and Depth
	// how deeply it is nested: 0 for a run nobody called.
	Workflow string
	Parent   string
	Caller   string
	Depth    int
	// Inputs is what the invocation carried, and Outputs what the done
	// workflow gives back.
	Inputs  any
	Outputs map[string]any
	// Outcome is the end the done workflow reached.
	Outcome string
	// Node is the node to run next, nil before the start and when done.
	Node *Node
	// Visit counts this arrival at Node and Attempt this try, from 1.
	Visit, Attempt int
	// Failure is the output of Node's failed attempt, until a retry.
	Failure string
	// Done says the last step has finished, and Reported that the
	// workflow has since said so. The conversation is then the model's.
	Done, Reported bool
	// Nudges counts the reminders given since Node became current.
	Nudges int
	// Begun says the transcript holds the message that opens this visit
	// of an Agent node.
	Begun bool
	// Advice is the judge's verdict on the question now being asked.
	Advice *Verdict
	// Held is a person's answer waiting for a judge to give its verdict
	// unseen, before the step finishes with it.
	Held *Result
	// Ended says flow_end has written down how this instance finished.
	Ended bool
	// Callee is the key of the run a Flow node started for this
	// attempt, and Returned its ending once that is written.
	Callee   string
	Returned *Ending
	// Entries counts the steps entered across the whole tree of runs
	// this one belongs to, attempts and calls included.
	Entries int
	// Path is the way from the root run to this one, with the step each
	// is at.
	Path []Place
	// Steps are the finished nodes, oldest first.
	Steps []Step

	// Fresh says the run is in a child session of its own, which its
	// caller's call holds.
	Fresh bool
	// Left says the runs under way were abandoned with their caller, and
	// none is to start until an invocation.
	Left bool

	// abandoned are earlier runs another was asked for over, innermost
	// first, with nothing yet written of their ends.
	abandoned []*Ending
	// refused counts the verdicts a judge gave that it may not give.
	refused int
	// kept are the children the run keeps, by name, each with its spawn.
	kept map[string]string

	g        *Graph
	prefix   string
	maxDepth int
	// calleeInputs is what Callee was started with.
	calleeInputs any
	// limit names the bound across the tree the run failed on, which no
	// caller may retry or route on.
	limit string
}

// limits are the bounds across a tree of runs, by the name an ending
// records.
var limits = map[string]error{"depth": ErrDepth}

// limitError is a bound across the tree that a run nested deeper hit.
type limitError struct {
	limit  error
	reason string
}

func (e *limitError) Error() string { return e.reason }
func (e *limitError) Unwrap() error { return e.limit }

// startPayload is flow_start's arguments.
type startPayload struct {
	Workflow string `json:"workflow"`
	// Digest is, for a run nobody called, that of the whole closure
	// ([Graph.Digest]), and for a call the callee's own.
	Digest string `json:"digest"`
	Inputs any    `json:"inputs,omitempty"`
	Format int    `json:"format"`
	// Definition is the graph itself, so the transcript says what ran
	// after the workflow has changed. The model is never shown it. A
	// call's start leaves it out: the root's start pins it.
	Definition json.RawMessage `json:"definition,omitempty"`
	// Definitions are every other workflow the root can reach, by name.
	Definitions map[string]pinned `json:"definitions,omitempty"`
	Run         string            `json:"run"`
	Parent      string            `json:"parent,omitempty"`
	Caller      string            `json:"caller,omitempty"`
	Depth       int               `json:"depth,omitempty"`
	MaxDepth    int               `json:"max_depth,omitempty"`
	// Context is fresh for a run in a child session, whose caller's call
	// holds it. Path is then the way to the call from the run nobody
	// called, so the child reads on its own.
	Context string  `json:"context,omitempty"`
	Path    []Place `json:"path,omitempty"`
}

// pinned is one workflow a run may reach, as its start recorded it.
type pinned struct {
	Digest     string          `json:"digest"`
	Definition json.RawMessage `json:"definition"`
}

// Invocation is the item that asks for a new instance, carrying its
// inputs, nil for none. A transcript with no instance starts one without
// it, with no inputs.
func (g *Graph) Invocation(inputs any) (openresponses.Item, error) {
	text := invocationPrefix + g.Name
	if inputs != nil {
		if err := checkData(g.Inputs, inputs); err != nil {
			return nil, fmt.Errorf("agentflow: inputs: %w", err)
		}
		b, err := json.Marshal(inputs)
		if err != nil {
			return nil, err
		}
		text += " " + string(b)
	}
	return openresponses.DeveloperText(text), nil
}

// invoked reads an invocation of g and the inputs it carries.
func (g *Graph) invoked(text string) (inputs any, ok bool) {
	rest, ok := strings.CutPrefix(text, invocationPrefix+g.Name)
	switch {
	case !ok || rest != "" && rest[0] != ' ':
		return nil, false
	case rest == "":
		return nil, true
	}
	return inputs, decode([]byte(rest), &inputs) == nil
}

// newKey draws a run's key.
func newKey() (string, error) {
	var b [5]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// keyPrefix starts every call ID of the run with key.
func keyPrefix(key string) string { return reserved + key + "_" }

// callID is the ID of the call the program makes for an attempt.
func (st State) callID() string {
	id := st.prefix + st.Node.ID + "_v" + strconv.Itoa(st.Visit)
	if st.Attempt > 1 {
		id += "-a" + strconv.Itoa(st.Attempt)
	}
	return id
}

// retryID is the ID of the question a failed attempt raises.
func (st State) retryID() string { return st.callID() + "-retry" }

func (st State) endID() string { return st.prefix + "end" }

// judgeID is the ID of the call that has a judge look at a question.
func (st State) judgeID() string { return st.callID() + "-judge" }

// frame is one run as replay reads it.
type frame struct {
	st     State
	visits map[string]int
	// since is where the current node began: a finish made before then
	// was not about it.
	since  int
	failed error
	// start and end are the items of the run's start and of its ending,
	// end -1 while there is none.
	start, end int
	ending     *Ending
	parent     *frame
	// children are the calls it made into child sessions, in order.
	children []freshCall
	// path is where a run in a child stands in the tree it was called
	// from.
	path []Place
}

// freshCall is a call a run made into a child session.
type freshCall struct {
	call                 string
	workflow, run, spawn string
}

// replay reads a transcript as a stack of runs.
type replay struct {
	g       *Graph
	closure map[string]*Graph
	calls   map[string]*openresponses.FunctionCall
	at      map[string]int
	// root is the run nobody called, started or about to be, and stack
	// the runs under way, root first.
	root    *frame
	stack   []*frame
	frames  []*frame
	entries int
	// abandoned outlives a root: it is written before the next starts.
	abandoned []*Ending
	// prefixes are those of every run started, of any workflow, so a
	// call none of them made is known to have lost its start.
	prefixes []string
	orphaned bool
}

// Replay reads where the workflow stands from a transcript: the
// innermost run under way, or the last that ran. It is a pure function
// of the graph, those it can call, and the transcript. On error the
// state is where that run stopped.
func (g *Graph) Replay(t agentturn.Transcript) (State, error) {
	r := g.replay(t)
	f := r.top()
	st := r.state(f)
	if r.orphaned && !r.root.st.Started && f.failed == nil {
		return st, fmt.Errorf("%w: the transcript holds a run of %q without its start", ErrPositionLost, g.Name)
	}
	return st, f.failed
}

func (g *Graph) replay(t agentturn.Transcript) *replay {
	r := &replay{g: g, closure: g.closure(), calls: map[string]*openresponses.FunctionCall{}, at: map[string]int{}}
	r.root = r.newRoot(nil)
	for i, item := range t {
		switch it := item.(type) {
		case *openresponses.Message:
			r.message(i, it)
		case *openresponses.FunctionCall:
			r.call(i, it)
		case *openresponses.FunctionCallOutput:
			r.output(i, it)
		}
	}
	return r
}

func (r *replay) newRoot(inputs any) *frame {
	return &frame{st: State{g: r.g, Workflow: r.g.Name, Inputs: inputs}, visits: map[string]int{}, since: -1, start: -1, end: -1}
}

// top is the innermost run under way, or the root when none is.
func (r *replay) top() *frame {
	if len(r.stack) > 0 {
		return r.stack[len(r.stack)-1]
	}
	return r.root
}

// state is f's state as the rest of the package reads it.
func (r *replay) state(f *frame) State {
	st := f.st
	st.abandoned = r.abandoned
	st.Entries = r.entries
	st.Path = slices.Clone(r.root.path)
	for _, s := range r.stack {
		p := Place{Workflow: s.st.Workflow, Run: s.st.Instance}
		if s.st.Node != nil {
			p.Step = s.st.Node.ID
		}
		st.Path = append(st.Path, p)
		if s == f {
			break
		}
	}
	return st
}

var tagPattern = regexp.MustCompile(`\(run ([A-Za-z0-9_-]+), visit (\d+)\)\s*$`)

// tagged says which run a workflow message is about, "" for none.
func tagged(text string) string {
	if m := tagPattern.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

// mine says a workflow message names st's run.
func (st *State) mine(text string) bool { return tagged(text) == st.Instance }

// known says a workflow message names a run the transcript started.
func (r *replay) known(text string) bool {
	key := tagged(text)
	return key != "" && slices.Contains(r.prefixes, keyPrefix(key))
}

func (r *replay) message(i int, m *openresponses.Message) {
	if m.Role == openresponses.RoleDeveloper {
		if inputs, ok := r.g.invoked(m.Text()); ok {
			r.invoke(i, inputs)
			return
		}
		if m.Text() == abandonText {
			r.invoke(i, nil)
			r.root.st.Left = true
			return
		}
	}
	f := r.top()
	st := &f.st
	text := m.Text()
	switch {
	case !st.Started && m.Role == openresponses.RoleUser && strings.HasPrefix(text, beginPrefix):
		// An earlier run's step, of another workflow a kept child ran,
		// has its start; one without has lost it.
		r.orphaned = r.orphaned || !r.known(text)
	case m.Role == openresponses.RoleUser && strings.HasPrefix(text, nudgePrefix):
		if st.mine(text) {
			st.Nudges++
		}
	case st.Node != nil && opens(m, st):
		st.Begun = true
	case m.Role == openresponses.RoleAssistant && st.Done && st.Ended:
		st.Reported = true
	}
}

// invoke asks for a new run, abandoning every one under way, innermost
// first.
func (r *replay) invoke(i int, inputs any) {
	var left []*Ending
	for _, f := range slices.Backward(r.stack) {
		if f.ending == nil {
			e := ending(f.st, f.failed)
			left = append(left, e)
		}
	}
	r.abandoned = append(left, r.abandoned...)
	r.stack, r.entries, r.orphaned = nil, 0, false
	r.root = r.newRoot(inputs)
}

func (r *replay) call(i int, c *openresponses.FunctionCall) {
	r.calls[c.CallID] = c
	r.at[c.CallID] = i
	if !strings.HasPrefix(c.CallID, reserved) {
		return
	}
	if c.Name == startTool {
		if prefix, ok := strings.CutSuffix(c.CallID, "start"); ok {
			r.prefixes = append(r.prefixes, prefix)
		}
		return
	}
	// A call into a child is the run's from when it is made: the child
	// it keeps is kept from then on, whatever comes of the call.
	if f := r.top(); f.st.Started && strings.HasPrefix(c.CallID, f.st.prefix) && (c.Name == callTool || c.Name == judgeTool) {
		var a callArgs
		if decode([]byte(c.Arguments), &a) == nil && a.Context == ContextFresh {
			f.children = append(f.children, freshCall{call: c.CallID, workflow: a.Workflow, run: a.Run, spawn: a.Spawn})
			if a.Child != "" {
				if f.st.kept == nil {
					f.st.kept = map[string]string{}
				}
				if f.st.kept[a.Child] == "" {
					f.st.kept[a.Child] = a.Spawn
				}
			}
		}
	}
	// Only the program makes these IDs. An earlier run's end is the one
	// that stands without its run.
	known := slices.ContainsFunc(r.prefixes, func(p string) bool { return strings.HasPrefix(c.CallID, p) })
	r.orphaned = r.orphaned || !r.root.st.Started && c.Name != endTool && !known
}

func (r *replay) output(i int, out *openresponses.FunctionCallOutput) {
	c := r.calls[out.CallID]
	if c != nil && c.Name == endTool {
		r.ended(c, out)
		return
	}
	f := r.top()
	if c != nil && c.Name == startTool && strings.HasPrefix(c.CallID, reserved) {
		r.started(i, f, c, out)
		return
	}
	st := &f.st
	if f.failed != nil || st.Done || !st.Started || c == nil {
		return
	}
	// What is not the run's is not its progress.
	switch {
	case strings.HasPrefix(c.CallID, reserved):
		if !strings.HasPrefix(c.CallID, st.prefix) {
			return
		}
	case c.Name == finishTool || c.Name == judgeTool:
		if runOf(c.Arguments) != st.Instance {
			return
		}
	}
	n := st.Node
	res, invalid := parseResult(n, out.Output.String())
	switch {
	case st.Failure != "":
		if out.CallID != st.retryID() {
			return
		}
		var answer Result
		_ = json.Unmarshal([]byte(out.Output.String()), &answer)
		if answer.Outcome == retryOutcome {
			st.Attempt, st.Failure = st.Attempt+1, ""
			st.Callee, st.Returned, st.calleeInputs = "", nil, nil
			r.entries++
		} else {
			f.failed = fmt.Errorf("%w: %s: %s", ErrStepFailed, n.ID, st.Failure)
		}
		return
	case c.Name == judgeTool && strings.HasPrefix(c.CallID, reserved):
		// A judge's look, from its child: under shadow it finishes the
		// held answer's step, and otherwise it advises the question.
		if out.CallID != st.judgeID() || n.Kind != Person {
			return
		}
		v, ok := verdictOf(n, out)
		switch {
		case st.Held != nil:
			advice := &v
			if !ok || v.None {
				advice = nil
			}
			r.finish(f, i, Step{Result: *st.Held, Advice: advice, Blind: true})
		case st.Advice != nil:
		case ok:
			st.Advice = &v
		default:
			// A look that failed is no verdict, and the person is asked.
			st.Advice = &Verdict{Outcome: HandBack, Reason: "the judge's look failed: " + out.Output.String(), None: true}
		}
		return
	case st.Held != nil:
		// Nothing but the judge's look moves a held answer on.
		return
	case n.Kind == Agent && !n.fresh():
		// A finish that was rejected or is for another node is not
		// progress; the model saw the error and goes on. A judge that
		// gives two verdicts it may not give has given none.
		finish := finishTool
		if n.Verdict {
			finish = judgeTool
		}
		if c.Name != finish || finishStep(c.Arguments) != n.ID || r.at[out.CallID] < f.since {
			return
		}
		if invalid != nil {
			if st.refused++; n.Verdict && st.refused >= 2 && f.failed == nil {
				f.failed = fmt.Errorf("%w: %s: two verdicts it may not give", ErrStepFailed, n.ID)
			}
			return
		}
	default:
		if out.CallID != st.callID() || n.Flow != "" && !n.fresh() && st.Returned == nil {
			return
		}
		if invalid != nil {
			st.Failure = out.Output.String()
			if json.Valid([]byte(st.Failure)) {
				st.Failure = invalid.Error() + ": " + st.Failure
			}
			return
		}
	}
	// A person's answer to a question judged unseen waits for the judge.
	// An answer from anything else does not.
	if n.Kind == Person && res.By == "" && askedUnseen(c) {
		f.since, st.Nudges, st.Begun, st.Held = i, 0, false, &res
		return
	}
	r.finish(f, i, Step{Result: res, Advice: st.Advice})
}

// started reads a flow_start: the root's, or that of a call nested in
// the run under way.
func (r *replay) started(i int, f *frame, c *openresponses.FunctionCall, out *openresponses.FunctionCallOutput) {
	var p startPayload
	if decode([]byte(c.Arguments), &p) != nil {
		return
	}
	ok := func() bool {
		var res Result
		return json.Unmarshal([]byte(out.Output.String()), &res) == nil && res.Outcome == OK
	}
	prefix := strings.TrimSuffix(c.CallID, "start")
	if p.Parent == "" || p.Context == ContextFresh {
		// A run nobody called here: the root, or a child's, whose caller
		// is in another session.
		root := r.root
		if f != root || root.st.Started || root.failed != nil || p.Workflow != r.g.Name {
			return
		}
		switch {
		case !ok():
			root.failed = fmt.Errorf("%w: start: %s", ErrStepFailed, out.Output.String())
			return
		case p.Format != RecordFormat:
			root.failed = fmt.Errorf("%w: record format %d, and this reads %d", ErrChanged, p.Format, RecordFormat)
			return
		case p.Digest != r.g.Digest():
			root.failed = r.changed(p)
			return
		}
		root.st = State{Started: true, Instance: p.Run, Workflow: r.g.Name, Inputs: p.Inputs, Parent: p.Parent, Caller: p.Caller,
			Depth: p.Depth, Fresh: p.Context == ContextFresh, g: r.g, prefix: prefix, maxDepth: cmpDepth(p.MaxDepth)}
		root.path = p.Path
		root.start = r.at[c.CallID]
		r.stack = []*frame{root}
		r.frames = append(r.frames, root)
		r.enter(root, i, &r.g.Nodes[0])
		return
	}
	// A call: the run under way must be at the node that makes it, and
	// not have made it yet.
	st := &f.st
	n := st.Node
	if f.failed != nil || !st.Started || st.Done || n == nil || n.Flow == "" || n.fresh() || st.Callee != "" || st.Failure != "" ||
		p.Parent != st.Instance || p.Caller != st.callID() {
		return
	}
	g := r.closure[p.Workflow]
	switch {
	case g == nil || g.OwnDigest() != p.Digest || st.g.Callees[n.Flow] != g:
		f.failed = fmt.Errorf("%w: step %q started %s, which the run did not pin", ErrChanged, n.ID, p.Workflow)
		return
	case !ok():
		st.Failure = "the call could not start: " + out.Output.String()
		return
	}
	st.Callee, st.calleeInputs = p.Run, p.Inputs
	child := &frame{st: State{Started: true, Instance: p.Run, Workflow: g.Name, Parent: p.Parent, Caller: p.Caller, Depth: p.Depth,
		Inputs: p.Inputs, g: g, prefix: prefix, maxDepth: st.maxDepth},
		visits: map[string]int{}, since: -1, start: r.at[c.CallID], end: -1, parent: f}
	r.stack = append(r.stack, child)
	r.frames = append(r.frames, child)
	r.enter(child, i, &g.Nodes[0])
}

// changed is the error of a root that started with another definition,
// naming what changed.
func (r *replay) changed(p startPayload) error {
	var names []string
	if old, err := decodeGraph(p.Definition); err != nil || old.OwnDigest() != r.g.OwnDigest() {
		names = append(names, r.g.Name)
	}
	for _, name := range slices.Sorted(maps.Keys(r.closure)) {
		if name == r.g.Name {
			continue
		}
		if pin, ok := p.Definitions[name]; !ok || pin.Digest != r.closure[name].OwnDigest() {
			names = append(names, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.Definitions)) {
		if r.closure[name] == nil {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ErrChanged
	}
	return fmt.Errorf("%w: %s", ErrChanged, strings.Join(names, ", "))
}

func cmpDepth(d int) int {
	if d <= 0 {
		return DefaultMaxDepth
	}
	return d
}

// ended reads a flow_end. An ending counts however the call went: it is
// what was found, and asking again would not make it more so.
func (r *replay) ended(c *openresponses.FunctionCall, out *openresponses.FunctionCallOutput) {
	if k := slices.IndexFunc(r.abandoned, func(e *Ending) bool { return e.id == out.CallID }); k >= 0 {
		r.abandoned = slices.Delete(slices.Clone(r.abandoned), k, k+1)
		return
	}
	f := r.top()
	if !f.st.Started || f.ending != nil || out.CallID != f.st.endID() {
		return
	}
	var e Ending
	_ = decode([]byte(c.Arguments), &e)
	e.id = out.CallID
	f.ending, f.end = &e, r.at[out.CallID]
	f.st.Ended = true
	// A step that failed with nobody to ask is over only by saying so.
	if e.Status == Failed && f.failed == nil {
		f.failed = fmt.Errorf("%w: %s", ErrStepFailed, e.Reason)
	}
	r.stack = r.stack[:len(r.stack)-1]
	p := f.parent
	if p == nil {
		return
	}
	// The caller learns what came of the call.
	if p.st.Callee == f.st.Instance {
		p.st.Returned = &e
		if e.Limit != "" && p.failed == nil {
			p.failed, p.st.limit = &limitError{limits[e.Limit], e.Reason}, e.Limit
		}
	}
}

// finish ends f's current node with step, at item i.
func (r *replay) finish(f *frame, i int, step Step) {
	st := &f.st
	n := st.Node
	step.Node, step.Visit = n.ID, st.Visit
	st.Steps = append(st.Steps, step)
	f.since, st.Nudges, st.Begun, st.Advice, st.Held, st.refused = i, 0, false, nil, nil, 0
	st.Callee, st.Returned, st.calleeInputs = "", nil, nil
	next := n.Next[step.Result.Outcome]
	if next == "" {
		st.Node, st.Visit, st.Attempt, st.Done = nil, 0, 0, true
		st.Outcome = n.ending(step.Result.Outcome)
		var err error
		if st.Outputs, err = st.resolveAll(st.g.outputs(st.Outcome)); err != nil {
			f.failed = fmt.Errorf("agentflow: outputs: %w", err)
		}
		if st.Outcome == Failed && f.failed == nil {
			f.failed = fmt.Errorf("%w: %s ended with %s", ErrStepFailed, n.ID, step.Result.Outcome)
		}
		return
	}
	g := st.g
	r.enter(f, i, g.node(next))
	if bound := g.visits(st.Node); st.Visit > bound {
		from := "its max_visits"
		if st.Node.MaxVisits == 0 {
			from = "the default for a step on a loop"
		}
		f.failed = fmt.Errorf("%w: step %q of workflow %q has run %d times, %s, and %q came back to it on %q; set max_visits for the step to change it",
			ErrVisits, next, g.Name, bound, from, n.ID, step.Result.Outcome)
	}
}

// enter makes n f's current node, at item i.
func (r *replay) enter(f *frame, i int, n *Node) {
	st := &f.st
	st.Node = n
	f.visits[n.ID]++
	st.Visit, st.Attempt = f.visits[n.ID], 1
	f.since = i
	r.entries++
	if n.Flow != "" && st.Depth+1 > st.maxDepth && f.failed == nil {
		f.failed = fmt.Errorf("%w: step %q of workflow %q would call %s at depth %d, and the run allows %d",
			ErrDepth, n.ID, st.Workflow, n.Flow, st.Depth+1, st.maxDepth)
		st.limit = "depth"
	}
}

// verdictOf reads the verdict a judge's look at n came to.
func verdictOf(n *Node, out *openresponses.FunctionCallOutput) (v Verdict, ok bool) {
	ok = decode([]byte(out.Output.String()), &v) == nil &&
		(v.Outcome == HandBack || n.Judge != nil && n.judged(v) == nil)
	return v, ok
}

// askedUnseen says the question was put with its judge held back.
func askedUnseen(c *openresponses.FunctionCall) bool {
	var q Question
	return c != nil && c.Name == askTool && decode([]byte(c.Arguments), &q) == nil && q.Mode == Shadow.String()
}

// visitTag tells one visit's messages from every other's, and names the
// run they belong to. They must not read alike: agentturn quarantines by
// content, so after a resume a message like one that was once left out
// is left out too.
func visitTag(st State) string { return fmt.Sprintf(" (run %s, visit %d)", st.Instance, st.Visit) }

// beginning starts the message that opens a visit of an Agent node.
func beginning(id string) string { return beginPrefix + strconv.Quote(id) + "." }

// opens says m is the message that opens the current visit of st's node,
// in st's run.
func opens(m *openresponses.Message, st *State) bool {
	n := st.Node
	return n.Kind == Agent && !n.fresh() && m.Role == openresponses.RoleUser && strings.HasPrefix(m.Text(), beginning(n.ID)) && st.mine(m.Text())
}

func parseResult(n *Node, output string) (Result, error) {
	var res Result
	if err := decode([]byte(output), &res); err != nil {
		return Result{}, err
	}
	return res, n.check(res)
}

func finishStep(args string) string {
	var a finishArgs
	_ = json.Unmarshal([]byte(args), &a)
	return a.Step
}

// runOf is the run a model's finish or verdict names.
func runOf(args string) string {
	var a struct {
		Run string `json:"run"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	return a.Run
}
