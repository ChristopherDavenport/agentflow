package agentflow

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// sayer is a tool that says its name, and counts its calls.
func sayer(name string, calls *int) agenttool.Tool {
	return agenttool.New(name, "Say "+name+".", func(context.Context, struct{}) (string, error) {
		*calls++
		return name, nil
	})
}

// offered are the host's tools a request offers, the workflow's left out.
func offered(req openresponses.Request) []string {
	var out []string
	for _, t := range req.Tools {
		if fn, ok := t.(*openresponses.FunctionTool); ok && !strings.HasPrefix(fn.Name, reserved) {
			out = append(out, fn.Name)
		}
	}
	slices.Sort(out)
	return out
}

func allButBash() *Graph {
	return &Graph{Name: "tidy", ExcludeTools: []string{"bash"}, Nodes: []Node{
		{ID: "work", Kind: Agent, Prompt: "Tidy up.", AllTools: true, ExcludeTools: []string{"fetch"}},
	}}
}

func TestToolsAllOffersEveryToolButThoseExcluded(t *testing.T) {
	var reads, bashes, fetches int
	m := &cast{script: []says{{"bash", `{}`}, {"read", `{}`}, {finishTool, `{"step":"work","outcome":"ok"}`}}}
	a := nestedAgent(t, allButBash(), m, []agenttool.Tool{sayer("read", &reads), sayer("bash", &bashes), sayer("fetch", &fetches)})
	end := start(t, a)
	if end.Reason != agentturn.ReasonDone || end.Err != nil {
		t.Fatalf("end = %s, %v", end.Reason, end.Err)
	}
	if got := offered(m.requests[0]); !slices.Equal(got, []string{"read"}) {
		t.Errorf("offered %v", got)
	}
	// An excluded tool is not only left out of the request: a call to it
	// is refused.
	if bashes != 0 || reads != 1 || fetches != 0 {
		t.Errorf("bash ran %d, read %d, fetch %d", bashes, reads, fetches)
	}
}

func TestExcludedToolsAreCheckedAtApply(t *testing.T) {
	var n int
	tools := []agenttool.Tool{sayer("read", &n), sayer("bash", &n), sayer("fetch", &n)}
	g := allButBash()
	g.ExcludeTools = []string{"bsh"}
	if _, err := Apply(g, agentturn.Config{Model: &cast{}, ModelName: "fake", Tools: tools}); err == nil || !strings.Contains(err.Error(), `excludes the tool bsh, which the config does not offer; did you mean "bash"?`) {
		t.Errorf("workflow's: %v", err)
	}
	g = allButBash()
	g.Nodes[0].ExcludeTools = []string{"fetc"}
	if _, err := Apply(g, agentturn.Config{Model: &cast{}, ModelName: "fake", Tools: tools}); err == nil || !strings.Contains(err.Error(), `names the tool fetc, which the config does not offer; did you mean "fetch"?`) {
		t.Errorf("step's: %v", err)
	}
}

func TestExcludedToolsAreCheckedAtLoad(t *testing.T) {
	for name, change := range map[string]struct {
		edit func(*Graph)
		want string
	}{
		"named and excluded": {func(g *Graph) { g.Nodes[0].AllTools, g.Nodes[0].Tools = false, []string{"bash"} }, "names the tool bash, which it excludes, or its workflow does"},
		"both":               {func(g *Graph) { g.Nodes[0].Tools = []string{"read"} }, "tools is all, or a list of tools, not both"},
		"program's":          {func(g *Graph) { g.ExcludeTools = []string{ScriptTool} }, "is the program's"},
		"not a model step": {func(g *Graph) {
			g.Nodes = append(g.Nodes, Node{ID: "run", Kind: Run, Tool: "read", ExcludeTools: []string{"bash"}})
			g.Nodes[0].Next = map[string]string{OK: "run"}
		}, "a model step's"},
	} {
		t.Run(name, func(t *testing.T) {
			g := allButBash()
			change.edit(g)
			if err := g.Validate(); err == nil || !strings.Contains(err.Error(), change.want) {
				t.Errorf("err = %v, want %q", err, change.want)
			}
		})
	}
}

func TestAChildIsOfferedWhatItsStepIs(t *testing.T) {
	g := allButBash()
	g.Nodes[0].Context = ContextFresh
	var n int
	m := &cast{script: []says{{finishTool, `{"step":"work","outcome":"ok"}`}}}
	tools := []agenttool.Tool{sayer("read", &n), sayer("bash", &n), sayer("fetch", &n)}
	k := newKin(m, "fake", tools...)
	a := nestedAgent(t, g, m, tools, k.option())
	end, _, err := Drive(context.Background(), g, a, start(t, a))
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("end = %+v, %v", end, err)
	}
	if len(m.requests) != 1 {
		t.Fatalf("%d requests", len(m.requests))
	}
	if got := offered(m.requests[0]); !slices.Equal(got, []string{"read"}) {
		t.Errorf("the child was offered %v", got)
	}
}
