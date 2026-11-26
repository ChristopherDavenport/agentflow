package agentflow

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// special is a tool only a child's opener offers.
func special(calls *int) agenttool.Tool {
	return agenttool.New("special", "Do the special thing.", func(context.Context, struct{}) (Result, error) {
		*calls++
		return Result{Outcome: OK}, nil
	})
}

// handsOff calls, in a child, a workflow whose step calls tool with args.
func handsOff(t *testing.T, tool, args string) *Graph {
	t.Helper()
	inner := &Graph{Name: "inner", Nodes: []Node{{ID: "go", Kind: Run, Tool: tool, Args: json.RawMessage(args)}}}
	return linked(t, &Graph{Name: "handsoff", Nodes: []Node{{ID: "call", Kind: Run, Flow: "inner", Context: ContextFresh}}}, inner)
}

func TestAChildsToolsAreItsOpeners(t *testing.T) {
	// The parent offers no special tool, and is not held to one only its
	// child calls.
	var calls int
	g := handsOff(t, "special", `{}`)
	m := &fakeModel{}
	kids := newKin(m, "fake", special(&calls))
	a := nestedAgent(t, g, m, nil, kids.option())
	end, st, err := Drive(t.Context(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone || !st.Done || calls != 1 {
		t.Fatalf("end = %+v, %v, done %v, calls %d", end, err, st.Done, calls)
	}
}

func TestAChildWhoseToolsDoNotFitFailsItsCall(t *testing.T) {
	for _, tc := range []struct {
		name, tool, args string
		offered          []agenttool.Tool
		want             string
	}{
		{"a missing tool", "special", `{}`, nil, `node "go" names the tool special, which the config does not offer`},
		{"a missing argument", "tag", `{"sign":true}`, []agenttool.Tool{tagger()}, `parameter "name" is required`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := handsOff(t, tc.tool, tc.args)
			m := &fakeModel{}
			kids := newKin(m, "fake", tc.offered...)
			a := nestedAgent(t, g, m, nil, kids.option())
			end, _, err := Drive(t.Context(), g, a, start(t, a))
			if err != nil {
				t.Fatal(err)
			}
			// The call fails as the step's own would, and the step asks to be
			// run again, with the check's own words.
			q := question(t, end)
			if !q.Retry || q.Step != "call" || !strings.Contains(q.Question, tc.want) || !strings.Contains(q.Question, "cannot run on the tools it was given") {
				t.Fatalf("question = %+v", q)
			}
			// Unattended, the run fails with them.
			u := nestedAgent(t, g, m, nil, newKin(m, "fake", tc.offered...).option(), Unattended())
			_, _, err = Drive(t.Context(), g, u, start(t, u))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("unattended err = %v", err)
			}
		})
	}
}
