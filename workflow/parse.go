// Package workflow reads and writes WORKFLOW.md, and loads a workflow
// from an Agent Skill.
//
// A file is a Mermaid flowchart, which is the control flow, and a
// section per node, which says what the node does. Only what is
// understood is accepted: nothing in the diagram or the settings is
// ignored, so the picture cannot disagree with what runs.
package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agenttool"
	"go.yaml.in/yaml/v3"
)

const (
	nodePattern  = `([A-Za-z0-9_]+(?:-[A-Za-z0-9_]+)*)`
	labelPattern = `(?:"[^"]*"|[^\[\]()|"{}]+)`
	shapePattern = `(\[\[` + labelPattern + `\]\]|\(\[` + labelPattern + `\]\)|\(\(` + labelPattern + `\)\)|\[` + labelPattern + `\])?`
)

var (
	// A line is a node, or an edge between two.
	mermaidLine = regexp.MustCompile(`^` + nodePattern + shapePattern +
		`(?:\s*-->\s*(?:\|([A-Za-z0-9_-]+)\|)?\s*` + nodePattern + shapePattern + `)?$`)
	mermaidHead = regexp.MustCompile(`^flowchart\s+(TD|LR)$`)
	fenceLine   = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})\\s*(.*?)\\s*$")
	reference   = regexp.MustCompile(`^\$\{\{\s*([^{}()\s]+)\s*\}\}$`)
	function    = regexp.MustCompile(`^\$\{\{\s*(latest|optional)\(([^{}()]*)\)\s*\}\}$`)
	// An end is where an outcome stops the workflow, drawn so a reader
	// sees it. It is not a node and has no section.
	endShape = regexp.MustCompile(nodePattern + `\(\(`)

	// Words Mermaid's flowchart grammar uses, refused as node IDs so
	// that a file that loads is also one that can be drawn.
	reserved = []string{"end", "graph", "subgraph", "flowchart", "direction", "style", "linkStyle", "classDef", "class", "click", "default"}
)

// block is a fenced block, or a run of other lines when info is nil.
type block struct {
	info  *string
	lines []string
	line  int
}

type section struct {
	id     string
	line   int
	blocks []block
}

// split cuts a document into what is before the first "## " heading and
// the sections after it, reading fences so that a heading inside one is
// text.
func split(lines []string, offset int) (preamble section, sections []section, err error) {
	cur := &preamble
	add := func(b block) {
		if b.info == nil && strings.TrimSpace(strings.Join(b.lines, "")) == "" && len(cur.blocks) == 0 {
			return
		}
		cur.blocks = append(cur.blocks, b)
	}
	var text block
	flush := func() {
		if len(text.lines) > 0 {
			add(text)
		}
		text = block{}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if m := fenceLine.FindStringSubmatch(line); m != nil {
			flush()
			open, info := m[1], m[2]
			b := block{info: &info, line: offset + i + 1}
			closed := false
			for i++; i < len(lines); i++ {
				c := fenceLine.FindStringSubmatch(lines[i])
				if c != nil && c[2] == "" && c[1][0] == open[0] && len(c[1]) >= len(open) {
					closed = true
					break
				}
				b.lines = append(b.lines, lines[i])
			}
			if !closed {
				return preamble, nil, fmt.Errorf("line %d: the fence is never closed", b.line)
			}
			add(b)
			continue
		}
		if id, ok := strings.CutPrefix(line, "## "); ok {
			flush()
			sections = append(sections, section{id: strings.TrimSpace(id), line: offset + i + 1})
			cur = &sections[len(sections)-1]
			continue
		}
		if len(text.lines) == 0 {
			text.line = offset + i + 1
		}
		text.lines = append(text.lines, line)
	}
	flush()
	return preamble, sections, nil
}

type frontmatter struct {
	Inputs  yaml.Node `yaml:"inputs"`
	Outputs yaml.Node `yaml:"outputs"`
}

type settings struct {
	Run     string         `yaml:"run"`
	Tool    string         `yaml:"tool"`
	Args    map[string]any `yaml:"args"`
	With    yaml.Node      `yaml:"with"`
	Outputs yaml.Node      `yaml:"outputs"`
	Tools   []string       `yaml:"tools"`
	Context string         `yaml:"context"`
	Judge   *struct {
		Criteria string   `yaml:"criteria"`
		Outcomes []string `yaml:"outcomes"`
		Tools    []string `yaml:"tools"`
		Returns  int      `yaml:"max_returns"`
	} `yaml:"judge"`
	Outcomes   []string  `yaml:"outcomes"`
	Unattended yaml.Node `yaml:"unattended"`
	MaxVisits  int       `yaml:"max_visits"`
}

