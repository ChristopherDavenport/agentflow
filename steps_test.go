package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// named are the arguments of a script step.
var named = json.RawMessage(`{"name":"step"}`)

// sleepy blocks on its first call until its context ends, and returns at
// once after.
func sleepy(calls *int) agenttool.Tool {
	return agenttool.New("sleepy", "Take a while.", func(ctx context.Context, _ struct{}) (Result, error) {
		*calls++
		if *calls == 1 {
			<-ctx.Done()
			return Result{}, ctx.Err()
		}
		return Result{Outcome: OK}, nil
	})
}

func TestAToolStepTimesOut(t *testing.T) {
	g := &Graph{Name: "slow", Nodes: []Node{
		{ID: "wait", Kind: Run, Tool: "sleepy", Timeout: 30 * time.Millisecond, Next: map[string]string{OK: "done"}},
		{ID: "done", Kind: Run, Tool: "script", Args: named},
	}}
	var calls int
	var ran []string
	a := nestedAgent(t, g, &fakeModel{}, []agenttool.Tool{sleepy(&calls), scriptTool(&ran)})
	end := start(t, a)
	q := question(t, end)
	if !q.Retry || !strings.Contains(q.Question, `step "wait" timed out after 30ms`) {
		t.Fatalf("question = %+v", q)
	}
	if end = reply(t, g, a, end, retryOutcome); end.Reason != agentturn.ReasonDone || calls != 2 || len(ran) != 1 {
		t.Fatalf("end = %s %v, calls %d, ran %v", end.Reason, end.Err, calls, ran)
	}
}

// stall is a model that never answers its first request, and finishes
// the step it is asked about after.
type stall struct {
	fakeModel
	stalled bool
}

func (m *stall) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if !m.stalled {
		m.stalled = true
		m.requests = append(m.requests, req)
		<-ctx.Done()
		return ctx.Err()
	}
	return m.fakeModel.CreateStream(ctx, req, sink)
}

func TestAModelStepTimesOut(t *testing.T) {
	g := &Graph{Name: "think", Nodes: []Node{
		{ID: "work", Kind: Agent, Prompt: "Work.", Timeout: 40 * time.Millisecond},
	}}
	ctx := context.Background()
	m := &stall{fakeModel: fakeModel{outcomes: []string{OK}}}
	a := nestedAgent(t, g, m, nil)
	end, _, err := Drive(ctx, g, a, run(a))
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, end)
	if !q.Retry || !strings.Contains(q.Question, "ran past its timeout of 40ms") {
		t.Fatalf("question = %+v", q)
	}
	// The deadline is a recorded result, which replay reads.
	var clock, timeout bool
	for _, item := range a.State().Transcript {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == clockTool {
			clock = clock || strings.HasSuffix(c.CallID, "-clock")
			timeout = timeout || strings.HasSuffix(c.CallID, "-timeout")
		}
	}
	if !clock || !timeout {
		t.Errorf("clock read %v, timeout said %v", clock, timeout)
	}
	// Retried, the step gets a deadline of its own and finishes.
	ans, err := g.Answer(end.Pending[0], Result{Outcome: retryOutcome})
	if err != nil {
		t.Fatal(err)
	}
	next, err := a.Resume(ctx, ans)
	if next == nil {
		t.Fatal(err)
	}
	end, st, err := Drive(ctx, g, a, next)
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if st.Steps[0].Visit != 1 {
		t.Errorf("steps = %+v", st.Steps)
	}
}

