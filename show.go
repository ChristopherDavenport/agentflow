package agentflow

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
)

// Show says what a model sees of a script or tool step's result: the
// outcome and the members of its data Members names, none for an empty
// list. The whole result stays in the record, and every step's with reads
// it; only what a working model is sent is the shown one.
type Show struct {
	Members []string `json:",omitempty"`
}

const shownTool = "flow_shown"

// shownArgs is flow_shown's arguments: the step, and the call whose
// result it shows.
type shownArgs struct {
	Run   string `json:"run"`
	Step  string `json:"step"`
	Visit int    `json:"visit"`
	Of    string `json:"of"`
}

// unshown is a step whose result a model is to see only as its Show
// says, and whose flow_shown the program has yet to make.
type unshown struct {
	of, run, step string
	visit         int
	show          Show
	result        Result
}

func (u *unshown) id() string { return u.of + "-shown" }

func (u *unshown) args() shownArgs {
	return shownArgs{Run: u.run, Step: u.step, Visit: u.visit, Of: u.of}
}

// shown is what a model sees of a result: its outcome, and of its data
// the members show names, those it has.
func (s Show) shown(res Result) Result {
	out := Result{Outcome: res.Outcome}
	data, ok := res.Data.(map[string]any)
	if !ok || len(s.Members) == 0 {
		return out
	}
	kept := map[string]any{}
	for _, m := range s.Members {
		if v, ok := data[m]; ok {
			kept[m] = v
		}
	}
	if len(kept) > 0 {
		out.Data = kept
	}
	return out
}

// checkShow holds show to a script or tool step, each member to one
// some outcome of its data declares. show: none needs no schema; a member
// does, since without one there is nothing to hold it to.
func (n *Node) checkShow() []error {
	s := n.Show
	if s == nil {
		return nil
	}
	bad := func(format string, args ...any) error {
		return fmt.Errorf("agentflow: node %q: "+format, append([]any{n.ID}, args...)...)
	}
	if n.Kind != Run || n.Flow != "" {
		return []error{bad("only a script or tool step has show: a model step's result is its own, and a call's is its callee's outputs")}
	}
	if len(s.Members) > 0 && len(n.Data) == 0 {
		return []error{bad("show names members, and the step declares no outputs to hold them to; declare its outputs, or show none")}
	}
	var errs []error
	var names []string
	for _, schema := range n.Data {
		names = append(names, memberNames(schema)...)
	}
	seen := map[string]bool{}
	for _, m := range s.Members {
		switch {
		case seen[m]:
			errs = append(errs, bad("show names %q twice", m))
		case !slices.ContainsFunc(n.outcomes(), func(o string) bool { return n.Data[o] != nil && declares(n.Data[o], m) }):
			errs = append(errs, bad("show: no outcome declares %q%s", m, suggest(m, names)))
		}
		seen[m] = true
	}
	return errs
}

// shownTool says what a model sees of a step's result, read from the
// record: the outcome, and the members its show names. The call it shows
// is left out of what a working model is sent.
func (f *flow) shownTool() agenttool.Tool {
	return agenttool.New(shownTool, "Show a workflow step's result as the step says.", func(ctx context.Context, a shownArgs) (Result, error) {
		if err := exact(ctx, &a); err != nil {
			return Result{}, err
		}
		st, err := f.current(ctx)
		if st.show == nil {
			if err != nil {
				return Result{}, err
			}
			return Result{}, errors.New("agentflow: no step's result waits to be shown")
		}
		if a != st.show.args() {
			return Result{}, fmt.Errorf("agentflow: the result waiting to be shown is that of %s", st.show.of)
		}
		return st.show.show.shown(st.show.result), nil
	}, agenttool.WithReplay(safeReplay))
}

// veiled are the calls a flow_shown in t shows, which no working model
// reads whole.
func veiled(t openresponses.Items) map[string]bool {
	var out map[string]bool
	for _, item := range t {
		c, ok := item.(*openresponses.FunctionCall)
		if !ok || c.Name != shownTool || !strings.HasPrefix(c.CallID, reserved) {
			continue
		}
		var a shownArgs
		if decode([]byte(c.Arguments), &a) == nil && a.Of != "" {
			if out == nil {
				out = map[string]bool{}
			}
			out[a.Of] = true
		}
	}
	return out
}
