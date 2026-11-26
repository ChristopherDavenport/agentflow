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
	"time"

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
	clockTool  = "flow_clock"
	stuckTool  = "flow_stuck"

	// The outcomes of the question a failed step raises.
	retryOutcome = "retry"
	stopOutcome  = "stop"
	// The outcomes of the question a bound raises.
	grantOutcome = "grant"

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
	// ErrSteps is a tree of runs that has entered more steps than its
	// bound. It fails every run of the tree.
	ErrSteps = errors.New("agentflow: step limit reached")
	// ErrRepeat is a program step that gave the same outcome and data
	// [RepeatLimit] times in a row: a loop that is not getting anywhere.
	ErrRepeat = errors.New("agentflow: a step repeats itself")
	// ErrStepTimeout is a model step that ran past its timeout. The
	// program records it as the step's failure on its next turn.
	ErrStepTimeout = errors.New("agentflow: the step ran past its timeout")
)

// DefaultMaxSteps is how many steps a tree of runs may enter when its
// workflow sets no bound of its own.
const DefaultMaxSteps = 200

// RepeatLimit is how many times in a row a program step may give the
// same outcome and data before the loop it is on is stopped.
const RepeatLimit = 3

// Reached is a bound a run has come to, which a person may raise.
type Reached struct {
	// Limit names the bound: visits, depth, steps or repeat.
	Limit string
	// Reason says what reached it.
	Reason string
	// Step is the step that repeated, for repeat.
	Step string
}

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
	// Documents are, for a fresh model step, the texts its child left the
	// documents the step edits in, which the caller takes back.
	Documents map[string]string `json:"documents,omitempty"`
	// Entries and Granted are, for a call into a child, the steps its
	// runs entered and the more a person granted there, which count
	// toward the caller's tree. Limit names a bound across the tree its
	// run failed on, which fails the caller too.
	Entries int    `json:"entries,omitempty"`
	Granted int    `json:"granted,omitempty"`
	Limit   string `json:"limit,omitempty"`
	// Summary is, for a call into a child, what the child's end summed
	// up, which the caller's summary takes in.
	Summary *Summary `json:"summary,omitempty"`
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
	// this one belongs to, attempts and calls included, and Granted the
	// steps a person granted it past its bound.
	Entries, Granted int
	// Reached is a bound the run has come to at Node, which waits on a
	// person to grant more or to stop.
	Reached *Reached
	// Deadline is when the current attempt of a model step must be done
	// by, as the program's clock recorded it; zero for no timeout.
	Deadline time.Time
	// Documents are what the run's documents say, by name.
	Documents map[string]string
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

	// Aborting says the run has been abandoned and is ending, and Cleaning
	// that its cleanup path runs first. Cleanup says how that went, once
	// over: done, or why it stopped.
	Aborting, Cleaning bool
	Cleanup            string
	// Summary is what the run's end summed up, once it is written.
	Summary *Summary
	// started is when the run started, as its start's result says;
	// treeSteps counts the steps it and the runs it called entered; and
	// called are the runs it called, as each one's end summed it up.
	started   time.Time
	treeSteps int
	called    []Called
	// Note is what an operator said with the override that brought the
	// run to its step, or ran it again, until the step finishes.
	Note string
	// request is an operator's override for the program to apply, the ID
	// of its call, and why it cannot be, if it cannot.
	request    *Override
	requestID  string
	requestErr error
	// endNow says the run's end is due: it was abandoned, and has no
	// cleanup to run, or its cleanup is over.
	endNow bool
	// refused counts the verdicts a judge gave that it may not give.
	refused int
	// kept are the children the run keeps, by name, each with its spawn.
	kept map[string]string
	// callers are the calls the run is nested in, in the transcript,
	// outermost first.
	callers []caller
	// clocked says the program has read the clock for this attempt, and
	// asked counts the bounds asked about in it.
	clocked bool
	asked   int
	// filed says the program has given the attempt's model step its
	// files, and passedFiles are, for the step of a run in a child, the
	// files its call passed in.
	filed       bool
	passedFiles []fileRef
	// reasked counts the retry questions put again after an override
	// that was refused, and reask says a question waits to be put again
	// should the override waiting on it be refused.
	reasked int
	reask   bool

	g        *Graph
	prefix   string
	maxDepth int
	// maxSteps is the tree's bound on step entries.
	maxSteps int
	// calleeInputs is what Callee was started with.
	calleeInputs any
	// docAt says, for each document written, how many steps had finished
	// when it was, plus one: which is newer than which, for latest.
	docAt map[string]int
	// limit names the bound across the tree the run failed on, which no
	// caller may retry or route on.
	limit string
}

