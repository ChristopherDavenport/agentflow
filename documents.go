package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	slashpath "path"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
)

// Document is one of a workflow's named markdown documents. Its text
// starts as the file From of the workflow's skill, which loading reads
// into Text so the digest pins it, or as the input Input; with neither it
// starts empty. Each write is a call whose result holds the whole new
// text, so what a document says at any point is read from the record.
type Document struct {
	From  string `json:",omitempty"`
	Input string `json:",omitempty"`
	Text  string `json:",omitempty"`
}

// MaxDocument bounds a document's text. A write over it is refused, not
// cut.
const MaxDocument = 1 << 20

const (
	docReadTool  = "flow_doc_read"
	docWriteTool = "flow_doc_write"
)

// docRead is flow_doc_read's arguments, and docWrite flow_doc_write's: the
// run and step the model works on, and the document. A write carries the
// document's whole new text.
type docRead struct {
	Run  string `json:"run"`
	Step string `json:"step"`
	Name string `json:"name"`
}

type docWrite struct {
	Run  string `json:"run"`
	Step string `json:"step"`
	Name string `json:"name"`
	Text string `json:"text"`
}

// documentNames are the workflow's documents, sorted.
func (g *Graph) documentNames() []string { return slices.Sorted(maps.Keys(g.Documents)) }

// reads says a model step may read the document name, and edits that it
// may write it, which reading comes with.
func (n *Node) reads(name string) bool { return slices.Contains(n.Reads, name) || n.edits(name) }
func (n *Node) edits(name string) bool { return slices.Contains(n.Edits, name) }

// documented says a model step works with documents.
func (n *Node) documented() bool { return len(n.Reads)+len(n.Edits) > 0 }

// checkDocuments holds the documents a workflow declares, and the steps
// that read and write them, to what can be.
func (g *Graph) checkDocuments() []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("agentflow: "+format, args...)) }
	names := g.documentNames()
	for _, name := range names {
		d := g.Documents[name]
		switch {
		case !nodeID.MatchString(name):
			bad("document %q: want 1 to 20 letters, digits, _ or -", name)
		case d.From != "" && d.Input != "":
			bad("document %q starts from a file or from an input, not both", name)
		case d.From != "" && (slashpath.IsAbs(d.From) || slashpath.Clean(d.From) != d.From || strings.HasPrefix(d.From, "../") || d.From == ".."):
			bad("document %q: from %q: want a clean path within the skill", name, d.From)
		case d.Input != "" && (g.Inputs == nil || !declares(g.Inputs, d.Input)):
			bad("document %q starts as input %q, which is not an input of the workflow%s", name, d.Input, suggest(d.Input, memberNames(g.Inputs)))
		case d.Input != "":
			if s := member(g.Inputs, []string{d.Input}); s != nil && s.Type != "" && s.Type != "string" {
				bad("document %q starts as input %q, which is not text", name, d.Input)
			}
		}
		if len(d.Text) > MaxDocument {
			bad("document %q is over %d bytes", name, MaxDocument)
		}
	}
	for i := range g.Nodes {
		n := &g.Nodes[i]
		for _, list := range [][]string{n.Reads, n.Edits} {
			for _, name := range list {
				switch {
				case n.Kind != Agent || n.Verdict:
					bad("node %q: only a model step reads or edits documents", n.ID)
				case g.Documents == nil || !slices.Contains(names, name):
					bad("node %q: no document %q%s", n.ID, name, suggest(name, names))
				}
			}
		}
		for _, name := range slices.Sorted(maps.Keys(n.Writes)) {
			field := n.Writes[name]
			switch {
			case n.Kind != Run || n.Flow != "":
				bad("node %q: only a script or tool step writes documents from its result", n.ID)
			case !slices.Contains(names, name):
				bad("node %q: no document %q%s", n.ID, name, suggest(name, names))
			case field == "":
				bad("node %q: document %q: name the member of the result that holds its text", n.ID, name)
			case len(n.schemas()) > 0 && g.possible(ref{step: n.ID, path: []string{field}}) != nil:
				bad("node %q: document %q: %v", n.ID, name, g.possible(ref{step: n.ID, path: []string{field}}))
			default:
				if s := g.refSchema("steps." + n.ID + ".outputs." + field); s != nil && s.Type != "" && s.Type != "string" {
					bad("node %q: document %q: member %q is not text", n.ID, name, field)
				}
			}
		}
	}
	return errs
}

