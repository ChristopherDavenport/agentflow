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

// Node is one step.
type Node struct {
	ID   string
	Kind Kind

	// Tool and Args are the call a Run node makes. A nil Args is {}.
	Tool string
	Args json.RawMessage

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
			if n.Tool == "" || strings.HasPrefix(n.Tool, reserved) {
				bad("node %q: a run node needs a tool of its own", n.ID)
			}
			if n.Args != nil && !json.Valid(n.Args) {
				bad("node %q: args are not JSON", n.ID)
			}
		case Agent, Person:
			if n.Prompt == "" {
				bad("node %q: no prompt", n.ID)
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
		if n.MaxVisits < 0 || n.MaxVisits > 99 {
			bad("node %q: max visits must be 0 to 99", n.ID)
		}
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