func TestAnUnattendedModelStepHasATimeout(t *testing.T) {
	g := &Graph{Name: "think", Nodes: []Node{{ID: "work", Kind: Agent, Prompt: "Work."}}}
	a := nestedAgent(t, g, &stall{}, nil, Unattended(), UnattendedTimeout(30*time.Millisecond))
	end, _, err := Drive(context.Background(), g, a, run(a))
	if err == nil || !strings.Contains(err.Error(), "ran past its timeout of 30ms") {
		t.Fatalf("end = %+v, err = %v", end, err)
	}
	runs, err := Runs(a.State().Transcript, under(a))
	if err != nil || len(runs) != 1 || runs[0].Status != Failed {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	// Attended, the step sets the only timeout there is.
	if d := (&flow{}).timeout(State{Node: &g.Nodes[0]}); d != 0 {
		t.Errorf("attended timeout = %s", d)
	}
	if d := (&flow{unattended: true}).timeout(State{Node: &g.Nodes[0]}); d != DefaultUnattendedTimeout {
		t.Errorf("unattended timeout = %s", d)
	}
}

func TestStepsAskForTheirModelAndThinking(t *testing.T) {
	g := &Graph{Name: "models", Model: "big", Thinking: "low", Nodes: []Node{
		{ID: "plan", Kind: Agent, Prompt: "Plan.", Next: map[string]string{OK: "check"}},
		{ID: "check", Kind: Agent, Prompt: "Check.", Model: "small", Thinking: "max"},
	}}
	m := &fakeModel{outcomes: []string{OK, OK}}
	a := nestedAgent(t, g, m, nil, Models("big", "small", "fake"))
	if end, _, err := Drive(t.Context(), g, a, start(t, a)); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(m.requests) != 2 {
		t.Fatalf("requests = %d", len(m.requests))
	}
	if r := m.requests[0]; r.Model != "big" || r.Reasoning.Effort != openresponses.ReasoningEffortLow {
		t.Errorf("plan asked %s at %q", r.Model, r.Reasoning.Effort)
	}
	if r := m.requests[1]; r.Model != "small" || r.Reasoning.ExtendedEffort() != openresponses.EffortMax {
		t.Errorf("check asked %s at %q", r.Model, r.Reasoning.ExtendedEffort())
	}
	if _, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake"}, Models("big")); err == nil || !strings.Contains(err.Error(), `"small"`) {
		t.Errorf("err = %v", err)
	}
	g.Nodes[1].Thinking = "lots"
	if err := g.Validate(); err == nil || !strings.Contains(err.Error(), "thinking") {
		t.Errorf("err = %v", err)
	}
	// A fresh step's child asks for them too.
	g = &Graph{Name: "models", Thinking: "high", Nodes: []Node{{ID: "plan", Kind: Agent, Prompt: "Plan.", Context: ContextFresh}}}
	m = &fakeModel{outcomes: []string{OK}}
	cfg, err := kinApply(g, agentturn.Config{Model: m, ModelName: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	a = agentturn.New(cfg)
	if end, _, err := Drive(context.Background(), g, a, start(t, a)); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(m.requests) != 1 || m.requests[0].Reasoning.Effort != openresponses.ReasoningEffortHigh {
		t.Errorf("requests = %+v", m.requests)
	}
}

func TestAnotherModelRunsInAChild(t *testing.T) {
	g := &Graph{Name: "models", Model: "big", Nodes: []Node{
		{ID: "plan", Kind: Agent, Prompt: "Plan.", Model: "big", Next: map[string]string{OK: "check"}},
		{ID: "check", Kind: Agent, Prompt: "Check.", Model: "small", Next: map[string]string{OK: "again"}},
		{ID: "again", Kind: Agent, Prompt: "Again.", Model: "small", Context: ContextFresh},
	}}
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	// Decided by the definition: the workflow's own model stays, and
	// another goes to a child, said or not.
	if g.node("plan").fresh() || !g.node("check").fresh() || !g.node("again").fresh() {
		t.Fatalf("fresh: plan %v, check %v, again %v", g.node("plan").fresh(), g.node("check").fresh(), g.node("again").fresh())
	}
	// With no default, any model a step names is another.
	none := &Graph{Name: "models", Nodes: []Node{{ID: "check", Kind: Agent, Prompt: "Check.", Model: "small"}}}
	if err := none.Validate(); err != nil || !none.node("check").fresh() {
		t.Fatalf("no default: %v, fresh %v", err, none.node("check").fresh())
	}
	m := &fakeModel{outcomes: []string{OK, OK, OK}}
	kids := newKin(m, "fake")
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake"}, kids.option())
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if end, _, err := Drive(t.Context(), g, a, start(t, a)); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(kids.agents) != 2 {
		t.Errorf("children = %d, want one each for check and again", len(kids.agents))
	}
	var models []string
	for _, r := range m.requests {
		models = append(models, r.Model)
	}
	if !slices.Equal(models, []string{"big", "small", "small"}) {
		t.Errorf("models = %v", models)
	}
}

// touching records what each tool was asked to touch.
func touching(did *[]string) []agenttool.Tool {
	tool := func(name string) agenttool.Tool {
		return agenttool.NewFunc(name, name, json.RawMessage(`{"type":"object"}`), func(_ context.Context, call agenttool.Call) (agenttool.Result, error) {
			*did = append(*did, name+" "+string(call.Args))
			return agenttool.Text("done"), nil
		})
	}
	return []agenttool.Tool{tool("read"), tool("write"), tool("bash")}
}

// shell splits a command at the operators that join commands, and
// refuses one holding a substitution, which no rule can see into: what a
// host's splitter does, since agentpolicy holds no shell parser.
func shell(_ context.Context, args json.RawMessage) ([]agentpolicy.Subject, error) {
	var a struct{ Command string }
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, err
	}
	if strings.Contains(a.Command, "$(") || strings.Contains(a.Command, "`") {
		return nil, errors.New("a substitution cannot be read")
	}
	var out []agentpolicy.Subject
	for _, part := range strings.FieldsFunc(a.Command, func(r rune) bool { return r == '&' || r == '|' || r == ';' || r == '\n' }) {
		if part = strings.TrimSpace(part); part != "" {
			b, _ := json.Marshal(map[string]string{"command": part})
			out = append(out, agentpolicy.Subject{Args: b, Text: part})
		}
	}
	return out, nil
}

// matchers are the host's: paths below each child's dir, and commands
// split by shell.
var matchers = PolicyMatchers(func(dir string) map[string]agentpolicy.ToolMatcher {
	return map[string]agentpolicy.ToolMatcher{
		"read":  {Match: InDir(dir, "path")},
		"write": {Match: InDir(dir, "path")},
		"bash":  {Match: agentpolicy.GlobMatcher("command"), Subjects: shell},
	}
})

func ruleList(list ...string) []PolicyRule {
	out := []PolicyRule{}
	for _, r := range list {
		out = append(out, PolicyRule{Rule: r})
	}
	return out
}

var limited = &Policy{
	Allow: ruleList("read", "write(src/*)", "bash(go test:*)"),
	Deny: append(ruleList("read(.env)", "write(.env)", "bash(go test -exec:*)"),
		PolicyRule{Rule: "write(src/gen/*)", Note: "generated; run go generate"}),
}

// refusals are the outputs of the calls a step refused.
func refusals(t agentturn.Transcript) []string {
	var out []string
	for _, item := range t {
		if o, ok := item.(*openresponses.FunctionCallOutput); ok && strings.Contains(o.Output.String(), "refuses this call") {
			out = append(out, o.Output.String())
		}
	}
	return out
}

func TestAPolicyLimitsAStepsTools(t *testing.T) {
	g := &Graph{Name: "edit", Nodes: []Node{
		{ID: "edit", Kind: Agent, Prompt: "Edit.", Tools: []string{"read", "write", "bash"}, Policy: limited},
	}}
	var did []string
	m := &cast{script: []says{
		{"write", `{"path":"src/gen/a.go"}`},
		{"read", `{"path":".env"}`},
		{"bash", `{"command":"go test ./... && git push"}`},
		{"bash", `{"command":"go test -exec evil ./..."}`},
		{"write", `{"path":"docs/../src/a.go"}`},
		{"write", `{"path":"../a.go"}`},
		{"bash", `{"command":"go test $(cat list)"}`},
		{"bash", `{"command":"go test ./..."}`},
		{finishTool, `{"step":"edit","outcome":"ok"}`},
	}}
	a := nestedAgent(t, g, m, touching(&did), matchers)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	if want := []string{`write {"path":"docs/../src/a.go"}`, `bash {"command":"go test ./..."}`}; !slices.Equal(did, want) {
		t.Errorf("did %v, want %v", did, want)
	}
	got := refusals(a.State().Transcript)
	for i, want := range []string{
		`the policy of step "edit" of edit: denied by write(src/gen/*): generated; run go generate`,
		`denied by read(.env)`,
		// A call outside the step's allow-list names that list, the rule
		// that refused it. A live trial found only "denied by default".
		`no rule allows bash: denied by default on "git push"`,
		`denied by bash(go test -exec:*)`,
		`no rule allows write: denied by default; it allows only calls matching read, write(src/*), bash(go test:*)`,
		`could not be evaluated: a substitution cannot be read`,
	} {
		if i >= len(got) || !strings.Contains(got[i], want) {
			t.Errorf("refusal %d = %q, want %q", i, got[min(i, len(got)-1)], want)
		}
	}
	if len(got) != 6 || !strings.HasSuffix(got[2], "; it allows only calls matching read, write(src/*), bash(go test:*)") {
		t.Errorf("refusals = %q", got)
	}
	// The model was told its limits before it tried, notes and all.
	if in := m.requests[0].Instructions; !strings.Contains(in, "Your tools are limited") ||
		!strings.Contains(in, `only calls matching read, write(src/*), bash(go test:*)`) || !strings.Contains(in, "write(src/gen/*) (generated; run go generate)") {
		t.Errorf("instructions = %s", in)
	}

	// What agentpolicy cannot read is a load error: a specifier for a tool
	// the host gave no matcher, a rule for a tool the step does not offer,
	// and a rule that is not one.
	if _, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: touching(&did)}); err == nil || !strings.Contains(err.Error(), "no matcher") {
		t.Errorf("err = %v", err)
	}
	for policy, want := range map[*Policy]string{
		{Deny: ruleList("fetch")}:      "the step offers no tool fetch",
		{Deny: ruleList("bash(")}:      `rule "bash("`,
		{Deny: ruleList("read write")}: "want one rule to an entry",
		{Allow: []PolicyRule{}}:        "allows nothing",
	} {
		g.Nodes[0].Policy = policy
		if err := g.Validate(); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: err = %v, want %q", policy, err, want)
		}
	}
}

