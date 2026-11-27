package agentflow

import (
	"context"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// This file makes finished trees of sessions for the summary package's
// tests, which are in summary_test.go, outside this package: a summary
// reads only what a host can, and the runs are made with this package's
// test hosts.

// metered reports that each response used 10 tokens in and 2 out.
type metered struct{ openresponses.Streamer }

func (m metered) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	return m.Streamer.CreateStream(ctx, req, openresponses.EventSinkFunc(func(ev openresponses.StreamEvent) error {
		if done, ok := ev.(*openresponses.ResponseCompletedEvent); ok && done.Response != nil {
			done.Response.Usage = &openresponses.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
		}
		return sink.Send(ev)
	}))
}

// RecordedResponse is a response a session holds, as a host reads it
// back: the model, what it used, and how many of the record's items came
// before its own.
type RecordedResponse struct {
	Model string
	Usage openresponses.Usage
	At    int
}

// Sums is a tree of sessions as a summary reads it: the runs of the
// session nobody called, read with the tree's children, and the
// responses of each session, nil for a host that kept none.
type Sums struct {
	Runs      []*Instance
	Responses func(spawn string) ([]RecordedResponse, error)
}

func sumsOf(t *testing.T, record agentturn.Transcript, open func(string) (agentturn.Transcript, error)) Sums {
	t.Helper()
	runs, err := Runs(record, open)
	if err != nil {
		t.Fatal(err)
	}
	return Sums{Runs: runs}
}

// stored is a tree read from a store: the session id names, and every
// session spawned below it, by spawn.
func stored(t *testing.T, store *jsonl.Store, id string) Sums {
	t.Helper()
	ctx := context.Background()
	ids := map[string]string{"": id}
	for sum, err := range store.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		if sum.Header.SpawnedBy != "" {
			ids[sum.Header.SpawnedBy] = sum.Header.ID
		}
	}
	find := func(spawn string) (*agentsession.Session, error) {
		sid, ok := ids[spawn]
		if !ok {
			return nil, errors.New("no session of spawn " + spawn)
		}
		return store.Open(ctx, sid)
	}
	root, err := find("")
	if err != nil {
		t.Fatal(err)
	}
	out := sumsOf(t, pathItems(root), func(spawn string) (agentturn.Transcript, error) {
		s, err := find(spawn)
		if err != nil {
			return nil, err
		}
		return pathItems(s), nil
	})
	out.Responses = func(spawn string) ([]RecordedResponse, error) {
		s, err := find(spawn)
		if err != nil {
			return nil, err
		}
		return responsesOf(s), nil
	}
	return out
}

// responsesOf reads a session's responses back as a host does: each
// with the items of the session's record before its own. A session's
// record is its own entries: a fork's opens after what it inherited.
func responsesOf(s *agentsession.Session) []RecordedResponse {
	path := session.OwnPath(s)
	first, n := map[string]int{}, 0
	for _, e := range path {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if _, seen := first[it.ResponseID]; it.ResponseID != "" && !seen {
				first[it.ResponseID] = n
			}
			n++
		}
	}
	var out []RecordedResponse
	n = 0
	for _, e := range path {
		switch e := e.(type) {
		case *agentsession.ItemEntry:
			n++
		case *agentsession.ResponseEntry:
			at, ok := first[e.ResponseID]
			if !ok {
				at = n
			}
			r := RecordedResponse{Model: e.Model, At: at}
			if e.Usage != nil {
				r.Usage = *e.Usage
			}
			out = append(out, r)
		}
	}
	return out
}

// SumsOfARun is the release workflow run to its end, on a host that
// kept no responses.
func SumsOfARun(t *testing.T) Sums {
	t.Helper()
	var ran []string
	cfg, err := Apply(release(), agentturn.Config{Model: metered{&fakeModel{outcomes: []string{"clean"}}}, ModelName: "fake", Tools: []agenttool.Tool{scriptTool(&ran)}})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	return sumsOf(t, a.State().Transcript, under(a))
}

// SumsOfANest is top, which calls middle, which calls countdown from 2,
// each in a child that opens with its caller's context, in memory.
func SumsOfANest(t *testing.T) Sums {
	t.Helper()
	g := linked(t, top(), middle(), countdown())
	var counts []int
	var ran []string
	a := nestedAgent(t, g, metered{&fakeModel{outcomes: []string{OK}}}, []agenttool.Tool{countTool(&counts), scriptTool(&ran)}, Unattended())
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	return sumsOf(t, a.State().Transcript, under(a))
}

