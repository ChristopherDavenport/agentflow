package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// Question is what a Person node asks.
type Question struct {
	Step     string   `json:"step"`
	Question string   `json:"question"`
	Outcomes []string `json:"outcomes"`
	// Answer is empty as asked. [Answer] fills it in.
	Answer *Result `json:"answer,omitempty"`
}

type finishArgs struct {
	Step    string         `json:"step"`
	Outcome string         `json:"outcome"`
	Data    map[string]any `json:"data,omitempty"`
}

// Option configures [Apply].
type Option func(*flow)

// Unattended says nobody is there to ask. A Person node takes its
// unattended outcome; one without waits as it would otherwise.
func Unattended() Option { return func(f *flow) { f.unattended = true } }

type flow struct {
	g          *Graph
	model      openresponses.Streamer
	modelName  string
	unattended bool
}

// programPrefix starts the model name of a turn the program takes, so a
// record never passes a program's call off as a model's.
const programPrefix = "agentflow/"

// Apply returns cfg made to follow g. cfg's Model and ModelName serve
// the Agent nodes, and its tools serve both kinds that call tools. cfg's
// own BeforeModelCall and BeforeToolCall still run, and may refuse what
// the workflow asks for.
func Apply(g *Graph, cfg agentturn.Config, opts ...Option) (agentturn.Config, error) {
	if err := g.Validate(); err != nil {
		return cfg, err
	}
	f := &flow{g: g, model: cfg.Model, modelName: cfg.ModelName}
	for _, opt := range opts {
		opt(f)
	}
	if f.modelName == "" {
		f.modelName = cfg.Request.Model
	}
	if f.modelName == "" || strings.HasPrefix(f.modelName, programPrefix) {
		return cfg, errors.New("agentflow: the config names no model")
	}

	own := []agenttool.Tool{f.askTool(), f.finishTool()}
	if p := cfg.ToolProvider; p != nil {
		cfg.ToolProvider = func(ctx context.Context) []agenttool.Tool {
			return append(slices.Clone(p(ctx)), own...)
		}
	} else {
		cfg.Tools = append(slices.Clone(cfg.Tools), own...)
	}

	cfg.Model = f
	if transform := cfg.Transform; transform != nil {
		cfg.Transform = func(ctx context.Context, t agentturn.Transcript) (agentturn.Transcript, error) {
			out, err := transform(ctx, t)
			if err != nil {
				return nil, err
			}
			return out, f.samePosition(t, out)
		}
	}
	userModel := cfg.BeforeModelCall
	cfg.BeforeModelCall = func(ctx context.Context, req *openresponses.Request) error {
		if userModel != nil {
			if err := userModel(ctx, req); err != nil {
				return err
			}
		}
		return f.prepare(req)
	}
	userTool := cfg.BeforeToolCall
	cfg.BeforeToolCall = func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		d, err := f.gate(ctx, info)
		if d != nil || err != nil || userTool == nil {
			return d, err
		}
		return userTool(ctx, info)
	}
	return cfg, nil
}

// ErrPositionLost is a transform, a compaction say, whose result no
// longer says where the workflow stands.
var ErrPositionLost = errors.New("agentflow: the transform dropped what the workflow's position is read from")

func (f *flow) samePosition(before, after agentturn.Transcript) error {
	b, err := f.g.Replay(before)
	if err != nil {
		return err
	}
	a, err := f.g.Replay(after)
	if err != nil {
		return err
	}
	if a.Node != b.Node || a.Visit != b.Visit || len(a.Steps) != len(b.Steps) {
		return ErrPositionLost
	}
	return nil
}

// prepare shapes the request for whoever takes this turn. It runs
// before the turn is recorded, so the record holds what was sent.
func (f *flow) prepare(req *openresponses.Request) error {
	st, err := f.g.Replay(req.Input)
	if err != nil {
		return err
	}
	if st.Node == nil || st.Node.Kind != Agent {
		req.Model = programPrefix + f.g.Name
		return nil
	}
	n := st.Node
	req.Model = f.modelName
	req.Instructions = strings.TrimSpace(req.Instructions + "\n\n" + fmt.Sprintf(
		"You are on step %q of workflow %q.\n%s\n\nWhen the step is done, call %s with step %q and one outcome of: %s.",
		n.ID, f.g.Name, n.Prompt, finishTool, n.ID, strings.Join(n.outcomes(), ", ")))
	req.Tools = slices.DeleteFunc(slices.Clone(req.Tools), func(t openresponses.Tool) bool {
		fn, ok := t.(*openresponses.FunctionTool)
		return !ok || (fn.Name != finishTool && !slices.Contains(n.Tools, fn.Name))
	})
	return nil
}

