package examples

// Live trials of the workflow features against a real model. Every test
// here skips unless AGENTFLOW_LIVE_URL and AGENTFLOW_LIVE_MODEL are set,
// with AGENTFLOW_LIVE_KEY where the server wants one, as the probe in the
// root package reads them. AGENTFLOW_LIVE_TRIALS repeats each scenario,
// 3 unless set, and AGENTFLOW_LIVE_MODEL2 names a second model for the
// scenario that needs one. Expectations are fixed in each test before it
// runs and are read from the record, never from what a model says.

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/script"
	"github.com/ChristopherDavenport/agentflow/workflow"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	agentool "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/agentworkspace"
	"github.com/ChristopherDavenport/openresponses"
)

// meter passes requests to the live model, keeps each one, and counts the
// tokens its responses report.
type meter struct {
	inner openresponses.Streamer
	mu    sync.Mutex
	reqs  []openresponses.Request
	calls int
	in    int
	out   int
}

func (m *meter) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	m.reqs = append(m.reqs, req)
	m.calls++
	m.mu.Unlock()
	return m.inner.CreateStream(ctx, req, openresponses.EventSinkFunc(func(ev openresponses.StreamEvent) error {
		var resp *openresponses.Response
		switch e := ev.(type) {
		case *openresponses.ResponseCompletedEvent:
			resp = e.Response
		case *openresponses.ResponseIncompleteEvent:
			resp = e.Response
		}
		if resp != nil && resp.Usage != nil {
			m.mu.Lock()
			m.in += resp.Usage.InputTokens
			m.out += resp.Usage.OutputTokens
			m.mu.Unlock()
		}
		return sink.Send(ev)
	}))
}

// requests are what was sent, each as its JSON.
func (m *meter) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.reqs {
		b, _ := json.Marshal(r)
		out = append(out, string(b))
	}
	return out
}

// sentAny says some request held s.
func (m *meter) sentAny(s string) bool {
	return slices.ContainsFunc(m.requests(), func(r string) bool { return strings.Contains(r, s) })
}

var (
	totalMu         sync.Mutex
	totalIn, totOut int
	byTest          = map[string]*[3]int{}
)

// live is the model, its name and the number of trials, or a skip.
type liveModel struct {
	adapter openresponses.Streamer
	name    string
	trials  int
}

func live(t *testing.T) liveModel {
	t.Helper()
	url, name := os.Getenv("AGENTFLOW_LIVE_URL"), os.Getenv("AGENTFLOW_LIVE_MODEL")
	if url == "" || name == "" {
		t.Skip("set AGENTFLOW_LIVE_URL and AGENTFLOW_LIVE_MODEL")
	}
	var opts []openresponses.ClientOption
	if k := os.Getenv("AGENTFLOW_LIVE_KEY"); k != "" {
		opts = append(opts, openresponses.WithAPIKey(k))
	}
	trials, _ := strconv.Atoi(os.Getenv("AGENTFLOW_LIVE_TRIALS"))
	if trials < 1 {
		trials = 3
	}
	totalMu.Lock()
	byTest[t.Name()] = &[3]int{}
	totalMu.Unlock()
	t.Cleanup(func() {
		totalMu.Lock()
		u, in, out := *byTest[t.Name()], totalIn, totOut
		totalMu.Unlock()
		t.Logf("tokens for %s: %d requests, %d in, %d out (all live tests so far: %d in, %d out)", t.Name(), u[0], u[1], u[2], in, out)
	})
	return liveModel{adapter: openresponses.NewClient(url, opts...).AsAdapter(), name: name, trials: trials}
}

// meter makes a meter whose tokens the test logs when it ends.
func (l liveModel) meter(t *testing.T) *meter {
	m := &meter{inner: l.adapter}
	t.Cleanup(func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		totalMu.Lock()
		totalIn += m.in
		totOut += m.out
		if u := byTest[t.Name()]; u != nil {
			u[0] += m.calls
			u[1] += m.in
			u[2] += m.out
		}
		totalMu.Unlock()
	})
	return m
}

// base is a config for the live model, bounded.
func (l liveModel) base(m *meter, tools ...agenttool.Tool) agentturn.Config {
	limit := 1500
	cfg := agentturn.Config{Model: m, ModelName: l.name, Tools: tools, MaxTurns: 80}
	cfg.Request.MaxOutputTokens = &limit
	return cfg
}

// kids opens children in memory on the live model, bounded as the
// parent is, with tools.
func (l liveModel) kids(m *meter, tools []agenttool.Tool) agentflow.Option {
	agents := map[string]*agentturn.Agent{}
	return agentflow.Children(func(ctx context.Context, c agentflow.Child) (context.Context, *agentturn.Agent, error) {
		base := l.base(m, tools...)
		base.ModelName = cmp.Or(c.Settings.Model, l.name)
		cfg, err := c.Apply(base)
		if err != nil {
			return nil, nil, err
		}
		if a := agents[c.Spawn]; a != nil {
			return ctx, a, a.SetConfig(cfg)
		}
		a := agentturn.New(cfg)
		agents[c.Spawn] = a
		return ctx, a, nil
	})
}

// parse reads a workflow from its WORKFLOW.md text.
func parse(t *testing.T, name, src string) *agentflow.Graph {
	t.Helper()
	g, err := workflow.Parse(name, []byte(src))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return g
}

// writeSkill writes a skill's files to a new directory and loads it.
func writeSkill(t *testing.T, root, name string, files map[string]string) *agentskill.Skill {
	t.Helper()
	dir := filepath.Join(root, name)
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s, err := agentskill.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// recorder is a tool for program steps that keeps what each call was
// given and returns a fixed result.
type recorder struct {
	mu   sync.Mutex
	got  []map[string]any
	name string
	res  func(args map[string]any) (agentflow.Result, error)
}

func (r *recorder) tool() agenttool.Tool {
	return agenttool.NewFunc(r.name, "A program step.", json.RawMessage(`{"type":"object"}`),
		func(_ context.Context, call agenttool.Call) (agenttool.Result, error) {
			var args map[string]any
			_ = json.Unmarshal(call.Args, &args)
			r.mu.Lock()
			r.got = append(r.got, args)
			r.mu.Unlock()
			res := agentflow.Result{Outcome: agentflow.OK}
			if r.res != nil {
				var err error
				if res, err = r.res(args); err != nil {
					return agenttool.Result{}, err
				}
			}
			b, err := json.Marshal(res)
			if err != nil {
				return agenttool.Result{}, err
			}
			return agenttool.Text(string(b)), nil
		})
}

// runLive invokes g on a and drives it to its end, answering questions
// with answer, which returns false to stop.
func runLive(t *testing.T, g *agentflow.Graph, a *agentturn.Agent, inputs any, answer func(agentflow.Question) (agentflow.Result, bool), opts ...agentflow.Option) (*agentturn.RunEnd, agentflow.State, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	item, err := g.Invocation(inputs)
	if err != nil {
		t.Fatal(err)
	}
	end, err := a.Prompt(ctx, item)
	var st agentflow.State
	if end != nil {
		// A run that ended in an error is driven too: a model cut off at
		// its step's deadline ends its run so, and Drive goes on from it.
		end, st, err = agentflow.Drive(ctx, g, a, end, opts...)
	}
	for err == nil && end.Reason == agentturn.ReasonInputRequired && answer != nil {
		var q agentflow.Question
		if json.Unmarshal([]byte(end.Pending[0].Call.Arguments), &q) != nil {
			break
		}
		res, ok := answer(q)
		if !ok {
			break
		}
		ans, aerr := g.Answer(end.Pending[0], res)
		if aerr != nil {
			return end, st, aerr
		}
		end, st, err = agentflow.Resume(ctx, g, a, []agentturn.Answer{ans}, opts...)
	}
	return end, st, err
}

// finishes counts a transcript's flow_finish calls and those refused.
func finishes(tr agentturn.Transcript) (made, refused int) {
	calls := map[string]string{}
	for _, item := range tr {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			calls[it.CallID] = it.Name
			if it.Name == "flow_finish" {
				made++
			}
		case *openresponses.FunctionCallOutput:
			if calls[it.CallID] == "flow_finish" && !json.Valid([]byte(it.Output.String())) {
				refused++
			}
		}
	}
	return made, refused
}

// treeOf draws a transcript's runs as workflow:status:outcome, children
// indented.
func treeOf(t *testing.T, tr agentturn.Transcript, open func(string) (agentturn.Transcript, error)) []string {
	t.Helper()
	runs, err := agentflow.Runs(tr, open)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	var walk func(rs []*agentflow.Instance, depth int)
	walk = func(rs []*agentflow.Instance, depth int) {
		for _, r := range rs {
			s := fmt.Sprintf("%s%s:%s:%s", strings.Repeat(" ", depth), r.Workflow, r.Status, r.Outcome)
			if r.Problem != "" {
				s += " problem: " + r.Problem
			}
			out = append(out, s)
			walk(r.Children, depth+1)
		}
	}
	walk(runs, 0)
	return out
}

// steps is the path a state took, as step:outcome.
func steps(st agentflow.State) []string {
	var out []string
	for _, s := range st.Steps {
		out = append(out, s.Node+":"+s.Result.Outcome)
	}
	return out
}

// tally keeps a scenario's results against its expectation.
type tally struct {
	t     *testing.T
	trial int
	ok    int
	notes []string
}

func (s *tally) check(cond bool, format string, args ...any) {
	if !cond {
		s.notes = append(s.notes, fmt.Sprintf("trial %d: ", s.trial)+fmt.Sprintf(format, args...))
	}
}

func (s *tally) report(name string, trials int) {
	s.t.Logf("%s: %d of %d trials met every expectation", name, s.ok, trials)
	for _, n := range s.notes {
		s.t.Log("  " + n)
	}
	if s.ok == 0 {
		s.t.Errorf("%s: no trial met its expectations", name)
	}
}

// --- 1. Nesting in the current context -----------------------------

const nestOuter = "```mermaid\nflowchart TD\n  prep[[prep]] --> mid[[mid]]\n  mid -->|done| report[[report]]\n  mid -->|failed| fallback[[fallback]]\n```\n\n" +
	"## prep\n```yaml\ntool: prep\n```\n\n" +
	"## mid\n```yaml\nflow: middle\nwith:\n  n: ${{ inputs.n }}\n```\n\n" +
	"## report\n```yaml\ntool: report\nwith:\n  parity: ${{ steps.mid.outputs.parity }}\n```\n\n" +
	"## fallback\n```yaml\ntool: report\nwith:\n  reason: ${{ steps.mid.outputs.reason }}\n```\n"

const nestOuterFront = "---\ninputs:\n  n: {type: integer, required: true}\n---\n"

const nestMiddle = "---\ninputs:\n  n: {type: integer, required: true}\noutputs:\n  parity: ${{ steps.inner.outputs.parity }}\n---\n" +
	"```mermaid\nflowchart TD\n  inner[[inner]] -->|ok| done((done))\n```\n\n" +
	"## inner\n```yaml\nflow: leaf\nwith:\n  n: ${{ inputs.n }}\n```\n"

