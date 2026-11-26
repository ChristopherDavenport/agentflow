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
	if g.Inputs != nil {
		front.add("inputs", fields(g.Inputs))
	}
	if g.Outputs != nil {
		front.add("outputs", valuesNode(g.Outputs))
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
	// Where a step branches, draw the outcomes that stop the workflow.
	end := "done"
	for slices.ContainsFunc(g.Nodes, func(n agentflow.Node) bool { return n.ID == end }) {
		end += "_"
	}
	for _, n := range g.Nodes {
		if len(n.Outcomes) < 2 {
			continue
		}
		for _, outcome := range n.Outcomes {
			if n.Next[outcome] == "" {
				fmt.Fprintf(&b, "  %s -->|%s| %s((%s))\n", n.ID, outcome, end, end)
			}
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
	if back.Digest() != written.Digest() {
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
	set("run", n.Script != "", n.Script)
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
	set("context", n.Context != "", n.Context)
	set("outcomes", n.Outcomes != nil, n.Outcomes)
	if n.Data != nil {
		out := mapping()
		for _, outcome := range slices.Sorted(maps.Keys(n.Data)) {
			out.add(outcome, fields(n.Data[outcome]))
		}
		m.add("outputs", &out.Node)
	}
	switch {
	case n.UnattendedData != nil:
		set("unattended", true, map[string]any{"outcome": n.Unattended, "data": n.UnattendedData})
	default:
		set("unattended", n.Unattended != "", n.Unattended)
	}
	set("max_visits", n.MaxVisits != 0, n.MaxVisits)
	if j := n.Judge; j != nil {
		judge := map[string]any{"outcomes": j.Outcomes, "criteria": j.Criteria}
		if j.Tools != nil {
			judge["tools"] = j.Tools
		}
		if j.Returns != 0 {
			judge["max_returns"] = j.Returns
		}
		set("judge", true, judge)
	}
	return m, errors.Join(errs...)
}

func valuesNode(values map[string]agentflow.Value) *yaml.Node {
	m := mapping()
	for _, name := range slices.Sorted(maps.Keys(values)) {
		v := values[name].Literal
		if ref := values[name]; ref.From != "" {
			expr := strings.Join(append([]string{ref.From}, ref.Or...), ", ")
			switch {
			case ref.Optional:
				expr = "optional(" + expr + ")"
			case len(ref.Or) > 0:
				expr = "latest(" + expr + ")"
			}
			v = "${{ " + expr + " }}"
		}
		if err := m.set(name, v); err != nil {
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
