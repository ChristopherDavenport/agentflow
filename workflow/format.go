package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agenttool"
	"go.yaml.in/yaml/v3"
)

var shapes = map[agentflow.Kind][2]string{
	agentflow.Run:    {"[[", "]]"},
	agentflow.Agent:  {"[", "]"},
	agentflow.Person: {"([", "])"},
}

// Format writes a graph as WORKFLOW.md under title. It fails for a graph
// the file cannot say exactly: what it writes always parses back to the
// same definition.
func Format(g *agentflow.Graph, title string) ([]byte, error) {
	if err := g.Validate(); err != nil {
		return nil, err
	}
	var b strings.Builder
	front := mapping()
	if g.Description != "" {
		if err := front.set("description", g.Description); err != nil {
			return nil, err
		}
	}
	if g.Inputs != nil {
		inputs := fields(g.Inputs)
		// An input's default is written beside its schema.
		for i := 0; i+1 < len(inputs.Content); i += 2 {
			if d, ok := g.InputDefaults[inputs.Content[i].Value]; ok {
				var v yaml.Node
				if err := v.Encode(d); err != nil {
					return nil, err
				}
				m := inputs.Content[i+1]
				m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "default"}, &v)
			}
		}
		front.add("inputs", inputs)
	}
	if g.Outputs != nil {
		front.add("outputs", valuesNode(g.Outputs))
	}
	if g.OutputsBy != nil {
		out := mapping()
		for _, outcome := range slices.Sorted(maps.Keys(g.OutputsBy)) {
			out.add(outcome, valuesNode(g.OutputsBy[outcome]))
		}
		front.add("outputs_by", &out.Node)
	}
	if g.Documents != nil {
		docs := mapping()
		for _, name := range slices.Sorted(maps.Keys(g.Documents)) {
			d := g.Documents[name]
			spec := map[string]string{}
			if d.From != "" {
				spec["from"] = d.From
			}
			if d.Input != "" {
				spec["input"] = d.Input
			}
			if err := docs.set(name, spec); err != nil {
				return nil, err
			}
		}
		front.add("documents", &docs.Node)
	}
	for _, s := range []struct {
		key string
		set bool
		v   any
	}{{"max_depth", g.MaxDepth != 0, g.MaxDepth}, {"max_steps", g.MaxSteps != 0, g.MaxSteps}, {"model", g.Model != "", g.Model}, {"thinking", g.Thinking != "", g.Thinking}, {"on_abort", g.OnAbort != "", g.OnAbort}, {"exclude_tools", g.ExcludeTools != nil, g.ExcludeTools},
		{"context", g.Context != "", g.Context}, {"hidden", g.Hidden, true}, {"timeout", g.Timeout != 0, g.Timeout.String()}} {
		if !s.set {
			continue
		}
		if err := front.set(s.key, s.v); err != nil {
			return nil, err
		}
	}
	if len(front.Content) > 0 {
		y, err := yaml.Marshal(&front.Node)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "---\n%s---\n", y)
	}
	fmt.Fprintf(&b, "# %s\n\n```mermaid\nflowchart TD\n", title)
	// Every node first, in order, so the first mentioned is the start.
	for _, n := range g.Nodes {
		s := shapes[n.Kind]
		fmt.Fprintf(&b, "  %s%s%s%s\n", n.ID, s[0], n.ID, s[1])
	}
	for _, n := range g.Nodes {
		for _, outcome := range slices.Sorted(maps.Keys(n.Next)) {
			label := "|" + outcome + "|"
			if outcome == agentflow.OK {
				label = ""
			}
			fmt.Fprintf(&b, "  %s -->%s %s\n", n.ID, label, n.Next[outcome])
		}
	}
	// Draw every end that is named, and where a step branches each
	// outcome that stops the workflow, as an end named for it: an end's
	// name is the outcome the workflow ends with.
	for _, n := range g.Nodes {
		drawn := slices.Sorted(maps.Keys(n.Ends))
		if len(n.Outcomes) >= 2 {
			for _, outcome := range n.Outcomes {
				named := slices.ContainsFunc(g.Nodes, func(m agentflow.Node) bool { return m.ID == outcome })
				if n.Next[outcome] == "" && n.Ends[outcome] == "" && !named && !slices.Contains(reserved, outcome) {
					drawn = append(drawn, outcome)
				}
			}
		}
		for _, outcome := range drawn {
			label := "|" + outcome + "|"
			if outcome == agentflow.OK {
				label = ""
			}
			end := n.Ends[outcome]
			if end == "" {
				end = outcome
			}
			fmt.Fprintf(&b, "  %s -->%s %s((%s))\n", n.ID, label, end, end)
		}
	}
	b.WriteString("```\n")
	for _, n := range g.Nodes {
		set, err := settingsNode(n)
		if err != nil {
			return nil, fmt.Errorf("workflow: node %q: %w", n.ID, err)
		}
		fmt.Fprintf(&b, "\n## %s\n", n.ID)
		if len(set.Content) > 0 {
			y, err := yaml.Marshal(&set.Node)
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "```yaml\n%s```\n", y)
		}
		if n.Prompt != "" {
			fmt.Fprintf(&b, "%s\n", n.Prompt)
		}
	}
	out := []byte(b.String())
	back, err := Parse(g.Name, out)
	if err != nil {
		return nil, fmt.Errorf("workflow: the graph cannot be written as a file: %w", err)
	}
	written := *g
	written.Sources = ""
	// The file names a step's instructions, and loading reads them.
	written.Nodes = slices.Clone(g.Nodes)
	// And each file of the skill a step is given.
	for i := range written.Nodes {
		written.Nodes[i].SkillFiles = nil
	}
	// And each document's first text, from its file.
	if g.Documents != nil {
		written.Documents = maps.Clone(g.Documents)
		for name, d := range written.Documents {
			if d.From != "" {
				d.Text = ""
				written.Documents[name] = d
			}
		}
	}
	for i := range written.Nodes {
		if written.Nodes[i].InstructionsFile != "" {
			written.Nodes[i].Instructions = ""
		}
		// So does each skill's text.
		if skills := written.Nodes[i].Skills; skills != nil {
			written.Nodes[i].Skills = make([]agentflow.StepSkill, len(skills))
			for j, sk := range skills {
				written.Nodes[i].Skills[j] = agentflow.StepSkill{Name: sk.Name}
			}
		}
	}
	for i := range back.Nodes {
		back.Nodes[i].SkillFiles = nil
	}
	if back.OwnDigest() != written.OwnDigest() {
		return nil, errors.New("workflow: the graph cannot be written as a file without changing it")
	}
	return out, nil
}

