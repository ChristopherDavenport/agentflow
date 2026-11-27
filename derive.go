package agentflow

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
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
	if !n.fresh() && !n.unlinked() {
		for _, s := range []struct {
			key string
			set bool
		}{{"instructions", n.Instructions != "" || n.InstructionsFile != ""}, {"child", n.Child != ""}} {
			if s.set {
				bad("%s is a setting of a child; it needs context: %s", s.key, ContextFresh)
			}
		}
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
	case n.Model != "" && n.Kind != Agent && !(call && (n.fresh() || n.unlinked())):
		bad("model is a model step's, or a child's")
	case n.Thinking != "" && n.Kind != Agent:
		bad("thinking is a model step's")
	case n.Thinking != "" && !slices.Contains(thinkingLevels, n.Thinking):
		bad("thinking %q: want one of %s", n.Thinking, strings.Join(thinkingLevels, ", "))
	}
	switch {
	case n.Timeout < 0 || n.Timeout > MaxTimeout:
		bad("timeout must be above 0 and at most %s", MaxTimeout)
	case n.Timeout > 0 && n.Verdict:
		bad("a judge's look has the timeout of an unattended model step")
	}
	switch {
	case n.Policy != nil && n.Kind != Agent && !call:
		bad("a policy is a model step's, or a call's for what it calls")
	case n.Policy != nil:
		// A call's policy covers steps whose tools are their own.
		// A step given workspace files may hold their reads to rules, with
		// the read tool it reads them with.
		tools := n.Tools
		if len(n.Files) > 0 && !slices.Contains(tools, "read") {
			tools = append(slices.Clone(tools), "read")
		}
		if err := n.Policy.check(tools, !call && !n.AllTools); err != nil {
			bad("policy: %v", err)
		}
	}
	if j := n.Judge; j != nil && j.Policy != nil {
		if err := j.Policy.check(j.Tools, true); err != nil {
			bad("judge: policy: %v", err)
		}
		if j.Policy.fromData() {
			bad("judge: policy: a judge's rules are fixed, and take nothing from the record")
		}
	}
	seen := map[string]bool{}
	for _, sk := range n.Skills {
		switch {
		case n.Kind != Agent || n.Verdict:
			bad("skills are a model step's")
		case !skillName.MatchString(sk.Name):
			bad("skill %q: want 1 to 64 letters, digits, _ or -", sk.Name)
		case seen[sk.Name]:
			bad("skill %q is named twice", sk.Name)
		}
		seen[sk.Name] = true
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
// directory, model and instructions. A directory taken from data is
// checked when the call is made, against the one the child started in.
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
		literal := f.Dir.From == "" && n.Dir.From == ""
		if literal && fmt.Sprint(f.Dir.Literal) != fmt.Sprint(n.Dir.Literal) || f.Model != n.Model || f.Instructions != n.Instructions {
			errs = append(errs, fmt.Errorf("agentflow: nodes %q and %q keep child %q and differ in its dir, model or instructions", f.ID, n.ID, n.Child))
		}
	}
	return errs
}

// fresh says the node's work runs in a child: it says context fresh, or
// it is a model step that asks for another model than its workflow's and
// does not say context session. The other model would otherwise read a
// conversation another model wrote, without that model's reasoning.
//
// A call also runs in a child when the workflow it calls says context
// fresh of itself, which linking finds.
func (n *Node) fresh() bool {
	return n.Context == ContextFresh || n.Context == "" && (n.otherModel || n.call != nil && n.call.g.Context == ContextFresh)
}

// unlinked says n calls a workflow linking has not yet found, so where
// it runs is not yet known: checks that turn on it wait for the link.
func (n *Node) unlinked() bool { return n.Flow != "" && n.call == nil }

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
	step := Node{ID: n.ID, Kind: Agent, Description: n.Description, Prompt: n.Prompt, Tools: n.Tools, AllTools: n.AllTools, ExcludeTools: g.excluded(n), Outcomes: n.Outcomes, Data: n.Data, With: with,
		Model: cmp.Or(n.Model, g.Model), Thinking: cmp.Or(n.Thinking, g.Thinking), Timeout: n.Timeout, Policy: n.Policy, Skills: n.Skills,
		Ends: map[string]string{Stuck: Stuck}, Reads: n.Reads, Edits: n.Edits, SkillFiles: n.SkillFiles}
	// A policy that takes its rules from the record is resolved where the
	// record is, and reaches the child with its call: the child's own
	// record holds none of the steps it names.
	if step.Policy.fromData() {
		step.Policy = nil
	}
	// The documents it reads are the child's, as its call passes them in.
	var docs map[string]Document
	for name := range g.Documents {
		if n.reads(name) {
			if docs == nil {
				docs = map[string]Document{}
			}
			docs[name] = Document{}
		}
	}
	// The step's model is its workflow's, so the step reads its own
	// child's conversation rather than opening another.
	return &Graph{Name: g.Name + ":" + n.ID, Description: g.Description, Nodes: []Node{step}, Inputs: inputs, Sources: g.Sources, Model: step.Model, Documents: docs, Timeout: g.Timeout}
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
	judge := Node{ID: n.ID, Kind: Agent, Verdict: true, Description: n.Description, Prompt: prompt, Tools: n.Judge.Tools, Outcomes: outcomes, Data: data, With: with, Policy: n.Judge.Policy}
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
