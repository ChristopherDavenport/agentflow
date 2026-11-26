package agentflow

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// RecordFormat versions what a workflow writes into a transcript and how
// it is read. flow_start carries it, and a reader that does not know the
// one it finds says so rather than guess.
const RecordFormat = 1

// The statuses of an [Instance]. All but running are written down, by
// flow_end, so that having left a workflow is not something to infer.
const (
	Running   = "running"
	Done      = "done"
	Failed    = "failed"
	Abandoned = "abandoned"
)

// PathStep is a finished step as flow_end lists it.
type PathStep struct {
	Step    string `json:"step"`
	Visit   int    `json:"visit"`
	Outcome string `json:"outcome"`
	By      string `json:"by,omitempty"`
}

// Ending is flow_end's arguments: that an instance is over, how, and the
// way it went, which a reader's own replay can be held to.
type Ending struct {
	Instance string         `json:"instance"`
	Status   string         `json:"status"`
	Reason   string         `json:"reason,omitempty"`
	Path     []PathStep     `json:"path,omitempty"`
	Outputs  map[string]any `json:"outputs,omitempty"`
}

func pathOf(steps []Step) []PathStep {
	var out []PathStep
	for _, s := range steps {
		out = append(out, PathStep{s.Node, s.Visit, s.Result.Outcome, s.Result.By})
	}
	return out
}

// ending is how the instance st stands would be ended: what it came to,
// or abandoned if it came to nothing.
func ending(st State, failed error) *Ending {
	e := &Ending{Instance: st.Instance, Status: Abandoned, Path: pathOf(st.Steps)}
	switch {
	case failed != nil:
		e.Status, e.Reason = Failed, failed.Error()
	case st.Done:
		e.Status, e.Outputs = Done, st.Outputs
	}
	return e
}

func (g *Graph) endID(instance string) string { return g.idPrefix() + instance + "_end" }

// Instance is one run of a workflow, as a transcript tells it. It needs
// nothing but the transcript: the definition is the one the run started
// with, however the workflow has changed since.
type Instance struct {
	Workflow, ID, Digest string
	Graph                *Graph
	Inputs               any
	// Start is the item that called flow_start, and End the item that
	// recorded the ending, -1 while there is none.
	Start, End int
	Status     string
	Reason     string
	// State is where the run got to.
	State State
	// Problem says the record cannot be read as it claims to be: an
	// unknown format, a definition that does not match its digest, or a
	// replay that disagrees with the recorded ending.
	Problem string

	// stop is the first item that is no longer the run's.
	stop int
}

// Instances lists the workflow runs of a transcript, in the order they
// started.
func Instances(t agentturn.Transcript) ([]Instance, error) {
	calls := map[string]int{}
	var out []Instance
	for i, item := range t {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			calls[it.CallID] = i
		case *openresponses.FunctionCallOutput:
			at, ok := calls[it.CallID]
			if !ok {
				continue
			}
			c := t[at].(*openresponses.FunctionCall)
			if c.Name != startTool || !strings.HasPrefix(c.CallID, reserved) {
				continue
			}
			in, err := instance(t, at, c, it)
			if err != nil {
				return nil, fmt.Errorf("agentflow: item %d: %w", at, err)
			}
			out = append(out, in)
		}
	}
	return out, nil
}

// At is where the run stood once item i of t had happened. Items before
// the run or after it give where it began or ended.
func (in Instance) At(t agentturn.Transcript, i int) (State, error) {
	if in.Graph == nil {
		return State{}, errors.New("agentflow: no state to read: " + cmp.Or(in.Problem, "the run never started: "+in.Reason))
	}
	upto := min(max(i+1, in.Start), in.stop, len(t))
	var run agentturn.Transcript
	for _, item := range t[in.Start:upto] {
		// A request for the next run is not part of this one.
		if m, ok := item.(*openresponses.Message); ok && m.Role == openresponses.RoleDeveloper {
			if _, invoked := in.Graph.invoked(m.Text()); invoked {
				continue
			}
		}
		run = append(run, item)
	}
	return in.Graph.Replay(run)
}

