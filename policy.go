package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	slash "path"
	"slices"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentturn"
)

// Policy narrows what a model step's tools may do, in agentpolicy's
// rule grammar: "read", "edit(src/**)", "bash(go test:*)". It can only
// refuse. A call it lets through is still the host's own BeforeToolCall
// to decide, so a workflow cannot give itself what the host does not.
//
// Deny refuses the calls its rules match. Allow, if set, refuses every
// call none of its rules matches. Deny is read first, as agentpolicy
// reads it. A path in a rule is below the workspace's root, in a child
// too.
type Policy struct {
	Allow []PolicyRule `json:"allow,omitempty"`
	Deny  []PolicyRule `json:"deny,omitempty"`
}

// PolicyRule is one rule, and the note that says why it is there, which
// a refusal and the step's instructions give after it.
type PolicyRule struct {
	Rule string `json:"rule"`
	Note string `json:"note,omitempty"`
}

func (p *Policy) empty() bool { return p == nil || len(p.Allow)+len(p.Deny) == 0 }

// rules parses the rules of one list.
func rules(list []PolicyRule) ([]agentpolicy.Rule, error) {
	var out []agentpolicy.Rule
	for _, r := range list {
		parsed, err := agentpolicy.ParseRules(r.Rule)
		switch {
		case err != nil:
			return nil, fmt.Errorf("rule %q: %w", r.Rule, err)
		case len(parsed) != 1:
			return nil, fmt.Errorf("rule %q: want one rule to an entry", r.Rule)
		}
		parsed[0].Note = r.Note
		out = append(out, parsed[0])
	}
	return out, nil
}

// parsed is the policy as agentpolicy reads it. A policy with an
// allowance denies by default what it does not allow, and one without
// allows by default what it does not deny, which the host then decides.
func (p *Policy) parsed() (agentpolicy.Policy, error) {
	allow, err := rules(p.Allow)
	if err != nil {
		return agentpolicy.Policy{}, fmt.Errorf("allow: %w", err)
	}
	deny, err := rules(p.Deny)
	if err != nil {
		return agentpolicy.Policy{}, fmt.Errorf("deny: %w", err)
	}
	out := agentpolicy.Policy{Allow: allow, Deny: deny, Default: agentpolicy.Allow()}
	if p.Allow != nil {
		out.Default = agentpolicy.Deny()
	}
	return out, nil
}

// check holds a policy's rules to the grammar, and to the tools of the
// step it is on where those are known: a rule for a tool the step does
// not offer could never be read.
func (p *Policy) check(tools []string, known bool) error {
	if p == nil {
		return nil
	}
	var errs []error
	if p.Allow != nil && len(p.Allow) == 0 {
		errs = append(errs, errors.New("an empty allow allows nothing: leave the key out, or name what is allowed"))
	}
	parsed, err := p.parsed()
	if err != nil {
		return err
	}
	if known {
		for _, r := range slices.Concat(parsed.Allow, parsed.Deny) {
			if !r.Glob() && !slices.Contains(tools, r.Tool) {
				errs = append(errs, fmt.Errorf("rule %s: the step offers no tool %s", r, r.Tool))
			}
		}
	}
	return errors.Join(errs...)
}

// PolicyMatchers gives step policies what agentpolicy needs of the host's
// tools: for each tool whose rules take a specifier, how a specifier is
// matched and how a call is split, as [agentpolicy.Build] takes them. A
// rule with a specifier for a tool with no matcher is a load error.
//
// matchers is asked once for each working directory a workflow's model
// steps run in: "" for the workspace's root, and a child's dir below it.
// Rules are written below the root, so a child's matchers read its tools'
// paths below its dir, as [InDir] does. opts go to every engine:
// [agentpolicy.WithAliases], say.
func PolicyMatchers(matchers func(dir string) map[string]agentpolicy.ToolMatcher, opts ...agentpolicy.Option) Option {
	return func(f *flow) { f.matchers, f.policyOpts = matchers, opts }
}

// InDir matches a rule's specifier against one string field of a call's
// arguments, read as a clean path below dir, as [agentpolicy.GlobMatcher]
// does: "edit(src/*)" matches a child at "src" that edits "a.go", and
// "docs/../src/a.go" at the root. A path that leaves the root keeps its
// "..", so only a rule written for it matches it.
func InDir(dir, field string) agentpolicy.Matcher {
	glob := agentpolicy.GlobMatcher(field)
	return func(spec string, args json.RawMessage) bool {
		var m map[string]any
		if json.Unmarshal(args, &m) != nil {
			return false
		}
		value, ok := m[field].(string)
		if !ok {
			return false
		}
		if !slash.IsAbs(value) {
			value = slash.Join(dir, value)
		}
		m[field] = slash.Clean(value)
		b, err := json.Marshal(m)
		return err == nil && glob(spec, b)
	}
}

// engines are the agentpolicy engines of the policies a flow holds its
// steps to, one for each, built for the flow's working directory.
type engines struct {
	mu    sync.Mutex
	built map[*Policy]*agentpolicy.Engine
}

// dir is the working directory the flow's model steps run in.
func (f *flow) dir() string {
	if f.child != nil {
		return f.child.Settings.Dir
	}
	return ""
}