func TestAPolicyOnlyNarrowsTheHosts(t *testing.T) {
	// The step allows any write below src; the host refuses one of them.
	// The host's refusal stands: a workflow cannot allow what the host
	// does not.
	g := &Graph{Name: "edit", Nodes: []Node{
		{ID: "edit", Kind: Agent, Prompt: "Edit.", Tools: []string{"write"}, Policy: &Policy{Allow: ruleList("write(src/*)")}},
	}}
	var did []string
	m := &cast{script: []says{
		{"write", `{"path":"src/locked.go"}`},
		{"write", `{"path":"src/a.go"}`},
		{finishTool, `{"step":"edit","outcome":"ok"}`},
	}}
	host := func(_ context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		if strings.Contains(string(info.Args), "locked") {
			return &agentturn.ToolDecision{Action: agentturn.Block, Reason: "host: locked"}, nil
		}
		return nil, nil
	}
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: touching(&did), BeforeToolCall: host}, matchers)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	if want := []string{`write {"path":"src/a.go"}`}; !slices.Equal(did, want) {
		t.Errorf("did %v, want %v", did, want)
	}
}

func TestACallsPolicyHoldsWhatItCalls(t *testing.T) {
	inner := &Graph{Name: "inner", Nodes: []Node{
		{ID: "edit", Kind: Agent, Prompt: "Edit.", Tools: []string{"write"}},
	}}
	// In the current context, in a child, and in a child at a dir, where
	// the rule is still read below the workspace's root.
	for _, c := range []struct{ where, dir, path string }{{"", "", "secret/key"}, {ContextFresh, "", "secret/key"}, {ContextFresh, "secret", "key"}} {
		t.Run("context "+c.where+" dir "+c.dir, func(t *testing.T) {
			g := linked(t, &Graph{Name: "outer", Nodes: []Node{
				{ID: "call", Kind: Run, Flow: "inner", Context: c.where, Dir: literalDir(c.dir), Policy: &Policy{Deny: ruleList("write(secret/*)")}},
			}}, inner)
			var did []string
			m := &cast{script: []says{
				{"write", `{"path":"` + c.path + `"}`},
				{finishTool, `{"step":"edit","outcome":"ok"}`},
			}}
			kids := newKin(m, "fake", touching(&did)...)
			cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: touching(&did)}, matchers, kids.option())
			if err != nil {
				t.Fatal(err)
			}
			a := agentturn.New(cfg)
			end, _, err := Drive(t.Context(), g, a, start(t, a))
			if err != nil || end.Reason != agentturn.ReasonDone || len(did) != 0 {
				t.Fatalf("end = %+v, %v, did %v", end, err, did)
			}
			var refused []string
			for _, child := range kids.agents {
				refused = append(refused, refusals(child.State().Transcript)...)
			}
			refused = append(refused, refusals(a.State().Transcript)...)
			if len(refused) != 1 || !strings.Contains(refused[0], `the policy of step "call" of outer: denied by write(secret/*)`) {
				t.Errorf("refused = %v", refused)
			}
		})
	}
}