// caller is a call a run is nested in.
type caller struct {
	node     *Node
	workflow string
}

// limits are the bounds a run can reach, by the name an ending records.
var limits = map[string]error{"depth": ErrDepth, "steps": ErrSteps, "visits": ErrVisits, "repeat": ErrRepeat}

// treeWide says a bound is the tree's: a run that fails on it fails every
// run it is nested in, which neither retries it nor routes on it.
func treeWide(limit string) bool { return limit == "depth" || limit == "steps" }

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
	// MaxSteps bounds the steps the tree may enter: for a run nobody
	// called, the bound; for a child, what its caller had left.
	MaxSteps int `json:"max_steps,omitempty"`
	// Context is fresh for a run in a child session, whose caller's call
	// holds it. Path is then the way to the call from the run nobody
	// called, so the child reads on its own.
	Context string  `json:"context,omitempty"`
	Path    []Place `json:"path,omitempty"`
	// Files are, for a run in a child, the files its call passed in for
	// its step.
	Files []fileRef `json:"files,omitempty"`
	// Documents are, for a run in a child, the texts its call passed in of
	// the documents its step reads.
	Documents map[string]string `json:"documents,omitempty"`
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
func (st State) retryID() string {
	if st.reasked > 0 {
		return st.callID() + "-retry" + strconv.Itoa(st.reasked+1)
	}
	return st.callID() + "-retry"
}

func (st State) endID() string { return st.prefix + "end" }

// judgeID is the ID of the call that has a judge look at a question.
func (st State) judgeID() string { return st.callID() + "-judge" }

// clockID is the ID of the call that reads the clock for a model step's
// attempt, and timeoutID that of the one that says its time ran out.
func (st State) clockID() string   { return st.callID() + "-clock" }
func (st State) timeoutID() string { return st.callID() + "-timeout" }

// limitID is the ID of the question a bound raises.
func (st State) limitID() string { return st.callID() + "-limit" + strconv.Itoa(st.asked+1) }

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
	// extra are the visits a person granted each node past its bound, and
	// repeats the repeats; repeating names the step whose repeat waits to
	// be asked about.
	extra, repeats map[string]int
	repeating      string
	// came says how the current node was reached, for a bound to say.
	came string
	// aborting says the run was abandoned and is ending; cleaning that its
	// cleanup path has begun, and cleaned how it went once over; cut that
	// a second abandon cut it short.
	aborting, cleaning, cut bool
	cleaned                 string
}

// abortPlan is an abandon under way.
type abortPlan struct {
	// until is the outermost run it reaches, nil for every run under way,
	// and next the run asked for once they have ended, nil for none.
	until, next *frame
}

// canClean says f has a cleanup path to run as it is abandoned: one it
// has not begun, for a run that has neither failed nor finished.
func (f *frame) canClean() bool {
	return f.st.Started && f.st.g.OnAbort != "" && f.failed == nil && !f.st.Done && !f.cut && !f.cleaning
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
	// request is an operator's override waiting to be applied, and
	// overrides counts the flow_override calls made, for the next one's ID.
	request   *Override
	overrides int
	// abort is an abandon under way: the runs it reaches end, innermost
	// first, each after its cleanup, before the run asked for starts.
	abort *abortPlan
	// prefixes are those of every run started, of any workflow, so a
	// call none of them made is known to have lost its start.
	prefixes []string
	orphaned bool
	// maxSteps is the tree's bound on step entries, and granted what a
	// person added to it.
	maxSteps, granted int
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
		r.settle()
	}
	return r
}

