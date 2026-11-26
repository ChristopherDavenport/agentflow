package agentflow

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
)

var fileList = &agenttool.Schema{Type: "object", Required: []string{"files"}, Properties: []agenttool.Property{
	{Name: "files", Schema: &agenttool.Schema{Type: "array", Items: &agenttool.Schema{Type: "string"}}},
}}

// lister lists files, and told reports what it was given.
func lister(told *[]map[string]any) []agenttool.Tool {
	return []agenttool.Tool{
		agenttool.New("list", "List files.", func(context.Context, struct{}) (Result, error) {
			return Result{Outcome: OK, Data: map[string]any{"files": []any{"a.go", "b.go", "c.go"}}}, nil
		}),
		agenttool.New("tell", "Report.", func(_ context.Context, a struct {
			Files any    `json:"files,omitempty"`
			Note  string `json:"note,omitempty"`
			Said  string `json:"said,omitempty"`
		}) (Result, error) {
			*told = append(*told, map[string]any{"files": a.Files, "note": a.Note, "said": a.Said})
			return Result{Outcome: OK}, nil
		}),
	}
}

// picker offers the files listed, and tells what was picked.
func picker(many bool) *Graph {
	return &Graph{Name: "picker", Nodes: []Node{
		{ID: "list", Kind: Run, Tool: "list", Data: map[string]*agenttool.Schema{OK: fileList}, Next: map[string]string{OK: "pick"}},
		{ID: "pick", Kind: Person, Prompt: "Which files?", Outcomes: []string{"chosen", "none"},
			Choices: &Choices{From: From("steps.list.outputs.files"), Many: many, Outcome: "chosen", As: "files"},
			Next:    map[string]string{"chosen": "tell"}},
		{ID: "tell", Kind: Run, Tool: "tell", With: map[string]Value{"files": From("steps.pick.outputs.files")}},
	}}
}

func answerWith(t *testing.T, g *Graph, a *agentturn.Agent, end *agentturn.RunEnd, res Result) *agentturn.RunEnd {
	t.Helper()
	ans, err := g.Answer(end.Pending[0], res)
	if err != nil {
		t.Fatal(err)
	}
	next, err := a.Resume(context.Background(), ans)
	if next == nil {
		t.Fatal(err)
	}
	return next
}

func TestChoicesComeFromAnEarlierResult(t *testing.T) {
	var told []map[string]any
	g := picker(true)
	a := nestedAgent(t, g, &fakeModel{}, lister(&told))
	end := run(a)
	q := question(t, end)
	if q.Choices == nil || !q.Choices.Many || q.Choices.As != "files" || !slices.Equal(q.Choices.Items, []any{"a.go", "b.go", "c.go"}) {
		t.Fatalf("choices = %+v", q.Choices)
	}
	// The form a front draws holds the pick to the list.
	form, ok := q.Form["chosen"].(map[string]any)
	if !ok || !strings.Contains(describeJSON(form), `"enum":["a.go","b.go","c.go"]`) {
		t.Errorf("form = %v", describeJSON(q.Form))
	}
	// A pick from outside the list is refused, and the question waits.
	for _, bad := range []any{[]any{"d.go"}, []any{"a.go", "a.go"}, []any{}, "a.go"} {
		if _, err := g.Answer(end.Pending[0], Result{Outcome: "chosen", Data: map[string]any{"files": bad}}); err == nil {
			t.Errorf("answer %v was taken", bad)
		}
	}
	if _, err := Pick(q, "d.go"); err == nil {
		t.Error("Pick took a file it was not offered")
	}
	res, err := Pick(q, "c.go", "a.go")
	if err != nil {
		t.Fatal(err)
	}
	end = answerWith(t, g, a, end, res)
	if end.Reason != agentturn.ReasonDone || len(told) != 1 || !slices.Equal(told[0]["files"].([]any), []any{"c.go", "a.go"}) {
		t.Fatalf("end = %+v, told %v", end, told)
	}
}

