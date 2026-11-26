package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// callArgs is the arguments of flow_call, and of the flow_judge the
// program makes: what a step that calls a workflow passed it, and which
// run it was. In the current context the result is the run's ending.
// For a run in a child, Context is fresh, Spawn names the child's
// session, and what came of the run is added when the call is let go.
type callArgs struct {
	Workflow string         `json:"workflow"`
	Digest   string         `json:"digest"`
	Run      string         `json:"run"`
	With     map[string]any `json:"with,omitempty"`
	Context  string         `json:"context,omitempty"`
	// Child is the name of a child the run keeps, and Spawn the spawn ID
	// of the child's session: the run's key, or the key of the run that
	// first started a kept child.
	Child string `json:"child,omitempty"`
	Spawn string `json:"spawn,omitempty"`
	// MaxSteps is what the caller's tree has left of its steps, which the
	// child's runs may enter.
	MaxSteps int `json:"max_steps,omitempty"`
	// Step and Mode are a judge's: the step it looks at, and how far the
	// host lets it answer.
	Step string `json:"step,omitempty"`
	Mode string `json:"mode,omitempty"`
	// Files are the files a fresh model step is given, passed into its
	// child, which reads them there.
	Files []fileRef `json:"files,omitempty"`
	// Documents are the texts of the documents a fresh model step reads,
	// passed into its child.
	Documents map[string]string `json:"documents,omitempty"`
	// Result, Failure and Verdict are what came of the child.
	Result  *Result  `json:"result,omitempty"`
	Failure string   `json:"failure,omitempty"`
	Verdict *Verdict `json:"verdict,omitempty"`
	// Entries and Granted are what the child's runs spent of the tree's
	// steps, and what a person granted there, whatever came of it.
	Entries int `json:"entries,omitempty"`
	Granted int `json:"granted,omitempty"`
	// Summary is what the child's end summed up.
	Summary *Summary `json:"summary,omitempty"`
}

// callFailure is the output of a child's call that failed: the failure,
// and what the child spent, so its caller counts the steps a failed child
// entered. It is no [Result], so the step has failed.
type callFailure struct {
	Failure string   `json:"failure"`
	Entries int      `json:"entries,omitempty"`
	Granted int      `json:"granted,omitempty"`
	Summary *Summary `json:"summary,omitempty"`
}

// spent reads what the output of a child's call says the child spent:
// its result, its failure and its verdict carry it alike.
func spent(output string) (entries, granted int) {
	var s struct {
		Entries int `json:"entries"`
		Granted int `json:"granted"`
	}
	_ = decode([]byte(output), &s)
	return s.Entries, s.Granted
}

// skillOf is the skill a workflow's name says it is of.
func skillOf(name string) string {
	skill, _, _ := strings.Cut(name, ":")
	skill, _, _ = strings.Cut(skill, "/")
	return skill
}

// runClause is how instructions name the run a finish belongs to.
func runClause(st State) string { return fmt.Sprintf(", run %q", st.Instance) }

// where says which run asks a question, and the way to it.
func (q *Question) where(st State) { q.Workflow, q.Run, q.Path = st.Workflow, st.Instance, st.Path }