func instance(t agentturn.Transcript, at int, c *openresponses.FunctionCall, out *openresponses.FunctionCallOutput) (Instance, error) {
	var p struct {
		Workflow   string          `json:"workflow"`
		Digest     string          `json:"digest"`
		Inputs     any             `json:"inputs"`
		Format     int             `json:"format"`
		Definition json.RawMessage `json:"definition"`
	}
	if err := decode([]byte(c.Arguments), &p); err != nil {
		return Instance{}, fmt.Errorf("flow_start: %w", err)
	}
	in := Instance{Workflow: p.Workflow, Digest: p.Digest, Inputs: p.Inputs, Start: at, End: -1, Status: Running, stop: len(t)}
	var res Result
	if json.Unmarshal([]byte(out.Output.String()), &res) != nil || res.Outcome != OK {
		in.Status, in.Reason, in.stop = Failed, out.Output.String(), at
		return in, nil
	}
	if p.Format != RecordFormat {
		in.Problem = fmt.Sprintf("record format %d, and this reads %d", p.Format, RecordFormat)
		return in, nil
	}
	g, err := decodeGraph(p.Definition)
	if err != nil {
		in.Problem = "definition: " + err.Error()
		return in, nil
	}
	if g.Digest() != p.Digest {
		in.Problem = "the definition does not match its digest"
		return in, nil
	}
	in.Graph = g
	in.ID = strings.TrimSuffix(strings.TrimPrefix(c.CallID, g.idPrefix()), "_start")

	// The run stops where another is asked for, and ends where flow_end
	// says, which for an abandoned run is after that.
	var end *Ending
	for i := at + 1; i < len(t) && end == nil; i++ {
		switch it := t[i].(type) {
		case *openresponses.Message:
			if it.Role == openresponses.RoleDeveloper {
				if _, invoked := g.invoked(it.Text()); invoked {
					in.stop = min(in.stop, i)
				}
			}
		case *openresponses.FunctionCall:
			if it.Name == startTool && strings.HasPrefix(it.CallID, g.idPrefix()) {
				in.stop = min(in.stop, i)
			}
			if it.Name == endTool && it.CallID == g.endID(in.ID) {
				var e Ending
				if err := decode([]byte(it.Arguments), &e); err != nil {
					return in, fmt.Errorf("flow_end: %w", err)
				}
				end, in.End = &e, i
				in.stop = min(in.stop, i)
			}
		}
	}
	st, replayErr := in.At(t, in.stop-1)
	in.State = st
	if end == nil {
		return in, nil
	}
	in.Status, in.Reason = end.Status, end.Reason
	switch {
	case !slices.Equal(pathOf(st.Steps), end.Path):
		in.Problem = "the replayed path is not the one recorded at the end"
	case end.Status == Done && !st.Done:
		in.Problem = "recorded as done, and the replay is not"
	case end.Status != Done && st.Done:
		in.Problem = "recorded as " + end.Status + ", and the replay is done"
	case end.Status != Failed && replayErr != nil:
		in.Problem = replayErr.Error()
	}
	return in, nil
}

// decodeGraph reads a graph back from the JSON its digest is taken of.
func decodeGraph(raw []byte) (*Graph, error) {
	type node struct {
		Node
		Data map[string]json.RawMessage
	}
	var def struct {
		Graph
		Nodes  []node
		Inputs json.RawMessage
	}
	if err := decode(raw, &def); err != nil {
		return nil, err
	}
	g := def.Graph
	var err error
	if g.Inputs, err = decodeSchema(def.Inputs); err != nil {
		return nil, err
	}
	for _, n := range def.Nodes {
		if n.Data != nil {
			n.Node.Data = map[string]*agenttool.Schema{}
			for outcome, s := range n.Data {
				if n.Node.Data[outcome], err = decodeSchema(s); err != nil {
					return nil, err
				}
			}
		}
		if string(n.Node.Args) == "null" {
			n.Node.Args = nil
		}
		g.Nodes = append(g.Nodes, n.Node)
	}
	return &g, g.Validate()
}

// decodeSchema reads a schema back as agenttool writes one, which it
// does not itself offer. Properties keep their order.
func decodeSchema(raw []byte) (*agenttool.Schema, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var w struct {
		Type                 any             `json:"type"`
		Description          string          `json:"description"`
		Format               string          `json:"format"`
		Enum                 []any           `json:"enum"`
		Properties           json.RawMessage `json:"properties"`
		Required             []string        `json:"required"`
		Items                json.RawMessage `json:"items"`
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	}
	if err := decode(raw, &w); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	s := &agenttool.Schema{Description: w.Description, Format: w.Format, Enum: w.Enum, Required: w.Required}
	switch t := w.Type.(type) {
	case string:
		s.Type = t
	case []any:
		if len(t) == 2 {
			s.Type, _ = t[0].(string)
			s.Nullable = true
		}
	}
	var err error
	if s.Items, err = decodeSchema(w.Items); err != nil {
		return nil, err
	}
	switch string(w.AdditionalProperties) {
	case "false":
		s.NoAdditional = true
	default:
		if s.AdditionalProperties, err = decodeSchema(w.AdditionalProperties); err != nil {
			return nil, err
		}
	}
	if w.Properties == nil {
		return s, nil
	}
	s.Properties = []agenttool.Property{}
	d := json.NewDecoder(bytes.NewReader(w.Properties))
	if _, err := d.Token(); err != nil {
		return nil, fmt.Errorf("schema: properties: %w", err)
	}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, fmt.Errorf("schema: properties: %w", err)
		}
		var member json.RawMessage
		if err := d.Decode(&member); err != nil {
			return nil, fmt.Errorf("schema: properties: %w", err)
		}
		p := agenttool.Property{Name: fmt.Sprint(key)}
		if p.Schema, err = decodeSchema(member); err != nil {
			return nil, err
		}
		s.Properties = append(s.Properties, p)
	}
	return s, nil
}