// SumsOfFreshChildren is outer, whose fresh step calls thinker, which
// calls asker: children that open with nothing. With read false the
// children's sessions are not read.
func SumsOfFreshChildren(t *testing.T, read bool) Sums {
	t.Helper()
	g := linked(t, outer(), thinker(), asker())
	var ran []string
	m := metered{&fakeModel{outcomes: []string{OK}}}
	kids := newKin(m, "fake")
	a := nestedAgent(t, g, m, []agenttool.Tool{scriptTool(&ran)}, kids.option(), Unattended(), Reply(func(context.Context, Question) (Result, bool) {
		return Result{Outcome: "yes"}, true
	}))
	end, _, err := Drive(t.Context(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if !read {
		return sumsOf(t, a.State().Transcript, nil)
	}
	return sumsOf(t, a.State().Transcript, kids.read)
}

// SumsAfterAKill is the release workflow whose process died at its
// question, after one model request, and went on in another from the
// store: the review before the kill, the fix and the review after it.
func SumsAfterAKill(t *testing.T) Sums {
	t.Helper()
	var ran []string
	tool := func() agenttool.Tool { return scriptTool(&ran) }
	var kept *jsonl.Store
	var id string
	a := killedRecord(t,
		func() agentturn.Config {
			return apply(t, release(), metered{&fakeModel{outcomes: []string{"findings"}}}, tool())
		},
		func(store *jsonl.Store, sid string) agentturn.Config {
			kept, id = store, sid
			cfg, err := Apply(release(), agentturn.Config{Model: metered{&fakeModel{outcomes: []string{OK, "clean"}}}, ModelName: "fake", Tools: []agenttool.Tool{tool()}},
				recordOf(store, sid))
			if err != nil {
				t.Fatal(err)
			}
			return cfg
		},
		func(t *testing.T, a *agentturn.Agent) {
			question(t, start(t, a))
			die()
		})
	g := release()
	end := reply(t, g, a, &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: a.State().Pending}, "fix")
	if end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	return stored(t, kept, id)
}

// SumsOfARecordedNest is top, middle and countdown with every run in a
// recorded session of its own, a fork of its caller's.
func SumsOfARecordedNest(t *testing.T) Sums {
	t.Helper()
	ctx := context.Background()
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
	if err != nil {
		t.Fatal(err)
	}
	g := linked(t, top(), middle(), countdown())
	var counts []int
	var ran []string
	tools := []agenttool.Tool{countTool(&counts), scriptTool(&ran)}
	kids := &recordedKin{store: store, rec: rec, model: metered{&fakeModel{outcomes: []string{OK}}}, name: "fake", tools: tools}
	a := nestedAgent(t, g, metered{&fakeModel{outcomes: []string{OK}}}, tools, kids.option(), Unattended(), recordOf(store, s.ID()))
	rec.Attach(a)
	if end := start(t, a); end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v", end)
	}
	return stored(t, store, s.ID())
}

// SumsOfAKeptChild is keeper, whose draft step runs twice and whose
// polish step once, all in one kept child's session, recorded.
func SumsOfAKeptChild(t *testing.T) Sums {
	t.Helper()
	ctx := context.Background()
	store, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: "/"})
	if err != nil {
		t.Fatal(err)
	}
	var flips int
	m := metered{&fakeModel{outcomes: []string{OK, OK, OK}}}
	kids := &recordedKin{store: store, rec: rec, model: m, name: "fake"}
	g := keeper()
	a := nestedAgent(t, g, m, []agenttool.Tool{flip(&flips)}, kids.option(), Models("fake", "small"), recordOf(store, s.ID()))
	rec.Attach(a)
	end, st, err := Drive(ctx, g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done {
		t.Fatalf("end = %+v, %v", end, err)
	}
	return stored(t, store, s.ID())
}

// SumsOfAnAbandonedNest is top, middle and countdown stopped at middle's
// question and abandoned by a new invocation, which stops there too: the
// first run abandoned, and the second under way.
func SumsOfAnAbandonedNest(t *testing.T) Sums {
	t.Helper()
	g := linked(t, top(), middle(), countdown())
	var counts []int
	var ran []string
	a := nestedAgent(t, g, metered{&fakeModel{outcomes: []string{"ok", "ok"}}}, []agenttool.Tool{countTool(&counts), scriptTool(&ran)})
	question(t, start(t, a))
	end, _, err := Invoke(context.Background(), g, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	question(t, end)
	return sumsOf(t, a.State().Transcript, under(a))
}

// SumsOfAnOverride is careful, whose child waits on a question, with an
// operator's abandon of the run: the child is set aside after its
// cleanup. With read false the child's session is not read.
func SumsOfAnOverride(t *testing.T, read bool) Sums {
	t.Helper()
	g := linked(t, careful(), tidyIn())
	var ran []string
	m := &fakeModel{}
	kids := newKin(m, "fake", undoTool(&ran, nil))
	a := nestedAgent(t, g, m, nil, kids.option())
	end, _, err := Drive(t.Context(), g, a, start(t, a))
	if err != nil {
		t.Fatal(err)
	}
	q := question(t, end)
	if _, _, err := OverrideRun(t.Context(), g, a, Override{Action: OverrideAbandon, Run: q.Path[0].Run}); err != nil {
		t.Fatal(err)
	}
	if !read {
		return sumsOf(t, a.State().Transcript, nil)
	}
	return sumsOf(t, a.State().Transcript, kids.read)
}
