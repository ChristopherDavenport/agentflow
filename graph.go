// Package agentflow drives an agentturn loop through a workflow graph:
// each turn is taken by the program, the model or a person, and where
// the workflow stands is read from the transcript alone.
//
// This is a spike for docs/rfcs/0001-agentflow.md. The API will change.
package agentflow

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agenttool"
)

// A node's ID is part of call IDs, which providers hold to 64 characters
// of this set. A workflow's name is not, so it may name a file within a
// skill: <skill>/<path>.
var (
	graphName = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	// A workflow made from one step, to run it in a child, is named
	// <workflow>:<step>, and one made to judge a step <workflow>:<step>:judge.
	derivedName = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*:[A-Za-z0-9_-]{1,20}(:judge)?$`)
	childName   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
	skillName   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	flowName    = regexp.MustCompile(`^([A-Za-z0-9_-]+|\./[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*\.md)$`)
	nodeID      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,20}$`)
)

// maxName bounds a workflow's name.
const maxName = 128

// Kind says who takes a node's turn.
type Kind int

const (
	// Run is a tool call the program makes.
	Run Kind = iota + 1
	// Agent is work the model does until it calls flow_finish.
	Agent
	// Person is a question only a person, or the host for them, answers.
	Person
)

// Where a model step or a call does its work.
const (
	// ContextSession is the current context, and what empty means: a
	// model step reads the whole transcript, and a call's steps write to
	// it.
	ContextSession = "session"
	// ContextFresh is a child session, which sees only what the call
	// passes in, its own instructions and its own tools.
	ContextFresh = "fresh"
)

// OK is the outcome of a node that declares none.
const OK = "ok"

// Stuck is the outcome a model step takes when its model reports, with
// flow_stuck, that it cannot do the step. A step that draws an edge or
// an end for it goes on there; one that does not has failed.
const Stuck = "stuck"

// stuckSchema is the data of [Stuck]: the model's reason.
var stuckSchema = &agenttool.Schema{Type: "object", Properties: []agenttool.Property{{Name: "reason", Schema: &agenttool.Schema{Type: "string"}}}, Required: []string{"reason"}}

// Thinking levels a step may ask for, as openresponses names them; max
// is its extension above xhigh.
var thinkingLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// MaxTimeout bounds a step's timeout.
const MaxTimeout = 24 * time.Hour

// DefaultLoopVisits is how often a node on a loop runs when it sets no
// bound of its own.
const DefaultLoopVisits = 5

