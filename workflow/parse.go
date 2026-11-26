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
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

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
	Description string    `yaml:"description"`
	Inputs      yaml.Node `yaml:"inputs"`
	Outputs     yaml.Node `yaml:"outputs"`
	MaxDepth    int       `yaml:"max_depth"`
	MaxSteps    int       `yaml:"max_steps"`
	Model       string    `yaml:"model"`
	Thinking    string    `yaml:"thinking"`
	// Documents are the workflow's named markdown documents.
	Documents map[string]*documentSpec `yaml:"documents"`
	// OnAbort names the step the cleanup path starts at.
	OnAbort string `yaml:"on_abort"`
	// ExcludeTools are tools no model step is offered.
	ExcludeTools []string `yaml:"exclude_tools"`
	// Context fresh says every call runs the workflow in a child, and
	// Hidden that only a workflow that calls it runs it.
	Context string `yaml:"context"`
	Hidden  bool   `yaml:"hidden"`
}

// documentSpec is how a document starts: from a file of the skill, or as
// an input; empty with neither.
type documentSpec struct {
	From  string `yaml:"from"`
	Input string `yaml:"input"`
}

// stepDocuments are the documents a step works with: those a model step
// reads and edits, and those a script or tool step writes from members of
// its result.
type stepDocuments struct {
	Read  []string          `yaml:"read"`
	Edit  []string          `yaml:"edit"`
	Write map[string]string `yaml:"write"`
}

type settings struct {
	// Description is for people, and no model is sent it.
	Description string         `yaml:"description"`
	Run         string         `yaml:"run"`
	Flow        string         `yaml:"flow"`
	Tool        string         `yaml:"tool"`
	Args        map[string]any `yaml:"args"`
	With        yaml.Node      `yaml:"with"`
	Outputs     yaml.Node      `yaml:"outputs"`
	// Tools is a list of tools, or all for every tool the host offers.
	Tools        yaml.Node `yaml:"tools"`
	ExcludeTools []string  `yaml:"exclude_tools"`
	Skills       []string  `yaml:"skills"`
	Context      string    `yaml:"context"`
	// A child's own settings, with context: fresh. A script takes dir.
	Dir          yaml.Node `yaml:"dir"`
	Model        string    `yaml:"model"`
	Instructions string    `yaml:"instructions"`
	Child        string    `yaml:"child"`
	// A step's own settings.
	Thinking string      `yaml:"thinking"`
	Timeout  string      `yaml:"timeout"`
	Policy   *policySpec `yaml:"policy"`
	Judge    *struct {
		Criteria string      `yaml:"criteria"`
		Outcomes []string    `yaml:"outcomes"`
		Tools    []string    `yaml:"tools"`
		Returns  int         `yaml:"max_returns"`
		Policy   *policySpec `yaml:"policy"`
	} `yaml:"judge"`
	Outcomes   []string  `yaml:"outcomes"`
	Unattended yaml.Node `yaml:"unattended"`
	// MaxVisits is a number, or person for a loop a person drives.
	MaxVisits yaml.Node `yaml:"max_visits"`
	// The documents the step works with.
	Documents *stepDocuments `yaml:"documents"`
	// The files a model step is given as context.
	Files yaml.Node `yaml:"files"`
	// What a model sees of a script or tool step's result.
	Show yaml.Node `yaml:"show"`
	// A question's own settings.
	Choices   yaml.Node `yaml:"choices"`
	Text      yaml.Node `yaml:"text"`
	OnTimeout yaml.Node `yaml:"on_timeout"`
}

