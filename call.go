package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// callArgs is flow_call's arguments: what a step that calls a workflow
// passed it, and which run it was. The result is the run's ending.
type callArgs struct {
	Workflow string         `json:"workflow"`
	Digest   string         `json:"digest"`
	Run      string         `json:"run"`
	With     map[string]any `json:"with,omitempty"`
}

// skillOf is the skill a workflow's name says it is of.
func skillOf(name string) string {
	skill, _, _ := strings.Cut(name, "/")
	return skill
}

// runClause is how instructions name the run a finish belongs to.
func runClause(st State) string {
	if st.Format == 1 {
		return ""
	}
	return fmt.Sprintf(", run %q", st.Instance)
}

// where says which run asks a question, and the way to it.
func (q *Question) where(st State) {
	if st.Format == 1 {
		return
	}
	q.Workflow, q.Run, q.Path = st.Workflow, st.Instance, st.Path
}

// rootStart is the start of a run nobody called: the definition of the
// workflow and of every one it can reach, pinned.
func (f *flow) rootStart(key string, inputs any) (startPayload, error) {
	definition, err := json.Marshal(f.g)
	if err != nil {
		return startPayload{}, err
	}
	p := startPayload{Workflow: f.g.Name, Digest: f.g.Digest(), Inputs: inputs, Format: RecordFormat,
		Definition: json.RawMessage(definition), Run: key, MaxDepth: f.depth()}
	closure := f.g.closure()
	for _, name := range slices.Sorted(maps.Keys(closure)) {
		if name == f.g.Name {
			continue
		}
		b, err := json.Marshal(closure[name])
		if err != nil {
			return startPayload{}, err
		}
		if p.Definitions == nil {
			p.Definitions = map[string]pinned{}
		}
		p.Definitions[name] = pinned{Digest: closure[name].OwnDigest(), Definition: b}
	}
	return p, nil
}

// depth is the bound on nesting a new run records.
func (f *flow) depth() int {
	d := cmpDepth(f.g.MaxDepth)
	if f.maxDepth > 0 {
		d = min(d, f.maxDepth)
	}
	return d
}

// callStart holds the start of a call to what the calling step makes.
func callStart(st State, c *openresponses.FunctionCall) error {
	n := st.Node
	callee := st.g.Callees[n.Flow]
	var p startPayload
	switch {
	case c.Name != startTool:
		return fmt.Errorf("step %q calls %s and takes no other calls", n.ID, callee.Name)
	case decode([]byte(c.Arguments), &p) != nil:
		return errors.New("the start of a call is not readable")
	case p.Workflow != callee.Name || p.Digest != callee.OwnDigest():
		return fmt.Errorf("step %q calls %s as pinned, not %s", n.ID, callee.Name, p.Workflow)
	case p.Parent != st.Instance || p.Caller != st.callID() || p.Depth != st.Depth+1:
		return fmt.Errorf("a call of step %q starts under run %q from %s", n.ID, st.Instance, st.callID())
	case p.Format != RecordFormat || p.Definition != nil || p.Definitions != nil:
		return errors.New("a call's start pins nothing of its own")
	case c.CallID != keyPrefix(p.Run)+"start" || len(p.Run) != 10:
		return errors.New("a call's start is named by its key")
	}
	return nil
}

// callReturn holds a flow_call to the run it reports: the one the step
// started, with what that run was given.
func callReturn(st State, c *openresponses.FunctionCall) error {
	n := st.Node
	var a callArgs
	switch {
	case c.Name != callTool || c.CallID != st.callID() || st.Returned == nil:
		return fmt.Errorf("step %q is waiting on run %q", n.ID, st.Callee)
	case decode([]byte(c.Arguments), &a) != nil:
		return errors.New("the call is not readable")
	case a.Run != st.Returned.Instance || a.Workflow != st.g.Callees[n.Flow].Name:
		return fmt.Errorf("step %q called run %q", n.ID, st.Returned.Instance)
	case !sameJSON(a.With, st.calleeInputs):
		return fmt.Errorf("step %q passed run %q other inputs than it started with", n.ID, a.Run)
	}
	return nil
}

func sameJSON(a, b any) bool {
	norm := func(v any) any {
		raw, err := json.Marshal(v)
		if err != nil {
			return nil
		}
		var out any
		if decode(raw, &out) != nil {
			return nil
		}
		if m, ok := out.(map[string]any); ok && len(m) == 0 {
			return nil
		}
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// current is where the workflow stands, as the transcript on a tool's
// context says.
func (f *flow) current(ctx context.Context) (State, error) {
	t, ok := agentturn.TranscriptFromContext(ctx)
	if !ok {
		return State{}, errors.New("agentflow: no transcript on the context")
	}
	st, _, err := f.read(ctx, t)
	return st, err
}

// callTool returns what came of a run a step called, read from the
// record: its outcome and outputs, or the outcome failed where the step
// routes on it. A failure it does not route on fails the step.
func (f *flow) callTool() agenttool.Tool {
	return agenttool.New(callTool, "Return what a workflow a step called came to.", func(ctx context.Context, a callArgs) (Result, error) {
		if err := exact(ctx, &a); err != nil {
			return Result{}, err
		}
		st, err := f.current(ctx)
		if err != nil {
			return Result{}, err
		}
		e := st.Returned
		if st.Node == nil || st.Node.Flow == "" || e == nil || e.Instance != a.Run {
			return Result{}, fmt.Errorf("agentflow: run %q has not ended", a.Run)
		}
		switch {
		case e.Status == Done:
			return Result{Outcome: e.Outcome, Data: e.Outputs}, nil
		case e.Status == Failed && st.Node.routesFailure():
			data := map[string]any{"reason": e.Reason, "workflow": a.Workflow}
			if e.Step != "" {
				data["step"] = e.Step
			}
			return Result{Outcome: Failed, Data: data}, nil
		}
		return Result{}, fmt.Errorf("agentflow: %s (run %s) %s: %s", a.Workflow, a.Run, e.Status, e.Reason)
	}, agenttool.WithReplay(safeReplay))
}

// routesFailure says a call goes on, somewhere it names, when what it
// called failed.
func (n *Node) routesFailure() bool { return n.Next[Failed] != "" || n.Ends[Failed] != "" }

// mode is how far the host lets a judge answer st's question.
func (f *flow) mode(st State) Mode {
	if st.Node == nil {
		return 0
	}
	return f.modes[modeKey(st.Workflow, st.Node.ID)]
}

func modeKey(workflow, step string) string { return workflow + ":" + step }
