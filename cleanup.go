package agentflow

import (
	"fmt"
	"slices"
)

// reachable is every node some path from id reaches, id among them.
func (g *Graph) reachable(id string) map[string]bool {
	seen := map[string]bool{}
	queue := []string{id}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		n := g.node(at)
		if n == nil || seen[at] {
			continue
		}
		seen[at] = true
		for _, next := range n.Next {
			queue = append(queue, next)
		}
	}
	return seen
}

// cleanup is the steps of the workflow's cleanup path, none without one.
func (g *Graph) cleanup() map[string]bool {
	if g.OnAbort == "" || g.node(g.OnAbort) == nil {
		return nil
	}
	return g.reachable(g.OnAbort)
}

// checkCleanup holds a cleanup path to what can never keep a run from
// ending: script, tool and person steps, no loop, and no step the
// workflow's own path reaches, so it can neither go back into the
// workflow nor be reached from it.
func (g *Graph) checkCleanup() []error {
	if g.OnAbort == "" {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("agentflow: "+format, args...)) }
	if g.node(g.OnAbort) == nil {
		bad("on_abort names no step %q%s", g.OnAbort, suggest(g.OnAbort, g.nodeIDs()))
		return errs
	}
	main := g.reachable(g.Nodes[0].ID)
	clean := g.cleanup()
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if !clean[n.ID] {
			continue
		}
		switch {
		case main[n.ID]:
			bad("node %q is on both the workflow's path and its cleanup, which nothing on the workflow's path may reach", n.ID)
		case n.Kind == Agent:
			bad("node %q: a cleanup path has no model steps, only script, tool and person steps", n.ID)
		case n.Flow != "":
			bad("node %q: a cleanup path calls no workflow, only scripts and tools", n.ID)
		case n.Judge != nil:
			bad("node %q: a question on a cleanup path is a person's, and has no judge", n.ID)
		case g.onLoop(n.ID):
			bad("node %q: a cleanup path has no loop", n.ID)
		}
	}
	return errs
}

// cleans says node id is on the cleanup path.
func (g *Graph) cleans(id string) bool { return g.OnAbort != "" && g.cleanup()[id] }

// mainNodes are the steps of the workflow's own path and any other not
// on its cleanup path.
func (g *Graph) mainNodes() []*Node {
	clean := g.cleanup()
	var out []*Node
	for i := range g.Nodes {
		if !clean[g.Nodes[i].ID] {
			out = append(out, &g.Nodes[i])
		}
	}
	return slices.Clip(out)
}