// strict decodes one YAML document, refusing keys it does not know.
func strict(src []byte, v any) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return err
	}
	if err := knownKeys(&doc, reflect.TypeOf(v)); err != nil {
		return err
	}
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
	var outputs yaml.Node
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
		if g.Inputs, g.InputDefaults, err = inputsSchema(&fm.Inputs); err != nil {
			return nil, fmt.Errorf("workflow: inputs: %w", err)
		}
		outputs, g.MaxDepth, g.MaxSteps, g.Model, g.Thinking = fm.Outputs, fm.MaxDepth, fm.MaxSteps, fm.Model, fm.Thinking
		g.Description, g.OnAbort, g.ExcludeTools = strings.TrimSpace(fm.Description), fm.OnAbort, fm.ExcludeTools
		g.Context, g.Hidden = fm.Context, fm.Hidden
		for name, d := range fm.Documents {
			if g.Documents == nil {
				g.Documents = map[string]agentflow.Document{}
			}
			if d == nil {
				d = &documentSpec{}
			}
			g.Documents[name] = agentflow.Document{From: d.From, Input: d.Input}
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
			return nil, fmt.Errorf("workflow: line %d: section %q is not a node of the diagram%s", s.line, s.id, agentflow.Suggest(s.id, slices.Collect(maps.Keys(index))))
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
		n := &g.Nodes[index[e.from]]
		outcomes := n.Outcomes
		switch {
		case len(outcomes) == 0 && n.Flow != "":
			// A call's outcomes are those of what it calls, known once
			// the workflows are linked.
		case len(outcomes) == 0:
			outcomes = []string{agentflow.OK}
			fallthrough
		default:
			if !slices.Contains(outcomes, e.outcome) {
				return nil, fmt.Errorf("workflow: node %q: edge for undeclared outcome %q%s", e.from, e.outcome, agentflow.Suggest(e.outcome, outcomes))
			}
		}
		// An end's name is the outcome the workflow ends with. One named
		// for its outcome says nothing more, unless it is the failure a
		// call routes on.
		if e.end != e.outcome || e.outcome == agentflow.Failed {
			if n.Ends == nil {
				n.Ends = map[string]string{}
			}
			n.Ends[e.outcome] = e.end
		}
	}
	if err := readOutputs(g, &outputs); err != nil {
		return nil, fmt.Errorf("workflow: outputs: %w", err)
	}
	if err := g.Validate(); err != nil {
		return nil, err
	}
	return g, nil
}

