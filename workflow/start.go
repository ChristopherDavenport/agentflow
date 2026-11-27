package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"maps"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// SkillOption configures [SkillTool] and [StartPrompt].
type SkillOption func(*skillOptions)

type skillOptions struct {
	invokes []string
	history func(context.Context) (agentturn.Transcript, error)
	tool    []agentskill.ToolOption
}

// ModelInvokes names the workflows a model may start by asking for their
// skill. A workflow is code that runs at fixed points, so a catalog
// listing it does not make it a model's to start: with no names, a model
// starts none.
func ModelInvokes(names ...string) SkillOption {
	return func(o *skillOptions) { o.invokes = append(o.invokes, names...) }
}

// SkillHistory gives the tool the record, as [agentflow.History] gives
// it to a workflow, to tell from whether a run is under way. Without it
// the tool reads the transcript on the call's context, which a
// compaction can shorten.
func SkillHistory(read func(context.Context) (agentturn.Transcript, error)) SkillOption {
	return func(o *skillOptions) { o.history = read }
}

// SkillToolOptions are passed to the catalog's own skill tool, which
// serves every skill that is not a workflow.
func SkillToolOptions(opts ...agentskill.ToolOption) SkillOption {
	return func(o *skillOptions) { o.tool = append(o.tool, opts...) }
}

// skillArgs is what a call of [SkillTool] is read as.
type skillArgs struct {
	Name   string          `json:"name"`
	Path   string          `json:"path,omitempty"`
	Inputs json.RawMessage `json:"inputs,omitempty"`
}

// SkillTool stands in for the catalog's skill tool, under its name, for
// a host whose catalog holds workflows. An ordinary skill is served by
// the catalog's own tool, as it would be without workflows. A workflow
// skill, one of flows, is never read through it: its files hold the
// definition, a judge's criteria among it. Asked for by name, it starts,
// when the host lets a model start it ([ModelInvokes]), it is not
// [agentflow.Graph.Hidden] and no run is under way: the call's inputs
// are held to the workflow's, the result is [agentflow.StartText] and
// the model's turn ends there. The program then invokes it:
// [agentflow.Drive] reads the request from the record. Every refusal is
// an error the model reads.
//
// A name given to ModelInvokes that is not one of flows is an error.
func SkillTool(c *agentskill.Catalog, flows []*Workflow, opts ...SkillOption) (agenttool.Tool, error) {
	o := skillOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	byName, err := workflowsOf(flows, o.invokes)
	if err != nil {
		return nil, err
	}
	inner := c.Tool(o.tool...)
	params, err := withInputs(inner.Parameters())
	if err != nil {
		return nil, err
	}
	var topts []agenttool.Option
	if agenttool.IsFactual(inner) {
		topts = append(topts, agenttool.WithFacts(func(ctx context.Context, args json.RawMessage) (agenttool.Facts, error) {
			if a, err := agenttool.Decode[skillArgs](args); err == nil && byName[a.Name] != nil {
				// A workflow's files are not read, so nothing is claimed.
				return agenttool.Facts{}, nil
			}
			facts, _, err := agenttool.FactsOf(ctx, inner, args)
			return facts, err
		}))
	}
	description := inner.Description() + " A skill listed as a workflow is not read: calling with its name, and its inputs, starts it."
	return agenttool.NewFunc(inner.Name(), description, params, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
		a, err := agenttool.Decode[skillArgs](call.Args)
		if err != nil {
			return agenttool.Result{}, err
		}
		w := byName[a.Name]
		if w == nil {
			if len(a.Inputs) > 0 {
				return agenttool.Result{}, fmt.Errorf("%q is not a workflow, and only a workflow takes inputs", a.Name)
			}
			return inner.Execute(ctx, call)
		}
		return start(ctx, w, a, o)
	}, topts...), nil
}

// workflowsOf indexes flows by name and checks the names a model may
// start.
func workflowsOf(flows []*Workflow, invokes []string) (map[string]*Workflow, error) {
	byName := map[string]*Workflow{}
	for _, w := range flows {
		if w == nil || w.Graph == nil {
			return nil, errors.New("workflow: a workflow given to SkillTool is nil")
		}
		byName[w.Graph.Name] = w
	}
	var errs []error
	for _, name := range invokes {
		if byName[name] == nil {
			errs = append(errs, fmt.Errorf("workflow: ModelInvokes names %q, which is not a workflow given%s", name, agentflow.Suggest(name, slices.Sorted(maps.Keys(byName)))))
		}
	}
	return byName, errors.Join(errs...)
}