// onLoop reports whether some path leads from the node back to it.
func (g *Graph) onLoop(id string) bool {
	seen := map[string]bool{}
	queue := []string{id}
	for len(queue) > 0 {
		n := g.node(queue[0])
		queue = queue[1:]
		if n == nil {
			continue
		}
		for _, next := range n.Next {
			if next == id {
				return true
			}
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// visits is the node's bound.
func (g *Graph) visits(n *Node) int {
	switch {
	case n.MaxVisits > 0:
		return n.MaxVisits
	case g.onLoop(n.ID):
		return DefaultLoopVisits
	}
	return 1
}

// ScriptTool is the tool a script step calls; package script makes one.
// Only the program calls it.
const ScriptTool = "run_script"

// ScriptCall is [ScriptTool]'s arguments.
type ScriptCall struct {
	// Skill names the skill whose files the script is in, when it is not
	// that of the workflow the run started from.
	Skill string         `json:"skill,omitempty"`
	Run   string         `json:"run"`
	With  map[string]any `json:"with,omitempty"`
	// Outcomes is the node's declared ones, none when it only succeeds.
	Outcomes []string `json:"outcomes,omitempty"`
	// Dir is the directory below the workspace's root the script runs
	// in, resolved from the record as the call was made; empty for the
	// root.
	Dir string `json:"dir,omitempty"`
}

// Node is one step.
type Node struct {
	ID   string
	Kind Kind
	// Description says what the step is for, to people and the fronts
	// that show them a run. No model is sent it: it is in the definition,
	// which only the program reads, and nowhere else.
	Description string `json:",omitempty"`

	// A Run node runs Script, or calls Tool with Args, or calls the
	// workflow Flow names. A nil Args is {}.
	Script string
	Tool   string
	Args   json.RawMessage
	// Flow is a workflow's name, or ./<path>.md for a workflow file of
	// the caller's own skill. Its outcomes, if the node declares none,
	// are the callee's and [Failed]; its data is the callee's outputs.
	Flow string `json:",omitempty"`

	// With is the node's inputs, resolved from what is recorded. A
	// script reads them on standard input, a tool takes them as
	// arguments over Args, and a model or a person is shown them.
	With map[string]Value
	// Data is the schema of the node's data, by outcome. For a Person
	// node it is the form.
	Data map[string]*agenttool.Schema

	// Prompt is an Agent node's instructions or a Person node's question.
	Prompt string
	// Tools names the tools an Agent node may call. AllTools, written
	// tools: all, offers every tool the host does instead, and
	// ExcludeTools takes tools out of what the step is offered, with the
	// workflow's own [Graph.ExcludeTools].
	Tools        []string
	AllTools     bool     `json:",omitempty"`
	ExcludeTools []string `json:",omitempty"`
	// Context is [ContextSession], [ContextFresh] or empty, on an Agent
	// node or a call.
	Context string

	// A step in a child may set the child's own settings: Dir, its
	// working directory below the workspace's root; Model, its model's
	// name; and Instructions, text added to its instructions, read from
	// the skill's file InstructionsFile. Child names a child kept across
	// the visits and steps of a run that name it, on a model step.
	//
	// Dir is a literal path or a reference to one, resolved from the
	// record when the step's call is made and recorded in its arguments.
	// A script step takes it too, and runs there.
	Dir              Value  `json:",omitzero"`
	Instructions     string `json:",omitempty"`
	InstructionsFile string `json:",omitempty"`
	Child            string `json:",omitempty"`
	// Model names the model a model step asks, and Thinking how hard it
	// reasons: one of none, minimal, low, medium, high, xhigh and max.
	// Empty is the workflow's, and then the host's. On a call into a
	// child, Model is the child's.
	Model    string `json:",omitempty"`
	Thinking string `json:",omitempty"`
	// Timeout bounds a script, tool or model step. A step that runs past
	// it fails, with the error recorded, and asks to be retried.
	Timeout time.Duration `json:",omitempty"`
	// Policy limits what a model step's tools may touch. On a call, it
	// limits every model step and judge of the runs it calls.
	Policy *Policy `json:",omitempty"`
	// Skills are skills a model step names, whose instructions are added
	// to the step's. Loading reads each one's text into the definition,
	// so its digest pins what the model is told.
	Skills []StepSkill `json:",omitempty"`
	// Files are what a model step is given as context when it opens, in
	// order: workspace paths, literal or referred to, and ./<path> files of
	// the skill, whose texts loading reads into SkillFiles, by path within
	// the skill, so the digest pins them.
	Files      []Value           `json:",omitempty"`
	SkillFiles map[string]string `json:",omitempty"`
	// Reads and Edits name the documents a model step may read, and read
	// and edit. Writes, on a script or tool step, sets documents from
	// members of its result's data, by document.
	Reads  []string          `json:",omitempty"`
	Edits  []string          `json:",omitempty"`
	Writes map[string]string `json:",omitempty"`
	// Show, on a script or tool step, is what a working model sees of its
	// result; nil for all of it. See [Show].
	Show *Show `json:",omitempty"`
	// Verdict says the node is a judge's: it ends with flow_judge, giving
	// a reason. Only a workflow agentflow makes to judge a step has one.
	Verdict bool `json:",omitempty"`

	// Outcomes are the ways the node can end. Empty means only [OK].
	Outcomes []string
	// Next maps an outcome to the node that follows. An outcome with no
	// entry ends the workflow.
	Next map[string]string
	// Ends names the end an outcome with no Next reaches, where it is
	// not the outcome's own name. The workflow ends with that outcome.
	Ends map[string]string `json:",omitempty"`

	// Judge, on a Person node, says a model may answer it.
	Judge *Judge `json:",omitempty"`

	// Unattended is the outcome a Person node takes when nobody is
	// there to ask. Empty means the run waits.
	Unattended string
	// UnattendedData is the data that goes with it.
	UnattendedData any

	// A Person node may offer Choices computed from an earlier result,
	// take an answer in a person's own words as Text, and, with a
	// Timeout, take OnTimeout and its data when nobody answers in time.
	Choices       *Choices  `json:",omitempty"`
	Text          *FreeText `json:",omitempty"`
	OnTimeout     string    `json:",omitempty"`
	OnTimeoutData any       `json:",omitempty"`
	// MaxVisits bounds how often the node runs. 0 means
	// [DefaultLoopVisits] for a node a loop can come back to, and once
	// for any other.
	MaxVisits int
	// PersonPaced, written max_visits: person, lets a person drive the
	// node's loop. A visit is paced when a person, not rules, a judge, a
	// default or a timeout, answered a person step of the run since the
	// node's last visit. Paced visits are held to no bound and count for
	// none of the tree's steps. Every other visit is held to
	// [DefaultLoopVisits]. Every cycle through the node must pass a
	// person step, and MaxVisits stays 0.
	PersonPaced bool `json:",omitempty"`

	// call is what linking learnt of the callee of a Flow node.
	call *callee
	// otherModel says a model step names a model its workflow does not
	// default to, which [Graph.Validate] works out: with no Context, the
	// step runs in a child.
	otherModel bool
	// pick is the schema of one of a Person node's choices, as the list
	// they come from declares it, nil when unknown.
	pick *agenttool.Schema
}

// StepSkill is a skill a model step names, with the instructions loading
// read from it. A skill named here grants nothing: its allowed-tools are
// not the step's, which a workflow cannot widen.
type StepSkill struct {
	Name         string
	Instructions string `json:",omitempty"`
}

// callee is a Flow node's contract, as [Graph.Link] reads it from the
// workflow it calls.
type callee struct {
	g        *Graph
	outcomes []string
	data     map[string]*agenttool.Schema
}

func (n *Node) outcomes() []string {
	var out []string
	switch {
	case len(n.Outcomes) > 0:
		out = n.Outcomes
	case n.Flow != "" && n.call != nil:
		return n.call.outcomes
	default:
		out = []string{OK}
	}
	if n.routesStuck() {
		out = append(slices.Clone(out), Stuck)
	}
	return out
}

// finishable are the outcomes a model finishes its step with: all but
// [Stuck], which it reports with flow_stuck.
func (n *Node) finishable() []string {
	return slices.DeleteFunc(slices.Clone(n.outcomes()), func(o string) bool { return o == Stuck })
}

// canStick says the node's model may report it is stuck: a model step's,
// and not a judge's, which hands back instead.
func (n *Node) canStick() bool { return n.Kind == Agent && !n.Verdict }

// routesStuck says a model step goes on, somewhere it names, when its
// model is stuck.
func (n *Node) routesStuck() bool {
	return n.canStick() && (n.Next[Stuck] != "" || n.Ends[Stuck] != "")
}

// schemas is the node's data by outcome: declared, or for a call, the
// callee's outputs.
func (n *Node) schemas() map[string]*agenttool.Schema {
	if n.Flow != "" && n.call != nil {
		return n.call.data
	}
	if n.routesStuck() {
		out := maps.Clone(n.Data)
		if out == nil {
			out = map[string]*agenttool.Schema{}
		}
		out[Stuck] = stuckSchema
		return out
	}
	return n.asked(n.Data)
}

// ending is the workflow outcome a node's outcome ends with, when the
// outcome has nowhere to go.
func (n *Node) ending(outcome string) string {
	if end := n.Ends[outcome]; end != "" {
		return end
	}
	return outcome
}

// Graph is a workflow. The first node is where it starts.
type Graph struct {
	Name string
	// Description says what the workflow is for, as a step's does.
	Description string `json:",omitempty"`
	Nodes       []Node
	// Inputs is the schema of what an invocation carries.
	Inputs *agenttool.Schema
	// InputDefaults are the values of inputs an invocation or a caller may
	// leave out, by name. A run's start records its inputs with them
	// filled in, so the record holds what the run was given. An input with
	// a default is not required.
	InputDefaults map[string]any `json:",omitempty"`
	// Outputs is what the finished workflow gives back, whatever its
	// outcome.
	Outputs map[string]Value
	// OutputsBy is what it gives back by outcome, in place of Outputs.
	// An outcome it does not name gives nothing.
	OutputsBy map[string]map[string]Value `json:",omitempty"`
	// MaxDepth bounds how deeply a run started from this workflow may
	// nest calls. 0 means [DefaultMaxDepth]; a host's [MaxDepth] may
	// lower it.
	MaxDepth int `json:",omitempty"`
	// MaxSteps bounds the steps a run started from this workflow may
	// enter, across every run it calls. 0 means [DefaultMaxSteps]; a
	// host's [MaxSteps] may lower it.
	MaxSteps int `json:",omitempty"`
	// OnAbort names the step a cleanup path starts at, which runs when a
	// run is abandoned, before its end is written. Its steps are script,
	// tool and person steps that no other path reaches, and it has no loop.
	OnAbort string `json:",omitempty"`
	// Documents are the workflow's named markdown documents, by name.
	Documents map[string]Document `json:",omitempty"`
	// Context, set to [ContextFresh], says every call to the workflow runs
	// it in a child of its own, whatever the call says; a call that says
	// context session is a load error. Empty leaves it to each call.
	Context string `json:",omitempty"`
	// Hidden says the workflow is run only by other workflows that call
	// it: neither an invocation nor a model's request for its skill starts
	// it, and a catalog should leave it out.
	Hidden bool `json:",omitempty"`
	// ExcludeTools are tools no model step of the workflow is offered,
	// whatever it names, and no judge of it may call.
	ExcludeTools []string `json:",omitempty"`
	// Model and Thinking are what the workflow's model steps ask for when
	// they do not say.
	Model    string `json:",omitempty"`
	Thinking string `json:",omitempty"`
	// Sources is the digest of the files the graph was loaded from,
	// scripts among them, so a change to any is a change to the graph.
	Sources string

	// Callees are the workflows the graph's Flow nodes call, by the name
	// each node writes; [Graph.Link] fills them in.
	Callees map[string]*Graph `json:"-"`
}

// outputs is what the workflow gives back when it ends with outcome.
func (g *Graph) outputs(outcome string) map[string]Value {
	if g.OutputsBy != nil {
		return g.OutputsBy[outcome]
	}
	return g.Outputs
}

// Outcomes are the ways the workflow can end: the ends its steps reach.
func (g *Graph) Outcomes() []string {
	var out []string
	// A cleanup path's ends end no workflow: the run is abandoned.
	for _, n := range g.mainNodes() {
		for _, outcome := range n.outcomes() {
			if n.Next[outcome] != "" {
				continue
			}
			if end := n.ending(outcome); !slices.Contains(out, end) {
				out = append(out, end)
			}
		}
	}
	slices.Sort(out)
	return out
}

// Describe is the description of step id, or of the workflow for "".
// It is for people: no model is sent it.
func (g *Graph) Describe(id string) string {
	if id == "" {
		return g.Description
	}
	if n := g.node(id); n != nil {
		return n.Description
	}
	return ""
}

func (g *Graph) node(id string) *Node {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

// Validate reports every problem with the graph.
func (g *Graph) Validate() error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("agentflow: "+format, args...)) }
	if !graphName.MatchString(g.Name) && !derivedName.MatchString(g.Name) || len(g.Name) > maxName {
		bad("invalid graph name %q: want up to %d letters, digits, _ or -, in parts joined by /", g.Name, maxName)
	}
	if g.MaxDepth < 0 || g.MaxDepth > 99 {
		bad("max depth must be 0 to 99")
	}
	if g.MaxSteps < 0 || g.MaxSteps > 9999 {
		bad("max steps must be 0 to 9999")
	}
	if g.Thinking != "" && !slices.Contains(thinkingLevels, g.Thinking) {
		bad("thinking %q: want one of %s", g.Thinking, strings.Join(thinkingLevels, ", "))
	}
	if g.Outputs != nil && g.OutputsBy != nil {
		bad("outputs are either by outcome or for every outcome")
	}
	errs = append(errs, g.checkDefaults()...)
	if g.Context != "" && g.Context != ContextFresh {
		bad("context %q: a workflow may say only %q, that every call runs it in a child; leave it out to let each call say%s", g.Context, ContextFresh, suggest(g.Context, []string{ContextFresh}))
	}
	for _, tool := range g.ExcludeTools {
		if tool == ScriptTool || strings.HasPrefix(tool, reserved) {
			bad("exclude_tools: %s is the program's, which no model step is offered", tool)
		}
	}
	if len(g.Nodes) == 0 {
		bad("graph has no nodes")
	}
	// What a pick is, as the list it comes from declares it.
	for i := range g.Nodes {
		if n := &g.Nodes[i]; n.Choices != nil {
			n.pick = nil
			if s := g.valueSchema(n.Choices.From); s != nil && s.Type == "array" {
				n.pick = s.Items
			}
		}
	}
	seen := map[string]bool{}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if !nodeID.MatchString(n.ID) {
			bad("node %d: invalid id %q: want 1 to 20 letters, digits, _ or -", i, n.ID)
		}
		if seen[n.ID] {
			bad("node %q: declared twice", n.ID)
		}
		seen[n.ID] = true
		switch n.Kind {
		case Run:
			switch {
			case n.Flow != "" && (n.Script != "" || n.Tool != "" || n.Args != nil):
				bad("node %q: a node that calls a workflow has no run, tool or args", n.ID)
			case n.Flow != "" && !flowName.MatchString(n.Flow):
				bad("node %q: flow %q: want a workflow's name or ./<path>.md", n.ID, n.Flow)
			case n.Flow != "" && n.Data != nil:
				bad("node %q: a call's data is what the workflow it calls gives back, so it declares no outputs", n.ID)
			case n.Flow != "":
			case n.Script != "" && (n.Tool != "" || n.Args != nil):
				bad("node %q: a script node has no tool or args", n.ID)
			case n.Script == "" && (n.Tool == "" || n.Tool == ScriptTool || strings.HasPrefix(n.Tool, reserved)):
				bad("node %q: a run node needs a script or a tool of its own", n.ID)
			}
			var object map[string]any
			if n.Args != nil && n.With != nil && json.Unmarshal(n.Args, &object) != nil {
				bad("node %q: with needs args to be an object", n.ID)
			}
			if n.Args != nil && !json.Valid(n.Args) {
				bad("node %q: args are not JSON", n.ID)
			}
		case Agent, Person:
			if n.Prompt == "" {
				bad("node %q: no prompt", n.ID)
			}
			if slices.Contains(n.Tools, ScriptTool) {
				bad("node %q: only the program runs scripts", n.ID)
			}
		default:
			bad("node %q: unknown kind %d", n.ID, n.Kind)
		}
		switch {
		case n.Context != "" && n.Kind != Agent && n.Flow == "":
			bad("node %q: only a model node or a call has a context", n.ID)
		case n.Context != "" && n.Context != ContextSession && n.Context != ContextFresh:
			bad("node %q: context %q: want %q or %q", n.ID, n.Context, ContextSession, ContextFresh)
		}
		// A model step that asks for another model than its workflow's
		// reads nothing the other model wrote unless it says session: the
		// definition alone decides, so position does not turn on the host.
		n.otherModel = n.Kind == Agent && n.Model != "" && n.Model != g.Model
		errs = append(errs, n.checkChild()...)
		errs = append(errs, n.checkSettings()...)
		errs = append(errs, n.checkFiles()...)
		errs = append(errs, n.checkShow()...)
		errs = append(errs, g.checkOffer(n)...)
		errs = append(errs, g.checkDir(n)...)
		for _, v := range n.Files {
			if s := g.refSchema(v.From); v.From != "" && s != nil && s.Type != "" && s.Type != "string" && (s.Type != "array" || s.Items != nil && s.Items.Type != "" && s.Items.Type != "string") {
				bad("node %q: file %s is not a path, nor a list of paths", n.ID, v.From)
			}
		}
		if j := n.Judge; j != nil {
			switch {
			case n.Kind != Person:
				bad("node %q: only a person node can be judged", n.ID)
			case strings.TrimSpace(j.Criteria) == "":
				bad("node %q: a judge needs criteria", n.ID)
			case len(j.Outcomes) == 0:
				bad("node %q: a judge needs the outcomes it may choose", n.ID)
			case j.Returns < 0 || j.Returns > 99:
				bad("node %q: a judge's returns must be 0 to 99", n.ID)
			case slices.Contains(n.outcomes(), HandBack):
				bad("node %q: a judged node cannot declare the outcome %q", n.ID, HandBack)
			}
			for _, outcome := range j.Outcomes {
				if !slices.Contains(n.outcomes(), outcome) {
					bad("node %q: judge: undeclared outcome %q%s", n.ID, outcome, suggest(outcome, n.outcomes()))
				}
			}
			for _, tool := range j.Tools {
				if tool == ScriptTool || strings.HasPrefix(tool, reserved) {
					bad("node %q: a judge cannot call %q", n.ID, tool)
				}
			}
		}
		// Until it is linked, a call that declares no outcomes has none
		// known to check its edges against.
		unknown := n.Flow != "" && n.call == nil && len(n.Outcomes) == 0
		for outcome, next := range n.Next {
			if !unknown && !slices.Contains(n.outcomes(), outcome) {
				bad("node %q: edge for undeclared outcome %q%s", n.ID, outcome, suggest(outcome, n.outcomes()))
			}
			if g.node(next) == nil {
				bad("node %q: edge to unknown node %q%s", n.ID, next, suggest(next, g.nodeIDs()))
			}
		}
		for outcome, end := range n.Ends {
			switch {
			case !unknown && !slices.Contains(n.outcomes(), outcome):
				bad("node %q: end for undeclared outcome %q%s", n.ID, outcome, suggest(outcome, n.outcomes()))
			case n.Next[outcome] != "":
				bad("node %q: outcome %q both goes on and ends", n.ID, outcome)
			case !nodeID.MatchString(end) || g.node(end) != nil:
				bad("node %q: end %q: want a name no node has", n.ID, end)
			}
		}
		if n.Unattended != "" && (n.Kind != Person || !slices.Contains(n.outcomes(), n.Unattended)) {
			bad("node %q: unattended must be an outcome of a person node%s", n.ID, suggest(n.Unattended, n.outcomes()))
		}
		for outcome := range n.Data {
			if !slices.Contains(n.outcomes(), outcome) {
				bad("node %q: data for undeclared outcome %q%s", n.ID, outcome, suggest(outcome, n.outcomes()))
			}
		}
		if n.Unattended != "" {
			if err := checkData(n.schemas()[n.Unattended], n.UnattendedData); err != nil {
				bad("node %q: unattended data: %v", n.ID, err)
			}
		}
		errs = append(errs, g.checkAsk(n)...)
		if n.MaxVisits < 0 || n.MaxVisits > 99 {
			bad("node %q: max visits must be 0 to 99", n.ID)
		}
	}
	errs = append(errs, g.checkPaced()...)
	// Until it is linked, a call that declares no outcomes may end the
	// workflow with any of what it calls.
	unlinked := slices.ContainsFunc(g.Nodes, func(n Node) bool { return n.Flow != "" && n.call == nil && len(n.Outcomes) == 0 })
	if g.OutputsBy != nil && !unlinked {
		outcomes := g.Outcomes()
		for outcome, outputs := range g.OutputsBy {
			switch {
			case !slices.Contains(outcomes, outcome):
				bad("outputs for %q, which is not an outcome of the workflow%s: want one of %s", outcome, suggest(outcome, outcomes), strings.Join(outcomes, ", "))
			case outcome == Failed && len(outputs) > 0:
				bad("outputs for %q: a workflow that fails gives nothing back", outcome)
			}
		}
	}
	errs = append(errs, g.checkKept()...)
	errs = append(errs, g.checkDocuments()...)
	errs = append(errs, g.checkCleanup()...)
	if len(g.Nodes) > 0 {
		errs = append(errs, g.checkRefs()...)
	}
	return errors.Join(errs...)
}

