package examples

import (
	"slices"
	"testing"
)

// migrate-api is the example that uses most of the format: a call into
// a file of its own skill, kept fresh children, documents of each kind,
// a pick and words, a judge, timeouts, loops a person paces and a
// cleanup path.
func TestMigrateLoads(t *testing.T) {
	w := loadFrom(t, "skills/migrate-api")
	if len(w.Flows) != 1 || w.Flows[0].Name != "migrate-api/flows/package" {
		t.Fatalf("flows = %v", w.Flows)
	}
	var ids []string
	for _, n := range w.Graph.Nodes {
		ids = append(ids, n.ID)
	}
	for _, want := range []string{"scan", "pick", "plan", "lint", "next", "migrate", "triage", "review", "report", "undo", "told"} {
		if !slices.Contains(ids, want) {
			t.Errorf("no step %q in %v", want, ids)
		}
	}
}