// initialDocuments is what g's documents say as a run starts: each one's
// own first text or input, or for a run in a child, what its call
// passed in.
func initialDocuments(g *Graph, inputs any, passed map[string]string) (map[string]string, map[string]int) {
	if len(g.Documents) == 0 {
		return nil, nil
	}
	in, _ := inputs.(map[string]any)
	out := map[string]string{}
	for name, d := range g.Documents {
		text := d.Text
		if s, ok := in[d.Input].(string); d.Input != "" && ok {
			text = s
		}
		if s, ok := passed[name]; ok {
			text = s
		}
		out[name] = text
	}
	return out, map[string]int{}
}

// write sets a document's text at the current point of the run.
func (st *State) write(name, text string) {
	st.Documents = maps.Clone(st.Documents)
	st.Documents[name] = text
	if st.docAt == nil {
		st.docAt = map[string]int{}
	}
	st.docAt[name] = len(st.Steps) + 1
}

// docResult reads the text a document call's result holds.
func docResult(output string) (name, text string, ok bool) {
	var res struct {
		Outcome string `json:"outcome"`
		Data    struct {
			Name string  `json:"name"`
			Text *string `json:"text"`
		} `json:"data"`
	}
	if decode([]byte(output), &res) != nil || res.Outcome != OK || res.Data.Text == nil {
		return "", "", false
	}
	return res.Data.Name, *res.Data.Text, true
}