func TestAStuckModelRoutesOrFails(t *testing.T) {
	stuck := says{stuckTool, `{"step":"work","reason":"no access to the repository"}`}
	routed := &Graph{Name: "stuck", Nodes: []Node{
		{ID: "work", Kind: Agent, Prompt: "Work.", Next: map[string]string{Stuck: "report"}},
		{ID: "report", Kind: Run, Tool: "script", With: map[string]Value{"name": From("steps.work.outputs.reason")}},
	}}
	var ran []string
	a := nestedAgent(t, routed, &cast{script: []says{stuck}}, []agenttool.Tool{scriptTool(&ran)})
	if end := start(t, a); end.Reason != agentturn.ReasonDone || !slices.Equal(ran, []string{"no access to the repository"}) {
		t.Fatalf("end = %s %v, ran %v", end.Reason, end.Err, ran)
	}

	// With nowhere to go, it is a failed step, which a person may retry.
	plain := &Graph{Name: "stuck", Nodes: []Node{{ID: "work", Kind: Agent, Prompt: "Work."}}}
	m := &cast{script: []says{
		{finishTool, `{"step":"work","outcome":"stuck"}`},
		stuck,
		{finishTool, `{"step":"work","outcome":"ok"}`},
	}}
	a = nestedAgent(t, plain, m, nil)
	end := start(t, a)
	q := question(t, end)
	if !q.Retry || !strings.Contains(q.Question, "stuck: no access to the repository") {
		t.Fatalf("question = %+v", q)
	}
	if end = reply(t, plain, a, end, retryOutcome); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	if !strings.Contains(m.requests[0].Instructions, stuckTool) {
		t.Errorf("instructions = %s", m.requests[0].Instructions)
	}
	// The finish naming stuck was refused, and the model told how to say it.
	var told bool
	for _, item := range a.State().Transcript {
		if o, ok := item.(*openresponses.FunctionCallOutput); ok && strings.Contains(o.Output.String(), "to say you are stuck, call flow_stuck") {
			told = true
		}
	}
	if !told {
		t.Error("the model was not told how to say it is stuck")
	}
	// In a child, the child reports it, and the caller decides.
	for name, next := range map[string]map[string]string{"routed": {Stuck: "report"}, "not routed": nil} {
		t.Run("fresh "+name, func(t *testing.T) {
			g := &Graph{Name: "stuck", Nodes: []Node{
				{ID: "work", Kind: Agent, Prompt: "Work.", Context: ContextFresh, Next: next},
				{ID: "report", Kind: Run, Tool: "script", With: map[string]Value{"name": From("steps.work.outputs.reason")}},
			}}
			if next == nil {
				g.Nodes = g.Nodes[:1]
			}
			var ran []string
			m := &cast{script: []says{stuck}}
			cfg, err := kinApply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}})
			if err != nil {
				t.Fatal(err)
			}
			a := agentturn.New(cfg)
			end, _, err := Drive(t.Context(), g, a, start(t, a))
			if next != nil {
				if err != nil || end.Reason != agentturn.ReasonDone || !slices.Equal(ran, []string{"no access to the repository"}) {
					t.Fatalf("end = %+v, %v, ran %v", end, err, ran)
				}
				return
			}
			if q := question(t, end); !q.Retry || !strings.Contains(q.Question, "stuck: no access to the repository") {
				t.Fatalf("question = %+v", q)
			}
		})
	}
	// A step that declares stuck is refused.
	plain.Nodes[0].Outcomes = []string{OK, Stuck}
	if err := plain.Validate(); err == nil || !strings.Contains(err.Error(), "flow_stuck reports") {
		t.Errorf("err = %v", err)
	}
}

