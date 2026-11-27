package agentflow

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// Choices, on a Person node, are values an answer picks from: the list
// From resolves to when the question is asked. The pick is the data of
// Outcome, in its member As: one value, or with Many a list of them.
type Choices struct {
	From    Value  `json:"from"`
	Many    bool   `json:"many,omitempty"`
	Outcome string `json:"outcome"`
	As      string `json:"as"`
}

// FreeText, on a Person node, says an answer typed in words takes
// Outcome, with the words in its data's member As.
type FreeText struct {
	Outcome string `json:"outcome"`
	As      string `json:"as"`
}

// Offered is what a question offers to pick from, as it was asked: the
// list the reference resolved to then, which an answer is held to.
type Offered struct {
	Outcome string `json:"outcome"`
	As      string `json:"as"`
	Many    bool   `json:"many,omitempty"`
	Items   []any  `json:"items"`
}

// The data members choices and free text fill when a step names none.
const (
	DefaultChoice  = "choice"
	DefaultChoices = "choices"
	DefaultText    = "text"
)

// ByTimeout is the Result.By of an answer a question's timeout gave.
const ByTimeout = "timeout"

// ErrExpired is an answer given to a question after its deadline. The
// question takes its timeout outcome instead, when the host next drives.
var ErrExpired = errors.New("agentflow: the question's time has run out")

// member is the data member a pick or words go in.
func (c *Choices) member() string {
	switch {
	case c.As != "":
		return c.As
	case c.Many:
		return DefaultChoices
	}
	return DefaultChoice
}

func (t *FreeText) member() string { return cmp.Or(t.As, DefaultText) }

// withMember is s, an object schema or nil, with the required member
// name of schema m added.
func withMember(s *agenttool.Schema, name string, m *agenttool.Schema) *agenttool.Schema {
	out := &agenttool.Schema{Type: "object"}
	if s != nil {
		c := *s
		c.Properties, c.Required = slices.Clone(s.Properties), slices.Clone(s.Required)
		out = &c
	}
	out.Properties = append(out.Properties, agenttool.Property{Name: name, Schema: m})
	out.Required = append(out.Required, name)
	return out
}

// pickSchema is the schema of a pick: one item, or a list of them.
func (c *Choices) pickSchema(item *agenttool.Schema) *agenttool.Schema {
	if item == nil {
		item = &agenttool.Schema{}
	}
	if c.Many {
		return &agenttool.Schema{Type: "array", Items: item}
	}
	return item
}

// asked adds to a person node's data what its choices and free text put
// there.
func (n *Node) asked(data map[string]*agenttool.Schema) map[string]*agenttool.Schema {
	if n.Kind != Person || n.Choices == nil && n.Text == nil {
		return data
	}
	out := maps.Clone(data)
	if out == nil {
		out = map[string]*agenttool.Schema{}
	}
	if c := n.Choices; c != nil {
		out[c.Outcome] = withMember(out[c.Outcome], c.member(), c.pickSchema(n.pick))
	}
	if t := n.Text; t != nil {
		out[t.Outcome] = withMember(out[t.Outcome], t.member(), &agenttool.Schema{Type: "string"})
	}
	return out
}