// strict decodes one YAML document, refusing keys it does not know.
func strict(src []byte, v any) error {
	d := yaml.NewDecoder(bytes.NewReader(src))
	d.KnownFields(true)
	if err := d.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Parse reads a WORKFLOW.md as the workflow called name. The graph it
// returns is valid.
func Parse(name string, src []byte) (*agentflow.Graph, error) {
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	g := &agentflow.Graph{Name: name}
	offset := 0
	if len(lines) > 0 && lines[0] == "---" {
		end := slices.Index(lines[1:], "---")
		if end < 0 {
			return nil, errors.New("workflow: the frontmatter is never closed")
		}
		var fm frontmatter
		if err := strict([]byte(strings.Join(lines[1:1+end], "\n")), &fm); err != nil {
			return nil, fmt.Errorf("workflow: frontmatter: %w", err)
		}
		var err error
		if g.Inputs, err = objectSchema(&fm.Inputs); err != nil {
			return nil, fmt.Errorf("workflow: inputs: %w", err)
		}
		if g.Outputs, err = values(&fm.Outputs); err != nil {
			return nil, fmt.Errorf("workflow: outputs: %w", err)
		}
		offset = end + 2
		lines = lines[offset:]
	}
	preamble, sections, err := split(lines, offset)
	if err != nil {
		return nil, fmt.Errorf("workflow: %w", err)
	}

	var diagrams []block
	for _, b := range preamble.blocks {
		if b.info != nil && *b.info == "mermaid" {
			diagrams = append(diagrams, b)
		}
	}
	if len(diagrams) != 1 {
		return nil, fmt.Errorf("workflow: want one mermaid block before the first section, found %d", len(diagrams))
	}
	ends, err := diagram(g, diagrams[0])
	if err != nil {
		return nil, fmt.Errorf("workflow: %w", err)
	}

	index := map[string]int{}
	for i, n := range g.Nodes {
		index[n.ID] = i
	}
	seen := map[string]bool{}
	for _, s := range sections {
		i, ok := index[s.id]
		switch {
		case !ok:
			return nil, fmt.Errorf("workflow: line %d: section %q is not a node of the diagram", s.line, s.id)
		case seen[s.id]:
			return nil, fmt.Errorf("workflow: line %d: section %q appears twice", s.line, s.id)
		}
		seen[s.id] = true
		if err := node(&g.Nodes[i], s); err != nil {
			return nil, fmt.Errorf("workflow: node %q: %w", s.id, err)
		}
	}
	for _, n := range g.Nodes {
		if !seen[n.ID] {
			return nil, fmt.Errorf("workflow: node %q has no section", n.ID)
		}
	}
	for _, e := range ends {
		outcomes := g.Nodes[index[e.from]].Outcomes
		if len(outcomes) == 0 {
			outcomes = []string{agentflow.OK}
		}
		if !slices.Contains(outcomes, e.outcome) {
			return nil, fmt.Errorf("workflow: node %q: edge for undeclared outcome %q", e.from, e.outcome)
		}
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return g, nil
}

// ending is an edge into an end.
type ending struct{ from, outcome string }

// diagram reads the nodes, their kinds and their edges, and returns the
// edges into ends. The first node mentioned is where the workflow starts.
func diagram(g *agentflow.Graph, b block) ([]ending, error) {
	var ends []ending
	isEnd := map[string]bool{}
	for _, line := range b.lines {
		if !strings.HasPrefix(strings.TrimSpace(line), "%%") {
			for _, m := range endShape.FindAllStringSubmatch(line, -1) {
				isEnd[m[1]] = true
			}
		}
	}
	index := map[string]int{}
	mention := func(id, shape string, line int) (*agentflow.Node, error) {
		if slices.Contains(reserved, id) {
			return nil, fmt.Errorf("line %d: Mermaid reserves %q", line, id)
		}
		i, ok := index[id]
		if !ok {
			i = len(g.Nodes)
			index[id] = i
			g.Nodes = append(g.Nodes, agentflow.Node{ID: id})
		}
		var kind agentflow.Kind
		switch {
		case strings.HasPrefix(shape, "[["):
			kind = agentflow.Run
		case strings.HasPrefix(shape, "(["):
			kind = agentflow.Person
		case shape != "":
			kind = agentflow.Agent
		default:
			return &g.Nodes[i], nil
		}
		if old := g.Nodes[i].Kind; old != 0 && old != kind {
			return nil, fmt.Errorf("line %d: node %q is drawn with two shapes", line, id)
		}
		g.Nodes[i].Kind = kind
		return &g.Nodes[i], nil
	}
	head := false
	for i, raw := range b.lines {
		line, at := strings.TrimSpace(raw), b.line+i+1
		if line == "" || strings.HasPrefix(line, "%%") {
			continue
		}
		if !head {
			if !mermaidHead.MatchString(line) {
				return nil, fmt.Errorf("line %d: want \"flowchart TD\" or \"flowchart LR\", got %q", at, line)
			}
			head = true
			continue
		}
		m := mermaidLine.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("line %d: %q is not a node or an edge this dialect accepts", at, line)
		}
		to, outcome := m[4], m[3]
		if isEnd[m[1]] {
			if to != "" || m[2] != "" && !strings.HasPrefix(m[2], "((") {
				return nil, fmt.Errorf("line %d: %q is an end: nothing leaves it and it has one shape", at, m[1])
			}
			continue
		}
		if _, err := mention(m[1], m[2], at); err != nil {
			return nil, err
		}
		if to == "" {
			continue
		}
		if isEnd[to] {
			if m[5] != "" && !strings.HasPrefix(m[5], "((") || slices.Contains(reserved, to) {
				return nil, fmt.Errorf("line %d: %q is an end: it has one shape and no name Mermaid reserves", at, to)
			}
		} else if _, err := mention(to, m[5], at); err != nil {
			return nil, err
		}
		if outcome == "" {
			outcome = agentflow.OK
		}
		from := &g.Nodes[index[m[1]]]
		ended := slices.Contains(ends, ending{from.ID, outcome})
		old, ok := from.Next[outcome]
		switch {
		case ok && old != to:
			return nil, fmt.Errorf("line %d: %q already goes to %q on %q", at, from.ID, old, outcome)
		case ended && !isEnd[to]:
			return nil, fmt.Errorf("line %d: %q already ends on %q", at, from.ID, outcome)
		case isEnd[to]:
			if !ended {
				ends = append(ends, ending{from.ID, outcome})
			}
			continue
		}
		if from.Next == nil {
			from.Next = map[string]string{}
		}
		from.Next[outcome] = to
	}
	if len(g.Nodes) == 0 {
		return nil, errors.New("the diagram has no nodes")
	}
	for _, n := range g.Nodes {
		if n.Kind == 0 {
			return nil, fmt.Errorf("node %q has no shape: [[program]], [model] or ([person])", n.ID)
		}
	}
	return ends, nil
}

// node reads a section: a leading yaml block of settings, then prose.
func node(n *agentflow.Node, s section) error {
	blocks := s.blocks
	var set settings
	if len(blocks) > 0 && blocks[0].info != nil && *blocks[0].info == "yaml" {
		if err := strict([]byte(strings.Join(blocks[0].lines, "\n")), &set); err != nil {
			return err
		}
		blocks = blocks[1:]
	}
	var prose []string
	for _, b := range blocks {
		if b.info == nil {
			prose = append(prose, b.lines...)
			continue
		}
		// A yaml block anywhere else would read as settings and be none.
		if word, _, _ := strings.Cut(*b.info, " "); word == "yaml" || word == "yml" {
			return fmt.Errorf("line %d: settings are the yaml block that opens the section", b.line)
		}
		fence := "```"
		for _, l := range b.lines {
			for strings.Contains(l, fence) {
				fence += "`"
			}
		}
		prose = append(prose, fence+*b.info)
		prose = append(prose, b.lines...)
		prose = append(prose, fence)
	}
	n.Prompt = strings.TrimSpace(strings.Join(prose, "\n"))

	n.Script, n.Tool, n.Tools, n.Outcomes, n.MaxVisits = set.Run, set.Tool, set.Tools, set.Outcomes, set.MaxVisits
	if set.Args != nil {
		if hasReference(set.Args) {
			return errors.New("args are literals; put a reference under with")
		}
		b, err := json.Marshal(set.Args)
		if err != nil {
			return fmt.Errorf("args: %w", err)
		}
		n.Args = b
	}
	if n.Kind != agentflow.Run && (n.Script != "" || n.Tool != "" || n.Args != nil) {
		return errors.New("only a program node has run, tool or args")
	}
	n.Context = set.Context
	if j := set.Judge; j != nil {
		n.Judge = &agentflow.Judge{Criteria: strings.TrimSpace(j.Criteria), Outcomes: j.Outcomes, Tools: j.Tools, Returns: j.Returns}
	}
	if n.Kind != agentflow.Agent && n.Tools != nil {
		return errors.New("only a model node has tools")
	}
	var err error
	if n.With, err = values(&set.With); err != nil {
		return fmt.Errorf("with: %w", err)
	}
	if err := pairs(&set.Outputs, func(outcome string, v *yaml.Node) error {
		schema, err := objectSchema(v)
		if err != nil {
			return fmt.Errorf("%s: %w", outcome, err)
		}
		if n.Data == nil {
			n.Data = map[string]*agenttool.Schema{}
		}
		// An outcome with no fields wants no data, and so no schema.
		if schema != nil {
			n.Data[outcome] = schema
		} else if !slices.Contains(n.Outcomes, outcome) && outcome != agentflow.OK {
			return fmt.Errorf("outcome %q is not declared", outcome)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("outputs: %w", err)
	}
	if len(n.Data) == 0 {
		n.Data = nil
	}
	switch u := &set.Unattended; {
	case u.IsZero():
	case n.Kind != agentflow.Person:
		return errors.New("only a person node has unattended")
	case u.Kind == yaml.ScalarNode:
		n.Unattended = u.Value
	default:
		return pairs(u, func(key string, v *yaml.Node) error {
			switch key {
			case "outcome":
				return v.Decode(&n.Unattended)
			case "data":
				return v.Decode(&n.UnattendedData)
			}
			return fmt.Errorf("unattended: unknown key %q", key)
		})
	}
	return nil
}

// pairs visits a mapping in the order written. An absent node has none.
func pairs(n *yaml.Node, fn func(key string, v *yaml.Node) error) error {
	if n.IsZero() || n.Tag == "!!null" {
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: want a mapping", n.Line)
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if seen[key] {
			return fmt.Errorf("%q appears twice", key)
		}
		seen[key] = true
		if err := fn(key, n.Content[i+1]); err != nil {
			return err
		}
	}
	return nil
}

// values reads named arguments: each a literal or one reference.
func values(n *yaml.Node) (map[string]agentflow.Value, error) {
	var out map[string]agentflow.Value
	err := pairs(n, func(name string, v *yaml.Node) error {
		var decoded any
		if err := v.Decode(&decoded); err != nil {
			return err
		}
		value := agentflow.Literal(decoded)
		if s, ok := decoded.(string); ok {
			if m := reference.FindStringSubmatch(s); m != nil {
				value = agentflow.From(m[1])
			} else if m := function.FindStringSubmatch(s); m != nil {
				var err error
				if value, err = call(m[1], m[2]); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
			}
		}
		if value.From == "" && hasReference(decoded) {
			return fmt.Errorf("%s: a reference is the whole value, ${{ ... }}", name)
		}
		if out == nil {
			out = map[string]agentflow.Value{}
		}
		out[name] = value
		return nil
	})
	return out, err
}

// call reads latest(a, b) or optional(a, ...).
func call(name, args string) (agentflow.Value, error) {
	paths := strings.Split(args, ",")
	for i := range paths {
		paths[i] = strings.TrimSpace(paths[i])
		if paths[i] == "" || strings.ContainsAny(paths[i], " \t") {
			return agentflow.Value{}, fmt.Errorf("%s: want references separated by commas", name)
		}
	}
	var rest []string
	if len(paths) > 1 {
		rest = paths[1:]
	}
	if name == "optional" {
		return agentflow.Value{From: paths[0], Or: rest, Optional: true}, nil
	}
	if rest == nil {
		return agentflow.Value{}, errors.New("latest: one reference needs no latest")
	}
	return agentflow.Value{From: paths[0], Or: rest}, nil
}

func hasReference(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.Contains(v, "${{")
	case []any:
		return slices.ContainsFunc(v, hasReference)
	case map[string]any:
		for _, m := range v {
			if hasReference(m) {
				return true
			}
		}
	}
	return false
}

// objectSchema reads named fields as an object; none is no schema.
func objectSchema(n *yaml.Node) (*agenttool.Schema, error) {
	s := &agenttool.Schema{Type: "object"}
	err := pairs(n, func(name string, v *yaml.Node) error {
		field, required, err := fieldSchema(v)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		s.Properties = append(s.Properties, agenttool.Property{Name: name, Schema: field})
		if required {
			s.Required = append(s.Required, name)
		}
		return nil
	})
	if err != nil || len(s.Properties) == 0 {
		return nil, err
	}
	return s, nil
}

var types = []string{"string", "number", "integer", "boolean", "array", "object"}

func fieldSchema(n *yaml.Node) (s *agenttool.Schema, required bool, err error) {
	s = &agenttool.Schema{}
	err = pairs(n, func(key string, v *yaml.Node) error {
		switch key {
		case "type":
			if !slices.Contains(types, v.Value) {
				return fmt.Errorf("type %q: want one of %s", v.Value, strings.Join(types, ", "))
			}
			s.Type = v.Value
		case "required":
			return v.Decode(&required)
		case "description":
			return v.Decode(&s.Description)
		case "enum":
			return v.Decode(&s.Enum)
		case "items":
			item, _, err := fieldSchema(v)
			s.Items = item
			return err
		case "properties":
			object, err := objectSchema(v)
			if object != nil {
				s.Properties, s.Required = object.Properties, object.Required
			}
			return err
		default:
			return fmt.Errorf("unknown key %q", key)
		}
		return nil
	})
	return s, required, err
}
