package agentflow

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	slash "path"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// checkChild holds a node's child settings to where they may be set.
func (n *Node) checkChild() []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agentflow: node %q: "+format, append([]any{n.ID}, args...)...))
	}
	if !n.fresh() {
		for _, s := range []struct {
			key string
			set bool
		}{{"dir", n.Dir != ""}, {"instructions", n.Instructions != "" || n.InstructionsFile != ""}, {"child", n.Child != ""}} {
			if s.set {
				bad("%s is a setting of a child; it needs context: %s", s.key, ContextFresh)
			}
		}
	}
	if d := n.Dir; d != "" && (slash.IsAbs(d) || slash.Clean(d) != d || d == "." || d == ".." || strings.HasPrefix(d, "../") || strings.Contains(d, `\`)) {
		bad("dir %q: want a clean path below the workspace's root", d)
	}
	if n.Child != "" {
		switch {
		case n.Kind != Agent:
			bad("only a model step keeps a child")
		case !childName.MatchString(n.Child):
			bad("child %q: want 1 to 40 letters, digits, _ or -", n.Child)
		}
	}
	if n.Verdict && n.Kind != Agent {
		bad("only a model step gives a verdict")
	}
	return errs
}

// checkSettings holds a node's step settings to the steps they are for.
func (n *Node) checkSettings() []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agentflow: node %q: "+format, append([]any{n.ID}, args...)...))
	}
	call := n.Flow != ""
	switch {
	case n.Model != "" && n.Kind != Agent && !(call && n.fresh()):
		bad("model is a model step's, or a child's")
	case n.Thinking != "" && n.Kind != Agent:
		bad("thinking is a model step's")
	case n.Thinking != "" && !slices.Contains(thinkingLevels, n.Thinking):
		bad("thinking %q: want one of %s", n.Thinking, strings.Join(thinkingLevels, ", "))
	}
	switch {
	case n.Timeout < 0 || n.Timeout > MaxTimeout:
		bad("timeout must be above 0 and at most %s", MaxTimeout)
	case n.Timeout > 0 && (n.Kind == Person || call):
		bad("a timeout is a script's, a tool's or a model step's")
	case n.Timeout > 0 && n.Verdict:
		bad("a judge's look has the timeout of an unattended model step")
	}
	switch {
	case n.Policy != nil && n.Kind != Agent && !call:
		bad("a policy is a model step's, or a call's for what it calls")
	case n.Policy != nil:
		// A call's policy covers steps whose tools are their own.
		if err := n.Policy.check(n.Tools, !call); err != nil {
			bad("policy: %v", err)
		}
	}
	if j := n.Judge; j != nil && j.Policy != nil {
		if err := j.Policy.check(j.Tools, true); err != nil {
			bad("judge: policy: %v", err)
		}
	}
	if slices.Contains(n.Outcomes, Stuck) || n.Data[Stuck] != nil {
		bad("%q is the outcome flow_stuck reports, so no step declares it or its data", Stuck)
	}
	if !n.canStick() && (n.Next[Stuck] != "" || n.Ends[Stuck] != "") && !slices.Contains(n.outcomes(), Stuck) {
		bad("only a model step can be %s", Stuck)
	}
	return errs
}

// checkKept holds the steps that keep one child to the same child: one
// directory, model and instructions.
func (g *Graph) checkKept() []error {
	var errs []error
	first := map[string]*Node{}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.Child == "" {
			continue
		}
		f := first[n.Child]
		if f == nil {
			first[n.Child] = n
			continue
		}
		if f.Dir != n.Dir || f.Model != n.Model || f.Instructions != n.Instructions {
			errs = append(errs, fmt.Errorf("agentflow: nodes %q and %q keep child %q and differ in its dir, model or instructions", f.ID, n.ID, n.Child))
		}
	}
	return errs
}

// fresh says the node's work runs in a child: it says context fresh, or
// it is a model step that asks for another model than its workflow's and
// does not say context session. The other model would otherwise read a
// conversation another model wrote, without that model's reasoning.
func (n *Node) fresh() bool {
	return n.Context == ContextFresh || n.Context == "" && n.otherModel
}

// inputsOf is the inputs schema and the with of a workflow made from n,
// which reads n's inputs as its own.
func inputsOf(n *Node) (*agenttool.Schema, map[string]Value) {
	if len(n.With) == 0 {
		return nil, nil
	}
	s := &agenttool.Schema{Type: "object"}
	with := map[string]Value{}
	for _, name := range slices.Sorted(maps.Keys(n.With)) {
		s.Properties = append(s.Properties, agenttool.Property{Name: name, Schema: &agenttool.Schema{}})
		with[name] = Optional("inputs." + name)
	}
	return s, with
}

// stepGraph is the workflow of one step a fresh model step of g runs in
// its child: the step itself, reading its inputs from the child's start.
func (g *Graph) stepGraph(n *Node) *Graph {
	inputs, with := inputsOf(n)
	// The child reports a stuck model as its outcome, and the caller
	// decides what that means.
	step := Node{ID: n.ID, Kind: Agent, Prompt: n.Prompt, Tools: n.Tools, Outcomes: n.Outcomes, Data: n.Data, With: with,
		Model: cmp.Or(n.Model, g.Model), Thinking: cmp.Or(n.Thinking, g.Thinking), Timeout: n.Timeout, Policy: n.Policy,
		Ends: map[string]string{Stuck: Stuck}}
	// The step's model is its workflow's, so the step reads its own
	// child's conversation rather than opening another.
	return &Graph{Name: g.Name + ":" + n.ID, Nodes: []Node{step}, Inputs: inputs, Sources: g.Sources, Model: step.Model}
}

// judgeGraph is the workflow a judge of n runs in its child: a model step
// that weighs the question by the criteria and gives a verdict.
func (g *Graph) judgeGraph(n *Node) *Graph {
	inputs, with := inputsOf(n)
	outcomes := append(slices.Clone(n.Judge.Outcomes), HandBack)
	var data map[string]*agenttool.Schema
	for _, outcome := range n.Judge.Outcomes {
		if s := n.Data[outcome]; s != nil {
			if data == nil {
				data = map[string]*agenttool.Schema{}
			}
			data[outcome] = s
		}
	}
	prompt := fmt.Sprintf("The question: %s\n\nWhat to weigh:\n%s", n.Prompt, n.Judge.Criteria)
	judge := Node{ID: n.ID, Kind: Agent, Verdict: true, Prompt: prompt, Tools: n.Judge.Tools, Outcomes: outcomes, Data: data, With: with, Policy: n.Judge.Policy}
	return &Graph{Name: g.Name + ":" + n.ID + ":judge", Nodes: []Node{judge}, Inputs: inputs, Sources: g.Sources}
}

// find is the workflow a run may start under name: one of the closure,
// or one made from a step of it to run in a child.
func (g *Graph) find(name string) (*Graph, error) {
	closure := g.closure()
	if c := closure[name]; c != nil {
		return c, nil
	}
	base, step, ok := strings.Cut(name, ":")
	step, judge := strings.CutSuffix(step, ":judge")
	b := closure[base]
	if !ok || b == nil {
		return nil, fmt.Errorf("agentflow: no workflow %q", name)
	}
	n := b.node(step)
	switch {
	case n == nil:
		return nil, fmt.Errorf("agentflow: no step %q of %s", step, base)
	case judge && n.Judge != nil:
		return b.judgeGraph(n), nil
	case !judge && n.Kind == Agent && n.fresh():
		return b.stepGraph(n), nil
	}
	return nil, errors.New("agentflow: " + name + " is made from no step that runs in a child")
}