// written applies the documents a program step's result writes, or says
// why it cannot.
func (n *Node) written(st *State, res Result) error {
	if len(n.Writes) == 0 {
		return nil
	}
	data, _ := res.Data.(map[string]any)
	for _, name := range slices.Sorted(maps.Keys(n.Writes)) {
		field := n.Writes[name]
		text, ok := data[field].(string)
		switch {
		case !ok:
			return fmt.Errorf("document %q: the result's member %q is not text", name, field)
		case len(text) > MaxDocument:
			return fmt.Errorf("document %q: over %d bytes", name, MaxDocument)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(n.Writes)) {
		st.write(name, data[n.Writes[name]].(string))
	}
	return nil
}

// passedDocuments are the texts of the documents a fresh step reads or
// edits, which its call passes into the child.
func (st State) passedDocuments(n *Node) map[string]string {
	if !n.documented() {
		return nil
	}
	out := map[string]string{}
	for name, text := range st.Documents {
		if n.reads(name) {
			out[name] = text
		}
	}
	return out
}

// editedDocuments are the texts of the documents a step may edit, as its
// run in a child left them, for the caller to take back.
func editedDocuments(n *Node, docs map[string]string) map[string]string {
	if len(n.Edits) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, name := range n.Edits {
		if text, ok := docs[name]; ok {
			out[name] = text
		}
	}
	return out
}

// documentsText tells a model step which documents it may read and edit,
// and how.
func documentsText(b *strings.Builder, st State) {
	n := st.Node
	if !n.documented() {
		return
	}
	var reads []string
	for _, name := range st.g.documentNames() {
		if n.reads(name) && !n.edits(name) {
			reads = append(reads, name)
		}
	}
	b.WriteString("\n\nDocuments for this step:")
	if len(reads) > 0 {
		fmt.Fprintf(b, " you may read %s", strings.Join(reads, ", "))
	}
	if len(n.Edits) > 0 {
		if len(reads) > 0 {
			b.WriteString(", and")
		}
		fmt.Fprintf(b, " you may read and edit %s", strings.Join(n.Edits, ", "))
	}
	fmt.Fprintf(b, ". Read one with %s, giving its name.", docReadTool)
	if len(n.Edits) > 0 {
		fmt.Fprintf(b, " Write one with %s, giving its name and its whole new text, in markdown.", docWriteTool)
	}
	b.WriteString(" A document is not a file; no other tool reaches it.")
}

// checkDocCall holds a model's document call to what its step allows.
func checkDocCall(st State, name string, args json.RawMessage) error {
	n := st.Node
	var a docRead
	if err := decode(args, &a); err != nil {
		return errors.New("the call is not readable")
	}
	switch {
	case a.Step != n.ID:
		return fmt.Errorf("you are on step %q, not %q", n.ID, a.Step)
	case a.Run != st.Instance:
		return fmt.Errorf("you are on step %q of run %q, not of run %q", n.ID, st.Instance, a.Run)
	case name == docReadTool && !n.reads(a.Name):
		return fmt.Errorf("step %q does not read the document %q%s", n.ID, a.Name, suggest(a.Name, append(slices.Clone(n.Reads), n.Edits...)))
	case name == docWriteTool && !n.edits(a.Name):
		return fmt.Errorf("step %q does not edit the document %q%s", n.ID, a.Name, suggest(a.Name, n.Edits))
	}
	return nil
}

// docTools are the tools a model step reads and writes documents with.
// Their results hold the text, so the record holds every version.
func (f *flow) docTools() []agenttool.Tool {
	read := agenttool.New(docReadTool, "Read one of the workflow's documents.", func(ctx context.Context, a docRead) (Result, error) {
		if err := exact(ctx, &a); err != nil {
			return Result{}, err
		}
		st, err := f.current(ctx)
		if err != nil {
			return Result{}, err
		}
		if st.Node == nil || st.Node.Kind != Agent {
			return Result{}, errors.New("agentflow: no step reads documents now")
		}
		raw, _ := json.Marshal(a)
		if err := checkDocCall(st, docReadTool, raw); err != nil {
			return Result{}, fmt.Errorf("agentflow: %w", err)
		}
		return Result{Outcome: OK, Data: map[string]any{"name": a.Name, "text": st.Documents[a.Name]}}, nil
	})
	write := agenttool.New(docWriteTool, "Write the whole text of one of the workflow's documents.", func(ctx context.Context, a docWrite) (Result, error) {
		if err := exact(ctx, &a); err != nil {
			return Result{}, err
		}
		st, err := f.current(ctx)
		if err != nil {
			return Result{}, err
		}
		if st.Node == nil || st.Node.Kind != Agent {
			return Result{}, errors.New("agentflow: no step edits documents now")
		}
		raw, _ := json.Marshal(a)
		if err := checkDocCall(st, docWriteTool, raw); err != nil {
			return Result{}, fmt.Errorf("agentflow: %w", err)
		}
		if len(a.Text) > MaxDocument {
			return Result{}, fmt.Errorf("agentflow: the text is over %d bytes", MaxDocument)
		}
		return Result{Outcome: OK, Data: map[string]any{"name": a.Name, "text": a.Text}}, nil
	})
	return []agenttool.Tool{read, write}
}

// docSchemas are the parameters a step's request offers the document
// tools with: its run, its step, and the names it may use.
func docSchemas(st State) (read, write json.RawMessage, err error) {
	n := st.Node
	readable := append(slices.Clone(n.Reads), n.Edits...)
	slices.Sort(readable)
	readable = slices.Compact(readable)
	props := func(names []string, text bool) map[string]any {
		p := map[string]any{
			"run":  map[string]any{"type": "string", "enum": []string{st.Instance}},
			"step": map[string]any{"type": "string", "enum": []string{n.ID}},
			"name": map[string]any{"type": "string", "enum": names},
		}
		if text {
			p["text"] = map[string]any{"type": "string", "description": "The document's whole new text, in markdown."}
		}
		return p
	}
	if read, err = json.Marshal(map[string]any{"type": "object", "required": []string{"run", "step", "name"}, "properties": props(readable, false)}); err != nil {
		return nil, nil, err
	}
	if len(n.Edits) > 0 {
		write, err = json.Marshal(map[string]any{"type": "object", "required": []string{"run", "step", "name", "text"}, "properties": props(n.Edits, true)})
	}
	return read, write, err
}
