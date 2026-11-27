package agentflow

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// checkPaced holds max_visits: person to loops a person drives. A node
// that says it is on a loop, and every cycle through it passes a person
// step. A node with no bound of its own on a cycle through such a node
// would stop that loop at the default, so it must say what it wants.
func (g *Graph) checkPaced() []error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf("agentflow: "+format, args...)) }
	var paced []string
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if !n.PersonPaced {
			continue
		}
		switch {
		case n.MaxVisits != 0:
			bad("node %q: max_visits is a number or person, not both", n.ID)
		case !g.onLoop(n.ID):
			bad("node %q: max_visits: person is for a step on a loop, and no path comes back to %q", n.ID, n.ID)
		case n.Kind != Person:
			if cycle := g.unpaced(n.ID); cycle != nil {
				bad("node %q: max_visits: person needs a person step on every loop through it, and %s passes none; put a person step on that loop, or give %q a number", n.ID, strings.Join(cycle, " → "), n.ID)
				continue
			}
			paced = append(paced, n.ID)
		default:
			paced = append(paced, n.ID)
		}
	}
	for i := range g.Nodes {
		m := &g.Nodes[i]
		if m.PersonPaced || m.MaxVisits != 0 {
			continue
		}
		for _, id := range paced {
			if id != m.ID && g.reaches(id, m.ID) && g.reaches(m.ID, id) {
				bad("node %q: it is on a loop through %q, which is max_visits: person, and its own bound is the default of %d, which would stop that loop; set max_visits: person on it too, or a number", m.ID, id, DefaultLoopVisits)
				break
			}
		}
	}
	return errs
}

// successors are the nodes n's edges lead to, in the order of their
// outcomes, so what is reported does not depend on a map's order.
func (g *Graph) successors(n *Node) []string {
	var out []string
	for _, outcome := range slices.Sorted(maps.Keys(n.Next)) {
		if next := n.Next[outcome]; g.node(next) != nil && !slices.Contains(out, next) {
			out = append(out, next)
		}
	}
	return out
}

// reaches says some path of one or more edges leads from a to b.
func (g *Graph) reaches(a, b string) bool {
	n := g.node(a)
	return n != nil && g.reachable(g.successors(n)...)[b]
}

// unpaced is a cycle through id that passes no person step, from id back
// to it, or nil when every cycle through id passes one.
func (g *Graph) unpaced(id string) []string {
	from := map[string]string{}
	queue := []string{id}
	for len(queue) > 0 {
		at := queue[0]
		queue = queue[1:]
		for _, next := range g.successors(g.node(at)) {
			if next == id {
				cycle := []string{id}
				for k := at; k != id; k = from[k] {
					cycle = append(cycle, k)
				}
				cycle = append(cycle, id)
				slices.Reverse(cycle[1 : len(cycle)-1])
				return cycle
			}
			if _, seen := from[next]; seen || g.node(next).Kind == Person {
				continue
			}
			from[next] = at
			queue = append(queue, next)
		}
	}
	return nil
}

// pacedEntry says the visit about to be made of n is one a person paced:
// n is max_visits: person, has been visited before, and since then a
// person answered a person step of the run.
func (f *frame) pacedEntry(n *Node) bool {
	if !n.PersonPaced || f.visits[n.ID] == 0 {
		return false
	}
	for _, s := range f.st.Steps[min(f.mark[n.ID], len(f.st.Steps)):] {
		if m := f.st.g.node(s.Node); m != nil && m.Kind == Person && s.Result.By == "" {
			return true
		}
	}
	return false
}