// ending is an edge into an end.
type ending struct{ from, outcome, end string }

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
		k := slices.IndexFunc(ends, func(e ending) bool { return e.from == from.ID && e.outcome == outcome })
		old, ok := from.Next[outcome]
		switch {
		case ok && old != to:
			return nil, fmt.Errorf("line %d: %q already goes to %q on %q", at, from.ID, old, outcome)
		case k >= 0 && (!isEnd[to] || ends[k].end != to):
			return nil, fmt.Errorf("line %d: %q already ends on %q", at, from.ID, outcome)
		case isEnd[to]:
			if k < 0 {
				ends = append(ends, ending{from.ID, outcome, to})
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
	n.Description = strings.TrimSpace(set.Description)

	n.Script, n.Tool, n.Outcomes, n.Flow, n.ExcludeTools = set.Run, set.Tool, set.Outcomes, set.Flow, set.ExcludeTools
	switch t := &set.Tools; {
	case t.IsZero():
	case t.Kind == yaml.ScalarNode && t.Value == "all":
		n.AllTools = true
	case t.Kind != yaml.SequenceNode || t.Decode(&n.Tools) != nil:
		return fmt.Errorf("line %d: tools: want a list of tools, or all for every tool the host offers", t.Line)
	}
	switch v := &set.MaxVisits; {
	case v.IsZero():
	case v.Kind == yaml.ScalarNode && v.Value == "person":
		n.PersonPaced = true
	case v.Kind != yaml.ScalarNode || v.Decode(&n.MaxVisits) != nil:
		return fmt.Errorf("line %d: max_visits: want a number from 1 to 99, or person for a loop a person drives", v.Line)
	}
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
	if n.Kind != agentflow.Run && (n.Script != "" || n.Tool != "" || n.Args != nil || n.Flow != "") {
		return errors.New("only a program node has run, tool, args or flow")
	}
	n.Context = set.Context
	n.Model, n.InstructionsFile, n.Child = set.Model, set.Instructions, set.Child
	if !set.Dir.IsZero() && set.Dir.Tag != "!!null" {
		d, err := pathValue(&set.Dir, "dir is a path or a reference to one")
		if err != nil {
			return fmt.Errorf("dir: %w", err)
		}
		n.Dir = d
	}
	n.Thinking, n.Policy = set.Thinking, set.Policy.policy()
	files, err := fileValues(&set.Files)
	if err != nil {
		return fmt.Errorf("files: %w", err)
	}
	n.Files = files
	if d := set.Documents; d != nil {
		n.Reads, n.Edits, n.Writes = d.Read, d.Edit, d.Write
	}
	if n.Show, err = show(&set.Show); err != nil {
		return err
	}
	if set.Timeout != "" {
		d, err := time.ParseDuration(set.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("timeout %q: want a duration such as 90s, 10m or 1h", set.Timeout)
		}
		n.Timeout = d
	}
	if j := set.Judge; j != nil {
		n.Judge = &agentflow.Judge{Criteria: strings.TrimSpace(j.Criteria), Outcomes: j.Outcomes, Tools: j.Tools, Returns: j.Returns, Policy: j.Policy.policy()}
	}
	if n.Kind != agentflow.Agent && (n.Tools != nil || n.AllTools || n.ExcludeTools != nil) {
		return errors.New("only a model node has tools or exclude_tools")
	}
	if set.Skills != nil && n.Kind != agentflow.Agent {
		return errors.New("only a model node names skills")
	}
	for _, name := range set.Skills {
		n.Skills = append(n.Skills, agentflow.StepSkill{Name: name})
	}
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
			return fmt.Errorf("outcome %q is not declared%s", outcome, agentflow.Suggest(outcome, n.Outcomes))
		}
		return nil
	}); err != nil {
		return fmt.Errorf("outputs: %w", err)
	}
	if len(n.Data) == 0 {
		n.Data = nil
	}
	if err := question(n, &set); err != nil {
		return err
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
			return unknownKey("unattended", key, "outcome", "data")
		})
	}
	return nil
}

// question reads a person node's choices, free text and timeout outcome.
func question(n *agentflow.Node, set *settings) error {
	only := func(key string) error { return fmt.Errorf("only a person node has %s", key) }
	if c := &set.Choices; !c.IsZero() {
		if n.Kind != agentflow.Person {
			return only("choices")
		}
		if c.Kind != yaml.MappingNode {
			return errors.New("choices: want from, and select, outcome or as")
		}
		n.Choices = &agentflow.Choices{}
		from := &yaml.Node{Kind: yaml.MappingNode}
		if err := pairs(c, func(key string, v *yaml.Node) error {
			switch key {
			case "from":
				from.Content = append(from.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "from"}, v)
				return nil
			case "select":
				var sel string
				if err := v.Decode(&sel); err != nil {
					return err
				}
				switch sel {
				case "one":
				case "many":
					n.Choices.Many = true
				default:
					return fmt.Errorf("choices: select %q: want one or many", sel)
				}
				return nil
			case "outcome":
				return v.Decode(&n.Choices.Outcome)
			case "as":
				return v.Decode(&n.Choices.As)
			}
			return unknownKey("choices", key, "from", "select", "outcome", "as")
		}); err != nil {
			return err
		}
		values, err := values(from)
		if err != nil {
			return fmt.Errorf("choices: %w", err)
		}
		n.Choices.From = values["from"]
		// A question with one outcome has only that one for a pick to be.
		if n.Choices.Outcome == "" && len(n.Outcomes) <= 1 {
			n.Choices.Outcome = agentflow.OK
			if len(n.Outcomes) == 1 {
				n.Choices.Outcome = n.Outcomes[0]
			}
		}
	}
	if t := &set.Text; !t.IsZero() {
		if n.Kind != agentflow.Person {
			return only("text")
		}
		n.Text = &agentflow.FreeText{}
		if t.Kind == yaml.ScalarNode {
			n.Text.Outcome = t.Value
		} else if err := pairs(t, func(key string, v *yaml.Node) error {
			switch key {
			case "outcome":
				return v.Decode(&n.Text.Outcome)
			case "as":
				return v.Decode(&n.Text.As)
			}
			return unknownKey("text", key, "outcome", "as")
		}); err != nil {
			return err
		}
	}
	switch u := &set.OnTimeout; {
	case u.IsZero():
	case n.Kind != agentflow.Person:
		return only("on_timeout")
	case u.Kind == yaml.ScalarNode:
		n.OnTimeout = u.Value
	default:
		return pairs(u, func(key string, v *yaml.Node) error {
			switch key {
			case "outcome":
				return v.Decode(&n.OnTimeout)
			case "data":
				return v.Decode(&n.OnTimeoutData)
			}
			return unknownKey("on_timeout", key, "outcome", "data")
		})
	}
	return nil
}