// constant is a script that fails the same way every time.
func constant(ran *int) agenttool.Tool {
	return agenttool.New("lint", "Lint.", func(context.Context, struct{}) (Result, error) {
		*ran++
		return Result{Outcome: "fail", Data: map[string]any{"errors": 3}}, nil
	})
}

func lintLoop() *Graph {
	return &Graph{Name: "lint", Nodes: []Node{
		{ID: "lint", Kind: Run, Tool: "lint", Outcomes: []string{"pass", "fail"}, Next: map[string]string{"fail": "fix"}, MaxVisits: 20},
		{ID: "fix", Kind: Agent, Prompt: "Fix it.", Next: map[string]string{OK: "lint"}, MaxVisits: 20},
	}}
}

func TestARepeatingScriptStopsItsLoop(t *testing.T) {
	g := lintLoop()
	var ran int
	m := &fakeModel{outcomes: slices.Repeat([]string{OK}, 10)}
	a := nestedAgent(t, g, m, []agenttool.Tool{constant(&ran)})
	end := start(t, a)
	q := question(t, end)
	if q.Limit != "repeat" || q.Step != "fix" || !strings.Contains(q.Question, `step "lint" of workflow "lint" gave the same outcome and data 3 times in a row`) {
		t.Fatalf("question = %+v", q)
	}
	// One more allowed: the next repeat asks again.
	ans, err := g.Answer(end.Pending[0], Result{Outcome: grantOutcome, Data: map[string]any{"amount": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if end, err = a.Resume(t.Context(), ans); end == nil {
		t.Fatal(err)
	}
	if q := question(t, end); q.Limit != "repeat" || ran != 4 {
		t.Fatalf("question = %+v, ran %d", q, ran)
	}
	if end = stopAt(t, g, a, end, "repeat"); !errors.Is(end.Err, ErrRepeat) {
		t.Fatalf("end = %+v", end)
	}
	// Unattended, it fails.
	ran = 0
	a = nestedAgent(t, g, &fakeModel{outcomes: slices.Repeat([]string{OK}, 10)}, []agenttool.Tool{constant(&ran)}, Unattended())
	if end := run(a); !errors.Is(end.Err, ErrRepeat) || ran != 3 {
		t.Fatalf("end = %+v, ran %d", end, ran)
	}
}

func TestAResultWithNoDataDoesNotRepeat(t *testing.T) {
	// A poll that says only "not yet" has nothing to compare: the visit
	// bound holds its loop, and the repeat rule does not.
	g := lintLoop()
	g.Nodes[0].MaxVisits, g.Nodes[1].MaxVisits = 5, 5
	var ran int
	poll := agenttool.New("lint", "Poll.", func(context.Context, struct{}) (Result, error) {
		ran++
		return Result{Outcome: "fail"}, nil
	})
	a := nestedAgent(t, g, &fakeModel{outcomes: slices.Repeat([]string{OK}, 10)}, []agenttool.Tool{poll}, Unattended())
	if end := run(a); !errors.Is(end.Err, ErrVisits) || errors.Is(end.Err, ErrRepeat) || ran != 5 {
		t.Fatalf("end = %+v, ran %d", end, ran)
	}
}

func TestStepsAreBoundedAcrossTheTree(t *testing.T) {
	chain := &Graph{Name: "chain", MaxSteps: 2, Nodes: []Node{
		{ID: "a", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "b"}},
		{ID: "b", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "c"}},
		{ID: "c", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "d"}},
		{ID: "d", Kind: Run, Tool: "script", Args: named},
	}}
	var ran []string
	a := nestedAgent(t, chain, &fakeModel{}, []agenttool.Tool{scriptTool(&ran)})
	end := start(t, a)
	if q := question(t, end); q.Limit != "steps" || q.Step != "c" || q.Form[grantOutcome] == nil {
		t.Fatalf("question = %+v", q)
	}
	// Rules may grant it, and the grant is read back from the record.
	rules := Reply(func(_ context.Context, q Question) (Result, bool) {
		return Result{Outcome: grantOutcome, Data: map[string]any{"amount": 5}}, q.Limit == "steps"
	})
	a = nestedAgent(t, chain, &fakeModel{}, []agenttool.Tool{scriptTool(&ran)}, rules)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	st, err := chain.Replay(a.State().Transcript)
	if err != nil || st.Entries != 4 || st.Granted != 5 {
		t.Errorf("entries %d, granted %d, %v", st.Entries, st.Granted, err)
	}
	// The host's bound is the smaller.
	if _, err := Apply(chain, agentturn.Config{Model: &fakeModel{}, ModelName: "fake"}, MaxSteps(-1)); err == nil {
		t.Error("a negative bound was taken")
	}
}

