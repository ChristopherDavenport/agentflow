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
	"regexp"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// Names are part of call IDs, which providers hold to 64 characters of
// this set.
var (
	graphName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	nodeID    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,20}$`)
)

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
	Run  string         `json:"run"`
	With map[string]any `json:"with,omitempty"`
	// Outcomes is the node's declared ones, none when it only succeeds.
	Outcomes []string `json:"outcomes,omitempty"`
}

// Node is one step.
type Node struct {
	ID   string
	Kind Kind

	// A Run node runs Script, or calls Tool with Args. A nil Args is {}.
	Script string
	Tool   string
	Args   json.RawMessage

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
}

func (n *Node) outcomes() []string {
	if len(n.Outcomes) == 0 {
		return []string{OK}
	}
	return n.Outcomes
}

// Graph is a workflow. The first node is where it starts.
type Graph struct {
	Name  string
	Nodes []Node
	// Inputs is the schema of what an invocation carries.
	Inputs *agenttool.Schema
	// Outputs is what the finished workflow gives back.
	Outputs map[string]Value
	// Sources is the digest of the files the graph was loaded from,
	// scripts among them, so a change to any is a change to the graph.
	Sources string
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
	if !graphName.MatchString(g.Name) {
		bad("invalid graph name %q: want 1 to 64 letters, digits, _ or -", g.Name)
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
		for outcome, next := range n.Next {
			if !slices.Contains(n.outcomes(), outcome) {
				bad("node %q: edge for undeclared outcome %q", n.ID, outcome)
			}
			if g.node(next) == nil {
				bad("node %q: edge to unknown node %q", n.ID, next)
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

// Digest names the definition. A transcript holds the digest its
// instance started with, so a changed graph does not resume it.
func (g *Graph) Digest() string {
	b, err := json.Marshal(g)
	if err != nil {
		// Only Args can fail, and Validate refuses what is not JSON.
		panic(err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