type yamlMap struct{ yaml.Node }

func mapping() *yamlMap { return &yamlMap{yaml.Node{Kind: yaml.MappingNode}} }

func (m *yamlMap) add(key string, v *yaml.Node) {
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, v)
}

func (m *yamlMap) set(key string, v any) error {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return err
	}
	m.add(key, &n)
	return nil
}

func settingsNode(n agentflow.Node) (*yamlMap, error) {
	m := mapping()
	var errs []error
	set := func(key string, present bool, v any) {
		if present {
			errs = append(errs, m.set(key, v))
		}
	}
	// A question's fallback is written as its outcome, or with data as
	// both.
	fallback := func(key string, f agentflow.Fallback) {
		if f.Data != nil {
			set(key, true, map[string]any{"outcome": f.Outcome, "data": f.Data})
			return
		}
		set(key, f.Outcome != "", f.Outcome)
	}
	set("description", n.Description != "", n.Description)
	set("run", n.Script != "", n.Script)
	set("flow", n.Flow != "", n.Flow)
	set("tool", n.Tool != "", n.Tool)
	if n.Args != nil {
		var args map[string]any
		if err := json.Unmarshal(n.Args, &args); err != nil {
			return nil, fmt.Errorf("args are not an object: %w", err)
		}
		set("args", true, args)
	}
	if n.With != nil {
		m.add("with", valuesNode(n.With))
	}
	set("tools", n.Tools != nil, n.Tools)
	set("tools", n.AllTools, "all")
	set("exclude_tools", n.ExcludeTools != nil, n.ExcludeTools)
	if n.Files != nil {
		var files []any
		for _, v := range n.Files {
			files = append(files, written(v))
		}
		set("files", true, files)
	}
	var skills []string
	for _, sk := range n.Skills {
		skills = append(skills, sk.Name)
	}
	set("skills", skills != nil, skills)
	set("context", n.Context != "", n.Context)
	set("dir", n.Dir.From != "" || n.Dir.Literal != nil, written(n.Dir))
	set("model", n.Model != "", n.Model)
	set("thinking", n.Thinking != "", n.Thinking)
	set("timeout", n.Timeout != 0, n.Timeout.String())
	set("policy", n.Policy != nil, policyOut(n.Policy))
	set("instructions", n.InstructionsFile != "", n.InstructionsFile)
	set("child", n.Child != "", n.Child)
	if n.Reads != nil || n.Edits != nil || n.Writes != nil {
		docs := map[string]any{}
		if n.Reads != nil {
			docs["read"] = n.Reads
		}
		if n.Edits != nil {
			docs["edit"] = n.Edits
		}
		if n.Writes != nil {
			docs["write"] = n.Writes
		}
		set("documents", true, docs)
	}
	switch {
	case n.Show == nil:
	case len(n.Show.Members) == 0:
		set("show", true, "none")
	default:
		set("show", true, n.Show.Members)
	}
	set("outcomes", n.Outcomes != nil, n.Outcomes)
	if n.Data != nil {
		out := mapping()
		for _, outcome := range slices.Sorted(maps.Keys(n.Data)) {
			out.add(outcome, fields(n.Data[outcome]))
		}
		m.add("outputs", &out.Node)
	}
	fallback("unattended", n.Unattended)
	if c := n.Choices; c != nil {
		choices := &yamlMap{*valuesNode(map[string]agentflow.Value{"from": c.From})}
		if c.Many {
			errs = append(errs, choices.set("select", "many"))
		}
		errs = append(errs, choices.set("outcome", c.Outcome))
		if c.As != "" {
			errs = append(errs, choices.set("as", c.As))
		}
		m.add("choices", &choices.Node)
	}
	switch t := n.Text; {
	case t == nil:
	case t.As != "":
		set("text", true, map[string]any{"outcome": t.Outcome, "as": t.As})
	default:
		set("text", true, t.Outcome)
	}
	fallback("on_timeout", n.OnTimeout)
	switch {
	case n.PersonPaced:
		set("max_visits", true, "person")
	default:
		set("max_visits", n.MaxVisits != 0, n.MaxVisits)
	}
	if j := n.Judge; j != nil {
		judge := map[string]any{"outcomes": j.Outcomes, "criteria": j.Criteria}
		if j.Tools != nil {
			judge["tools"] = j.Tools
		}
		if j.Returns != 0 {
			judge["max_returns"] = j.Returns
		}
		if j.Policy != nil {
			judge["policy"] = policyOut(j.Policy)
		}
		set("judge", true, judge)
	}
	return m, errors.Join(errs...)
}

