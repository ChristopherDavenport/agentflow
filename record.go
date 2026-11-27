package agentflow

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// RecordFormat versions what a workflow writes into a transcript and how
// it is read. flow_start carries it, and a reader that does not know the
// one it finds says so rather than guess. Every run is keyed, and every
// call to a workflow runs in a child session, so a session holds no run
// another of its runs called.
const RecordFormat = 4

// The statuses of an [Instance]. All but running are written down, by
// flow_end, so that having left a workflow is not something to infer.
// Failed is also the outcome a call ends with when what it called
// failed.
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

// Ending is flow_end's arguments: that a run is over, how, and the way
// it went, which a reader's own replay can be held to.
type Ending struct {
	// Instance is the run's key.
	Instance string `json:"instance"`
	Status   string `json:"status"`
	// Outcome is the end a done run reached.
	Outcome string         `json:"outcome,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Path    []PathStep     `json:"path,omitempty"`
	Outputs map[string]any `json:"outputs,omitempty"`
	// Step is the step a failed run was at.
	Step string `json:"step,omitempty"`
	// Limit names the bound across the tree of runs the run failed on,
	// such as depth, so every run it is nested in fails with it.
	Limit string `json:"limit,omitempty"`
	// Cleanup says how an abandoned run's cleanup path went: done, or why
	// it stopped. It is empty for a run with none.
	Cleanup string `json:"cleanup,omitempty"`

	// id is the ID of the call that writes it.
	id string
}

func pathOf(steps []Step) []PathStep {
	var out []PathStep
	for _, s := range steps {
		out = append(out, PathStep{s.Node, s.Visit, s.Result.Outcome, s.Result.By})
	}
	return out
}

// ending is how the run st stands would be ended: what it came to, or
// abandoned if it came to nothing.
func ending(st State, failed error) *Ending {
	e := &Ending{Instance: st.Instance, Status: Abandoned, Path: pathOf(st.Steps), Cleanup: st.Cleanup, id: st.endID()}
	if st.Done {
		e.Outcome = st.Outcome
	}
	switch {
	case failed != nil:
		e.Status, e.Reason, e.Limit = Failed, failed.Error(), st.limit
		if st.Node != nil {
			e.Step = st.Node.ID
		}
	case st.Done:
		e.Status, e.Outputs = Done, st.Outputs
	default:
		e.Reason = st.abandonedFor
	}
	return e
}

// Instance is one run of a workflow, as a transcript tells it, with the runs
// it called. It needs nothing but the transcript: the definitions are
// those the run's root pinned, however the workflows have changed since.
type Instance struct {
	Workflow, ID, Digest string
	Graph                *Graph
	Inputs               any
	// Parent and Caller are the key of the run that called this one and
	// the ID of its call, empty for a run nobody called. Depth counts
	// the runs it is nested in.
	Parent, Caller string
	Depth          int
	// Spawn names the child session a run called into one works in, and
	// Fresh says that session began with nothing of its caller's.
	Spawn string
	Fresh bool
	// Start is the item that called flow_start, and End the item that
	// recorded the ending, -1 while there is none. Stop is the first item
	// past the run's span: where the session's next run starts, or the
	// transcript's length. They index the transcript the run was read
	// from, the child session's for a run called into one, and a run
	// read only from its call's result has none: Start and End are -1.
	Start, End, Stop int
	// Started and Ended are when the run's start and its end were
	// written, as their results say, zero where the record has none.
	Started, Ended time.Time
	Status         string
	Outcome        string
	Reason         string
	// State is where the run got to.
	State State
	// Problem says the record cannot be read as it claims to be: an
	// unknown format, a definition that does not match its digest, or a
	// replay that disagrees with the recorded ending.
	Problem string
	// Children are the runs it called, in the order they started.
	Children []*Instance

	// root's graph replays from from up to stop, the first item no
	// longer the root's.
	root       *Graph
	from, stop int
}

// Runs lists the runs of a transcript nobody called there, in the order
// they started, each with the runs it called. A run in a child session
// is one nobody called there, and names its caller. open reads the items
// of the child session a spawn ID names, as every item of its path; with
// it, a run called into a child is read from there, and without it, it
// is listed with what its call's result says.
func Runs(t agentturn.Transcript, open func(spawn string) (agentturn.Transcript, error)) ([]*Instance, error) {
	calls := map[string]int{}
	var out []*Instance
	started, made := false, false
	for i, item := range t {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			calls[it.CallID] = i
			if strings.HasPrefix(it.CallID, reserved) {
				started = started || it.Name == startTool
				made = true
			}
		case *openresponses.FunctionCallOutput:
			at, ok := calls[it.CallID]
			if !ok {
				continue
			}
			c := t[at].(*openresponses.FunctionCall)
			if c.Name != startTool || !strings.HasPrefix(c.CallID, reserved) {
				continue
			}
			run, err := rootRun(t, at, c, it, open)
			if err != nil {
				return nil, fmt.Errorf("agentflow: item %d: %w", at, err)
			}
			if run != nil {
				out = append(out, run)
			}
		}
	}
	if made && !started {
		return nil, fmt.Errorf("%w: calls of a workflow and no flow_start; read every item of the session's path, not the context rebuilt from it", ErrPositionLost)
	}
	return out, nil
}

// At is where the run stood once item i of t had happened. Items before
// the run or after it give where it began or ended.
func (run *Instance) At(t agentturn.Transcript, i int) (State, error) {
	if run.root == nil {
		return State{}, errors.New("agentflow: no state to read: " + cmp.Or(run.Problem, "the run never started: "+run.Reason))
	}
	r := run.root.replay(spanOf(t, run.from, min(max(i+1, run.from), run.stop, len(t))))
	for _, f := range r.frames {
		if f.st.Instance == run.ID {
			st := r.state(f)
			return st, f.failed
		}
	}
	// Not yet started: where it was about to begin.
	if run.Parent == "" {
		return r.state(r.root), nil
	}
	return State{Workflow: run.Workflow, Instance: run.ID, Parent: run.Parent, Caller: run.Caller, Depth: run.Depth}, nil
}

// spanOf is t's items from from to to. An invocation in it abandons the
// run, whose cleanup and end come after it; the next run is not part of
// the span, which stops at its start.
func spanOf(t agentturn.Transcript, from, to int) agentturn.Transcript {
	return slices.Clone(t[from:to])
}

// rootRun reads the run nobody called whose flow_start is item at, nil
// for a call's start.
func rootRun(t agentturn.Transcript, at int, c *openresponses.FunctionCall, out *openresponses.FunctionCallOutput, open func(string) (agentturn.Transcript, error)) (*Instance, error) {
	var p startPayload
	if err := decode([]byte(c.Arguments), &p); err != nil {
		return nil, fmt.Errorf("flow_start: %w", err)
	}
	run := &Instance{Workflow: p.Workflow, Digest: p.Digest, Inputs: p.Inputs, Start: at, End: -1, Stop: len(t), Status: Running, from: at, stop: len(t),
		ID: p.Run, Parent: p.Parent, Caller: p.Caller, Depth: p.Depth, Fresh: p.Context == ContextFresh}
	var res Result
	if json.Unmarshal([]byte(out.Output.String()), &res) != nil || res.Outcome != OK {
		run.Status, run.Reason = Failed, out.Output.String()
		return run, nil
	}
	if p.Format != RecordFormat {
		run.Problem = fmt.Sprintf("record format %d, and this reads %d", p.Format, RecordFormat)
		return run, nil
	}
	g, err := pinnedGraph(p)
	if err != nil {
		run.Problem = err.Error()
		return run, nil
	}
	run.Graph, run.root = g, g
	run.Started = stampOf(out.Output.String())
	// The run stops where another of its workflow starts. Where another
	// is asked for, the run is abandoned: its cleanup and its ending, and
	// its calls', come after that, before the next start.
	ends := map[string]int{}
	for i := at + 1; i < len(t); i++ {
		switch it := t[i].(type) {
		case *openresponses.FunctionCall:
			if it.Name == startTool && strings.HasPrefix(it.CallID, reserved) && i < run.stop {
				var q startPayload
				if decode([]byte(it.Arguments), &q) == nil && q.Workflow == g.Name {
					run.stop = i
				}
			}
			if it.Name == endTool && strings.HasPrefix(it.CallID, reserved) {
				if _, seen := ends[it.CallID]; !seen {
					ends[it.CallID] = i
				}
			}
		}
	}
	r := g.replay(spanOf(t, at, run.stop))
	for _, f := range r.frames {
		if k, ok := ends[f.st.endID()]; ok && f.ending == nil {
			var e Ending
			if err := decode([]byte(t[k].(*openresponses.FunctionCall).Arguments), &e); err != nil {
				return run, fmt.Errorf("flow_end: %w", err)
			}
			f.ending, f.end = &e, k-at
		}
		in := run
		in.State = r.state(f)
		judge(in, f)
		for _, fc := range f.children {
			kid, err := childRun(t, at, fc, in, open)
			if err != nil {
				return run, err
			}
			in.Children = append(in.Children, kid)
		}
	}
	run.Stop = run.stop
	if len(r.frames) > 0 && r.frames[0].end >= 0 {
		run.End = at + r.frames[0].end
		id := t[run.End].(*openresponses.FunctionCall).CallID
		for i := run.End + 1; i < len(t); i++ {
			if o, ok := t[i].(*openresponses.FunctionCallOutput); ok && o.CallID == id {
				run.Ended = stampOf(o.Output.String())
				break
			}
		}
	}
	return run, nil
}

// childRun is the run a call of in made into a child session: read from
// the session, or without open, what the call's result says of it.
func childRun(t agentturn.Transcript, at int, fc freshCall, in *Instance, open func(string) (agentturn.Transcript, error)) (*Instance, error) {
	if open != nil {
		ct, err := open(fc.spawn)
		if err != nil {
			return nil, fmt.Errorf("agentflow: child session %s: %w", fc.spawn, err)
		}
		runs, err := Runs(ct, open)
		if err != nil {
			return nil, fmt.Errorf("agentflow: child session %s: %w", fc.spawn, err)
		}
		for _, r := range runs {
			if r.ID == fc.run {
				r.Spawn = fc.spawn
				return r, nil
			}
		}
	}
	kid := &Instance{Workflow: fc.workflow, ID: fc.run, Parent: in.ID, Caller: fc.call, Depth: in.Depth + 1, Spawn: fc.spawn, Fresh: fc.fresh,
		Start: -1, End: -1, Status: Running}
	for i := at; i < len(t); i++ {
		o, ok := t[i].(*openresponses.FunctionCallOutput)
		if !ok || o.CallID != fc.call {
			continue
		}
		// What the call's result says of the run: how it came out, and
		// what its tree spent of the steps.
		var said struct {
			Outcome string `json:"outcome"`
			Failure string `json:"failure"`
			Limit   string `json:"limit"`
		}
		_ = decode([]byte(o.Output.String()), &said)
		kid.State.Entries, kid.State.Granted = spent(o.Output.String())
		switch {
		case said.Failure == abandonedText || said.Failure == overriddenText:
			// Set aside as the runs were abandoned, or for an override.
			kid.Status, kid.Reason = Abandoned, said.Failure
		case said.Failure != "" || said.Limit != "" || said.Outcome == Failed || said.Outcome == "":
			kid.Status, kid.Reason = Failed, cmp.Or(said.Failure, o.Output.String())
		default:
			kid.Status, kid.Outcome = Done, said.Outcome
		}
		break
	}
	if open == nil {
		return kid, nil
	}
	kid.Problem = "the child session holds no such run"
	return kid, nil
}

// judge sets a run's status from its ending, and holds the replay to it.
func judge(in *Instance, f *frame) {
	st, e := f.st, f.ending
	if e == nil {
		return
	}
	in.Status, in.Reason, in.Outcome = e.Status, e.Reason, e.Outcome
	ended := st.Done && st.Outcome != Failed
	switch {
	case !slices.Equal(pathOf(st.Steps), e.Path):
		in.Problem = "the replayed path is not the one recorded at the end"
	case e.Status == Done && !ended:
		in.Problem = "recorded as done, and the replay is not"
	case e.Status != Done && ended:
		in.Problem = "recorded as " + e.Status + ", and the replay is done"
	case e.Status != Failed && f.failed != nil && !f.st.Ended:
		in.Problem = f.failed.Error()
	}
}

// pinnedGraph is the root a start pinned, linked to the workflows it
// pinned beside it, held to their digests.
func pinnedGraph(p startPayload) (*Graph, error) {
	g, err := decodeGraph(p.Definition)
	if err != nil {
		return nil, fmt.Errorf("definition: %w", err)
	}
	var flows []*Graph
	for _, name := range slices.Sorted(maps.Keys(p.Definitions)) {
		pin := p.Definitions[name]
		c, err := decodeGraph(pin.Definition)
		if err != nil {
			return nil, fmt.Errorf("definition of %s: %w", name, err)
		}
		if c.OwnDigest() != pin.Digest {
			return nil, fmt.Errorf("the definition of %s does not match its digest", name)
		}
		flows = append(flows, c)
	}
	if len(flows) > 0 || slices.ContainsFunc(g.Nodes, func(n Node) bool { return n.Flow != "" }) {
		if err := g.Link(flows...); err != nil {
			return nil, fmt.Errorf("definitions: %w", err)
		}
	}
	if g.Digest() != p.Digest {
		return nil, errors.New("the definition does not match its digest")
	}
	return g, nil
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
	g.Nodes = nil
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