// engine is p's engine, built the first time it is asked for.
func (f *flow) engine(p *Policy) (*agentpolicy.Engine, error) {
	if f.engines == nil {
		f.engines = &engines{}
	}
	f.engines.mu.Lock()
	defer f.engines.mu.Unlock()
	if e := f.engines.built[p]; e != nil {
		return e, nil
	}
	parsed, err := p.parsed()
	if err != nil {
		return nil, err
	}
	var matchers map[string]agentpolicy.ToolMatcher
	if f.matchers != nil {
		matchers = f.matchers(f.dir())
	}
	e, err := agentpolicy.Build(parsed, matchers, f.policyOpts...)
	if err != nil {
		return nil, err
	}
	if f.engines.built == nil {
		f.engines.built = map[*Policy]*agentpolicy.Engine{}
	}
	f.engines.built[p] = e
	return e, nil
}

// buildPolicies builds the engine of every policy the flow's steps may be
// held to, so a rule agentpolicy cannot read is a load error: a
// specifier for a tool the host gave no matcher, say.
func (f *flow) buildPolicies() error {
	// A copy of the flow, Drive's say, shares what was built.
	f.engines = &engines{}
	var errs []error
	build := func(where string, p *Policy) {
		if p.empty() {
			return
		}
		if _, err := f.engine(p); err != nil {
			errs = append(errs, fmt.Errorf("agentflow: %s: policy: %w", where, err))
		}
	}
	if c := f.child; c != nil {
		for _, r := range c.policies {
			build(r.from, r.policy)
		}
	}
	for _, x := range f.g.closure() {
		for i := range x.Nodes {
			n := &x.Nodes[i]
			build(fmt.Sprintf("%s: node %q", x.Name, n.ID), n.Policy)
			if n.Judge != nil {
				build(fmt.Sprintf("%s: the judge of node %q", x.Name, n.ID), n.Judge.Policy)
			}
		}
	}
	return errors.Join(errs...)
}

// ruled is a policy with where it was set, for a refusal to name.
type ruled struct {
	from   string
	policy *Policy
}

// policies are the policies in force at st's step: those of the calls
// its run is nested in, outermost first, a child's from its caller
// included, and the step's own.
func (f *flow) policies(st State) []ruled {
	out := f.chain(st)
	if n := st.Node; n != nil && !n.Policy.empty() {
		from := fmt.Sprintf("step %q of %s", n.ID, st.Workflow)
		if n.Verdict {
			workflow, _, _ := strings.Cut(st.Workflow, ":")
			from = fmt.Sprintf("the judge of step %q of %s", n.ID, workflow)
		}
		out = append(out, ruled{from, n.Policy})
	}
	return out
}

// chain are the policies of the calls st's run is nested in, a child's
// from its caller included, outermost first.
func (f *flow) chain(st State) []ruled {
	var out []ruled
	if c := f.child; c != nil {
		out = append(out, c.policies...)
	}
	for _, caller := range st.callers {
		if !caller.node.Policy.empty() {
			out = append(out, ruled{fmt.Sprintf("step %q of %s", caller.node.ID, caller.workflow), caller.node.Policy})
		}
	}
	return out
}

// police holds a call of st's step to every policy in force, and returns
// the refusal, "" when there is none. Each policy is asked what it would
// decide, which remembers nothing and tells no observer: a policy here
// only refuses, so there is nothing to wait on and nothing to answer.
func (f *flow) police(ctx context.Context, st State, info agentturn.ToolCallInfo) string {
	for _, r := range f.policies(st) {
		e, err := f.engine(r.policy)
		if err != nil {
			return fmt.Sprintf("the policy of %s cannot be read: %v", r.from, err)
		}
		v, err := e.Would(ctx, info)
		switch {
		case err != nil:
			return fmt.Sprintf("the policy of %s cannot decide this call: %v", r.from, err)
		case v.Action != agentturn.Allow:
			return fmt.Sprintf("the policy of %s: %s", r.from, v.Reason)
		}
	}
	return ""
}

// describe says what the policies in force allow, for the step's
// instructions, "" when none is.
func describe(policies []ruled) string {
	if len(policies) == 0 {
		return ""
	}
	cite := func(list []PolicyRule) string {
		var out []string
		for _, r := range list {
			parsed, err := rules([]PolicyRule{r})
			if err != nil {
				continue
			}
			s := parsed[0].String()
			if r.Note != "" {
				s += " (" + r.Note + ")"
			}
			out = append(out, s)
		}
		return strings.Join(out, ", ")
	}
	var lines []string
	for _, r := range policies {
		p := r.policy
		var said []string
		if p.Allow != nil {
			said = append(said, "only calls matching "+cite(p.Allow))
		}
		if p.Deny != nil {
			said = append(said, "no calls matching "+cite(p.Deny))
		}
		lines = append(lines, "- set by "+r.from+": "+strings.Join(said, "; "))
	}
	return "Your tools are limited. Rules name a tool and, in parentheses, what its call may touch; paths are below the workspace's root. A call outside these limits is refused, naming the rule:\n" + strings.Join(lines, "\n")
}