func valuesNode(values map[string]agentflow.Value) *yaml.Node {
	m := mapping()
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if err := m.set(name, written(values[name])); err != nil {
			// Left out, so the round trip fails and says so.
			continue
		}
	}
	return &m.Node
}

// fields writes an object schema's properties.
func fields(s *agenttool.Schema) *yaml.Node {
	m := mapping()
	for _, p := range s.Properties {
		m.add(p.Name, field(p.Schema, slices.Contains(s.Required, p.Name)))
	}
	return &m.Node
}

func field(s *agenttool.Schema, required bool) *yaml.Node {
	m := mapping()
	m.Style = yaml.FlowStyle
	add := func(key string, present bool, v any) {
		if present {
			// A value yaml cannot hold fails the round trip.
			_ = m.set(key, v)
		}
	}
	add("type", s.Type != "", s.Type)
	add("required", required, true)
	add("description", s.Description != "", s.Description)
	add("enum", s.Enum != nil, s.Enum)
	if s.Items != nil {
		m.add("items", field(s.Items, false))
	}
	if s.Properties != nil {
		m.add("properties", fields(s))
	}
	return &m.Node
}

// policyOut is a policy as a section writes it: a rule alone where it
// has no note.
func policyOut(p *agentflow.Policy) map[string]any {
	if p == nil {
		return nil
	}
	list := func(rules []agentflow.PolicyRule) []any {
		out := []any{}
		for _, r := range rules {
			entry := map[string]any{"rule": r.Rule}
			if r.Note != "" {
				entry["note"] = r.Note
			}
			if r.Path.From != "" || r.Path.Literal != nil {
				entry["path"] = written(r.Path)
			}
			if r.Command.From != "" || r.Command.Literal != nil {
				entry["command"] = written(r.Command)
			}
			if len(entry) == 1 {
				out = append(out, r.Rule)
			} else {
				out = append(out, entry)
			}
		}
		return out
	}
	out := map[string]any{}
	if p.Allow != nil {
		out["allow"] = list(p.Allow)
	}
	if p.Deny != nil {
		out["deny"] = list(p.Deny)
	}
	return out
}

// written is a value as a file writes it: a literal, or one expression.
func written(ref agentflow.Value) any {
	if ref.From == "" {
		return ref.Literal
	}
	expr := strings.Join(append([]string{ref.From}, ref.Or...), ", ")
	switch {
	case ref.Optional:
		expr = "optional(" + expr + ")"
	case len(ref.Or) > 0:
		expr = "latest(" + expr + ")"
	}
	return "${{ " + expr + " }}"
}