func (r *replay) newRoot(inputs any) *frame {
	return newFrame(State{g: r.g, Workflow: r.g.Name, Inputs: inputs}, -1, nil)
}

func newFrame(st State, start int, parent *frame) *frame {
	return &frame{st: st, visits: map[string]int{}, since: -1, start: start, end: -1, parent: parent,
		extra: map[string]int{}, repeats: map[string]int{}}
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
	st.Documents = maps.Clone(f.st.Documents)
	st.called = slices.Clone(f.st.called)
	if f.ending != nil {
		st.Summary = f.ending.Summary
	}
	st.Aborting, st.Cleaning, st.Cleanup = f.aborting, f.cleaning && f.cleaned == "", f.cleaned
	st.endNow = f.aborting && f.ending == nil && f.st.Started && (f.cleaned != "" || !f.cleaning)
	if o := r.request; o != nil {
		own := *o
		st.request, st.requestID = &own, r.overrideID()
		_, st.requestErr = r.target(own)
	}
	st.Entries, st.Granted, st.maxSteps = r.entries, r.granted, r.maxSteps
	st.Path = slices.Clone(r.root.path)
	st.callers = nil
	for _, s := range r.stack {
		p := Place{Workflow: s.st.Workflow, Run: s.st.Instance}
		if s.st.Node != nil {
			p.Step = s.st.Node.ID
		}
		st.Path = append(st.Path, p)
		if s == f {
			break
		}
		if s.st.Node != nil {
			st.callers = append(st.callers, caller{s.st.Node, s.st.Workflow})
		}
	}
	return st
}