// readOutputs reads a workflow's outputs: by outcome, when every key is
// an outcome of the workflow and every value a mapping, and otherwise the
// same for every outcome.
func readOutputs(g *agentflow.Graph, n *yaml.Node) error {
	if n.IsZero() || n.Tag == "!!null" {
		return nil
	}
	outcomes := g.Outcomes()
	// A call's outcomes are not known until the workflows are linked, so
	// an outcome reached through one is taken on trust here and checked
	// then.
	calls := slices.ContainsFunc(g.Nodes, func(n agentflow.Node) bool { return n.Flow != "" && len(n.Outcomes) == 0 })
	byOutcome := n.Kind == yaml.MappingNode && len(n.Content) > 0
	for i := 0; byOutcome && i+1 < len(n.Content); i += 2 {
		key, v := n.Content[i].Value, n.Content[i+1]
		byOutcome = v.Kind == yaml.MappingNode && (slices.Contains(outcomes, key) || calls)
	}
	if !byOutcome {
		var err error
		g.Outputs, err = values(n)
		return err
	}
	g.OutputsBy = map[string]map[string]agentflow.Value{}
	return pairs(n, func(outcome string, v *yaml.Node) error {
		out, err := values(v)
		if err != nil {
			return fmt.Errorf("%s: %w", outcome, err)
		}
		if out == nil {
			out = map[string]agentflow.Value{}
		}
		g.OutputsBy[outcome] = out
		return nil
	})
}

// policySpec is a policy as a section writes it: allow and deny, each a
// list of rules in agentpolicy's grammar, a rule written alone or with
// the note that says why it is there.
type policySpec struct {
	Allow []ruleSpec `yaml:"allow"`
	Deny  []ruleSpec `yaml:"deny"`
}

// ruleSpec is "bash(go test:*)", or {rule: "bash(curl:*)", note: "use fetch"}.
type ruleSpec agentflow.PolicyRule

func (r *ruleSpec) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		r.Rule = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a rule is a string, or a mapping with rule and note", n.Line)
	}
	return pairs(n, func(key string, v *yaml.Node) error {
		var err error
		switch key {
		case "rule":
			return v.Decode(&r.Rule)
		case "note":
			return v.Decode(&r.Note)
		case "path":
			r.Path, err = pathValue(v, "a rule's path is a reference to a path, or a list of them")
			return err
		case "command":
			r.Command, err = pathValue(v, "a rule's command is a reference to a command, or a list of them")
			return err
		}
		return fmt.Errorf("line %d: a rule has no key %q%s", v.Line, key, agentflow.Suggest(key, []string{"rule", "note", "path", "command"}))
	})
}

