package script_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentflow/script"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// finisher finishes the step it is on, and keeps what it was sent.
type finisher struct{ requests []openresponses.Request }

func (m *finisher) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.requests = append(m.requests, req)
	e := openresponses.NewEmitter(sink, &openresponses.Response{Model: req.Model})
	call, err := e.FunctionCall("", "flow_finish")
	if err != nil {
		return err
	}
	if err := call.Arguments(withRun(req, `{"step":"review","outcome":"ok"}`)); err != nil {
		return err
	}
	if err := call.Close(); err != nil {
		return err
	}
	return e.Complete()
}

func TestProgressIsKeptInTheSessionAndSentToNoModel(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	g := &agentflow.Graph{Name: "p", Nodes: []agentflow.Node{
		{ID: "build", Kind: agentflow.Run, Script: "build.sh", Next: map[string]string{"ok": "review"}},
		{ID: "review", Kind: agentflow.Agent, Prompt: "Review the build."},
	}}
	// More lines than are kept, the first two of them told apart.
	ws := workspace(t, map[string]string{"build.sh": fmt.Sprintf(`printf '{"progress": 1, "total": 2, "message": "fetching-zq"}\n' >> "$AGENTFLOW_PROGRESS"
printf 'compiling-zq\n' >> "$AGENTFLOW_PROGRESS"
i=0; while [ $i -lt %d ]; do printf 'line-zq\n' >> "$AGENTFLOW_PROGRESS"; i=$((i+1)); done
echo '{"ok": true}'`, script.MaxKeptProgress)})
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, s, err := session.Start(ctx, store, agentsession.Header{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	m := &finisher{}
	cfg, err := agentflow.Apply(g, agentturn.Config{Model: m, ModelName: "fake", ToolRecorder: rec.RecordFunc(),
		Tools: []agenttool.Tool{script.New(ws, time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	rec.Attach(a)
	end, err := a.Prompt(ctx, openresponses.UserText("go"))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, err = %v", end, err)
	}
	st, err := g.Replay(a.State().Transcript)
	if err != nil || !st.Done {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if err := store.Sync(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}

	// Read back by another store, as a front would after a restart.
	again, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := again.Read(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	var kept []script.ProgressRecord
	var call string
	for _, e := range got.Path(got.Leaf()) {
		c, ok := e.(*agentsession.CustomEntry)
		if !ok || c.NS != script.ProgressNS {
			continue
		}
		var r script.ProgressRecord
		if err := json.Unmarshal(c.Data, &r); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, r)
		if call != "" && c.CallID != call {
			t.Errorf("records name calls %s and %s", call, c.CallID)
		}
		call = c.CallID
	}
	if len(kept) != script.MaxKeptProgress {
		t.Fatalf("kept %d updates, want the bound, %d", len(kept), script.MaxKeptProgress)
	}
	if kept[0] != (script.ProgressRecord{Seq: 1, Progress: 1, Total: 2, Message: "fetching-zq"}) || kept[1] != (script.ProgressRecord{Seq: 2, Message: "compiling-zq"}) {
		t.Errorf("first records = %+v, %+v", kept[0], kept[1])
	}
	if !strings.HasPrefix(call, "flow_") || !strings.Contains(call, "_build_v1") {
		t.Errorf("records name call %q, want the build step's", call)
	}

	// No model was sent any of it, and the transcript holds none.
	if len(m.requests) == 0 {
		t.Fatal("the model was never asked")
	}
	for i, req := range m.requests {
		b, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "-zq") {
			t.Errorf("request %d holds progress: %s", i, b)
		}
	}
	for _, item := range a.State().Transcript {
		b, _ := json.Marshal(item)
		if strings.Contains(string(b), "-zq") {
			t.Errorf("the transcript holds progress: %s", b)
		}
	}
}
