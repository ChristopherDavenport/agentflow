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
	graphName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,16}$`)
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

// OK is the outcome of a node that declares none.
const OK = "ok"

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

	// Outcomes are the ways the node can end. Empty means only [OK].
	Outcomes []string
	// Next maps an outcome to the node that follows. An outcome with no
	// entry ends the workflow.
	Next map[string]string

	// Unattended is the outcome a Person node takes when nobody is
	// there to ask. Empty means the run waits.
	Unattended string
	// UnattendedData is the data that goes with it.
	UnattendedData any
	// MaxVisits bounds how often the node runs. 0 means once.
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
		bad("invalid graph name %q: want 1 to 16 letters, digits, _ or -", g.Name)
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
