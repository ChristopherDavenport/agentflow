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

	"github.com/ChristopherDavenport/agenttool"
)

// A node's ID is part of call IDs, which providers hold to 64 characters
// of this set. A workflow's name is not, so it may name a file within a
// skill: <skill>/<path>.
var (
	graphName = regexp.MustCompile(`^[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*$`)
	flowName  = regexp.MustCompile(`^([A-Za-z0-9_-]+|\./[A-Za-z0-9_-]+(/[A-Za-z0-9_-]+)*\.md)$`)
	nodeID    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,20}$`)
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

// What an Agent node's model reads of the conversation.
const (
	// ContextSession is the whole transcript, and what empty means.
	ContextSession = "session"
	// ContextFresh is the transcript from where the step began, so the
	// model knows only the step's prompt and inputs.
	ContextFresh = "fresh"
)

// OK is the outcome of a node that declares none.
const OK = "ok"

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
}

// Node is one step.
type Node struct {
	ID   string
	Kind Kind

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
	// Tools names the tools an Agent node may call.
	Tools []string
	// Context is [ContextSession], [ContextFresh] or empty, on an Agent
	// node only.
	Context string

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
	// MaxVisits bounds how often the node runs. 0 means
	// [DefaultLoopVisits] for a node a loop can come back to, and once
	// for any other.
	MaxVisits int

	// call is what linking learnt of the callee of a Flow node.
	call *callee
}

// callee is a Flow node's contract, as [Graph.Link] reads it from the
// workflow it calls.
type callee struct {
	g        *Graph
	outcomes []string
	data     map[string]*agenttool.Schema
}

func (n *Node) outcomes() []string {
	switch {
	case len(n.Outcomes) > 0:
		return n.Outcomes
	case n.Flow != "" && n.call != nil:
		return n.call.outcomes
	}
	return []string{OK}
}

// schemas is the node's data by outcome: declared, or for a call, the
// callee's outputs.
func (n *Node) schemas() map[string]*agenttool.Schema {
	if n.Flow != "" && n.call != nil {
		return n.call.data
	}
	return n.Data
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
	Name  string
	Nodes []Node
	// Inputs is the schema of what an invocation carries.
	Inputs *agenttool.Schema
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
	for i := range g.Nodes {
		n := &g.Nodes[i]
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
	if !graphName.MatchString(g.Name) || len(g.Name) > maxName {
		bad("invalid graph name %q: want up to %d letters, digits, _ or -, in parts joined by /", g.Name, maxName)
	}
	if g.MaxDepth < 0 || g.MaxDepth > 99 {
		bad("max depth must be 0 to 99")
	}
	if g.Outputs != nil && g.OutputsBy != nil {
		bad("outputs are either by outcome or for every outcome")
	}
	if len(g.Nodes) == 0 {
		bad("graph has no nodes")
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
		case n.Context != "" && n.Kind != Agent:
			bad("node %q: only a model node has a context", n.ID)
		case n.Context != "" && n.Context != ContextSession && n.Context != ContextFresh:
			bad("node %q: context %q: want %q or %q", n.ID, n.Context, ContextSession, ContextFresh)
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
					bad("node %q: judge: undeclared outcome %q", n.ID, outcome)
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
				bad("node %q: edge for undeclared outcome %q", n.ID, outcome)
			}
			if g.node(next) == nil {
				bad("node %q: edge to unknown node %q", n.ID, next)
			}
		}
		for outcome, end := range n.Ends {
			switch {
			case !unknown && !slices.Contains(n.outcomes(), outcome):
				bad("node %q: end for undeclared outcome %q", n.ID, outcome)
			case n.Next[outcome] != "":
				bad("node %q: outcome %q both goes on and ends", n.ID, outcome)
			case !nodeID.MatchString(end) || g.node(end) != nil:
				bad("node %q: end %q: want a name no node has", n.ID, end)
			}
		}
		if n.Unattended != "" && (n.Kind != Person || !slices.Contains(n.outcomes(), n.Unattended)) {
			bad("node %q: unattended must be an outcome of a person node", n.ID)
		}
		for outcome := range n.Data {
			if !slices.Contains(n.outcomes(), outcome) {
				bad("node %q: data for undeclared outcome %q", n.ID, outcome)
			}
		}
		if n.Unattended != "" {
			if err := checkData(n.Data[n.Unattended], n.UnattendedData); err != nil {
				bad("node %q: unattended data: %v", n.ID, err)
			}
		}
		if n.MaxVisits < 0 || n.MaxVisits > 99 {
			bad("node %q: max visits must be 0 to 99", n.ID)
		}
	}
	// Until it is linked, a call that declares no outcomes may end the
	// workflow with any of what it calls.
	unlinked := slices.ContainsFunc(g.Nodes, func(n Node) bool { return n.Flow != "" && n.call == nil && len(n.Outcomes) == 0 })
	if g.OutputsBy != nil && !unlinked {
		outcomes := g.Outcomes()
		for outcome, outputs := range g.OutputsBy {
			switch {
			case !slices.Contains(outcomes, outcome):
				bad("outputs for %q, which is not an outcome of the workflow: want one of %s", outcome, strings.Join(outcomes, ", "))
			case outcome == Failed && len(outputs) > 0:
				bad("outputs for %q: a workflow that fails gives nothing back", outcome)
			}
		}
	}
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