// version names one node's definition: its question, inputs, outcomes,
// form, judge and edges.
func (n *Node) version() string {
	b, err := json.Marshal(n)
	if err != nil {
		panic(err) // As in [Graph.Digest].
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// OwnDigest names the graph's own definition, without the workflows it
// calls.
func (g *Graph) OwnDigest() string {
	b, err := json.Marshal(g)
	if err != nil {
		// Only Args can fail, and Validate refuses what is not JSON.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Digest names the definition and every workflow it can call, so that
// approving a digest approves all a run of it can do. A transcript holds
// the digest its run started with, so a change to any of them does not
// resume it. For a graph that calls nothing it is [Graph.OwnDigest].
func (g *Graph) Digest() string {
	closure := g.closure()
	if len(closure) == 1 {
		return g.OwnDigest()
	}
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(closure)) {
		fmt.Fprintf(h, "%q %s\n", name, closure[name].OwnDigest())
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// closure is the graph and every workflow it can reach, by name.
func (g *Graph) closure() map[string]*Graph {
	out := map[string]*Graph{g.Name: g}
	queue := []*Graph{g}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, c := range next.Callees {
			if c != nil && out[c.Name] == nil {
				out[c.Name] = c
				queue = append(queue, c)
			}
		}
	}
	return out
}