func TestOneChoice(t *testing.T) {
	var told []map[string]any
	g := picker(false)
	a := nestedAgent(t, g, &fakeModel{}, lister(&told))
	end := run(a)
	q := question(t, end)
	if q.Choices == nil || q.Choices.Many {
		t.Fatalf("choices = %+v", q.Choices)
	}
	if _, err := Pick(q, "a.go", "b.go"); err == nil {
		t.Error("two picks were taken for one")
	}
	res, err := Pick(q, "b.go")
	if err != nil {
		t.Fatal(err)
	}
	if end = answerWith(t, g, a, end, res); end.Reason != agentturn.ReasonDone || told[0]["files"] != "b.go" {
		t.Fatalf("end = %+v, told %v", end, told)
	}
}

// The host's rules may pick, and are held to the list as a person is.
func TestRulesPickFromTheList(t *testing.T) {
	var told []map[string]any
	g := picker(false)
	picks := []any{"z.go", "a.go"}
	var tried []any
	rules := Reply(func(_ context.Context, q Question) (Result, bool) {
		pick := picks[len(tried)]
		tried = append(tried, pick)
		return Result{Outcome: "chosen", Data: map[string]any{"files": pick}}, true
	})
	a := nestedAgent(t, g, &fakeModel{}, lister(&told), rules)
	// A rule that picks from outside the list leaves the question to the
	// person.
	end := run(a)
	if q := question(t, end); q.Step != "pick" {
		t.Fatalf("question = %+v", q)
	}
	end = answerWith(t, g, a, end, Result{Outcome: "chosen", Data: map[string]any{"files": "c.go"}})
	if end.Reason != agentturn.ReasonDone || told[0]["files"] != "c.go" {
		t.Fatalf("end = %+v, told %v", end, told)
	}
	a = nestedAgent(t, g, &fakeModel{}, lister(&told), rules)
	if end = run(a); end.Reason != agentturn.ReasonDone || told[1]["files"] != "a.go" {
		t.Fatalf("end = %+v, told %v", end, told)
	}
}

// reviewer asks go or change, where change takes the person's words.
func reviewer() *Graph {
	return &Graph{Name: "reviewer", Nodes: []Node{
		{ID: "ask", Kind: Person, Prompt: "Ship it?", Outcomes: []string{"go", "change"},
			Text: &FreeText{Outcome: "change", As: "note"}, Next: map[string]string{"change": "tell"}},
		{ID: "tell", Kind: Run, Tool: "tell", With: map[string]Value{"note": From("steps.ask.outputs.note")}},
	}}
}

func TestFreeTextRoutesToItsOutcome(t *testing.T) {
	var told []map[string]any
	g := reviewer()
	a := nestedAgent(t, g, &fakeModel{}, lister(&told))
	end := run(a)
	q := question(t, end)
	if q.Text == nil || q.Text.Outcome != "change" || q.Text.As != "note" {
		t.Fatalf("text = %+v", q.Text)
	}
	if _, err := Words(q, "   "); err == nil {
		t.Error("blank words were taken")
	}
	if _, err := g.Answer(end.Pending[0], Result{Outcome: "change"}); err == nil {
		t.Error("change was taken without words")
	}
	res, err := Words(q, "rename the flag first")
	if err != nil {
		t.Fatal(err)
	}
	end = answerWith(t, g, a, end, res)
	if end.Reason != agentturn.ReasonDone || told[0]["note"] != "rename the flag first" {
		t.Fatalf("end = %+v, told %v", end, told)
	}
}

// waiter asks, and after its timeout parks.
func waiter(timeout time.Duration) *Graph {
	return &Graph{Name: "waiter", Nodes: []Node{
		{ID: "ask", Kind: Person, Prompt: "Merge it?", Outcomes: []string{"merge", "park"},
			Data:    map[string]*agenttool.Schema{"park": one("why", true)},
			Timeout: timeout, OnTimeout: "park", OnTimeoutData: map[string]any{"why": "nobody answered"},
			Next: map[string]string{"park": "tell"}},
		{ID: "tell", Kind: Run, Tool: "tell", With: map[string]Value{"said": From("steps.ask.outputs.why")}},
	}}
}