// CreateStream takes a program turn itself and passes a model's on.
func (f *flow) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if !strings.HasPrefix(req.Model, programPrefix) {
		return f.model.CreateStream(ctx, req, sink)
	}
	st, err := f.g.Replay(req.Input)
	if err != nil {
		return err
	}
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	if st.Node == nil {
		if err := f.report(e, st); err != nil {
			return err
		}
		return e.Complete()
	}
	name, args := st.Node.Tool, st.Node.Args
	if st.Node.Kind == Person {
		name = askTool
		if args, err = json.Marshal(Question{Step: st.Node.ID, Question: st.Node.Prompt, Outcomes: st.Node.outcomes()}); err != nil {
			return err
		}
	} else if args == nil {
		args = json.RawMessage("{}")
	}
	call, err := e.FunctionCall(f.g.CallID(st.Node.ID, st.Visit), name)
	if err != nil {
		return err
	}
	if err := call.Arguments(string(args)); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

func (f *flow) report(e *openresponses.Emitter, st State) error {
	msg, err := e.Message("")
	if err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Workflow %s finished.", f.g.Name)
	for _, s := range st.Steps {
		fmt.Fprintf(&b, "\n- %s: %s", s.Node, s.Result.Outcome)
	}
	if err := msg.Text(b.String()); err != nil {
		return err
	}
	return msg.Close()
}

// gate keeps each turn to what its node allows. It returns nil to leave
// the call to the config's own hook.
func (f *flow) gate(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	if info.Parent != "" {
		return nil, nil
	}
	block := func(reason string) (*agentturn.ToolDecision, error) {
		return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "agentflow: " + reason}, nil
	}
	t, ok := agentturn.TranscriptFromContext(ctx)
	if !ok {
		return nil, errors.New("agentflow: no transcript on the context")
	}
	st, err := f.g.Replay(t)
	if err != nil {
		return nil, err
	}
	n, name := st.Node, info.Call.Name
	switch {
	case n == nil:
		return block("the workflow has finished")
	case n.Kind == Agent:
		if name == finishTool {
			if step := finishStep(info.Call.Arguments); step != n.ID {
				return block(fmt.Sprintf("you are on step %q, not %q", n.ID, step))
			}
			return nil, nil
		}
		if !slices.Contains(n.Tools, name) {
			return block(fmt.Sprintf("step %q does not allow %s", n.ID, name))
		}
	case info.Call.CallID != f.g.CallID(n.ID, st.Visit):
		return block(fmt.Sprintf("step %q takes no other calls", n.ID))
	case n.Kind == Person && !(f.unattended && n.Unattended != ""):
		return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: "agentflow: " + n.Prompt}, nil
	}
	return nil, nil
}

// askTool returns the answer an approval carried, or the node's default
// when the question was not deferred because nobody is there to ask.
func (f *flow) askTool() agenttool.Tool {
	return agenttool.New(askTool, "Ask the person a workflow question.", func(_ context.Context, q Question) (Result, error) {
		n := f.g.node(q.Step)
		switch {
		case n == nil || n.Kind != Person:
		case q.Answer != nil && slices.Contains(n.outcomes(), q.Answer.Outcome):
			return *q.Answer, nil
		case q.Answer == nil && f.unattended && n.Unattended != "":
			return Result{Outcome: n.Unattended, By: "unattended default"}, nil
		}
		return Result{}, errors.New("agentflow: this question needs one of its outcomes as an answer")
	}, agenttool.WithReplay(func(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe }))
}

func (f *flow) finishTool() agenttool.Tool {
	return agenttool.New(finishTool, "Finish the current workflow step.", func(_ context.Context, a finishArgs) (Result, error) {
		n := f.g.node(a.Step)
		if n == nil || n.Kind != Agent {
			return Result{}, fmt.Errorf("agentflow: %q is not a step you can finish", a.Step)
		}
		if !slices.Contains(n.outcomes(), a.Outcome) {
			return Result{}, fmt.Errorf("agentflow: outcome must be one of %s", strings.Join(n.outcomes(), ", "))
		}
		return Result{Outcome: a.Outcome, Data: a.Data}, nil
	})
}

// Answer answers a pending question on a person's behalf. It approves
// the question with the answer in its arguments, so the record reads as
// a call a person let proceed. An output supplied in its place would be
// recorded as a rejection.
func Answer(pending agentturn.PendingCall, res Result) (agentturn.Answer, error) {
	var q Question
	if pending.Call == nil || pending.Call.Name != askTool {
		return agentturn.Answer{}, errors.New("agentflow: not a workflow question")
	}
	if err := json.Unmarshal([]byte(pending.Call.Arguments), &q); err != nil {
		return agentturn.Answer{}, fmt.Errorf("agentflow: question: %w", err)
	}
	if !slices.Contains(q.Outcomes, res.Outcome) {
		return agentturn.Answer{}, fmt.Errorf("agentflow: outcome must be one of %s", strings.Join(q.Outcomes, ", "))
	}
	q.Answer = &res
	args, err := json.Marshal(q)
	if err != nil {
		return agentturn.Answer{}, err
	}
	return agentturn.ApproveWith(pending.Call.CallID, args).WithBy("human"), nil
}
