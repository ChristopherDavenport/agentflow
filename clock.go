package agentflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agenttool"
)

// clockArgs is flow_clock's arguments. The program calls it before a
// model step with a timeout works, and its result, the deadline, is
// what the step is held to: the time is read once, and recorded. When
// the deadline has passed it calls it again, Expired, and the error it
// returns is the step's failure.
type clockArgs struct {
	Run     string `json:"run"`
	Step    string `json:"step"`
	Timeout string `json:"timeout"`
	// Deadline and Expired say the step's time ran out.
	Deadline *time.Time `json:"deadline,omitempty"`
	Expired  bool       `json:"expired,omitempty"`
}

// deadline holds the deadline of the model step a request is for, from
// the hook that shapes it to the model that serves it.
type deadline struct {
	mu sync.Mutex
	at time.Time
}

func (d *deadline) set(at time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.at = at
}

func (d *deadline) get() time.Time {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.at
}

// clockTool reads the clock for a model step, or says its time is up.
func (f *flow) clockTool() agenttool.Tool {
	return agenttool.New(clockTool, "Read the clock for a workflow step.", func(ctx context.Context, a clockArgs) (Result, error) {
		if err := exact(ctx, &a); err != nil {
			return Result{}, err
		}
		if a.Expired {
			at := "its deadline"
			if a.Deadline != nil {
				at = a.Deadline.Format(time.RFC3339)
			}
			return Result{}, fmt.Errorf("agentflow: step %q ran past its timeout of %s, at %s", a.Step, a.Timeout, at)
		}
		d, err := time.ParseDuration(a.Timeout)
		if err != nil || d <= 0 {
			return Result{}, fmt.Errorf("agentflow: timeout %q: want a duration above 0", a.Timeout)
		}
		return Result{Outcome: OK, Data: map[string]any{"deadline": time.Now().Add(d).UTC().Format(time.RFC3339Nano)}}, nil
	}, agenttool.WithReplay(safeReplay))
}

// clockCall is the program's call that reads the clock for st's step, or
// says its time ran out.
func (f *flow) clockCall(st State, expired bool) (id string, args clockArgs) {
	args = clockArgs{Run: st.Instance, Step: st.Node.ID, Timeout: f.timeout(st).String()}
	if !expired {
		return st.clockID(), args
	}
	at := st.Deadline
	args.Deadline, args.Expired = &at, true
	return st.timeoutID(), args
}

// timed bounds the host's tools by the time of the step a call is made
// for: a script's or a tool's own timeout, or for a call a model makes,
// what its step has left. A tool that runs past it is cut off through
// its context, and its result is an error naming the step and the
// timeout, which fails a program's step and asks to retry it. A tool
// that does not heed its context is not stopped.
func (f *flow) timed(tools []agenttool.Tool) []agenttool.Tool {
	out := make([]agenttool.Tool, len(tools))
	for i, t := range tools {
		out[i] = agenttool.Wrap(t, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
			st, err := f.current(ctx)
			if err != nil || st.Node == nil {
				return t.Execute(ctx, call)
			}
			var cut context.Context
			var stop context.CancelFunc
			var why string
			n := st.Node
			switch {
			case n.Kind == Run && n.Flow == "" && call.ID == st.callID() && n.Timeout > 0:
				cut, stop = context.WithTimeout(ctx, n.Timeout)
				why = fmt.Sprintf("agentflow: step %q timed out after %s", n.ID, n.Timeout)
			case n.Kind == Agent && !strings.HasPrefix(call.ID, reserved) && !st.Deadline.IsZero():
				cut, stop = context.WithDeadline(ctx, st.Deadline)
				why = fmt.Sprintf("agentflow: step %q ran out of time, at %s", n.ID, st.Deadline.Format(time.RFC3339))
			default:
				return t.Execute(ctx, call)
			}
			defer stop()
			res, err := t.Execute(cut, call)
			if ctx.Err() == nil && errors.Is(cut.Err(), context.DeadlineExceeded) {
				return agenttool.Result{}, errors.New(why)
			}
			return res, err
		})
	}
	return out
}

type stuckArgs struct {
	Run    string `json:"run"`
	Step   string `json:"step"`
	Reason string `json:"reason"`
}

// stuckTool is how a model reports it cannot do its step. Its result is
// the step's outcome [Stuck], with the reason as its data.
func (f *flow) stuckTool() agenttool.Tool {
	return agenttool.New(stuckTool, "Report that you cannot do the current workflow step, and why.", func(ctx context.Context, a stuckArgs) (Result, error) {
		if err := exact(ctx, &a); err != nil {
			return Result{}, err
		}
		st, err := f.current(ctx)
		if err != nil {
			return Result{}, err
		}
		n := st.Node
		switch {
		case n == nil || !n.canStick() || n.fresh() || n.ID != a.Step || a.Run != st.Instance:
			return Result{}, fmt.Errorf("agentflow: %q is not a step you are on", a.Step)
		case strings.TrimSpace(a.Reason) == "":
			return Result{}, errors.New("agentflow: give your reason")
		}
		return Result{Outcome: Stuck, Data: map[string]any{"reason": a.Reason}, Reason: a.Reason}, nil
	})
}

// policed holds a step under a policy to tools the policy can see: each
// must have its subjects named with [ToolSubjects], or be a tool of cfg
// that claims its facts.
func (f *flow) policed(g *Graph, n *Node, tools []agenttool.Tool) error {
	var names []string
	switch {
	case n.Kind == Agent && (!n.Policy.empty() || f.child != nil && len(f.child.policies) > 0):
		names = n.Tools
	case n.Judge != nil && (!n.Judge.Policy.empty() || f.child != nil && len(f.child.policies) > 0):
		names = n.Judge.Tools
	case n.Flow != "" && !n.Policy.empty() && g.Callees[n.Flow] != nil:
		for _, x := range g.Callees[n.Flow].closure() {
			for _, m := range x.Nodes {
				names = append(names, m.Tools...)
				if m.Judge != nil {
					names = append(names, m.Judge.Tools...)
				}
			}
		}
	}
	for _, name := range names {
		if _, ok := f.subjects[name]; ok {
			continue
		}
		if k := slices.IndexFunc(tools, func(t agenttool.Tool) bool { return t.Name() == name }); k >= 0 && agenttool.IsFactual(tools[k]) {
			continue
		}
		return fmt.Errorf("agentflow: %s: step %q is under a policy that cannot see what %s touches: name its subjects with ToolSubjects", g.Name, n.ID, name)
	}
	return nil
}