func (p *policySpec) policy() *agentflow.Policy {
	if p == nil {
		return nil
	}
	out := &agentflow.Policy{}
	for _, r := range p.Allow {
		out.Allow = append(out.Allow, agentflow.PolicyRule(r))
	}
	for _, r := range p.Deny {
		out.Deny = append(out.Deny, agentflow.PolicyRule(r))
	}
	// An allow written empty is kept, so the load says it allows nothing.
	if p.Allow != nil && out.Allow == nil {
		out.Allow = []agentflow.PolicyRule{}
	}
	return out
}

// show reads what a model sees of a step's result: none, or a list of
// the members of its data.
func show(v *yaml.Node) (*agentflow.Show, error) {
	want := fmt.Errorf("line %d: show: want none, or a list of the members of the result a model sees", v.Line)
	switch {
	case v.IsZero():
		return nil, nil
	case v.Kind == yaml.ScalarNode && v.Value == "none":
		return &agentflow.Show{}, nil
	case v.Kind != yaml.SequenceNode || len(v.Content) == 0:
		return nil, want
	}
	s := &agentflow.Show{}
	for _, c := range v.Content {
		if c.Kind != yaml.ScalarNode || c.Value == "" {
			return nil, want
		}
		s.Members = append(s.Members, c.Value)
	}
	return s, nil
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

// inputsSchema reads a workflow's inputs: an object schema whose fields
// may each have a default, which only an input has.
func inputsSchema(n *yaml.Node) (*agenttool.Schema, map[string]any, error) {
	var defaults map[string]any
	var stripped []*yaml.Node
	err := pairs(n, func(name string, v *yaml.Node) error {
		field := v
		if v.Kind == yaml.MappingNode {
			field = &yaml.Node{Kind: yaml.MappingNode, Tag: v.Tag, Line: v.Line, Column: v.Column}
			for i := 0; i+1 < len(v.Content); i += 2 {
				if v.Content[i].Value != "default" {
					field.Content = append(field.Content, v.Content[i], v.Content[i+1])
					continue
				}
				var d any
				if err := v.Content[i+1].Decode(&d); err != nil {
					return fmt.Errorf("%s: default: %w", name, err)
				}
				if hasReference(d) {
					return fmt.Errorf("%s: a default is a literal", name)
				}
				if defaults == nil {
					defaults = map[string]any{}
				}
				defaults[name] = d
			}
		}
		stripped = append(stripped, &yaml.Node{Kind: yaml.ScalarNode, Value: name}, field)
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	s, err := objectSchema(&yaml.Node{Kind: yaml.MappingNode, Content: stripped})
	return s, defaults, err
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
		case "default":
			return errors.New("only a workflow's input has a default")
		default:
			return unknownKey("", key, "type", "required", "description", "enum", "items", "properties")
		}
		return nil
	})
	return s, required, err
}

// fileValues reads a model step's files: each a path, or a reference to
// one or to a list of them.
func fileValues(n *yaml.Node) ([]agentflow.Value, error) {
	if n.IsZero() || n.Tag == "!!null" {
		return nil, nil
	}
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: want a list of paths", n.Line)
	}
	var out []agentflow.Value
	for _, item := range n.Content {
		v, err := pathValue(item, "a file is a path or a reference to one")
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// pathValue reads one path: a literal, or a reference that is the whole
// value. what says what it must be, for the error.
func pathValue(item *yaml.Node, what string) (agentflow.Value, error) {
	var s string
	if item.Kind != yaml.ScalarNode || item.Decode(&s) != nil || s == "" {
		return agentflow.Value{}, fmt.Errorf("line %d: %s", item.Line, what)
	}
	if m := reference.FindStringSubmatch(s); m != nil {
		return agentflow.From(m[1]), nil
	}
	if m := function.FindStringSubmatch(s); m != nil {
		v, err := call(m[1], m[2])
		if err != nil {
			return agentflow.Value{}, fmt.Errorf("line %d: %w", item.Line, err)
		}
		return v, nil
	}
	if hasReference(s) {
		return agentflow.Value{}, fmt.Errorf("line %d: a reference is the whole value, ${{ ... }}", item.Line)
	}
	return agentflow.Literal(s), nil
}