func TestAChildHitsTheTreesBound(t *testing.T) {
	inner := &Graph{Name: "inner", Nodes: []Node{
		{ID: "a", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "b"}},
		{ID: "b", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "c"}},
		{ID: "c", Kind: Run, Tool: "script", Args: named},
	}}
	g := linked(t, &Graph{Name: "outer", Nodes: []Node{
		{ID: "first", Kind: Run, Tool: "script", Args: named, Next: map[string]string{OK: "call"}},
		{ID: "call", Kind: Run, Flow: "inner", Context: ContextFresh, Next: map[string]string{Failed: "never"}},
		{ID: "never", Kind: Run, Tool: "script", Args: named},
	}}, inner)
	var ran []string
	m := &fakeModel{}
	kids := newKin(m, "fake", scriptTool(&ran))
	cfg, err := Apply(g, agentturn.Config{Model: m, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}}, kids.option(), MaxSteps(4), Unattended())
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end, _, err := Drive(t.Context(), g, a, start(t, a))
	if !errors.Is(err, ErrSteps) || len(ran) != 3 {
		t.Fatalf("end = %+v, err = %v, ran %v", end, err, ran)
	}
	if len(kids.opened) != 1 || kids.opened[0].maxSteps != 2 {
		t.Errorf("opened = %+v", kids.opened)
	}
	runs, err := Runs(a.State().Transcript, under(a))
	if err != nil || len(runs) != 1 || runs[0].Status != Failed || !strings.Contains(runs[0].Reason, "step limit") {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
}

// literalDir is a dir setting of p, none for "".
func literalDir(p string) Value {
	if p == "" {
		return Value{}
	}
	return Literal(p)
}

// The deadline a model step works to is the program's, read from its
// flow_clock call: the model is not sent that call, which it took for
// one of its own in a live trial.
func TestAModelIsNotSentTheClock(t *testing.T) {
	g := &Graph{Name: "think", Nodes: []Node{{ID: "work", Kind: Agent, Prompt: "Work.", Timeout: time.Minute}}}
	m := &fakeModel{outcomes: []string{OK}}
	a := nestedAgent(t, g, m, nil)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %s %v", end.Reason, end.Err)
	}
	clocked := false
	for _, item := range a.State().Transcript {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.Name == clockTool {
			clocked = true
		}
	}
	if !clocked || len(m.requests) == 0 {
		t.Fatalf("clocked %v, %d requests", clocked, len(m.requests))
	}
	for _, r := range m.requests {
		b, _ := json.Marshal(r.Input)
		if strings.Contains(string(b), clockTool) {
			t.Errorf("a model was sent the clock: %s", b)
		}
	}
}
