package agentflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	// Retry says the question is whether to run a failed step again.
	Retry bool `json:"retry,omitempty"`
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
// unattended outcome, one without waits as it would otherwise, and a
// failed step ends the workflow.
func Unattended() Option { return func(f *flow) { f.unattended = true } }

// Reply answers questions by the host's own rules, before anyone is
// asked. It returns false to leave a question to the person.
func Reply(fn func(context.Context, Question) (Result, bool)) Option {
	return func(f *flow) { f.reply = fn }
}

type flow struct {
	g          *Graph
	model      openresponses.Streamer
	modelName  string
	unattended bool
	reply      func(context.Context, Question) (Result, bool)
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

	own := []agenttool.Tool{f.startTool(), f.askTool(), f.finishTool()}
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
	if a.Instance != b.Instance || a.Node != b.Node || a.Visit != b.Visit || a.Attempt != b.Attempt ||
		a.Done != b.Done || a.Reported != b.Reported || len(a.Steps) != len(b.Steps) {
		return ErrPositionLost
	}
	return nil
}

// modelTurn says the model takes the next turn.
func modelTurn(st State) bool {
	return st.Done && st.Reported || st.Node != nil && st.Node.Kind == Agent && st.Failure == ""
}

// prepare shapes the request for whoever takes this turn. It runs
// before the turn is recorded, so the record holds what was sent.
func (f *flow) prepare(req *openresponses.Request) error {
	st, err := f.g.Replay(req.Input)
	if err != nil {
		return err
	}
	if !modelTurn(st) {
		req.Model = programPrefix + f.g.Name
		return nil
	}
	req.Model = f.modelName
	keep := func(name string) bool { return !strings.HasPrefix(name, reserved) }
	if n := st.Node; n != nil {
		req.Instructions = strings.TrimSpace(req.Instructions + "\n\n" + fmt.Sprintf(
			"You are on step %q of workflow %q.\n%s\n\nWhen the step is done, call %s with step %q and one outcome of: %s.",
			n.ID, f.g.Name, n.Prompt, finishTool, n.ID, strings.Join(n.outcomes(), ", ")))
		keep = func(name string) bool { return name == finishTool || slices.Contains(n.Tools, name) }
	}
	req.Tools = slices.DeleteFunc(slices.Clone(req.Tools), func(t openresponses.Tool) bool {
		fn, ok := t.(*openresponses.FunctionTool)
		return !ok || !keep(fn.Name)
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
	var id, name string
	var args any
	switch {
	case st.Done:
		if err := f.report(e, st); err != nil {
			return err
		}
		return e.Complete()
	case !st.Started:
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return err
		}
		id, name, args = f.g.startID(hex.EncodeToString(b[:])), startTool, startPayload{Workflow: f.g.Name, Digest: f.g.Digest()}
	case st.Failure != "":
		if f.unattended && f.reply == nil {
			return fmt.Errorf("%w: %s: %s", ErrStepFailed, st.Node.ID, st.Failure)
		}
		id, name = f.g.retryID(st), askTool
		args = Question{Step: st.Node.ID, Retry: true, Outcomes: []string{retryOutcome, stopOutcome},
			Question: fmt.Sprintf("Step %q failed: %s\nRun it again?", st.Node.ID, st.Failure)}
	case st.Node.Kind == Person:
		id, name = f.g.callID(st), askTool
		args = Question{Step: st.Node.ID, Question: st.Node.Prompt, Outcomes: st.Node.outcomes()}
	default:
		id, name, args = f.g.callID(st), st.Node.Tool, st.Node.Args
		if st.Node.Args == nil {
			args = json.RawMessage("{}")
		}
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return err
	}
	call, err := e.FunctionCall(id, name)
	if err != nil {
		return err
	}
	if err := call.Arguments(string(encoded)); err != nil {
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

// gate keeps each turn to what the workflow is at. It returns nil to
// leave the call to the config's own hook.
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
	n, name, id := st.Node, info.Call.Name, info.Call.CallID
	switch {
	case st.Done && st.Reported:
		if strings.HasPrefix(name, reserved) {
			return block("no workflow is running")
		}
	case st.Done:
		return block("the workflow has finished")
	case !st.Started:
		if name != startTool || !strings.HasPrefix(id, reserved+f.g.Name+"_") {
			return block("the workflow has not started")
		}
	case st.Failure != "":
		if name != askTool || id != f.g.retryID(st) {
			return block(fmt.Sprintf("step %q failed", n.ID))
		}
		return f.ask(ctx, info, n)
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
	case id != f.g.callID(st):
		return block(fmt.Sprintf("step %q takes no other calls", n.ID))
	case n.Kind == Person:
		return f.ask(ctx, info, n)
	}
	return nil, nil
}

// ask decides who answers a question: the host's rules, then the node's
// default when nobody is there, then the person.
func (f *flow) ask(ctx context.Context, info agentturn.ToolCallInfo, n *Node) (*agentturn.ToolDecision, error) {
	var q Question
	if err := json.Unmarshal(info.Args, &q); err != nil {
		return nil, fmt.Errorf("agentflow: question: %w", err)
	}
	if f.reply != nil {
		if res, ok := f.reply(ctx, q); ok && slices.Contains(q.Outcomes, res.Outcome) {
			res.By = "rules"
			q.Answer = &res
			args, err := json.Marshal(q)
			if err != nil {
				return nil, err
			}
			return &agentturn.ToolDecision{Action: agentturn.Allow, Args: args, By: "policy", Reason: "agentflow: answered by the host's rules"}, nil
		}
	}
	// With nobody there, the tool takes the node's default, and refuses
	// a retry, which ends the workflow.
	if f.unattended && (q.Retry || n.Unattended != "") {
		return nil, nil
	}
	return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: "agentflow: " + q.Question}, nil
}

func safeReplay(context.Context, json.RawMessage) agenttool.Replay { return agenttool.ReplaySafe }

func (f *flow) startTool() agenttool.Tool {
	return agenttool.New(startTool, "Start a workflow.", func(_ context.Context, p startPayload) (Result, error) {
		return Result{Outcome: OK}, nil
	}, agenttool.WithReplay(safeReplay))
}

// askTool returns the answer an approval or a rule carried, or the
// node's default when the question was not deferred because nobody is
// there to ask.
func (f *flow) askTool() agenttool.Tool {
	return agenttool.New(askTool, "Ask the person a workflow question.", func(_ context.Context, q Question) (Result, error) {
		n := f.g.node(q.Step)
		if n == nil {
			return Result{}, fmt.Errorf("agentflow: no step %q", q.Step)
		}
		outcomes := n.outcomes()
		if q.Retry {
			outcomes = []string{retryOutcome, stopOutcome}
		} else if n.Kind != Person {
			return Result{}, fmt.Errorf("agentflow: step %q asks nothing", q.Step)
		}
		switch {
		case q.Answer != nil && slices.Contains(outcomes, q.Answer.Outcome):
			return *q.Answer, nil
		case q.Answer == nil && f.unattended && !q.Retry && n.Unattended != "":
			return Result{Outcome: n.Unattended, By: "unattended default"}, nil
		}
		return Result{}, errors.New("agentflow: this question needs one of its outcomes as an answer")
	}, agenttool.WithReplay(safeReplay))
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
