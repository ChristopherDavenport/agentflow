package agentflow

import (
	"fmt"
	"slices"
	"strings"
)

// excluded are the tools n is never offered: its own exclusions and its
// workflow's.
func (g *Graph) excluded(n *Node) []string {
	return slices.Concat(g.ExcludeTools, n.ExcludeTools)
}

// offers says model step n of g is offered the host's tool name: one it
// names, or any with tools: all, and none excluded. The program's own tools
// are never a step's to call.
func (g *Graph) offers(n *Node, name string) bool {
	switch {
	case name == ScriptTool || strings.HasPrefix(name, reserved):
		return false
	case slices.Contains(g.excluded(n), name):
		return false
	}
	return n.AllTools || slices.Contains(n.Tools, name)
}

// checkOffer holds a step's tools: all and exclude_tools to model steps,
// and a step to naming no tool it also excludes, which it could never call.
func (g *Graph) checkOffer(n *Node) []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agentflow: node %q: "+format, append([]any{n.ID}, args...)...))
	}
	if (n.AllTools || n.ExcludeTools != nil) && n.Kind != Agent {
		bad("tools: all and exclude_tools are a model step's")
	}
	if n.AllTools && len(n.Tools) > 0 {
		bad("tools is all, or a list of tools, not both")
	}
	for _, tool := range n.ExcludeTools {
		if tool == ScriptTool || strings.HasPrefix(tool, reserved) {
			bad("exclude_tools: %s is the program's, which no model step is offered", tool)
		}
	}
	for _, tool := range n.Tools {
		if slices.Contains(g.excluded(n), tool) {
			bad("names the tool %s, which it excludes, or its workflow does", tool)
		}
	}
	if j := n.Judge; j != nil {
		for _, tool := range j.Tools {
			if slices.Contains(g.ExcludeTools, tool) {
				bad("judge: names the tool %s, which its workflow excludes", tool)
			}
		}
	}
	return errs
}
