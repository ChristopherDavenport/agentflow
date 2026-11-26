package agentflow

import (
	"fmt"
	"slices"

	"github.com/ChristopherDavenport/agenttool"
)

// Suggest names the one of names closest to s, for a load error about a
// misspelt name: "; did you mean \"x\"?", or "" when none is close
// enough to be what was meant. Every load error that names something
// unknown, here and in package workflow, says it the same way.
func Suggest(s string, names []string) string {
	best, score := "", 3
	for _, name := range slices.Sorted(slices.Values(names)) {
		if name == s {
			continue
		}
		if d := distance(s, name); d < score && d <= max(1, len(name)/3) {
			best, score = name, d
		}
	}
	if best == "" {
		return ""
	}
	return fmt.Sprintf("; did you mean %q?", best)
}

func suggest(s string, names []string) string { return Suggest(s, names) }

// distance is the edit distance between a and b.
func distance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// nodeIDs are the IDs of g's steps, for a suggestion.
func (g *Graph) nodeIDs() []string {
	out := make([]string, 0, len(g.Nodes))
	for _, n := range g.Nodes {
		out = append(out, n.ID)
	}
	return out
}

// memberNames are the names of an object schema's members.
func memberNames(s *agenttool.Schema) []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Properties))
	for _, p := range s.Properties {
		out = append(out, p.Name)
	}
	return out
}