const nestLeaf = "---\ninputs:\n  n: {type: integer, required: true}\noutputs:\n  parity: ${{ steps.classify.outputs.parity }}\n---\n" +
	"```mermaid\nflowchart TD\n  gate[[gate]] --> classify[classify]\n```\n\n" +
	"## gate\n```yaml\ntool: gate\n```\n\n" +
	"## classify\n```yaml\nwith:\n  n: ${{ inputs.n }}\noutputs:\n  ok:\n    parity: {type: string, enum: [even, odd], required: true}\n```\n" +
	"Say whether the number n in your inputs is even or odd.\n"

func TestLiveNesting(t *testing.T) {
	l := live(t)
	s := &tally{t: t}
	for i := range l.trials {
		s.trial = i
		m := l.meter(t)
		fail := i%3 == 2 // every third trial fails inside the leaf
		n := []int{7, 12, 31}[i%3]
		prep, report := &recorder{name: "prep"}, &recorder{name: "report"}
		gate := &recorder{name: "gate", res: func(map[string]any) (agentflow.Result, error) {
			if fail {
				return agentflow.Result{}, errors.New("the gate is shut")
			}
			return agentflow.Result{Outcome: agentflow.OK}, nil
		}}
		outer := parse(t, "outer", nestOuterFront+nestOuter)
		if err := outer.Link(parse(t, "middle", nestMiddle), parse(t, "leaf", nestLeaf)); err != nil {
			t.Fatal(err)
		}
		tools := []agenttool.Tool{prep.tool(), report.tool(), gate.tool()}
		// Each call runs in a child that opens with its caller's context:
		// a fork of the caller's session, on a recorded host.
		h := newRecordedHost(t, m, l.name, localWorkspace(t), func(agentworkspace.Workspace) []agenttool.Tool { return tools })
		opts := []agentflow.Option{agentflow.Children(h.open), h.history(h.id), agentflow.Unattended()}
		cfg, err := agentflow.Apply(outer, l.base(m, tools...), opts...)
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		h.rec.Attach(a)
		end, st, err := runLive(t, outer, a, map[string]any{"n": n}, nil, opts...)
		if err != nil || end.Reason != agentturn.ReasonDone {
			s.check(false, "end %v: %v", end.Reason, err)
			continue
		}
		sessions := h.sessions(t)
		tree := treeOf(t, pathOf(sessions["root"]), h.openSpawn(t))
		made, refused, checked, inherited := 0, 0, 0, 0
		var bad []string
		for spawn, sess := range sessions {
			mk, rf := finishes(pathOf(sess))
			made, refused = made+mk, refused+rf
			c, b := hashes(t, sess)
			checked, bad = checked+c, append(bad, b...)
			if spawn != "root" && len(prefixOf(sess)) > 0 {
				inherited++
			}
		}
		seeded := 0
		for _, c := range h.opened {
			if c.Seeded {
				seeded++
			}
		}
		s.check(len(bad) == 0, "wrong hashes: %v", bad)
		s.check(seeded == len(h.opened) && inherited == len(sessions)-1, "%d of %d children opened seeded, %d of %d sessions inherited a prefix", seeded, len(h.opened), inherited, len(sessions)-1)
		want := "odd"
		if n%2 == 0 {
			want = "even"
		}
		before := len(s.notes)
		switch {
		case fail:
			s.check(slices.Equal(steps(st), []string{"prep:ok", "mid:failed", "fallback:ok"}), "path %v", steps(st))
			s.check(len(tree) == 3 && strings.HasPrefix(tree[2], "  leaf:failed"), "tree %v", tree)
			s.check(made == 0, "a model finished %d times in a failed leaf", made)
		default:
			s.check(slices.Equal(steps(st), []string{"prep:ok", "mid:done", "report:ok"}), "path %v", steps(st))
			s.check(slices.Equal(tree, []string{"outer:done:ok", " middle:done:done", "  leaf:done:ok"}), "tree %v", tree)
			s.check(len(report.got) == 1 && report.got[0]["parity"] == want, "report got %v, want parity %s", report.got, want)
			s.check(refused == 0, "%d of %d finishes refused", refused, made)
		}
		if len(s.notes) == before {
			s.ok++
		}
		t.Logf("trial %d (n=%d, fail=%v): path %v tree %v finishes %d refused %d, %d sessions, %d hashes checked", i, n, fail, steps(st), tree, made, refused, len(sessions), checked)
	}
	s.report("nesting", l.trials)
}

// --- 1b. What a callee knows of its caller's context ----------------

// keeperFlow has a model read a word no step's outputs carry, then calls
// a workflow that is asked for it and given nothing. %s is the call's
// context line.
const keeperFlow = "```mermaid\nflowchart TD\n  learn[learn] --> ask[[ask]]\n  ask -->|ok| report[[report]]\n```\n\n" +
	"## learn\n```yaml\ntools: [read_file]\noutputs:\n  ok:\n    read: {type: string, enum: [\"yes\"], required: true}\n```\n" +
	"Read the file secret.txt with read_file, then finish with read yes. Do not repeat the file's text anywhere: not in your data and not in a reason.\n\n" +
	"## ask\n```yaml\nflow: teller\n%s```\n\n" +
	"## report\n```yaml\ntool: report\nwith:\n  word: ${{ steps.ask.outputs.word }}\n```\n"

const tellerFlow = "---\noutputs:\n  word: ${{ steps.tell.outputs.word }}\n---\n" +
	"```mermaid\nflowchart TD\n  tell[tell]\n```\n\n" +
	"## tell\n```yaml\noutputs:\n  ok:\n    word: {type: string, required: true}\n```\n" +
	"A file named secret.txt holds one word. You have no tools and cannot read it now. If what you can see above tells you the word, give it as data word. If nothing you can see tells you, give the word UNKNOWN. Do not guess.\n"

// nonce is a word no model has seen, new each time.
func nonce() string {
	const syl = "bakodulimerovatunixepazo"
	b := make([]byte, 0, 8)
	for range 4 {
		k := 2 * rand.IntN(len(syl)/2)
		b = append(b, syl[k:k+2]...)
	}
	return string(b) + strconv.Itoa(100+rand.IntN(900))
}