// checkAsk holds a node's question settings to what they can mean, at
// load: choices, free text and a timeout.
func (g *Graph) checkAsk(n *Node) []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agentflow: node %q: "+format, append([]any{n.ID}, args...)...))
	}
	if n.Kind != Person {
		switch {
		case n.Choices != nil:
			bad("only a person node offers choices")
		case n.Text != nil:
			bad("only a person node takes free text")
		case n.OnTimeout != "" || n.OnTimeoutData != nil:
			bad("only a person node has on_timeout")
		}
		return errs
	}
	declared := func(what, outcome string) bool {
		if !slices.Contains(n.outcomes(), outcome) {
			bad("%s: outcome %q is not declared%s: want one of %s", what, outcome, suggest(outcome, n.outcomes()), strings.Join(n.outcomes(), ", "))
			return false
		}
		return true
	}
	// mine says a member of the outcome's declared data is the setting's
	// to fill, and the rest of its declared data does not need a person
	// to type it into a form as well.
	mine := func(what, outcome, member string) {
		s := n.Data[outcome]
		if s != nil && declares(s, member) {
			bad("%s: the data of %q declares %q, which %s fills", what, outcome, member, what)
		}
	}
	if c := n.Choices; c != nil {
		switch {
		case c.Outcome == "":
			bad("choices: name the outcome a pick is the data of")
		case declared("choices", c.Outcome):
			mine("choices", c.Outcome, c.member())
		}
		switch s := g.valueSchema(c.From); {
		case c.From.From == "":
			bad("choices: from is a reference, ${{ ... }}, to a list an earlier step made")
		case s != nil && s.Type != "" && s.Type != "array":
			bad("choices: %s is %s, not a list", c.From.From, s.Type)
		}
		if !validMember(c.member()) {
			bad("choices: as %q: want a name of letters, digits, _ or -", c.member())
		}
	}
	if t := n.Text; t != nil {
		switch {
		case t.Outcome == "":
			bad("text: name the outcome words are the data of")
		case n.Choices != nil && t.Outcome == n.Choices.Outcome:
			bad("text: outcome %q is the choices' already", t.Outcome)
		case declared("text", t.Outcome):
			mine("text", t.Outcome, t.member())
			if s := n.Data[t.Outcome]; s != nil && len(s.Required) > 0 {
				bad("text: the data of %q requires %s, which words alone do not give", t.Outcome, strings.Join(s.Required, ", "))
			}
		}
		if !validMember(t.member()) {
			bad("text: as %q: want a name of letters, digits, _ or -", t.member())
		}
	}
	// A pick is known only once the question is asked, so nothing written
	// beforehand can make one.
	if c := n.Choices; c != nil && c.Outcome != "" {
		if n.Unattended == c.Outcome {
			bad("unattended cannot be %q: what it picks is not known until the question is asked", c.Outcome)
		}
		if n.OnTimeout == c.Outcome {
			bad("on_timeout cannot be %q: what it picks is not known until the question is asked", c.Outcome)
		}
	}
	// A pick and a person's own words are a person's to give, so a judge
	// chooses neither.
	if j := n.Judge; j != nil {
		for _, outcome := range j.Outcomes {
			switch {
			case n.Choices != nil && outcome == n.Choices.Outcome:
				bad("judge: %q is a pick of the choices, which is a person's", outcome)
			case n.Text != nil && outcome == n.Text.Outcome:
				bad("judge: %q takes a person's own words", outcome)
			}
		}
	}
	switch {
	case n.Timeout > 0 && n.OnTimeout == "":
		bad("a question's timeout needs on_timeout, the outcome it then takes")
	case n.OnTimeout != "" && n.Timeout == 0:
		bad("on_timeout needs a timeout")
	case n.OnTimeout != "" && declared("on_timeout", n.OnTimeout):
		if err := checkData(n.schemas()[n.OnTimeout], n.OnTimeoutData); err != nil {
			bad("on_timeout data: %v", err)
		}
	}
	return errs
}

func validMember(name string) bool { return nodeID.MatchString(name) }

// offer resolves what a question offers to pick from.
func (st State) offer(n *Node) (*Offered, error) {
	c := n.Choices
	if c == nil {
		return nil, nil
	}
	v, _, err := st.resolve(c.From)
	if err != nil {
		return nil, fmt.Errorf("choices: %w", err)
	}
	items, ok := v.([]any)
	if !ok && v != nil {
		return nil, fmt.Errorf("choices: %s is not a list", c.From.From)
	}
	return &Offered{Outcome: c.Outcome, As: c.member(), Many: c.Many, Items: append([]any{}, items...)}, nil
}

// form is the schema a front draws for o: the pick, one of the items.
func (o *Offered) form(s *agenttool.Schema) *agenttool.Schema {
	item := &agenttool.Schema{Enum: o.Items}
	if o.Many {
		item = &agenttool.Schema{Type: "array", Items: item}
	}
	// The pick's member is the offered one, whatever schema it had.
	if s != nil {
		c := *s
		c.Properties = slices.DeleteFunc(slices.Clone(s.Properties), func(p agenttool.Property) bool { return p.Name == o.As })
		c.Required = slices.DeleteFunc(slices.Clone(s.Required), func(r string) bool { return r == o.As })
		s = &c
	}
	return withMember(s, o.As, item)
}

// holds checks an answer against what its question offered as asked:
// a pick from the recorded list, and words that say something.
func (q Question) holds(res Result) error {
	data, _ := numbers(res.Data).(map[string]any)
	if o := q.Choices; o != nil && res.Outcome == o.Outcome {
		picked, ok := data[o.As]
		if !ok {
			return fmt.Errorf("outcome %q: pick %s", o.Outcome, o.As)
		}
		in := func(v any) bool {
			return slices.ContainsFunc(o.Items, func(item any) bool { return sameJSON(item, v) })
		}
		if !o.Many {
			if !in(picked) {
				return fmt.Errorf("outcome %q: %s is not one of the choices offered", o.Outcome, describeJSON(picked))
			}
			return nil
		}
		list, ok := picked.([]any)
		switch {
		case !ok:
			return fmt.Errorf("outcome %q: %s is a list of choices", o.Outcome, o.As)
		case len(list) == 0:
			return fmt.Errorf("outcome %q: pick at least one", o.Outcome)
		}
		for i, v := range list {
			if !in(v) {
				return fmt.Errorf("outcome %q: %s is not one of the choices offered", o.Outcome, describeJSON(v))
			}
			if slices.ContainsFunc(list[:i], func(w any) bool { return sameJSON(v, w) }) {
				return fmt.Errorf("outcome %q: %s is picked twice", o.Outcome, describeJSON(v))
			}
		}
	}
	if t := q.Text; t != nil && res.Outcome == t.Outcome {
		if s, _ := data[t.As].(string); strings.TrimSpace(s) == "" {
			return fmt.Errorf("outcome %q: write something in %s", t.Outcome, t.As)
		}
	}
	if res.By == ByTimeout && (q.Deadline == nil || res.Outcome != q.OnTimeout) {
		return fmt.Errorf("only a question with a timeout takes its outcome %q by its timeout", q.OnTimeout)
	}
	return nil
}

func describeJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// Words answers q with a person's own words: the outcome its free text
// takes, with them in its data.
func Words(q Question, text string) (Result, error) {
	if q.Text == nil {
		return Result{}, fmt.Errorf("agentflow: step %q takes no free text", q.Step)
	}
	res := Result{Outcome: q.Text.Outcome, Data: map[string]any{q.Text.As: text}}
	return res, q.holds(res)
}

// Pick answers q with items it offered: one, or with Many several.
func Pick(q Question, items ...any) (Result, error) {
	o := q.Choices
	if o == nil {
		return Result{}, fmt.Errorf("agentflow: step %q offers no choices", q.Step)
	}
	var picked any = items
	if !o.Many {
		if len(items) != 1 {
			return Result{}, fmt.Errorf("agentflow: step %q takes one pick", q.Step)
		}
		picked = items[0]
	}
	res := Result{Outcome: o.Outcome, Data: map[string]any{o.As: picked}}
	return res, q.holds(res)
}

// overdue says a question has passed its deadline, at now.
func (q Question) overdue(now time.Time) bool {
	return q.Deadline != nil && q.OnTimeout != "" && !now.Before(*q.Deadline)
}

// timedOut is the answer a question gives itself once its time has run
// out: the node's timeout outcome and data.
func (f *flow) timedOut(q Question) (Result, error) {
	g, err := f.g.find(cmp.Or(q.Workflow, f.g.Name))
	if err != nil {
		return Result{}, err
	}
	n := g.node(q.Step)
	if n == nil {
		return Result{}, fmt.Errorf("agentflow: no step %q", q.Step)
	}
	return Result{Outcome: n.OnTimeout, Data: n.OnTimeoutData, By: ByTimeout,
		Reason: "nobody answered by " + q.Deadline.Format(time.RFC3339)}, nil
}

// expire answers the pending questions whose time has run out with their
// timeout outcomes, nil when none has.
func (f *flow) expire(pending []agentturn.PendingCall) ([]agentturn.Answer, error) {
	var out []agentturn.Answer
	now := time.Now()
	for _, p := range pending {
		if p.Call == nil || p.Call.Name != askTool {
			continue
		}
		var q Question
		if decode([]byte(p.Call.Arguments), &q) != nil || q.Answer != nil || !q.overdue(now) {
			continue
		}
		res, err := f.timedOut(q)
		if err != nil {
			return nil, err
		}
		q.Answer = &res
		args, err := json.Marshal(q)
		if err != nil {
			return nil, err
		}
		out = append(out, agentturn.ApproveWith(p.Call.CallID, args).WithBy("policy").
			WithReason("agentflow: the question's time ran out at "+q.Deadline.Format(time.RFC3339)))
	}
	return out, nil
}

// askClockDue says the program reads the clock before st's question is
// put, so the question's deadline is recorded before anyone sees it.
func (f *flow) askClockDue(ctx context.Context, st State) bool {
	n := st.Node
	return n != nil && n.Kind == Person && n.Timeout > 0 && !st.clocked && st.Failure == "" && st.Reached == nil &&
		st.Held == nil && !f.judging(ctx, st) && !(f.unattended && n.Unattended != "")
}

// clockQuestion is the call that reads the clock for st's question.
func (f *flow) clockQuestion(st State) (id string, args clockArgs) {
	return st.clockID(), clockArgs{Run: st.Instance, Step: st.Node.ID, Timeout: st.Node.Timeout.String()}
}

// answerByTimeout answers a question whose deadline has passed as it is
// asked, nil when it has not.
func (f *flow) answerByTimeout(_ context.Context, q Question) (*Result, error) {
	if q.Retry || q.Limit != "" || !q.overdue(time.Now()) {
		return nil, nil
	}
	res, err := f.timedOut(q)
	if err != nil {
		return nil, err
	}
	return &res, nil
}