// start meets a model's request for a workflow's skill, or says why it
// cannot be met.
func start(ctx context.Context, w *Workflow, a skillArgs, o skillOptions) (agenttool.Result, error) {
	g := w.Graph
	switch {
	case a.Path != "":
		return agenttool.Result{}, fmt.Errorf("%q is a workflow, whose files are not read; call with its name, and its inputs, to start it", g.Name)
	case !slices.Contains(o.invokes, g.Name):
		return agenttool.Result{}, fmt.Errorf("the workflow %q is not one you may start; a person can start it", g.Name)
	case g.Hidden:
		return agenttool.Result{}, fmt.Errorf("the workflow %q is only for other workflows to call, and is not started from a conversation", g.Name)
	}
	record, err := recordOf(ctx, o)
	if err != nil {
		return agenttool.Result{}, err
	}
	if err := agentflow.UnderWay(record); err != nil {
		return agenttool.Result{}, err
	}
	var inputs any
	if len(a.Inputs) > 0 && string(a.Inputs) != "null" {
		if err := json.Unmarshal(a.Inputs, &inputs); err != nil {
			return agenttool.Result{}, fmt.Errorf("inputs: %w", err)
		}
	}
	if inputs == nil && g.Inputs != nil {
		// Held to the schema as an empty object, so a required input is
		// asked for rather than left out.
		inputs = map[string]any{}
	}
	if inputs != nil {
		// The invocation's own check, so a request that passes here is one
		// the program can make.
		if _, err := g.Invocation(inputs); err != nil {
			return agenttool.Result{}, fmt.Errorf("the workflow %q was not started: %w", g.Name, err)
		}
	}
	res := agenttool.Text(agentflow.StartText(g.Name))
	res.Terminate = true
	return res, nil
}

// recordOf is what the tool reads a run under way from.
func recordOf(ctx context.Context, o skillOptions) (agentturn.Transcript, error) {
	if o.history != nil {
		t, err := o.history(ctx)
		if err != nil {
			return nil, fmt.Errorf("history: %w", err)
		}
		return t, nil
	}
	t, ok := agentturn.TranscriptFromContext(ctx)
	if !ok {
		return nil, errors.New("no transcript on the call's context to tell whether a workflow is running")
	}
	return t, nil
}

// withInputs adds the inputs argument to the skill tool's parameters.
func withInputs(raw json.RawMessage) (json.RawMessage, error) {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, fmt.Errorf("workflow: the skill tool's parameters: %w", err)
	}
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		schema["properties"] = props
	}
	props["inputs"] = map[string]any{
		"type":        "object",
		"description": "For a skill listed as a workflow: the inputs to start it with, as its schema says",
	}
	return json.Marshal(schema)
}

// StartPrompt is the text, for the instructions beside the catalog's
// own Catalog.Prompt, that tells a model which skills are workflows it
// may start, each with its description and the schema of its inputs.
// It lists only those [ModelInvokes] names that are not hidden, and is
// empty when there are none.
func StartPrompt(flows []*Workflow, opts ...SkillOption) string {
	o := skillOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	var b strings.Builder
	for _, w := range flows {
		if w == nil || w.Graph == nil || w.Graph.Hidden || !slices.Contains(o.invokes, w.Graph.Name) {
			continue
		}
		inputs := "{}"
		if w.Graph.Inputs != nil {
			if j, err := json.Marshal(w.Graph.Inputs); err == nil {
				inputs = string(j)
			}
		}
		fmt.Fprintf(&b, "<workflow name=\"%s\" inputs=\"%s\">%s</workflow>\n", html.EscapeString(w.Graph.Name), html.EscapeString(inputs), html.EscapeString(w.Graph.Description))
	}
	if b.Len() == 0 {
		return ""
	}
	return "<workflows>\nThese skills are workflows. Calling the skill tool with one's name starts it with the inputs given, instead of returning instructions, and ends your turn.\n" + b.String() + "</workflows>"
}