func TestLiveCallerContext(t *testing.T) {
	l := live(t)
	for _, tc := range []struct {
		name, line string
		seeded     bool
	}{
		{"session", "", true},
		{"fresh", "context: fresh\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &tally{t: t}
			told, in, out := 0, 0, 0
			for i := range l.trials {
				s.trial = i
				m := l.meter(t)
				word := nonce()
				ws := localWorkspace(t)
				if err := os.WriteFile(filepath.Join(ws.Root(), "secret.txt"), []byte(word+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				report := &recorder{name: "report"}
				g := parse(t, "keeper", fmt.Sprintf(keeperFlow, tc.line))
				if err := g.Link(parse(t, "teller", tellerFlow)); err != nil {
					t.Fatal(err)
				}
				tools := func(ws agentworkspace.Workspace) []agenttool.Tool { return append(fileTools(ws), report.tool()) }
				h := newRecordedHost(t, m, l.name, ws, tools)
				opts := []agentflow.Option{agentflow.Children(h.open), h.history(h.id), agentflow.Unattended()}
				cfg, err := agentflow.Apply(g, l.base(m, tools(ws)...), opts...)
				if err != nil {
					t.Fatal(err)
				}
				a := agentturn.New(cfg)
				h.rec.Attach(a)
				end, st, err := runLive(t, g, a, nil, nil, opts...)
				m.mu.Lock()
				in, out = in+m.in, out+m.out
				m.mu.Unlock()
				if err != nil || end.Reason != agentturn.ReasonDone {
					s.check(false, "end %v: %v", end.Reason, err)
					continue
				}
				before := len(s.notes)
				var said string
				if len(report.got) == 1 {
					said = strings.TrimSpace(fmt.Sprint(report.got[0]["word"]))
				}
				knew := strings.EqualFold(said, word)
				if knew {
					told++
				}
				sessions := h.sessions(t)
				root := pathOf(sessions["root"])
				s.check(slices.Equal(steps(st), []string{"learn:ok", "ask:ok", "report:ok"}), "path %v", steps(st))
				s.check(len(h.opened) == 1 && h.opened[0].Seeded == tc.seeded, "opened %d children, seeded %v, want one seeded %v", len(h.opened), len(h.opened) > 0 && h.opened[0].Seeded, tc.seeded)
				// The caller's record holds its own step's finish and the
				// call with its result, and nothing the callee did on the way.
				rootMade, _ := finishes(root)
				s.check(rootMade == 1, "the caller's session holds %d finishes, want its own one", rootMade)
				s.check(len(sessions) == 2, "%d sessions, want the caller's and the callee's", len(sessions))
				checked, childMade, inherited := 0, 0, false
				var bad []string
				for spawn, sess := range sessions {
					c, b := hashes(t, sess)
					checked, bad = checked+c, append(bad, b...)
					if spawn == "root" {
						continue
					}
					childMade, _ = finishes(pathOf(sess))
					for _, item := range prefixOf(sess) {
						if out, ok := item.(*openresponses.FunctionCallOutput); ok && strings.Contains(out.Output.String(), word) {
							inherited = true
						}
					}
					s.check((len(prefixOf(sess)) > 0) == tc.seeded, "the callee's session opened with %d items", len(prefixOf(sess)))
					for _, item := range pathOf(sess) {
						if call, ok := item.(*openresponses.FunctionCall); ok && call.Name == "read_file" {
							s.check(false, "the callee read the file itself")
						}
					}
				}
				s.check(childMade == 1, "the callee's session holds %d finishes, want one", childMade)
				s.check(inherited == tc.seeded, "the callee's session opened with the file's text: %v", inherited)
				s.check(len(bad) == 0, "wrong hashes: %v", bad)
				// A callee that opens with nothing must never have the word,
				// whatever else the trial did.
				if !tc.seeded && knew {
					t.Errorf("trial %d: a fresh callee reported %q, which only its caller's context held", i, word)
				}
				if tc.seeded {
					s.check(knew, "the callee said %q, want %q from its caller's context", said, word)
				}
				if len(s.notes) == before {
					s.ok++
				}
				t.Logf("trial %d (%s): path %v callee said %q, knew %v, %d hashes checked, %d requests", i, word, steps(st), said, knew, checked, len(m.requests()))
			}
			t.Logf("%s: the callee had the word in %d of %d trials; %d tokens in, %d out", tc.name, told, l.trials, in, out)
			s.report("caller context, "+tc.name, l.trials)
		})
	}
}

// --- Recorded hosts ------------------------------------------------

// pathOf is the items of a session's path.
func pathOf(s *agentsession.Session) (out agentturn.Transcript) {
	// A forked session's path opens with what its caller's held, which is
	// no part of its record.
	for _, e := range session.OwnPath(s) {
		if item, ok := e.(*agentsession.ItemEntry); ok {
			out = append(out, item.Item)
		}
	}
	return out
}

// prefixOf is what a forked session opened with: the items another
// session wrote.
func prefixOf(s *agentsession.Session) (out agentturn.Transcript) {
	for _, e := range session.PrefixPath(s) {
		if item, ok := e.(*agentsession.ItemEntry); ok {
			out = append(out, item.Item)
		}
	}
	return out
}

// hashes checks every response's request hash in a session, and returns
// how many it checked and which were wrong or missing.
func hashes(t *testing.T, s *agentsession.Session) (checked int, bad []string) {
	t.Helper()
	for _, e := range s.Path(s.Leaf()) {
		r, ok := e.(*agentsession.ResponseEntry)
		if !ok {
			continue
		}
		got, err := s.RequestHashFor(r.Parent, r)
		checked++
		if err != nil || r.RequestHash == "" || r.RequestHash != got {
			bad = append(bad, fmt.Sprintf("%s (%s): %q vs %q %v", r.ResponseID, r.Model, r.RequestHash, got, err))
		}
	}
	return checked, bad
}

// recordedHost runs a workflow in a jsonl store, its children in child
// sessions, each child's tools built on the workspace at its dir.
type recordedHost struct {
	store  *jsonl.Store
	rec    *session.Recorder
	id     string
	model  openresponses.Streamer
	name   string
	ws     agentworkspace.Workspace
	tools  func(ws agentworkspace.Workspace) []agenttool.Tool
	fold   func(rec *session.Recorder) func(context.Context, agentturn.Transcript) (agentturn.Transcript, error)
	opened []agentflow.Child
}

func newRecordedHost(t *testing.T, model openresponses.Streamer, name string, ws agentworkspace.Workspace, tools func(agentworkspace.Workspace) []agenttool.Tool) *recordedHost {
	t.Helper()
	dir := t.TempDir()
	if keep := os.Getenv("AGENTFLOW_LIVE_KEEP"); keep != "" {
		dir = filepath.Join(keep, strings.ReplaceAll(t.Name(), "/", "_"))
	}
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(context.Background(), store, agentsession.Header{CWD: ws.Root()})
	if err != nil {
		t.Fatal(err)
	}
	return &recordedHost{store: store, rec: rec, id: s.ID(), model: model, name: name, ws: ws, tools: tools}
}

// history reads a session's path from the store, empty before it exists.
func (h *recordedHost) history(id string) agentflow.Option {
	return agentflow.History(func(ctx context.Context) (agentturn.Transcript, error) {
		s, err := h.store.Open(ctx, id)
		if errors.Is(err, agentsession.ErrNoSession) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return pathOf(s), nil
	})
}

func (h *recordedHost) open(ctx context.Context, c agentflow.Child) (context.Context, *agentturn.Agent, error) {
	h.opened = append(h.opened, c)
	cctx, err := h.rec.ContinueChild(ctx, c.Spawn)
	switch {
	case errors.Is(err, session.ErrNoSpawn) && c.Seeded:
		// A seeded child's session is a fork of its caller's before the
		// call that waits on it, so it opens with what the caller's held
		// and every request of it rebuilds.
		cctx, err = h.rec.ForkChildBefore(ctx, c.Spawn, c.Caller)
	case errors.Is(err, session.ErrNoSpawn):
		cctx, err = h.rec.StartChild(ctx, c.Spawn)
	}
	if err != nil {
		return nil, nil, err
	}
	id := session.SessionIDFromContext(cctx)
	ws := h.ws
	if d := c.Settings.Dir; d != "" {
		if ws, err = script.Sub(h.ws, d); err != nil {
			return nil, nil, err
		}
	}
	limit := 1500
	base := agentturn.Config{Model: h.model, ModelName: cmp.Or(c.Settings.Model, h.name), Tools: h.tools(ws), MaxTurns: 80}
	base.Request.MaxOutputTokens = &limit
	if h.fold != nil {
		base.Transform = h.fold(h.rec)
	}
	// The child's record is what its session wrote itself; what a fork
	// opened with is given apart.
	inherited := agentflow.Inherited(func(ctx context.Context) (agentturn.Transcript, error) {
		s, err := h.store.Open(ctx, id)
		if errors.Is(err, agentsession.ErrNoSession) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return prefixOf(s), nil
	})
	cfg, err := c.Apply(base, h.history(id), inherited)
	if err != nil {
		return nil, nil, err
	}
	var seed []agentturn.Option
	switch s, err := h.store.Open(ctx, id); {
	case err == nil:
		if seed, err = session.AgentOptions(s, h.rec.ReadOptions()...); err != nil {
			return nil, nil, err
		}
	case !errors.Is(err, agentsession.ErrNoSession):
		return nil, nil, err
	}
	a := agentturn.New(cfg, seed...)
	a.Subscribe(func(ctx context.Context, ev agentturn.Event) error {
		h.rec.Observe(ctx, ev)
		return nil
	})
	return agentool.ContextWithConfig(cctx, cfg), a, nil
}

// sessions are the root's session and every child's, by spawn.
func (h *recordedHost) sessions(t *testing.T) map[string]*agentsession.Session {
	t.Helper()
	ctx := context.Background()
	out := map[string]*agentsession.Session{}
	for sum, err := range h.store.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		s, err := h.store.Open(ctx, sum.Header.ID)
		if err != nil {
			t.Fatal(err)
		}
		key := sum.Header.SpawnedBy
		if sum.Header.ID == h.id {
			key = "root"
		}
		out[key] = s
	}
	return out
}

// openSpawn reads a child's session by spawn, for Runs.
func (h *recordedHost) openSpawn(t *testing.T) func(string) (agentturn.Transcript, error) {
	all := h.sessions(t)
	return func(spawn string) (agentturn.Transcript, error) {
		if s := all[spawn]; s != nil {
			return pathOf(s), nil
		}
		return nil, errors.New("no session of spawn " + spawn)
	}
}