func TestAQuestionTimesOut(t *testing.T) {
	ctx := context.Background()
	var told []map[string]any
	g := waiter(80 * time.Millisecond)
	a := nestedAgent(t, g, &fakeModel{}, lister(&told))
	end := run(a)
	q := question(t, end)
	if q.Deadline == nil || q.OnTimeout != "park" || time.Until(*q.Deadline) > 80*time.Millisecond {
		t.Fatalf("question = %+v", q)
	}
	// Driving before the deadline leaves it waiting.
	if again, _, err := Drive(ctx, g, a, nil); err != nil || question(t, again).Step != "ask" {
		t.Fatalf("drive: %v, %+v", err, again)
	}
	time.Sleep(time.Until(*q.Deadline))
	// Too late for the person; the run takes its timeout outcome.
	if _, err := g.Answer(end.Pending[0], Result{Outcome: "merge"}); !errors.Is(err, ErrExpired) {
		t.Errorf("a late answer: %v", err)
	}
	if _, err := g.Answer(end.Pending[0], Result{Outcome: "park", Data: map[string]any{"why": "x"}, By: ByTimeout}); err == nil {
		t.Error("a person answered by the timeout")
	}
	end, st, err := Drive(ctx, g, a, nil)
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("drive: %v, %+v", err, end)
	}
	if told[0]["said"] != "nobody answered" || len(st.Steps) != 2 || st.Steps[0].Result.By != ByTimeout {
		t.Errorf("told %v, steps %+v", told, st.Steps)
	}
}

// A question asked when its deadline has already passed, as after a long
// wait between the clock and the question, takes its outcome at once.
func TestAQuestionAskedLateTimesOutAtOnce(t *testing.T) {
	var told []map[string]any
	g := waiter(time.Nanosecond)
	a := nestedAgent(t, g, &fakeModel{}, lister(&told))
	if end := run(a); end.Reason != agentturn.ReasonDone || told[0]["said"] != "nobody answered" {
		t.Fatalf("end = %+v, told %v", end, told)
	}
}

// Unattended, a question with an unattended outcome takes it at once,
// with no clock read.
func TestUnattendedDoesNotWaitForATimeout(t *testing.T) {
	var told []map[string]any
	g := waiter(time.Hour)
	g.Nodes[0].Unattended, g.Nodes[0].UnattendedData = "park", map[string]any{"why": "unattended"}
	a := nestedAgent(t, g, &fakeModel{}, lister(&told), Unattended())
	end := run(a)
	if end.Reason != agentturn.ReasonDone || told[0]["said"] != "unattended" {
		t.Fatalf("end = %+v, told %v", end, told)
	}
	for _, item := range a.State().Transcript {
		if strings.Contains(describeJSON(item), clockTool) {
			t.Errorf("read the clock: %s", describeJSON(item))
		}
	}
}

const timeoutEnv = "AGENTFLOW_TIMEOUT_STORE"

