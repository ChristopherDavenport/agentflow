package agentflow

import (
	"encoding/json"
	"fmt"
)

// set says v holds something: a literal or a reference.
func (v Value) set() bool { return v.From != "" || v.Literal != nil }

// checkDir holds a step's dir to what can be checked before it runs: a
// child's or a script's, a clean relative path or one reference to a
// string, which load can say no more of.
func (g *Graph) checkDir(n *Node) []error {
	d := n.Dir
	if !d.set() {
		return nil
	}
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agentflow: node %q: "+format, append([]any{n.ID}, args...)...))
	}
	if !n.inChild() && n.Script == "" {
		bad("dir is a setting of a child or of a script; it needs a call, context: %s, or run", ContextFresh)
	}
	switch {
	case d.From == "":
		if p, ok := d.Literal.(string); !ok {
			bad("dir %v: want a path", d.Literal)
		} else if !cleanPath(p) || p == "." {
			bad("dir %q: want a clean path below the workspace's root", p)
		}
	case d.Optional:
		bad("dir is one directory: a reference with no value leaves none to work in")
	default:
		for _, from := range d.refs() {
			if s := g.refSchema(from); s != nil && s.Type != "" && s.Type != "string" {
				bad("dir %s is no path: its schema says %s", from, s.Type)
			}
		}
	}
	return errs
}

// dirOf is the directory st's step works in, resolved from the record:
// empty for the workspace's root. A value that is no clean path below the
// root, or a kept child's that is not the one it was started in, is an
// error, which fails the step's call.
func (st State) dirOf(n *Node) (string, error) {
	if !n.Dir.set() {
		return "", nil
	}
	got, ok, err := st.resolve(n.Dir)
	switch {
	case err != nil:
		return "", fmt.Errorf("dir: %w", err)
	case !ok:
		// Checked at load: only an optional reference has no value.
		return "", fmt.Errorf("dir %s has no value", n.Dir.From)
	}
	p, isPath := got.(string)
	if !isPath {
		b, _ := json.Marshal(got)
		return "", fmt.Errorf("dir %s is %s, not a path", n.Dir.From, b)
	}
	if !cleanPath(p) || p == "." {
		return "", fmt.Errorf("dir %q: want a clean path below the workspace's root", p)
	}
	if n.Child != "" {
		if first, kept := st.keptDir[n.Child]; kept && first != p {
			return "", fmt.Errorf("child %q was started in %q, and dir is now %q", n.Child, first, p)
		}
	}
	return p, nil
}

// dirArg is what a call records of its step's dir: the resolved path,
// or, when that is no path, the value as it was, which the gate refuses.
func (st State) dirArg(n *Node) string {
	if !n.Dir.set() {
		return ""
	}
	if p, err := st.dirOf(n); err == nil {
		return p
	}
	got, _, _ := st.resolve(n.Dir)
	if s, ok := got.(string); ok {
		return s
	}
	b, _ := json.Marshal(got)
	return string(b)
}