// localWorkspace is a new workspace in a temporary directory.
func localWorkspace(t *testing.T) agentworkspace.Workspace {
	t.Helper()
	ws, err := agentworkspace.NewLocal(t.TempDir(), []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

type pathArgs struct {
	Path string `json:"path"`
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// fileTools read and write files of ws.
func fileTools(ws agentworkspace.Workspace) []agenttool.Tool {
	read := agenttool.New("read_file", "Read a file.", func(_ context.Context, a pathArgs) (string, error) {
		b, err := fs.ReadFile(ws.FS(), a.Path)
		return string(b), err
	})
	write := agenttool.New("write_file", "Write a file, making its directories.", func(ctx context.Context, a writeFileArgs) (string, error) {
		return "written", ws.WriteFile(ctx, a.Path, []byte(a.Content), 0o644)
	})
	return []agenttool.Tool{read, write}
}

// --- 2. Children ---------------------------------------------------

const scribeFlow = "---\ninputs:\n  word: {type: string, required: true}\n---\n" +
	"```mermaid\nflowchart TD\n  mk[[mk]] --> first[first]\n  first --> second[second]\n  second --> check[[check]]\n```\n\n" +
	"## mk\n```yaml\ntool: mk\noutputs:\n  ok:\n    dir: {type: string, required: true}\n```\n\n" +
	"## first\n```yaml\ncontext: fresh\nchild: scribe\ndir: ${{ steps.mk.outputs.dir }}\ntools: [write_file]\nwith:\n  word: ${{ inputs.word }}\n```\n" +
	"Write the word from your inputs, and nothing else, to the file word.txt.\n\n" +
	"## second\n```yaml\ncontext: fresh\nchild: scribe\ndir: ${{ steps.mk.outputs.dir }}\noutputs:\n  ok:\n    word: {type: string, required: true}\n```\n" +
	"Which word did you write to word.txt in the step before this one? You have no tools now: answer from what you did, as data word.\n\n" +
	"## check\n```yaml\ntool: check\nwith:\n  dir: ${{ steps.mk.outputs.dir }}\n```\n"

func TestLiveChildren(t *testing.T) {
	l := live(t)
	s := &tally{t: t}
	for i := range l.trials {
		s.trial = i
		m := l.meter(t)
		word := []string{"marigold", "basalt", "quokka"}[i%3]
		ws := localWorkspace(t)
		mk := &recorder{name: "mk", res: func(map[string]any) (agentflow.Result, error) {
			dir := fmt.Sprintf("trees/run-%d", i)
			if err := os.MkdirAll(filepath.Join(ws.Root(), dir), 0o755); err != nil {
				return agentflow.Result{}, err
			}
			return agentflow.Result{Outcome: agentflow.OK, Data: map[string]any{"dir": dir}}, nil
		}}
		var written string
		check := &recorder{name: "check", res: func(args map[string]any) (agentflow.Result, error) {
			b, err := os.ReadFile(filepath.Join(ws.Root(), fmt.Sprint(args["dir"]), "word.txt"))
			written = strings.TrimSpace(string(b))
			return agentflow.Result{Outcome: agentflow.OK}, err
		}}
		g := parse(t, "scribe", scribeFlow)
		h := newRecordedHost(t, m, l.name, ws, fileTools)
		cfg, err := agentflow.Apply(g, l.base(m, append(fileTools(ws), mk.tool(), check.tool())...),
			agentflow.Children(h.open), h.history(h.id), agentflow.Unattended())
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		h.rec.Attach(a)
		end, st, err := runLive(t, g, a, map[string]any{"word": word}, nil)
		if err != nil || end.Reason != agentturn.ReasonDone {
			s.check(false, "end %v: %v", end.Reason, err)
			continue
		}
		before := len(s.notes)
		var said any
		if len(st.Steps) == 4 {
			said, _ = st.Steps[2].Result.Data.(map[string]any)["word"]
		}
		spawns := map[string]bool{}
		for _, c := range h.opened {
			spawns[c.Spawn] = true
		}
		s.check(slices.Equal(steps(st), []string{"mk:ok", "first:ok", "second:ok", "check:ok"}), "path %v", steps(st))
		s.check(written == word, "word.txt in the child's dir holds %q, want %q", written, word)
		s.check(fmt.Sprint(said) == word, "the kept child's second visit said %q, want %q", said, word)
		s.check(len(spawns) == 1 && len(h.opened) == 2, "opened %d times over %d spawns, want 2 over 1", len(h.opened), len(spawns))
		checked, bad := 0, []string(nil)
		for _, sess := range h.sessions(t) {
			c, b := hashes(t, sess)
			checked += c
			bad = append(bad, b...)
		}
		s.check(len(bad) == 0, "wrong hashes: %v", bad)
		tree := treeOf(t, pathOf(h.sessions(t)["root"]), h.openSpawn(t))
		if len(s.notes) == before {
			s.ok++
		}
		t.Logf("trial %d (%s): path %v said %q written %q, %d hashes checked, tree %v", i, word, steps(st), said, written, checked, tree)
	}
	s.report("children", l.trials)
}

// --- 3. The judge as a child ---------------------------------------

const critMark = "CRIT-3141"

const approveFlow = "```mermaid\nflowchart TD\n  plan[[plan]] --> approve([approve])\n  approve -->|go| apply[apply]\n  approve -->|change| changed((changed))\n```\n\n" +
	"## plan\n```yaml\ntool: plan\noutputs:\n  ok:\n    plan: {type: string, required: true}\n```\n\n" +
	"## approve\n```yaml\nwith:\n  plan: ${{ steps.plan.outputs.plan }}\noutcomes: [go, change]\njudge:\n  outcomes: [go, change]\n  criteria: |\n" +
	"    The bug: Average in src/avg.go divides by zero when its slice is empty.\n" +
	"    Choose go only for a plan that fixes that bug in src/avg.go and keeps or adds tests.\n" +
	"    A plan that deletes or weakens tests is change. (Reference " + critMark + ".)\n```\n" +
	"Should this plan go ahead?\n\n" +
	"## apply\n```yaml\noutputs:\n  ok:\n    summary: {type: string, required: true}\n```\n" +
	"In one sentence, say what the approved plan will change, as data summary.\n"

func TestLiveJudge(t *testing.T) {
	l := live(t)
	plans := map[string]string{
		"go":     "In src/avg.go, return 0 from Average when the slice is empty, and add a test for the empty slice to src/avg_test.go.",
		"change": "Delete the failing empty-slice test from src/avg_test.go so that CI passes, and change nothing else.",
	}
	s := &tally{t: t}
	trial := 0
	for _, mode := range []agentflow.Mode{agentflow.Shadow, agentflow.Advise, agentflow.Decide} {
		for _, want := range []string{"go", "change"} {
			s.trial = trial
			trial++
			m, jm := l.meter(t), l.meter(t)
			plan := &recorder{name: "plan", res: func(map[string]any) (agentflow.Result, error) {
				return agentflow.Result{Outcome: agentflow.OK, Data: map[string]any{"plan": plans[want]}}, nil
			}}
			g := parse(t, "approval", approveFlow)
			tools := []agenttool.Tool{plan.tool()}
			cfg, err := agentflow.Apply(g, l.base(m, tools...), l.kids(jm, nil), agentflow.Delegate(mode, "approve"))
			if err != nil {
				t.Fatal(err)
			}
			a := agentturn.New(cfg)
			asked := 0
			end, st, err := runLive(t, g, a, nil, func(q agentflow.Question) (agentflow.Result, bool) {
				asked++
				// A careful person: go for the good plan, change for the other.
				return agentflow.Result{Outcome: want}, true
			})
			if err != nil || end.Reason != agentturn.ReasonDone {
				s.check(false, "%v: end %v: %v", mode, end.Reason, err)
				continue
			}
			before := len(s.notes)
			var got, by, verdict string
			for _, step := range st.Steps {
				if step.Node == "approve" {
					got, by = step.Result.Outcome, step.Result.By
					if step.Advice != nil {
						verdict = step.Advice.Outcome
					}
				}
			}
			for _, r := range m.requests() {
				if i := strings.Index(r, critMark); i >= 0 {
					t.Logf("%v: the criteria mark in a working request: ...%s...", mode, r[max(0, i-700):min(len(r), i+200)])
				}
			}
			s.check(!m.sentAny(critMark), "%v: a working request held the criteria", mode)
			s.check(!m.sentAny(`"definition"`), "%v: a working request held a definition", mode)
			s.check(jm.sentAny(critMark), "%v: the judge was not given the criteria", mode)
			switch mode {
			case agentflow.Shadow:
				s.check(got == want && by == "", "shadow: step %s by %q, want the person's %s", got, by, want)
				s.check(!m.sentAny("flow_judge"), "shadow: a working request held the blind verdict")
				s.check(verdict == want, "shadow: blind verdict %q, want %q", verdict, want)
			case agentflow.Advise:
				s.check(got == want, "advise: step %s, want %s", got, want)
				s.check(verdict == want, "advise: verdict %q, want %q", verdict, want)
			case agentflow.Decide:
				s.check(got == want && by == agentflow.ByJudge && asked == 0, "decide: %s by %q after %d questions, want %s by the judge", got, by, asked, want)
			}
			if len(s.notes) == before {
				s.ok++
			}
			t.Logf("%v, plan for %s: step %s by %q, verdict %q, %d questions; working requests %d, judge requests %d",
				mode, want, got, by, verdict, asked, len(m.requests()), len(jm.requests()))
		}
	}
	s.report("judge", trial)
}

// --- 5. Limits -----------------------------------------------------

const slowFlow = "```mermaid\nflowchart TD\n  fetch[fetch]\n```\n\n" +
	"## fetch\n```yaml\ntools: [slow]\ntimeout: 15s\n```\n" +
	"Call the slow tool once, then finish the step.\n"

const stuckFlow = "```mermaid\nflowchart TD\n  find[find] -->|stuck| report[[report]]\n```\n\n" +
	"## find\n```yaml\ntools: [read_file]\noutputs:\n  ok:\n    line: {type: string, required: true}\n```\n" +
	"Read the file notes/plan.md and give its third line as data line.\n\n" +
	"## report\n```yaml\ntool: report\n```\n"

const countFlow = "```mermaid\nflowchart TD\n  count[[count]] --> decide[decide]\n  decide -->|more| count\n```\n\n" +
	"## count\n```yaml\ntool: count\nmax_visits: 2\noutputs:\n  ok:\n    n: {type: integer, required: true}\n```\n\n" +
	"## decide\n```yaml\nmax_visits: 5\nwith:\n  n: ${{ steps.count.outputs.n }}\noutcomes: [more, enough]\n```\n" +
	"If n in your inputs is less than 3 choose more, otherwise enough.\n"

const pacedFlow = "```mermaid\nflowchart TD\n  draft[draft] --> review([review])\n  review -->|change| draft\n```\n\n" +
	"## draft\n```yaml\nmax_visits: person\noutputs:\n  ok:\n    title: {type: string, required: true}\n```\n" +
	"Write a short title for a note about tide pools, a different one each time you are asked, as data title.\n\n" +
	"## review\n```yaml\nmax_visits: person\nwith:\n  title: ${{ steps.draft.outputs.title }}\noutcomes: [go, change]\n```\n" +
	"Is this title right?\n"

func TestLiveLimits(t *testing.T) {
	l := live(t)
	t.Run("timeout", func(t *testing.T) {
		m := l.meter(t)
		var calls int
		slow := agenttool.New("slow", "Fetch something slowly.", func(ctx context.Context, _ struct{}) (string, error) {
			calls++
			if calls == 1 {
				select {
				case <-time.After(time.Minute):
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			return "fetched", nil
		})
		g := parse(t, "slow", slowFlow)
		retries := 0
		rules := agentflow.Reply(func(_ context.Context, q agentflow.Question) (agentflow.Result, bool) {
			if q.Retry {
				retries++
				return agentflow.Result{Outcome: "retry"}, true
			}
			return agentflow.Result{}, false
		})
		cfg, err := agentflow.Apply(g, l.base(m, slow), rules)
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		end, st, err := runLive(t, g, a, nil, nil, rules)
		t.Logf("end %v %v; path %v; slow called %d times; retries %d", end.Reason, err, steps(st), calls, retries)
		// The step's time covers the model's own turns too, so a slow
		// model can run past it again once retried: the first call must
		// time out and be retried, and the step must finish; how often is
		// the model's pace.
		if err != nil || !slices.Equal(steps(st), []string{"fetch:ok"}) || retries < 1 || calls < 2 {
			t.Errorf("want fetch:ok after the slow call timed out and the step was retried")
		}
	})
	t.Run("stuck", func(t *testing.T) {
		stuck := 0
		for i := range l.trials {
			m := l.meter(t)
			ws := localWorkspace(t)
			report := &recorder{name: "report"}
			g := parse(t, "stuck", stuckFlow)
			cfg, err := agentflow.Apply(g, l.base(m, append(fileTools(ws), report.tool())...), agentflow.Unattended())
			if err != nil {
				t.Fatal(err)
			}
			a := agentturn.New(cfg)
			end, st, err := runLive(t, g, a, nil, nil, agentflow.Unattended())
			t.Logf("trial %d: end %v %v; path %v; data %v", i, end.Reason, err, steps(st), st.Steps)
			if slices.Equal(steps(st), []string{"find:stuck", "report:ok"}) {
				stuck++
			}
		}
		t.Logf("stuck: the model called flow_stuck in %d of %d trials", stuck, l.trials)
	})
	t.Run("allowance", func(t *testing.T) {
		m := l.meter(t)
		n := 0
		count := &recorder{name: "count", res: func(map[string]any) (agentflow.Result, error) {
			n++
			return agentflow.Result{Outcome: agentflow.OK, Data: map[string]any{"n": n}}, nil
		}}
		var limits []string
		rules := agentflow.Reply(func(_ context.Context, q agentflow.Question) (agentflow.Result, bool) {
			if q.Limit != "" {
				limits = append(limits, q.Limit)
				return agentflow.Result{Outcome: "grant", Data: map[string]any{"amount": 2}}, true
			}
			return agentflow.Result{}, false
		})
		g := parse(t, "count", countFlow)
		cfg, err := agentflow.Apply(g, l.base(m, count.tool()), rules)
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		end, st, err := runLive(t, g, a, nil, nil, rules)
		t.Logf("end %v %v; path %v; limits asked %v", end.Reason, err, steps(st), limits)
		want := []string{"count:ok", "decide:more", "count:ok", "decide:more", "count:ok", "decide:enough"}
		if err != nil || !slices.Equal(steps(st), want) || !slices.Equal(limits, []string{"visits"}) {
			t.Errorf("want %v with one visits question granted", want)
		}
	})
	t.Run("paced", func(t *testing.T) {
		m := l.meter(t)
		g := parse(t, "paced", pacedFlow)
		cfg, err := agentflow.Apply(g, l.base(m))
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		answered, limits, retries := 0, 0, 0
		end, st, err := runLive(t, g, a, nil, func(q agentflow.Question) (agentflow.Result, bool) {
			if q.Limit != "" {
				limits++
				return agentflow.Result{Outcome: "stop"}, true
			}
			if q.Retry {
				// A step the model failed is run again: the trial is of
				// the loop's pace, not of one request.
				retries++
				t.Logf("step %q failed and is retried: %s", q.Step, q.Question)
				return agentflow.Result{Outcome: "retry"}, retries <= 3
			}
			answered++
			if answered < 7 {
				return agentflow.Result{Outcome: "change"}, true
			}
			return agentflow.Result{Outcome: "go"}, true
		})
		drafts := 0
		for _, s := range st.Steps {
			if s.Node == "draft" {
				drafts++
			}
		}
		t.Logf("end %v %v; %d drafts, %d answers, %d limit questions, %d retries, paced %d", end.Reason, err, drafts, answered, limits, retries, st.Paced)
		if err != nil || drafts != 7 || limits != 0 || !st.Done {
			t.Errorf("want 7 drafts past the default bound of 5 and no limit question")
		}
	})
}

// skillFiles is a workflow skill's SKILL.md beside its other files.
func skillFiles(name string, files map[string]string) map[string]string {
	out := map[string]string{"SKILL.md": "---\nname: " + name + "\ndescription: A live trial's workflow.\nmetadata:\n  agentflow: WORKFLOW.md\n---\nRun the workflow.\n"}
	for p, b := range files {
		out[p] = b
	}
	return out
}

// --- 6. Policy -----------------------------------------------------

const policyFlow = "```mermaid\nflowchart TD\n  plan[[plan]] --> write[write]\n```\n\n" +
	"## plan\n```yaml\ntool: plan\noutputs:\n  ok:\n    path: {type: string, required: true}\n```\n\n" +
	"## write\n```yaml\ntools: [write_file]\npolicy:\n  deny:\n    - {rule: write_file(secret/*), note: secret files are off limits}\n" +
	"  allow:\n    - {rule: write_file, path: '${{ steps.plan.outputs.path }}', note: the planned file}\n```\n" +
	"Write the word hello to one file: secret/x.txt if this step may write it, and otherwise the one path this step may write. Then finish.\n"

// policyFlow leaves the choice of path to what the step's instructions
// say of its limits: a task that told the model to write the denied path
// would be followed, by a model that does as it is told, and refused.
//
// policyFlowTry is policyFlow, told to make a write outside its limits,
// so the refusal is seen.
var policyFlowTry = strings.Replace(policyFlow, "Write the word hello to one file: secret/x.txt if this step may write it, and otherwise the one path this step may write. Then finish.",
	"Write the word hello to out/ok.txt, and a copy of it to notes/copy.txt. If a write is refused, say why in one sentence and finish.", 1)

func TestLivePolicy(t *testing.T) {
	l := live(t)
	t.Run("stated", func(t *testing.T) { livePolicy(t, l, policyFlow, false) })
	t.Run("tried", func(t *testing.T) { livePolicy(t, l, policyFlowTry, true) })
}

// livePolicy runs a policed write. tried says the model is told to try
// the denied write, so a refusal is expected.
func livePolicy(t *testing.T, l liveModel, src string, tried bool) {
	s := &tally{t: t}
	for i := range l.trials {
		s.trial = i
		m := l.meter(t)
		ws := localWorkspace(t)
		plan := &recorder{name: "plan", res: func(map[string]any) (agentflow.Result, error) {
			return agentflow.Result{Outcome: agentflow.OK, Data: map[string]any{"path": "out/ok.txt"}}, nil
		}}
		g := parse(t, "policed", src)
		matchers := agentflow.PolicyMatchers(func(dir string) map[string]agentpolicy.ToolMatcher {
			return map[string]agentpolicy.ToolMatcher{"write_file": {Match: agentflow.InDir(dir, "path")}}
		})
		cfg, err := agentflow.Apply(g, l.base(m, append(fileTools(ws), plan.tool())...), matchers, agentflow.Unattended())
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		end, st, err := runLive(t, g, a, nil, nil, agentflow.Unattended())
		if err != nil || end.Reason != agentturn.ReasonDone {
			if tried && errors.Is(err, agentflow.ErrStepFailed) {
				// It reported itself stuck rather than try the write.
				t.Logf("trial %d: %v", i, err)
				s.ok++
				continue
			}
			s.check(false, "end %v: %v", end.Reason, err)
			continue
		}
		var refusals, writes []string
		calls := map[string]*openresponses.FunctionCall{}
		for _, item := range a.State().Transcript {
			switch it := item.(type) {
			case *openresponses.FunctionCall:
				calls[it.CallID] = it
			case *openresponses.FunctionCallOutput:
				if c := calls[it.CallID]; c != nil && c.Name == "write_file" {
					writes = append(writes, c.Arguments)
					if out := it.Output.String(); strings.Contains(out, "denied by") || strings.Contains(out, "agentflow:") {
						refusals = append(refusals, out)
					}
				}
			}
		}
		_, secretErr := os.Stat(filepath.Join(ws.Root(), "secret/x.txt"))
		okBody, okErr := os.ReadFile(filepath.Join(ws.Root(), "out/ok.txt"))
		before := len(s.notes)
		s.check(slices.Equal(steps(st), []string{"plan:ok", "write:ok"}), "path %v", steps(st))
		s.check(m.sentAny("write_file(secret/*)") && m.sentAny("write_file(out/ok.txt)"), "the step's instructions did not state both rules")
		s.check(os.IsNotExist(secretErr), "secret/x.txt was written")
		_, copyErr := os.Stat(filepath.Join(ws.Root(), "notes/copy.txt"))
		s.check(os.IsNotExist(copyErr), "notes/copy.txt was written")
		s.check(okErr == nil && strings.Contains(string(okBody), "hello"), "out/ok.txt: %q %v", okBody, okErr)
		if tried {
			// Whether it tries the write is the model's; that a write it
			// tries is refused, naming what the step allows, is not.
			for _, r := range refusals {
				s.check(strings.Contains(r, "it allows only calls matching write_file(out/ok.txt)"), "refusal %q", r)
			}
		} else {
			s.check(len(refusals) == 0 && len(writes) == 1, "with the limits stated it wrote %v, refused %v", writes, refusals)
		}
		if len(s.notes) == before {
			s.ok++
		}
		t.Logf("trial %d: path %v; writes %v; refusals %v", i, steps(st), writes, refusals)
	}
	s.report("policy", l.trials)
}

// --- 7. Documents and files ----------------------------------------

const briefFlow = "---\ndocuments:\n  plan:\n---\n```mermaid\nflowchart TD\n  work[work] --> count[[count]]\n```\n\n" +
	"## work\n```yaml\ntools: [read_file]\ndocuments:\n  edit: [plan]\nfiles: [./notes/codes.md, data/answer.txt]\noutputs:\n  ok:\n    word: {type: string, required: true}\n    answer: {type: string, required: true}\n```\n" +
	"First write a checklist of exactly three lines, each starting with \"- \", into the document plan. " +
	"Then give, as data, the code word from notes/codes.md as word and the answer from data/answer.txt as answer.\n\n" +
	"## count\n```yaml\ntool: count\nwith:\n  text: ${{ documents.plan }}\n```\n"

func TestLiveDocumentsAndFiles(t *testing.T) {
	l := live(t)
	s := &tally{t: t}
	for i := range l.trials {
		s.trial = i
		m := l.meter(t)
		ws := localWorkspace(t)
		if err := ws.WriteFile(context.Background(), "data/answer.txt", []byte("The answer is 42.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		sk := writeSkill(t, t.TempDir(), "brief", skillFiles("brief", map[string]string{
			"WORKFLOW.md": briefFlow, "notes/codes.md": "The code word is PELICAN.\n",
		}))
		w, err := workflow.Load(sk)
		if err != nil {
			t.Fatal(err)
		}
		var lines int
		count := &recorder{name: "count", res: func(args map[string]any) (agentflow.Result, error) {
			for _, line := range strings.Split(fmt.Sprint(args["text"]), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "- ") {
					lines++
				}
			}
			return agentflow.Result{Outcome: agentflow.OK}, nil
		}}
		cfg, err := agentflow.Apply(w.Graph, l.base(m, append(fileTools(ws), count.tool())...), agentflow.FileReader("read_file", "path"), agentflow.Unattended())
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		end, st, err := runLive(t, w.Graph, a, nil, nil, agentflow.Unattended())
		if err != nil || end.Reason != agentturn.ReasonDone {
			s.check(false, "end %v: %v", end.Reason, err)
			continue
		}
		var data map[string]any
		if len(st.Steps) > 0 {
			data, _ = st.Steps[0].Result.Data.(map[string]any)
		}
		before := len(s.notes)
		s.check(slices.Equal(steps(st), []string{"work:ok", "count:ok"}), "path %v", steps(st))
		s.check(strings.Contains(strings.ToUpper(fmt.Sprint(data["word"])), "PELICAN"), "word %v", data["word"])
		s.check(strings.Contains(fmt.Sprint(data["answer"]), "42"), "answer %v", data["answer"])
		s.check(lines == 3, "the script read %d checklist lines from the document, want 3", lines)
		if len(s.notes) == before {
			s.ok++
		}
		t.Logf("trial %d: path %v; data %v; lines %d; document %q", i, steps(st), data, lines, st.Documents)
	}
	s.report("documents and files", l.trials)
}

// --- 8. Showing part of a result -----------------------------------

const showFlow = "```mermaid\nflowchart TD\n  gather[[gather]] --> answer[answer]\n```\n\n" +
	"## gather\n```yaml\ntool: gather\noutputs:\n  ok:\n    rows: {type: array, required: true}\n    summary: {type: string, required: true}\n    failed: {type: integer, required: true}\nshow: [summary, failed]\n```\n\n" +
	"## answer\n```yaml\noutputs:\n  ok:\n    count: {type: integer, required: true}\n    ids: {type: array, items: {type: integer}, required: true}\n```\n" +
	"From the gather step's result, say how many rows failed, as data count, and their ids, as data ids.\n"

func TestLiveShow(t *testing.T) {
	l := live(t)
	s := &tally{t: t}
	for i := range l.trials {
		s.trial = i
		m := l.meter(t)
		gather := &recorder{name: "gather", res: func(map[string]any) (agentflow.Result, error) {
			var rows []any
			for n := range 300 {
				status := "passed"
				if n == 17 || n == 104 || n == 230 {
					status = "failed"
				}
				rows = append(rows, map[string]any{"id": n, "status": status, "note": fmt.Sprintf("ROWMARK-%d", n)})
			}
			return agentflow.Result{Outcome: agentflow.OK, Data: map[string]any{
				"rows": rows, "summary": "3 of 300 rows failed: ids 17, 104 and 230.", "failed": 3}}, nil
		}}
		g := parse(t, "shown", showFlow)
		cfg, err := agentflow.Apply(g, l.base(m, gather.tool()), agentflow.Unattended())
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		end, st, err := runLive(t, g, a, nil, nil, agentflow.Unattended())
		if err != nil || end.Reason != agentturn.ReasonDone {
			s.check(false, "end %v: %v", end.Reason, err)
			continue
		}
		var data map[string]any
		if len(st.Steps) == 2 {
			data, _ = st.Steps[1].Result.Data.(map[string]any)
		}
		before := len(s.notes)
		s.check(!m.sentAny("ROWMARK-"), "a request held the rows")
		s.check(fmt.Sprint(data["count"]) == "3", "count %v", data["count"])
		s.check(fmt.Sprint(data["ids"]) == "[17 104 230]", "ids %v", data["ids"])
		s.check(len(st.Steps) == 2 && len(st.Steps[0].Result.Data.(map[string]any)["rows"].([]any)) == 300, "the record lost rows")
		if len(s.notes) == before {
			s.ok++
		}
		t.Logf("trial %d: path %v; data %v; requests %d", i, steps(st), data, len(m.requests()))
	}
	s.report("show", l.trials)
}

// --- 9. Skills a step names ----------------------------------------

const summFlow = "```mermaid\nflowchart TD\n  summ[summ]\n```\n\n" +
	"## summ\n```yaml\nskills: [house-style]\nwith:\n  text: ${{ inputs.text }}\noutputs:\n  ok:\n    summary: {type: string, required: true}\n```\n" +
	"Summarise the text in your inputs in one sentence, as data summary, following your skills.\n"

func TestLiveSkills(t *testing.T) {
	l := live(t)
	s := &tally{t: t}
	for i := range l.trials {
		s.trial = i
		m := l.meter(t)
		root := t.TempDir()
		writeSkill(t, root, "house-style", map[string]string{"SKILL.md": "---\nname: house-style\ndescription: How we write.\n---\n" +
			"Every summary you write ends with the exact token ~fin~ and nothing after it.\n"})
		sk := writeSkill(t, root, "summ", skillFiles("summ", map[string]string{"WORKFLOW.md": "---\ninputs:\n  text: {type: string, required: true}\n---\n" + summFlow}))
		catalog, err := agentskill.DiscoverDirs(root)
		if err != nil {
			t.Fatal(err)
		}
		w, err := workflow.LoadWith(sk, workflow.FromCatalog(catalog))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := agentflow.Apply(w.Graph, l.base(m), agentflow.Unattended())
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		text := "Tide pools form where the sea leaves water among rocks at low tide; anemones, crabs and snails live in them."
		end, st, err := runLive(t, w.Graph, a, map[string]any{"text": text}, nil, agentflow.Unattended())
		if err != nil || end.Reason != agentturn.ReasonDone {
			s.check(false, "end %v: %v", end.Reason, err)
			continue
		}
		summary := ""
		if len(st.Steps) == 1 {
			summary = fmt.Sprint(st.Steps[0].Result.Data.(map[string]any)["summary"])
		}
		before := len(s.notes)
		s.check(m.sentAny("~fin~"), "the skill's text was not sent")
		s.check(strings.HasSuffix(strings.TrimSpace(summary), "~fin~"), "summary %q", summary)
		if len(s.notes) == before {
			s.ok++
		}
		t.Logf("trial %d: summary %q", i, summary)
	}
	s.report("skills", l.trials)
}

// --- 12. Overrides -------------------------------------------------

const titleFlow = "```mermaid\nflowchart TD\n  write[write] --> review([review])\n  review -->|change| write\n```\n\n" +
	"## write\n```yaml\noutputs:\n  ok:\n    title: {type: string, required: true}\n```\n" +
	"Write a short title for a note about lighthouses, as data title.\n\n" +
	"## review\n```yaml\nwith:\n  title: ${{ steps.write.outputs.title }}\noutcomes: [go, change]\n```\nIs this title right?\n"

func TestLiveOverride(t *testing.T) {
	l := live(t)
	s := &tally{t: t}
	for i := range l.trials {
		s.trial = i
		m := l.meter(t)
		g := parse(t, "titled", titleFlow)
		cfg, err := agentflow.Apply(g, l.base(m))
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		end, st, err := runLive(t, g, a, nil, func(agentflow.Question) (agentflow.Result, bool) { return agentflow.Result{}, false })
		if err != nil || end.Reason != agentturn.ReasonInputRequired {
			s.check(false, "end %v: %v", end.Reason, err)
			continue
		}
		var q agentflow.Question
		_ = json.Unmarshal([]byte(end.Pending[0].Call.Arguments), &q)
		ctx := context.Background()
		end, _, err = agentflow.OverrideRun(ctx, g, a, agentflow.Override{Run: q.Run, Action: agentflow.OverrideJump, Step: "write", Note: "Put the word BLUE in the title."})
		if err != nil || end.Reason != agentturn.ReasonInputRequired {
			s.check(false, "after the override: end %v: %v", end.Reason, err)
			continue
		}
		_ = json.Unmarshal([]byte(end.Pending[0].Call.Arguments), &q)
		ans, err := g.Answer(end.Pending[0], agentflow.Result{Outcome: "go"})
		if err != nil {
			t.Fatal(err)
		}
		end, st, err = agentflow.Resume(ctx, g, a, []agentturn.Answer{ans})
		if err != nil || end.Reason != agentturn.ReasonDone {
			s.check(false, "resume: end %v: %v", end.Reason, err)
			continue
		}
		var titles []string
		for _, step := range st.Steps {
			if step.Node == "write" {
				titles = append(titles, fmt.Sprint(step.Result.Data.(map[string]any)["title"]))
			}
		}
		before := len(s.notes)
		s.check(len(titles) == 2 && strings.Contains(strings.ToUpper(titles[1]), "BLUE"), "titles %v", titles)
		s.check(m.sentAny("Put the word BLUE in the title."), "the note was not sent")
		if len(s.notes) == before {
			s.ok++
		}
		t.Logf("trial %d: path %v; titles %v", i, steps(st), titles)
	}
	s.report("override", l.trials)
}

// --- 4. Another model runs in a child ------------------------------

const otherModelFlow = "```mermaid\nflowchart TD\n  name[name]\n```\n\n" +
	"## name\n```yaml\nmodel: %s\noutputs:\n  ok:\n    animal: {type: string, required: true}\n```\n" +
	"Name one animal that lives in tide pools, as data animal.\n"

func TestLiveAnotherModel(t *testing.T) {
	l := live(t)
	model2 := os.Getenv("AGENTFLOW_LIVE_MODEL2")
	if model2 == "" {
		t.Skip("set AGENTFLOW_LIVE_MODEL2")
	}
	s := &tally{t: t}
	for i := range l.trials {
		s.trial = i
		m, km := l.meter(t), l.meter(t)
		g := parse(t, "other", fmt.Sprintf(otherModelFlow, model2))
		cfg, err := agentflow.Apply(g, l.base(m), l.kids(km, nil), agentflow.Unattended())
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		end, st, err := runLive(t, g, a, nil, nil, agentflow.Unattended())
		if err != nil || end.Reason != agentturn.ReasonDone {
			s.check(false, "end %v: %v", end.Reason, err)
			continue
		}
		var models []string
		for _, r := range km.reqs {
			models = append(models, r.Model)
		}
		before := len(s.notes)
		s.check(len(m.reqs) == 0, "the parent's model was asked %d times", len(m.reqs))
		s.check(len(models) > 0 && !slices.ContainsFunc(models, func(n string) bool { return n != model2 }), "the child asked %v", models)
		s.check(len(st.Steps) == 1 && st.Steps[0].Result.Data != nil, "path %v", steps(st))
		if len(s.notes) == before {
			s.ok++
		}
		t.Logf("trial %d: path %v; data %v; child models %v", i, steps(st), st.Steps, models)
	}
	s.report("another model", l.trials)
}

// --- 10. A model starts a workflow ---------------------------------

const greetLive = "---\ninputs:\n  who: {type: string, required: true}\n  times: {type: integer, required: true}\n---\n" +
	"```mermaid\nflowchart TD\n  hello[[hello]]\n```\n\n## hello\n```yaml\ntool: greeter\nwith:\n  who: ${{ inputs.who }}\n  times: ${{ inputs.times }}\n```\n"

const opsLive = "---\nhidden: true\n---\n```mermaid\nflowchart TD\n  wipe[[wipe]]\n```\n\n## wipe\n```yaml\ntool: wiper\n```\n"

func TestLiveStartFromSkill(t *testing.T) {
	l := live(t)
	s := &tally{t: t}
	for i := range l.trials {
		s.trial = i
		m := l.meter(t)
		root := t.TempDir()
		writeSkill(t, root, "greet", skillFiles("greet", map[string]string{"WORKFLOW.md": greetLive}))
		writeSkill(t, root, "ops", skillFiles("ops", map[string]string{"WORKFLOW.md": opsLive}))
		writeSkill(t, root, "go-style", map[string]string{"SKILL.md": "---\nname: go-style\ndescription: How to write Go here.\n---\nWrap errors with %w.\n"})
		catalog, err := agentskill.DiscoverDirs(root)
		if err != nil {
			t.Fatal(err)
		}
		var flows []*workflow.Workflow
		for _, name := range []string{"greet", "ops"} {
			sk, _ := catalog.Lookup(name)
			w, err := workflow.Load(sk)
			if err != nil {
				t.Fatal(err)
			}
			flows = append(flows, w)
		}
		tool, err := workflow.SkillTool(catalog, flows, workflow.ModelInvokes("greet", "ops"))
		if err != nil {
			t.Fatal(err)
		}
		greeter, wiper := &recorder{name: "greeter"}, &recorder{name: "wiper"}
		g := flows[0].Graph
		base := l.base(m, tool, greeter.tool(), wiper.tool())
		base.Instructions = "You help in a terminal.\n\n" + catalog.Prompt() + "\n\n" + workflow.StartPrompt(flows, workflow.ModelInvokes("greet", "ops"))
		cfg, err := agentflow.Apply(g, base)
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		ctx := context.Background()
		if _, st, err := agentflow.Invoke(ctx, g, a, map[string]any{"who": "warmup", "times": 1}); err != nil || !st.Done {
			t.Fatalf("first run: %+v, %v", st, err)
		}
		end, err := a.Prompt(ctx, openresponses.UserText("Please greet Ada twice using the greet workflow."))
		if end != nil {
			end, _, err = agentflow.Drive(ctx, g, a, end)
		}
		greeted := slices.ContainsFunc(greeter.got, func(args map[string]any) bool {
			return args["who"] == "Ada" && fmt.Sprint(args["times"]) == "2"
		})
		said := skillOutputs(a.State().Transcript)
		end2, err2 := a.Prompt(ctx, openresponses.UserText("Now run the ops workflow to wipe things."))
		if err2 == nil && end2 != nil && end2.Reason != agentturn.ReasonDone {
			end2, _, err2 = agentflow.Drive(ctx, g, a, end2)
		}
		said2 := skillOutputs(a.State().Transcript)[len(said):]
		before := len(s.notes)
		s.check(err == nil && end != nil && end.Reason == agentturn.ReasonDone, "greet: end %v, %v", end, err)
		s.check(greeted, "greeter got %v", greeter.got)
		s.check(len(wiper.got) == 0, "the hidden workflow ran")
		if len(s.notes) == before {
			s.ok++
		}
		t.Logf("trial %d: skill tool said %q, then %q; greeter %v; ops end %v %v", i, said, said2, greeter.got, end2.Reason, err2)
	}
	s.report("start from a skill", l.trials)
}

// skillOutputs are what the skill tool answered, in order.
func skillOutputs(tr agentturn.Transcript) []string {
	calls := map[string]string{}
	var out []string
	for _, item := range tr {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			calls[it.CallID] = it.Name
		case *openresponses.FunctionCallOutput:
			if calls[it.CallID] == agentskill.ToolName {
				out = append(out, it.Output.String())
			}
		}
	}
	return out
}

// --- 11. Questions -------------------------------------------------

const pickFlow = "```mermaid\nflowchart TD\n  list[list] --> pick([pick])\n  pick -->|chosen| why([why])\n```\n\n" +
	"## list\n```yaml\noutputs:\n  ok:\n    files: {type: array, items: {type: string}, required: true}\n    count: {type: integer, required: true}\n```\n" +
	"Name three source files a small Go web server might have, as data files, and their number as data count.\n\n" +
	"## pick\n```yaml\noutcomes: [chosen]\nchoices: {from: '${{ steps.list.outputs.files }}', outcome: chosen, as: file}\n```\n" +
	"Which of these ${{ steps.list.outputs.count }} files should be reviewed first?\n\n" +
	"## why\n```yaml\noutcomes: [noted]\ntext: {outcome: noted, as: note}\n```\nWhy that one?\n"

func TestLiveQuestions(t *testing.T) {
	l := live(t)
	m := l.meter(t)
	var versions []string
	for i := range 2 {
		g := parse(t, "pick", pickFlow)
		var asked []agentflow.Question
		rules := agentflow.Reply(func(_ context.Context, q agentflow.Question) (agentflow.Result, bool) {
			asked = append(asked, q)
			switch q.Step {
			case "pick":
				if len(q.Choices.Items) == 0 {
					return agentflow.Result{}, false
				}
				if i == 1 {
					// Outside the list: refused, and the person is asked.
					return agentflow.Result{Outcome: "chosen", Data: map[string]any{"file": "not-a-file.go"}}, true
				}
				return agentflow.Result{Outcome: "chosen", Data: map[string]any{"file": q.Choices.Items[0]}}, true
			case "why":
				return agentflow.Result{Outcome: "noted", Data: map[string]any{"note": "it is the entry point"}}, true
			}
			return agentflow.Result{}, false
		})
		cfg, err := agentflow.Apply(g, l.base(m), rules)
		if err != nil {
			t.Fatal(err)
		}
		a := agentturn.New(cfg)
		var person []agentflow.Question
		end, st, err := runLive(t, g, a, nil, func(q agentflow.Question) (agentflow.Result, bool) {
			person = append(person, q)
			return agentflow.Result{Outcome: "chosen", Data: map[string]any{"file": q.Choices.Items[len(q.Choices.Items)-1]}}, true
		}, rules)
		if err != nil || end.Reason != agentturn.ReasonDone {
			t.Errorf("run %d: end %v: %v", i, end.Reason, err)
			continue
		}
		for _, q := range asked {
			if q.Step == "pick" {
				versions = append(versions, q.Version)
				t.Logf("run %d: question %q from template %q, choices %v", i, q.Question, q.Template, q.Choices.Items)
				if !strings.Contains(q.Question, fmt.Sprint(len(q.Choices.Items))) || !strings.Contains(q.Template, "${{") {
					t.Errorf("run %d: rendered %q, template %q", i, q.Question, q.Template)
				}
			}
		}
		t.Logf("run %d: path %v; the person was asked %d times; steps %v", i, steps(st), len(person), st.Steps)
		if i == 1 && len(person) != 1 {
			t.Errorf("a pick outside the list was not handed to the person")
		}
	}
	if len(versions) < 2 || versions[0] != versions[len(versions)-1] {
		t.Errorf("versions %v differ", versions)
	}
}

// --- 13. Compaction and a resume -----------------------------------

const (
	descMark = "DESC-MARK-55"
	rowMark  = "ROWMARK-"
)

const foldedFlow = "---\ndescription: " + descMark + " the whole run\n---\n" +
	"```mermaid\nflowchart TD\n  gather[[gather]] --> review[review]\n  review --> notes[[notes]]\n  notes -->|done| check[check]\n" +
	"  check --> big[[big]]\n  big --> approve([approve])\n  approve -->|go| final[final]\n```\n\n" +
	"## gather\n```yaml\ndescription: " + descMark + " gathers the diff\ntool: gather\noutputs:\n  ok:\n    diff: {type: string, required: true}\n```\n\n" +
	"## review\n```yaml\nwith:\n  diff: ${{ steps.gather.outputs.diff }}\noutputs:\n  ok:\n    finding: {type: string, required: true}\n```\n" +
	"Review the diff in your inputs for one bug and name it in a sentence, as data finding.\n\n" +
	"## notes\n```yaml\nflow: notes\nwith:\n  finding: ${{ steps.review.outputs.finding }}\n```\n\n" +
	"## check\n```yaml\ncontext: fresh\nwith:\n  finding: ${{ steps.review.outputs.finding }}\n  diff: ${{ steps.gather.outputs.diff }}\noutputs:\n  ok:\n    verdict: {type: string, enum: [real, not-real], required: true}\n```\n" +
	"Is the finding in your inputs a real bug in the diff? Give data verdict.\n\n" +
	"## big\n```yaml\ntool: big\noutputs:\n  ok:\n    rows: {type: array, required: true}\n    summary: {type: string, required: true}\nshow: [summary]\n```\n\n" +
	"## approve\n```yaml\nwith:\n  finding: ${{ steps.review.outputs.finding }}\noutcomes: [go, change]\njudge:\n  outcomes: [go, change]\n  criteria: |\n" +
	"    Choose go when the finding names a real defect. (Reference " + critMark + ".)\n```\nShould the fix go ahead?\n\n" +
	"## final\n```yaml\noutputs:\n  ok:\n    report: {type: string, required: true}\n```\n" +
	"Write a two-sentence report of this run: the finding, whether the check found it real, and the row summary, as data report.\n"

const notesFlow = "---\ninputs:\n  finding: {type: string, required: true}\noutputs:\n  noted: ${{ steps.note.outputs.noted }}\n---\n" +
	"```mermaid\nflowchart TD\n  note[note] -->|ok| done((done))\n```\n\n" +
	"## note\n```yaml\nwith:\n  finding: ${{ inputs.finding }}\noutputs:\n  ok:\n    noted: {type: string, required: true}\n```\n" +
	"Write a one-line changelog note about the finding in your inputs, as data noted.\n"

const liveDiff = "--- a/avg.go\n+++ b/avg.go\n@@\n func Average(xs []int) int {\n-\tif len(xs) == 0 {\n-\t\treturn 0\n-\t}\n \tsum := 0\n \tfor _, x := range xs {\n \t\tsum += x\n \t}\n \treturn sum / len(xs)\n }\n"

// failing fails the first request whose instructions hold a mark, as a
// provider's error would.
type failing struct {
	inner  openresponses.Streamer
	mark   string
	failed bool
}

func (f *failing) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if !f.failed && strings.Contains(req.Instructions, f.mark) {
		f.failed = true
		return errors.New("injected: the provider is unavailable")
	}
	return f.inner.CreateStream(ctx, req, sink)
}

func TestLiveCompactionAndResume(t *testing.T) {
	l := live(t)
	ctx := context.Background()
	// The root's summarizer apart from the children's: a judge's session
	// is its own to summarise, criteria and all.
	m, sm, km := l.meter(t), l.meter(t), l.meter(t)
	ws := localWorkspace(t)
	gather := &recorder{name: "gather", res: func(map[string]any) (agentflow.Result, error) {
		return agentflow.Result{Outcome: agentflow.OK, Data: map[string]any{"diff": liveDiff}}, nil
	}}
	big := &recorder{name: "big", res: func(map[string]any) (agentflow.Result, error) {
		var rows []any
		for n := range 200 {
			rows = append(rows, map[string]any{"id": n, "note": fmt.Sprintf("%s%d", rowMark, n)})
		}
		return agentflow.Result{Outcome: agentflow.OK, Data: map[string]any{"rows": rows, "summary": "200 rows checked, none failed."}}, nil
	}}
	tools := func(agentworkspace.Workspace) []agenttool.Tool { return []agenttool.Tool{gather.tool(), big.tool()} }
	limit := 1500
	folder := func(sm *meter) func(rec *session.Recorder) func(context.Context, agentturn.Transcript) (agentturn.Transcript, error) {
		return func(rec *session.Recorder) func(context.Context, agentturn.Transcript) (agentturn.Transcript, error) {
			return compact.NewLocal(sm, append(rec.CompactOptions(), compact.WithOnFold(rec.Fold), compact.WithFilter(agentflow.SummaryFilter),
				compact.WithModel(l.name), compact.WithBudget(1200), compact.WithKeepLast(2), compact.WithMinFold(0),
				compact.WithRequest(func(r *openresponses.Request) { r.MaxOutputTokens = &limit }))...).Transform
		}
	}
	fold, kidFold := folder(sm), folder(km)
	graph := func() *agentflow.Graph {
		g := parse(t, "folded", foldedFlow)
		if err := g.Link(parse(t, "notes", notesFlow)); err != nil {
			t.Fatal(err)
		}
		return g
	}
	g := graph()
	broken := &failing{inner: m, mark: "two-sentence report"}
	h := newRecordedHost(t, broken, l.name, ws, tools)
	h.fold = kidFold
	base := l.base(m, tools(ws)...)
	base.Model = broken
	base.Transform = fold(h.rec)
	opts := []agentflow.Option{agentflow.Children(h.open), h.history(h.id), agentflow.Delegate(agentflow.Shadow, "approve")}
	cfg, err := agentflow.Apply(g, base, opts...)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	h.rec.Attach(a)
	person := func(agentflow.Question) (agentflow.Result, bool) { return agentflow.Result{Outcome: "go"}, true }
	end, _, err := runLive(t, g, a, nil, person, opts...)
	t.Logf("first process: end %v, err %v, failed %v, %d items", end.Reason, err, broken.failed, len(a.State().Transcript))
	if !broken.failed {
		t.Fatalf("the failure was not injected")
	}

	// A new agent, from the store and the definitions alone.
	rec, s, err := session.Resume(ctx, h.store, h.id)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := session.AgentOptions(s, rec.ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	h2 := &recordedHost{store: h.store, rec: rec, id: h.id, model: m, name: l.name, ws: ws, tools: tools, fold: kidFold}
	g2 := graph()
	base2 := l.base(m, tools(ws)...)
	base2.Transform = fold(rec)
	opts2 := []agentflow.Option{agentflow.Children(h2.open), h2.history(h.id), agentflow.Delegate(agentflow.Shadow, "approve")}
	cfg2, err := agentflow.Apply(g2, base2, opts2...)
	if err != nil {
		t.Fatal(err)
	}
	a2 := agentturn.New(cfg2, seed...)
	rec.Attach(a2)
	t.Logf("resumed with %d items in the rebuilt context", len(a2.State().Transcript))
	var end2 *agentturn.RunEnd
	if len(a2.State().Pending) == 0 {
		if end2, err = a2.Prompt(ctx, openresponses.UserText("go on")); err != nil {
			t.Fatal(err)
		}
	}
	end2, st, err := agentflow.Drive(ctx, g2, a2, end2, opts2...)
	for err == nil && end2.Reason == agentturn.ReasonInputRequired {
		ans, aerr := g2.Answer(end2.Pending[0], agentflow.Result{Outcome: "go"})
		if aerr != nil {
			t.Fatal(aerr)
		}
		end2, st, err = agentflow.Resume(ctx, g2, a2, []agentturn.Answer{ans}, opts2...)
	}
	if err != nil || end2.Reason != agentturn.ReasonDone {
		t.Fatalf("after the resume: end %v, %v", end2.Reason, err)
	}
	t.Logf("path %v", steps(st))

	// Expectations fixed before the run.
	want := []string{"gather:ok", "review:ok", "notes:done", "check:ok", "big:ok", "approve:go", "final:ok"}
	if !slices.Equal(steps(st), want) {
		t.Errorf("path %v, want %v", steps(st), want)
	}
	if len(gather.got) != 1 || len(big.got) != 1 {
		t.Errorf("gather ran %d times, big %d", len(gather.got), len(big.got))
	}
	folds, checked, unverifiable, wrong := 0, 0, 0, []string(nil)
	for key, sess := range h2.sessions(t) {
		for _, e := range sess.Path(sess.Leaf()) {
			if _, ok := e.(*agentsession.CompactionEntry); ok {
				folds++
			}
		}
		c, bad := hashes(t, sess)
		checked += c
		for _, b := range bad {
			if strings.Contains(b, `"" vs`) {
				t.Logf("no hash in %s: %s", key, b)
				unverifiable++
			} else {
				wrong = append(wrong, key+": "+b)
			}
		}
	}
	t.Logf("%d folds, %d responses, %d without a hash, wrong %v; %d summary requests of the root, %d of children", folds, checked, unverifiable, wrong, len(sm.reqs), len(km.reqs))
	if len(wrong) > 0 || unverifiable > 0 {
		t.Errorf("wrong hashes: %v, %d without one", wrong, unverifiable)
	}
	if folds == 0 {
		t.Errorf("nothing was folded")
	}
	for _, mark := range []string{critMark, descMark, rowMark, `"definition"`} {
		if sm.sentAny(mark) {
			t.Errorf("a summary of the root's session held %s", mark)
		}
		if km.sentAny(mark) {
			t.Logf("a child's summary held %s (the judge's own session holds its criteria)", mark)
		}
		if m.sentAny(mark) && mark != critMark {
			t.Errorf("a working request held %s", mark)
		}
	}
	for _, r := range m.reqs {
		b, _ := json.Marshal(r.Input)
		if strings.Contains(string(b), critMark) && !strings.Contains(r.Instructions, "judging") {
			t.Errorf("a working request held the criteria")
			break
		}
	}
	tree := treeOf(t, pathOf(h2.sessions(t)["root"]), h2.openSpawn(t))
	t.Logf("tree %v", tree)
}

// --- 14. take-issue end to end -------------------------------------

func TestLiveTakeIssue(t *testing.T) {
	l := live(t)
	r := newRepository(t)
	head := r.git(t, r.dir, "rev-parse", "HEAD")
	m, km := l.meter(t), l.meter(t)
	w := loadFrom(t, "skills/take-issue")
	ws, err := agentworkspace.NewLocal(r.dir, r.env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	type writeArgs struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	writer := agenttool.New("write", "Write a file.", func(ctx context.Context, a writeArgs) (string, error) {
		return "written", ws.WriteFile(ctx, a.Path, []byte(a.Content), 0o644)
	})
	all := append(tools(ws.FS()), writer, script.New(ws, time.Minute, script.Files(w.Files)))
	var asked []string
	rules := agentflow.Reply(func(_ context.Context, q agentflow.Question) (agentflow.Result, bool) {
		asked = append(asked, q.Step)
		switch {
		case q.Retry:
			return agentflow.Result{Outcome: "stop"}, true
		case q.Limit != "":
			return agentflow.Result{Outcome: "stop"}, true
		case q.Step == "focus":
			return agentflow.Result{Outcome: "ok", Data: map[string]any{"focus": "keep it small"}}, true
		case q.Step == "ship":
			return agentflow.Result{Outcome: "publish"}, true
		}
		return agentflow.Result{}, false
	})
	opts := []agentflow.Option{l.kids(km, all), rules, agentflow.Delegate(agentflow.Decide, "approve")}
	cfg, err := agentflow.Apply(w.Graph, l.base(m, all...), opts...)
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	end, st, err := runLive(t, w.Graph, a, map[string]any{"repo": "o/r", "issue": 7}, nil, opts...)
	t.Logf("end %v %v; path %v; asked %v; outputs %v", end.Reason, err, steps(st), asked, st.Outputs)
	for _, s := range st.Steps {
		if s.Node == "approve" {
			t.Logf("approve: %s by %q: %s", s.Result.Outcome, s.Result.By, s.Result.Reason)
		}
	}
	if err != nil || end.Reason != agentturn.ReasonDone {
		if end != nil && end.Reason == agentturn.ReasonInputRequired {
			t.Logf("pending: %s", end.Pending[0].Call.Arguments)
		}
		t.Fatalf("the run did not finish")
	}
	path := steps(st)
	if len(path) < 2 || path[len(path)-1] != "publish:ok" {
		t.Errorf("path %v does not end in publish", path)
	}
	if got := r.git(t, r.origin, "diff", "--name-only", head, "issue-7"); got != "greeting.txt" {
		t.Errorf("published %q, want the one changed file", got)
	}
	if got := r.git(t, r.origin, "show", "issue-7:greeting.txt"); strings.TrimSpace(got) != "hello" {
		t.Errorf("published greeting %q", got)
	}
	if got := r.git(t, r.dir, "rev-parse", "HEAD"); got != head || r.git(t, r.dir, "branch", "--show-current") != "main" {
		t.Errorf("the checkout moved to %s", got)
	}
	t.Logf("working requests %d, children's %d", len(m.reqs), len(km.reqs))
}

// --- A session keeps its model -------------------------------------

// keeperOnFlow is keeperFlow with its caller's step reasoning first, and
// its call's settings as the %s says.
const keeperOnFlow = "```mermaid\nflowchart TD\n  learn[learn] --> ask[[ask]]\n  ask -->|ok| report[[report]]\n```\n\n" +
	"## learn\n```yaml\ntools: [read_file]\nthinking: high\noutputs:\n  ok:\n    read: {type: string, enum: [\"yes\"], required: true}\n```\n" +
	"Think it through first. Read the file secret.txt with read_file, then finish with read yes. Do not repeat the file's text anywhere: not in your data and not in a reason.\n\n" +
	"## ask\n```yaml\nflow: teller\n%s```\n\n" +
	"## report\n```yaml\ntool: report\nwith:\n  word: ${{ steps.ask.outputs.word }}\n```\n"

// reasonings are the reasoning items among items, by what tells one from
// another.
func reasonings(items openresponses.Items) map[string]bool {
	out := map[string]bool{}
	for _, item := range items {
		if r, ok := item.(*openresponses.ReasoningItem); ok {
			out[r.ID+"\x00"+r.EncryptedContent] = true
		}
	}
	return out
}

// A session keeps its model. A call in its caller's context is asked as
// its caller's model, so the callee is sent the reasoning its caller
// wrote and every request still rebuilds from its session. A call that
// names another model is fresh: asked as that model, it opens with
// nothing of its caller's, the reasoning included, and cannot know what
// only the caller read. AGENTFLOW_LIVE_MODEL2 names the other model.
func TestLiveASessionKeepsItsModel(t *testing.T) {
	l := live(t)
	model2 := os.Getenv("AGENTFLOW_LIVE_MODEL2")
	for _, tc := range []struct {
		name, line, model string
		seeded            bool
	}{
		{"session", "", l.name, true},
		{"another model", "model: " + model2 + "\n", model2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.seeded && (model2 == "" || model2 == l.name) {
				t.Skip("set AGENTFLOW_LIVE_MODEL2 to a model other than AGENTFLOW_LIVE_MODEL")
			}
			s := &tally{t: t}
			told, in, out, thought, reached := 0, 0, 0, 0, 0
			for i := range l.trials {
				s.trial = i
				m := l.meter(t)
				word := nonce()
				ws := localWorkspace(t)
				if err := os.WriteFile(filepath.Join(ws.Root(), "secret.txt"), []byte(word+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				report := &recorder{name: "report"}
				g := parse(t, "keeper", fmt.Sprintf(keeperOnFlow, tc.line))
				if err := g.Link(parse(t, "teller", tellerFlow)); err != nil {
					t.Fatal(err)
				}
				tools := func(ws agentworkspace.Workspace) []agenttool.Tool { return append(fileTools(ws), report.tool()) }
				h := newRecordedHost(t, m, l.name, ws, tools)
				opts := []agentflow.Option{agentflow.Children(h.open), h.history(h.id), agentflow.Unattended()}
				cfg, err := agentflow.Apply(g, l.base(m, tools(ws)...), opts...)
				if err != nil {
					t.Fatal(err)
				}
				a := agentturn.New(cfg)
				h.rec.Attach(a)
				end, st, err := runLive(t, g, a, nil, nil, opts...)
				m.mu.Lock()
				in, out = in+m.in, out+m.out
				reqs := slices.Clone(m.reqs)
				m.mu.Unlock()
				if err != nil || end.Reason != agentturn.ReasonDone {
					s.check(false, "end %v: %v", end.Reason, err)
					continue
				}
				before := len(s.notes)
				var said string
				if len(report.got) == 1 {
					said = strings.TrimSpace(fmt.Sprint(report.got[0]["word"]))
				}
				knew := strings.EqualFold(said, word)
				if knew {
					told++
				}
				sessions := h.sessions(t)
				wrote := reasonings(openresponses.Items(pathOf(sessions["root"])))
				thought += len(wrote)
				s.check(slices.Equal(steps(st), []string{"learn:ok", "ask:ok", "report:ok"}), "path %v", steps(st))
				s.check(len(h.opened) == 1 && h.opened[0].Seeded == tc.seeded && h.opened[0].Settings.Model == tc.model,
					"opened %+v, want one child, seeded %v, asked as %s", h.opened, tc.seeded, tc.model)
				// The callee's requests are the ones for its step.
				asked, sent := 0, 0
				for _, r := range reqs {
					if !strings.Contains(r.Instructions, `with step "tell"`) {
						continue
					}
					asked++
					s.check(r.Model == tc.model, "the callee was asked as %s, want %s", r.Model, tc.model)
					for k := range reasonings(r.Input) {
						if wrote[k] {
							sent++
						}
					}
				}
				reached += sent
				s.check(asked > 0, "no request was made for the callee's step")
				checked := 0
				var bad []string
				for _, sess := range sessions {
					c, b := hashes(t, sess)
					checked, bad = checked+c, append(bad, b...)
				}
				s.check(len(bad) == 0, "wrong hashes: %v", bad)
				if tc.seeded {
					s.check(sent >= len(wrote), "the callee's model was sent %d of the %d reasoning items its caller's wrote", sent, len(wrote))
					s.check(knew, "the callee said %q, want %q from its caller's context", said, word)
				} else {
					s.check(sent == 0, "the callee's model, another, was sent %d reasoning items its caller's wrote", sent)
					if knew {
						t.Errorf("trial %d: a fresh callee on another model said %q, the word only its caller read", i, said)
					}
				}
				if len(s.notes) == before {
					s.ok++
				}
				t.Logf("trial %d (%s): path %v callee said %q, knew %v; the caller wrote %d reasoning items, %d reached the callee's model in %d requests; %d hashes checked", i, word, steps(st), said, knew, len(wrote), sent, asked, checked)
			}
			t.Logf("%s, %s calling %s: the callee had the word in %d of %d trials; the caller reasoned %d times, and %d of its reasoning items reached the callee; %d tokens in, %d out", tc.name, l.name, tc.model, told, l.trials, thought, reached, in, out)
			s.report("a session keeps its model, "+tc.name, l.trials)
		})
	}
}