// The question is pending when the process dies; the deadline passes
// while nothing runs; a new process, with the store and the definitions,
// takes the timeout outcome.
func TestATimeoutIsReachedAfterAResume(t *testing.T) {
	g := func() *Graph { return waiter(300 * time.Millisecond) }
	var told []map[string]any
	if dir := os.Getenv(timeoutEnv); dir != "" {
		ctx := context.Background()
		store, err := jsonl.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
		if err != nil {
			t.Fatal(err)
		}
		a := nestedAgent(t, g(), &fakeModel{}, lister(&told))
		rec.Attach(a)
		if q := question(t, run(a)); q.Deadline == nil {
			t.Fatalf("question = %+v", q)
		}
		if err := store.Sync(ctx, s.ID()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir+"/id", []byte(s.ID()), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		select {}
	}
	ctx := context.Background()
	dir := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestATimeoutIsReachedAfterAResume$")
	child.Env = append(os.Environ(), timeoutEnv+"="+dir)
	out, err := child.CombinedOutput()
	id, readErr := os.ReadFile(dir + "/id")
	if err == nil || readErr != nil {
		t.Fatalf("child: err = %v, read = %v\n%s", err, readErr, out)
	}
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Resume(ctx, store, string(id))
	if err != nil {
		t.Fatal(err)
	}
	seed, err := session.AgentOptions(s, rec.ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	graph := g()
	a := agentturn.New(must(Apply(graph, agentturn.Config{Model: &fakeModel{}, ModelName: "fake", Tools: lister(&told)})), seed...)
	rec.Attach(a)
	pending := a.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %+v", pending)
	}
	q := question(t, &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: pending})
	if q.Deadline == nil {
		t.Fatalf("question = %+v", q)
	}
	time.Sleep(time.Until(*q.Deadline))
	end, _, err := Drive(ctx, graph, a, nil)
	if err != nil || end.Reason != agentturn.ReasonDone || len(told) != 1 || told[0]["said"] != "nobody answered" {
		t.Fatalf("drive: %v, %+v, told %v", err, end, told)
	}
	got, err := store.Open(ctx, string(id))
	if err != nil {
		t.Fatal(err)
	}
	checkHashes(t, got)
	runs, err := Runs(pathItems(got), nil)
	if err != nil || len(runs) != 1 || runs[0].Status != Done || runs[0].Problem != "" {
		t.Fatalf("runs = %+v, %v", runs, err)
	}
	if steps := runs[0].State.Steps; len(steps) != 2 || steps[0].Result.By != ByTimeout {
		t.Errorf("steps = %+v", steps)
	}
}

// A question in a fresh child times out there, and the caller goes on
// with what the child came to.
func TestATimeoutInsideAChild(t *testing.T) {
	ctx := context.Background()
	var told []map[string]any
	inner := waiter(80 * time.Millisecond)
	inner.Nodes[0].Next = nil
	inner.Nodes = inner.Nodes[:1]
	inner.OutputsBy = map[string]map[string]Value{"merge": {}, "park": {"why": From("steps.ask.outputs.why")}}
	caller := &Graph{Name: "caller", Nodes: []Node{
		{ID: "call", Kind: Run, Flow: "waiter", Context: ContextFresh, Next: map[string]string{"park": "tell"}},
		{ID: "tell", Kind: Run, Tool: "tell", With: map[string]Value{"said": From("steps.call.outputs.why")}},
	}}
	g := linked(t, caller, inner)
	m := &fakeModel{}
	kids := newKin(m, "fake", lister(&told)...)
	a := nestedAgent(t, g, m, lister(&told), kids.option())
	end, _, err := Drive(ctx, g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, end)
	if q.Workflow != "waiter" || q.Deadline == nil || len(q.Path) != 2 {
		t.Fatalf("question = %+v", q)
	}
	time.Sleep(time.Until(*q.Deadline))
	end, st, err := Drive(ctx, g, a, nil)
	if err != nil || end.Reason != agentturn.ReasonDone || len(told) != 1 || told[0]["said"] != "nobody answered" {
		t.Fatalf("drive: %v, %+v, told %v", err, end, told)
	}
	if st.Workflow != "caller" || len(st.Steps) != 2 || st.Steps[0].Result.Outcome != "park" {
		t.Errorf("state = %+v", st.Steps)
	}
}