var tagPattern = regexp.MustCompile(`\(run ([A-Za-z0-9_-]+), visit (\d+)(?:, attempt \d+)?\)\s*$`)

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
			r.invoke(i, inputs, false)
			return
		}
		if o, ok := requested(m.Text()); ok {
			// An override waits for the program, which applies it or says
			// why not; with no run under way it asks nothing.
			if len(r.stack) > 0 {
				r.request = &o
			}
			return
		}
		if m.Text() == abandonText {
			r.invoke(i, nil, true)
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
// first. left says none is to start: the runs here were abandoned with
// the run that called them.
func (r *replay) invoke(i int, inputs any, left bool) {
	next := r.newRoot(inputs)
	next.st.Left = left
	r.abandon(i, nil, next)
}

// abandon abandons the runs under way, innermost first, out to until or,
// for nil, all of them, and then starts next, if any. Each run it reaches
// runs its cleanup path, if it has one, before its end. A second abandon
// over the first cuts any cleanup under way short and reaches the runs of
// both.
func (r *replay) abandon(i int, until, next *frame) {
	if old := r.abort; old != nil {
		for _, f := range r.stack {
			if f.cleaning && f.cleaned == "" {
				f.cleaned = cutShort
			}
			f.cut = f.cut || f.aborting
		}
		if old.until == nil {
			until = nil
		}
		if next == nil {
			next = old.next
		}
	}
	r.abort = &abortPlan{until: until, next: next}
	for _, f := range slices.Backward(r.stack) {
		f.aborting = true
		if f == until {
			break
		}
	}
	r.advance(i)
}

// advance moves an abandon on: the innermost run it reaches begins its
// cleanup, or waits for its end to be written. Once every run it reaches
// has ended, the run asked for is next.
func (r *replay) advance(i int) {
	p := r.abort
	if p == nil {
		return
	}
	if f := r.top(); len(r.stack) > 0 && f.aborting {
		if f.canClean() {
			r.clean(f, i)
		}
		return
	}
	r.abort = nil
	if p.next != nil {
		r.stack, r.entries, r.orphaned = nil, 0, false
		r.root = p.next
	}
}

// clean begins f's cleanup path, at item i. Whatever the run was waiting
// on is no longer its business.
func (r *replay) clean(f *frame, i int) {
	st := &f.st
	f.cleaning = true
	st.Failure, st.Reached, st.Callee, st.Returned, st.calleeInputs, st.Held, st.Advice = "", nil, "", nil, nil, nil, nil
	f.came = "the run was abandoned"
	r.enter(f, i, st.g.node(st.g.OnAbort))
}

// cutShort is how a cleanup a second abandon stopped went.
const cutShort = "cut short: abandoned again"

// settle ends a cleanup that failed: a cleanup step that fails is not
// asked about again, and the run's end is written.
func (r *replay) settle() {
	f := r.top()
	if !f.cleaning || f.cleaned != "" {
		return
	}
	switch {
	case f.st.Failure == abandonedText:
		// The host refused the question the cleanup waited on as it
		// abandoned the runs again: cut short, not failed.
		f.cleaned = cutShort
		f.st.Failure = ""
	case f.st.Failure != "":
		f.cleaned = fmt.Sprintf("failed at %s: %s", f.st.Node.ID, f.st.Failure)
		f.st.Failure = ""
	case f.failed != nil:
		f.cleaned = "failed: " + f.failed.Error()
		f.failed = nil
	}
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
		r.ended(i, c, out)
		return
	}
	if c != nil && c.Name == overrideTool && strings.HasPrefix(c.CallID, reserved) {
		r.overridden(i, c, out)
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
	case c.Name == finishTool || c.Name == judgeTool || c.Name == docReadTool || c.Name == docWriteTool:
		if runOf(c.Arguments) != st.Instance {
			return
		}
	}
	n := st.Node
	// A question the host set aside to ask for an override is not
	// answered: it is put again should the override be refused.
	if out.Output.String() == overriddenText && c.Name == askTool && strings.HasPrefix(c.CallID, st.prefix) &&
		(out.CallID == st.retryID() || out.CallID == st.limitID() || n.Kind == Person && out.CallID == st.callID()) {
		st.reask = true
		return
	}
	res, invalid := parseResult(n, out.Output.String())
	if invalid == nil && n.Kind == Person && c.Name == askTool && out.CallID == st.callID() {
		// An answer is held to what its question offered as it was asked,
		// which the call records.
		var q Question
		if decode([]byte(c.Arguments), &q) == nil {
			invalid = q.holds(res)
		}
	}
	switch {
	case st.Reached != nil:
		if out.CallID != st.limitID() {
			return
		}
		r.grant(f, out.Output.String())
		return
	case st.Failure != "":
		if out.CallID != st.retryID() {
			return
		}
		var answer Result
		_ = json.Unmarshal([]byte(out.Output.String()), &answer)
		if answer.Outcome == retryOutcome {
			st.Attempt, st.Failure = st.Attempt+1, ""
			st.Callee, st.Returned, st.calleeInputs = "", nil, nil
			st.clocked, st.Deadline, st.asked, st.reasked, st.reask, st.filed = false, time.Time{}, 0, 0, false, false
			if n.Kind == Agent {
				// The step opens again, and what came before is not about
				// this attempt.
				f.since, st.Nudges, st.Begun, st.refused = i, 0, false, 0
			}
			r.entries++
			st.treeSteps++
			r.bounds(f)
		} else {
			f.failed = fmt.Errorf("%w: %s: %s", ErrStepFailed, n.ID, st.Failure)
		}
		return
	case c.Name == filesTool && strings.HasPrefix(c.CallID, reserved):
		// The step's files were given, or could not be, which fails it.
		if out.CallID == st.filesID() {
			if r, err := parseResult(&Node{ID: "files"}, out.Output.String()); err == nil && r.Outcome == OK {
				st.filed = true
			} else {
				st.Failure = out.Output.String()
			}
		}
		return
	case c.Name == clockTool && strings.HasPrefix(c.CallID, reserved):
		switch out.CallID {
		case st.clockID():
			st.clocked = true
			var read struct {
				Data struct {
					Deadline time.Time `json:"deadline"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(out.Output.String()), &read) == nil {
				st.Deadline = read.Data.Deadline
			}
		case st.timeoutID():
			// Its output is the failure, whatever it says.
			st.Failure = out.Output.String()
		}
		return
	case c.Name == judgeTool && strings.HasPrefix(c.CallID, reserved):
		// A judge's look, from its child: under shadow it finishes the
		// held answer's step, and otherwise it advises the question.
		if out.CallID != st.judgeID() || n.Kind != Person {
			return
		}
		// The judge's look is steps of this tree.
		entries, granted := spent(out.Output.String())
		r.entries += entries
		r.granted += granted
		st.treeSteps += entries
		st.called = append(st.called, calledFresh(argsOf(c), out.Output.String()))
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
	case c.Name == docReadTool || c.Name == docWriteTool:
		// A write the step may make sets the document's text, at this
		// point of the run; a refused one, or a read, changes nothing.
		if c.Name == docWriteTool && n.Kind == Agent && finishStep(c.Arguments) == n.ID && r.at[out.CallID] >= f.since {
			if name, text, ok := docResult(out.Output.String()); ok && n.edits(name) {
				st.write(name, text)
			}
		}
		return
	case n.Kind == Agent && !n.fresh():
		// A finish that was rejected or is for another node is not
		// progress; the model saw the error and goes on. A judge that
		// gives two verdicts it may not give has given none.
		finish := finishTool
		if n.Verdict {
			finish = judgeTool
		}
		if c.Name != finish && (c.Name != stuckTool || !n.canStick()) || finishStep(c.Arguments) != n.ID || r.at[out.CallID] < f.since {
			return
		}
		if c.Name == stuckTool {
			var said Result
			if decode([]byte(out.Output.String()), &said) != nil || said.Outcome != Stuck {
				// A refused report is not progress; the model saw why.
				return
			}
			if !n.routesStuck() {
				st.Failure = "the model reported it is stuck: " + said.Reason
				return
			}
			res, invalid = said, nil
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
		if n.fresh() {
			// What the child's runs spent counts toward this tree, however
			// it came out, and a bound across the tree it failed on fails
			// this run.
			entries, granted := spent(out.Output.String())
			r.entries += entries
			r.granted += granted
			st.treeSteps += entries
			st.called = append(st.called, calledFresh(argsOf(c), out.Output.String()))
			var said Result
			if decode([]byte(out.Output.String()), &said) == nil && said.Limit != "" {
				f.failed, st.limit = &limitError{limits[said.Limit], said.Reason}, said.Limit
				return
			}
			var failed callFailure
			if decode([]byte(out.Output.String()), &failed) == nil && failed.Failure != "" {
				st.Failure = failed.Failure
				return
			}
		}
		if invalid != nil {
			st.Failure = out.Output.String()
			if json.Valid([]byte(st.Failure)) {
				st.Failure = invalid.Error() + ": " + st.Failure
			}
			return
		}
		if err := n.written(st, res); err != nil {
			st.Failure = err.Error()
			return
		}
		if n.Kind == Agent && n.fresh() {
			// What the child left the documents the step edits saying is
			// theirs now.
			for _, name := range slices.Sorted(maps.Keys(res.Documents)) {
				if n.edits(name) && len(res.Documents[name]) <= MaxDocument {
					st.write(name, res.Documents[name])
				}
			}
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
		root.st.Documents, root.st.docAt = initialDocuments(r.g, p.Inputs, p.Documents)
		root.st.started = startedAt(out.Output.String())
		root.st.passedFiles = p.Files
		root.start = r.at[c.CallID]
		// A child is given what its caller's tree had left, none included.
		r.maxSteps = p.MaxSteps
		if p.Context != ContextFresh {
			r.maxSteps = cmpSteps(p.MaxSteps)
		}
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
	child := newFrame(State{Started: true, Instance: p.Run, Workflow: g.Name, Parent: p.Parent, Caller: p.Caller, Depth: p.Depth,
		Inputs: p.Inputs, g: g, prefix: prefix, maxDepth: st.maxDepth}, r.at[c.CallID], f)
	child.st.Documents, child.st.docAt = initialDocuments(g, p.Inputs, nil)
	child.st.started = startedAt(out.Output.String())
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

func cmpSteps(n int) int {
	if n <= 0 {
		return DefaultMaxSteps
	}
	return n
}

func cmpDepth(d int) int {
	if d <= 0 {
		return DefaultMaxDepth
	}
	return d
}

// ended reads a flow_end. An ending counts however the call went: it is
// what was found, and asking again would not make it more so.
func (r *replay) ended(i int, c *openresponses.FunctionCall, out *openresponses.FunctionCallOutput) {
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
	switch {
	case e.Status == Failed && f.failed == nil && limits[e.Limit] != nil:
		f.failed, f.st.limit = &limitError{limits[e.Limit], e.Reason}, e.Limit
	case e.Status == Failed && f.failed == nil:
		f.failed = fmt.Errorf("%w: %s", ErrStepFailed, e.Reason)
	}
	r.stack = r.stack[:len(r.stack)-1]
	// The caller learns what came of the call.
	if p := f.parent; p != nil && p.st.Callee == f.st.Instance {
		p.st.Returned = &e
		p.st.treeSteps += f.st.treeSteps
		p.st.called = append(p.st.called, calledOf(f.st.Workflow, f.st.Instance, e.Status, e.Outcome, e.Summary))
		if treeWide(e.Limit) && p.failed == nil {
			p.failed, p.st.limit = &limitError{limits[e.Limit], e.Reason}, e.Limit
		}
	}
	r.advance(i)
}

// finish ends f's current node with step, at item i.
func (r *replay) finish(f *frame, i int, step Step) {
	st := &f.st
	n := st.Node
	step.Node, step.Visit = n.ID, st.Visit
	st.Steps = append(st.Steps, step)
	f.since, st.Nudges, st.Begun, st.Advice, st.Held, st.refused = i, 0, false, nil, nil, 0
	st.Callee, st.Returned, st.calleeInputs, st.Note = "", nil, nil, ""
	next := n.Next[step.Result.Outcome]
	if next == "" && f.cleaning {
		// The cleanup is over; the run's end is written next.
		st.Node, f.cleaned = nil, "done"
		return
	}
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
	// A program step that keeps giving the same result is on a loop that
	// is not getting anywhere, which is asked about before going on. A
	// result with no data says nothing to compare, a poll's "not yet" say,
	// so only the visit bound holds such a loop.
	if n.Kind == Run && n.Flow == "" && step.Result.Data != nil && repeated(st.Steps, n.ID) >= RepeatLimit+f.repeats[n.ID] {
		f.repeating = n.ID
	}
	f.came = fmt.Sprintf("%q came back to it on %q", n.ID, step.Result.Outcome)
	r.enter(f, i, st.g.node(next))
}

// repeated is how many times in a row, among steps, the step id last
// gave the same outcome and data.
func repeated(steps []Step, id string) int {
	count := 0
	var last *Result
	for k := len(steps) - 1; k >= 0; k-- {
		s := steps[k]
		if s.Node != id {
			continue
		}
		if last != nil && (s.Result.Outcome != last.Outcome || !sameJSON(s.Result.Data, last.Data)) {
			break
		}
		last = &steps[k].Result
		count++
	}
	return count
}

// bounds raises the first bound f's current step has come to, unless
// one is already raised: the step's visits, the depth of a call, the
// tree's step entries, and a step that repeats itself.
func (r *replay) bounds(f *frame) {
	st := &f.st
	n := st.Node
	// A cleanup path has no loop, so it is bounded by its length, and no
	// bound stops it.
	if n == nil || st.Reached != nil || f.failed != nil || st.Done || f.cleaning {
		return
	}
	g := st.g
	switch bound := g.visits(n) + f.extra[n.ID]; {
	case st.Visit > bound:
		from := "its max_visits"
		if n.MaxVisits == 0 {
			from = "the default for a step on a loop"
		}
		if f.extra[n.ID] > 0 {
			from += fmt.Sprintf(" and %d granted", f.extra[n.ID])
		}
		st.Reached = &Reached{Limit: "visits", Reason: fmt.Sprintf("%v: step %q of workflow %q has run %d times, %s, and %s; set max_visits for the step to change it",
			ErrVisits, n.ID, g.Name, bound, from, f.came)}
	case n.Flow != "" && st.Depth+1 > st.maxDepth:
		st.Reached = &Reached{Limit: "depth", Reason: fmt.Sprintf("%v: step %q of workflow %q would call %s at depth %d, and the run allows %d",
			ErrDepth, n.ID, st.Workflow, n.Flow, st.Depth+1, st.maxDepth)}
	case r.entries > r.maxSteps+r.granted:
		st.Reached = &Reached{Limit: "steps", Reason: fmt.Sprintf("%v: the tree of runs has entered %d steps, at step %q of workflow %q, and it allows %d; set max_steps for the workflow to change it",
			ErrSteps, r.entries, n.ID, g.Name, r.maxSteps+r.granted)}
	case f.repeating != "":
		st.Reached = &Reached{Limit: "repeat", Step: f.repeating, Reason: fmt.Sprintf("%v: step %q of workflow %q gave the same outcome and data %d times in a row",
			ErrRepeat, f.repeating, g.Name, repeated(st.Steps, f.repeating))}
	}
}

// grant reads a person's answer to the bound f's run has reached: more
// of it, or a stop, which fails the run on it.
func (r *replay) grant(f *frame, output string) {
	st := &f.st
	reached := st.Reached
	st.asked++
	var answer Result
	amount := 0
	if decode([]byte(output), &answer) == nil && answer.Outcome == grantOutcome {
		amount = grantOf(answer.Data)
	}
	if amount <= 0 {
		f.failed, st.limit = &limitError{limits[reached.Limit], reached.Reason}, reached.Limit
		return
	}
	st.Reached = nil
	switch reached.Limit {
	case "visits":
		f.extra[st.Node.ID] += amount
	case "depth":
		st.maxDepth += amount
	case "steps":
		r.granted += amount
	case "repeat":
		f.repeats[reached.Step] += amount
		f.repeating = ""
	}
	r.bounds(f)
}

// grantOf is the amount a grant's data carries.
func grantOf(data any) int {
	m, _ := data.(map[string]any)
	switch v := m["amount"].(type) {
	case json.Number:
		n, err := v.Int64()
		if err == nil && n > 0 && n <= maxGrant {
			return int(n)
		}
	case float64:
		if v >= 1 && v <= maxGrant && v == float64(int(v)) {
			return int(v)
		}
	}
	return 0
}

// maxGrant bounds what one answer may grant.
const maxGrant = 999

// enter makes n f's current node, at item i.
func (r *replay) enter(f *frame, i int, n *Node) {
	st := &f.st
	st.Node = n
	f.visits[n.ID]++
	st.Visit, st.Attempt = f.visits[n.ID], 1
	f.since = i
	st.clocked, st.Deadline, st.asked, st.reasked, st.reask, st.filed = false, time.Time{}, 0, 0, false, false
	r.entries++
	st.treeSteps++
	r.bounds(f)
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
func visitTag(st State) string {
	if st.Attempt > 1 {
		return fmt.Sprintf(" (run %s, visit %d, attempt %d)", st.Instance, st.Visit, st.Attempt)
	}
	return fmt.Sprintf(" (run %s, visit %d)", st.Instance, st.Visit)
}

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