// rootStart is the start of a run nobody called here: the definition of
// the workflow and of every one it can reach, pinned. A run in a child
// also records who called it, so the child's session reads on its own.
func (f *flow) rootStart(inputs any) (startPayload, error) {
	definition, err := json.Marshal(f.g)
	if err != nil {
		return startPayload{}, err
	}
	p := startPayload{Workflow: f.g.Name, Digest: f.g.Digest(), Inputs: inputs, Format: RecordFormat,
		Definition: json.RawMessage(definition), MaxDepth: f.depth(), MaxSteps: f.steps()}
	if c := f.child; c != nil {
		p.Run, p.Parent, p.Caller, p.Depth, p.Path, p.Context = c.Run, c.Parent, c.Caller, c.Depth, c.Path, ContextFresh
		p.MaxDepth, p.MaxSteps, p.Documents, p.Files = c.maxDepth, c.maxSteps, c.documents, c.files
	} else if p.Run, err = newKey(); err != nil {
		return startPayload{}, err
	}
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

// steps is the bound on step entries a new tree records.
func (f *flow) steps() int {
	n := cmpSteps(f.g.MaxSteps)
	if f.maxSteps > 0 {
		n = min(n, f.maxSteps)
	}
	return n
}

// left is what st's tree has left of its steps, for a child to enter.
func (st State) left() int { return max(0, st.maxSteps+st.Granted-st.Entries) }

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

// callFresh holds a call into a child to what the calling step makes.
func callFresh(st State, c *openresponses.FunctionCall) error {
	n := st.Node
	want := st.g.Callees[n.Flow]
	if n.Kind == Agent {
		want = st.g.stepGraph(n)
	}
	var a callArgs
	switch {
	case c.Name != callTool || c.CallID != st.callID():
		return fmt.Errorf("step %q runs in a child and takes no other calls", n.ID)
	case decode([]byte(c.Arguments), &a) != nil:
		return errors.New("the call is not readable")
	case a.Context != ContextFresh || a.Workflow != want.Name || a.Digest != want.OwnDigest():
		return fmt.Errorf("step %q runs %s, as pinned, in a child", n.ID, want.Name)
	case !keyPattern.MatchString(a.Run) || a.Child != n.Child || a.Step != "" || a.Mode != "":
		return errors.New("the call is not one the step makes")
	case a.Spawn != a.Run && (n.Child == "" || a.Spawn != st.kept[n.Child]):
		return fmt.Errorf("step %q keeps child %q in the session it started", n.ID, n.Child)
	case a.Result != nil || a.Failure != "" || a.Verdict != nil:
		return errors.New("what came of a child is the program's to say, when it has ended")
	case a.MaxSteps != st.left():
		return fmt.Errorf("step %q gives its child the %d steps its tree has left", n.ID, st.left())
	case n.Kind == Agent && !sameJSON(a.Files, st.filesFor(n)):
		return fmt.Errorf("step %q passes its child the files it is given", n.ID)
	case n.Kind == Agent && !sameJSON(a.Documents, st.passedDocuments(n)):
		return fmt.Errorf("step %q passes its child the documents as they stand", n.ID)
	}
	with, err := st.resolveAll(n.With)
	if err != nil {
		return err
	}
	if !sameJSON(a.With, with) {
		return fmt.Errorf("step %q passes its child other inputs than its own", n.ID)
	}
	return nil
}

var keyPattern = regexp.MustCompile(`^[0-9a-f]{10}$`)

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
	return agenttool.New(callTool, "Return what a workflow a step called came to.", func(ctx context.Context, a callArgs) (any, error) {
		if err := exact(ctx, &a); err != nil {
			return nil, err
		}
		st, err := f.current(ctx)
		if err != nil {
			return nil, err
		}
		if a.Context == ContextFresh {
			switch {
			case a.Failure != "":
				// The failure says what the child spent, which an error
				// could not carry.
				return callFailure{Failure: a.Failure, Entries: a.Entries, Granted: a.Granted, Summary: a.Summary}, nil
			case a.Result == nil:
				return nil, fmt.Errorf("agentflow: run %q has not ended", a.Run)
			}
			r := *a.Result
			r.Entries, r.Granted, r.Summary = a.Entries, a.Granted, a.Summary
			return r, nil
		}
		e := st.Returned
		if st.Node == nil || st.Node.Flow == "" || e == nil || e.Instance != a.Run {
			return nil, fmt.Errorf("agentflow: run %q has not ended", a.Run)
		}
		return returned(st.Node, a.Workflow, e)
	}, agenttool.WithReplay(safeReplay))
}

// returned is what a run's ending gives the step that called it: its
// outcome and outputs, or the outcome failed where the step routes on
// it. A failure it does not route on fails the step.
func returned(n *Node, workflow string, e *Ending) (Result, error) {
	switch {
	case e.Status == Done:
		return Result{Outcome: e.Outcome, Data: e.Outputs}, nil
	case e.Status == Failed && !treeWide(e.Limit) && n.routesFailure():
		data := map[string]any{"reason": e.Reason, "workflow": workflow}
		if e.Step != "" {
			data["step"] = e.Step
		}
		return Result{Outcome: Failed, Data: data}, nil
	}
	return Result{}, fmt.Errorf("agentflow: %s (run %s) %s: %s", workflow, e.Instance, e.Status, e.Reason)
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