func TestQuestionSettingsAreChecked(t *testing.T) {
	list := Node{ID: "list", Kind: Run, Tool: "list", Data: map[string]*agenttool.Schema{OK: fileList}, Next: map[string]string{OK: "ask"}}
	ask := func(edit func(n *Node)) *Graph {
		n := Node{ID: "ask", Kind: Person, Prompt: "Which?", Outcomes: []string{"chosen", "change", "park"}}
		edit(&n)
		return &Graph{Name: "checked", Nodes: []Node{list, n}}
	}
	pick := func() *Choices { return &Choices{From: From("steps.list.outputs.files"), Outcome: "chosen"} }
	for want, g := range map[string]*Graph{
		"only a person node offers choices": {Name: "checked", Nodes: []Node{
			{ID: "work", Kind: Agent, Prompt: "Work.", Choices: pick()},
		}},
		`choices: outcome "picked" is not declared`: ask(func(n *Node) { n.Choices = &Choices{From: From("steps.list.outputs.files"), Outcome: "picked"} }),
		"choices: from is a reference":              ask(func(n *Node) { n.Choices = &Choices{From: Literal([]any{"a"}), Outcome: "chosen"} }),
		"is object, not a list":                     ask(func(n *Node) { n.Choices = &Choices{From: From("steps.list.outputs"), Outcome: "chosen"} }),
		`declares "choice", which choices fills`: ask(func(n *Node) {
			n.Choices, n.Data = pick(), map[string]*agenttool.Schema{"chosen": one("choice", true)}
		}),
		`unattended cannot be "chosen"`:          ask(func(n *Node) { n.Choices, n.Unattended = pick(), "chosen" }),
		`on_timeout cannot be "chosen"`:          ask(func(n *Node) { n.Choices, n.Timeout, n.OnTimeout = pick(), time.Hour, "chosen" }),
		`text: outcome "said" is not declared`:   ask(func(n *Node) { n.Text = &FreeText{Outcome: "said"} }),
		`text: outcome "chosen" is the choices'`: ask(func(n *Node) { n.Choices, n.Text = pick(), &FreeText{Outcome: "chosen"} }),
		`requires why, which words alone do not`: ask(func(n *Node) {
			n.Text, n.Data = &FreeText{Outcome: "change"}, map[string]*agenttool.Schema{"change": one("why", true)}
		}),
		"a question's timeout needs on_timeout": ask(func(n *Node) { n.Timeout = time.Hour }),
		"on_timeout needs a timeout":            ask(func(n *Node) { n.OnTimeout = "park" }),
		`on_timeout: outcome "gone" is not`:     ask(func(n *Node) { n.Timeout, n.OnTimeout = time.Hour, "gone" }),
		"on_timeout data": ask(func(n *Node) {
			n.Timeout, n.OnTimeout, n.OnTimeoutData = time.Hour, "park", map[string]any{"x": 1}
			n.Data = map[string]*agenttool.Schema{"park": one("why", true)}
		}),
		"unattended data": ask(func(n *Node) { n.Text, n.Unattended = &FreeText{Outcome: "change"}, "change" }),
		`judge: "change" takes a person's own words`: ask(func(n *Node) {
			n.Text, n.Judge = &FreeText{Outcome: "change"}, &Judge{Criteria: "Weigh it.", Outcomes: []string{"change"}}
		}),
		`judge: "chosen" is a pick of the choices`: ask(func(n *Node) {
			n.Choices, n.Judge = pick(), &Judge{Criteria: "Weigh it.", Outcomes: []string{"chosen"}}
		}),
		"some path gets here without running": {Name: "checked", Nodes: []Node{
			{ID: "ask", Kind: Person, Prompt: "Which?", Outcomes: []string{"chosen"}, Choices: &Choices{From: From("steps.list.outputs.files"), Outcome: "chosen"},
				Next: map[string]string{"chosen": "list"}},
			{ID: "list", Kind: Run, Tool: "list", Data: map[string]*agenttool.Schema{OK: fileList}},
		}},
	} {
		err := g.Validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q, got %v", want, err)
		}
	}
	// What a pick and words give is certain where they lead.
	fine := ask(func(n *Node) {
		n.Choices, n.Text = pick(), &FreeText{Outcome: "change"}
		n.Next = map[string]string{"chosen": "use", "change": "use"}
	})
	fine.Nodes = append(fine.Nodes, Node{ID: "use", Kind: Run, Tool: "tell", With: map[string]Value{
		"files": Optional("steps.ask.outputs.choice"), "note": Optional("steps.ask.outputs.text")}})
	if err := fine.Validate(); err != nil {
		t.Error(err)
	}
}
