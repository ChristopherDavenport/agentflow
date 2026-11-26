package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	slash "path"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// Policy limits what a model step's tools may touch. Paths are slash
// paths below the workspace's root, matched by patterns in which * is
// any part of one name and ** any number of names. Commands are matched
// by their leading words. A refusal is decided before an allowance, and
// names the rule that made it.
type Policy struct {
	// Write, if set, are the only paths the step may write.
	Write []string `json:"write,omitempty" yaml:"write,omitempty"`
	// NoWrite are paths it may not write.
	NoWrite []string `json:"no_write,omitempty" yaml:"no_write,omitempty"`
	// NoRead are paths it may not read, nor write, since a write can show
	// what it replaced.
	NoRead []string `json:"no_read,omitempty" yaml:"no_read,omitempty"`
	// Bash, if set, are the only commands it may run, each by its leading
	// words: "go test" allows "go test ./...".
	Bash []string `json:"bash,omitempty" yaml:"bash,omitempty"`
	// NoBash are commands it may not run, by their leading words.
	NoBash []string `json:"no_bash,omitempty" yaml:"no_bash,omitempty"`
}

// Access is what a tool does with one of its arguments.
type Access int

const (
	// Reads says the argument is a path the tool reads.
	Reads Access = iota + 1
	// Writes says it is a path the tool writes.
	Writes
	// Executes says it is a command the tool runs.
	Executes
)

func (a Access) String() string {
	switch a {
	case Reads:
		return "read"
	case Writes:
		return "write"
	case Executes:
		return "run"
	}
	return fmt.Sprintf("access %d", int(a))
}

// Subject names one argument of a tool that a step's policy decides on:
// a string, or an array of strings, at the top of the call's arguments.
type Subject struct {
	Arg    string
	Access Access
}

// ToolSubjects tells a step's policy what the host's tools touch, by
// tool name. A tool named with no subjects touches no path and runs no
// command. Under a policy, a tool that is not named here and makes no
// claim of its own ([agenttool.Factual]) cannot be read, and is refused:
// [Apply] refuses a step that names one in its tools.
//
// A tool that claims facts is decided on the calls it says the call
// amounts to, each by the subjects of the tool it names, so a shell that
// says which files a command reads is held to the path rules too. Facts
// a host does not trust ([agenttool.Facts.Hint]) are decided beside the
// call itself.
func ToolSubjects(subjects map[string][]Subject) Option {
	return func(f *flow) {
		f.subjects = map[string][]Subject{}
		for name, s := range subjects {
			f.subjects[name] = slices.Clone(s)
		}
	}
}

func (p *Policy) empty() bool {
	return p == nil || len(p.Write)+len(p.NoWrite)+len(p.NoRead)+len(p.Bash)+len(p.NoBash) == 0
}

// check holds a policy's rules to what they can say.
func (p *Policy) check() error {
	if p == nil {
		return nil
	}
	var errs []error
	for _, rules := range []struct {
		key  string
		list []string
	}{{"write", p.Write}, {"no_write", p.NoWrite}, {"no_read", p.NoRead}} {
		for _, r := range rules.list {
			if err := checkPattern(r); err != nil {
				errs = append(errs, fmt.Errorf("%s %q: %w", rules.key, r, err))
			}
		}
	}
	for _, rules := range []struct {
		key  string
		list []string
	}{{"bash", p.Bash}, {"no_bash", p.NoBash}} {
		for _, r := range rules.list {
			if len(strings.Fields(r)) == 0 {
				errs = append(errs, fmt.Errorf("%s: an empty command", rules.key))
			}
		}
	}
	if p.Write != nil && len(p.Write) == 0 || p.Bash != nil && len(p.Bash) == 0 {
		errs = append(errs, errors.New("an empty allowance allows nothing: leave the key out, or name what is allowed"))
	}
	return errors.Join(errs...)
}

func checkPattern(p string) error {
	switch {
	case p == "" || slash.IsAbs(p) || strings.Contains(p, `\`):
		return errors.New("want a slash path below the workspace's root")
	case slash.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../"):
		return errors.New("want a clean path, with no .. in it")
	}
	for _, part := range strings.Split(p, "/") {
		if part == "**" {
			continue
		}
		if _, err := slash.Match(part, ""); err != nil {
			return err
		}
	}
	return nil
}

// match says name matches the pattern: * is any part of one name, and
// ** any number of names, none included.
func match(pattern, name string) bool {
	return matchParts(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchParts(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchParts(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, _ := slash.Match(pattern[0], name[0]); !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// matchAny is the first pattern of list that name matches.
func matchAny(list []string, name string) (string, bool) {
	for _, p := range list {
		if match(p, name) {
			return p, true
		}
	}
	return "", false
}

// commandParts splits a shell command into the commands it runs, by
// the operators that join them.
func commandParts(cmd string) []string {
	var parts []string
	for _, line := range strings.Split(cmd, "\n") {
		for _, a := range strings.Split(line, "&&") {
			for _, b := range strings.Split(a, "||") {
				for _, c := range strings.Split(b, ";") {
					for _, d := range strings.Split(c, "|") {
						for _, e := range strings.Split(d, "&") {
							if strings.TrimSpace(e) != "" {
								parts = append(parts, strings.TrimSpace(e))
							}
						}
					}
				}
			}
		}
	}
	return parts
}

// leads says the command starts with the rule's words.
func leads(rule, command string) bool {
	r, c := strings.Fields(rule), strings.Fields(command)
	return len(c) >= len(r) && slices.Equal(c[:len(r)], r)
}

// ruled is a policy with where it was set, for a refusal to name.
type ruled struct {
	from   string
	policy *Policy
}

// decide holds one subject of a call to every policy in force, and
// returns why it is refused, "" when it is not.
func decide(policies []ruled, access Access, value string) string {
	for _, r := range policies {
		p := r.policy
		switch access {
		case Reads, Writes:
			if rule, ok := matchAny(p.NoRead, value); ok {
				return fmt.Sprintf("%s %q: no_read %q of %s", access, value, rule, r.from)
			}
			if access == Reads {
				continue
			}
			if rule, ok := matchAny(p.NoWrite, value); ok {
				return fmt.Sprintf("write %q: no_write %q of %s", value, rule, r.from)
			}
			if p.Write != nil {
				if _, ok := matchAny(p.Write, value); !ok {
					return fmt.Sprintf("write %q: not in write %s of %s", value, strings.Join(quoted(p.Write), ", "), r.from)
				}
			}
		case Executes:
			if p.Bash != nil && strings.ContainsAny(value, "`") || p.Bash != nil && (strings.Contains(value, "$(") || strings.Contains(value, "<(") || strings.Contains(value, ">(")) {
				return fmt.Sprintf("run %q: a substitution cannot be held to bash of %s", value, r.from)
			}
			for _, part := range commandParts(value) {
				for _, rule := range p.NoBash {
					if leads(rule, part) {
						return fmt.Sprintf("run %q: no_bash %q of %s", part, rule, r.from)
					}
				}
				if p.Bash != nil && !slices.ContainsFunc(p.Bash, func(rule string) bool { return leads(rule, part) }) {
					return fmt.Sprintf("run %q: not in bash %s of %s", part, strings.Join(quoted(p.Bash), ", "), r.from)
				}
			}
		}
	}
	return ""
}

func quoted(list []string) []string {
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
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

// inChild is a path the child's tools name, below its directory, as the
// policy reads it: below the workspace's root.
func (f *flow) inChild(value string) string {
	if c := f.child; c != nil && c.Settings.Dir != "" && !slash.IsAbs(value) {
		return slash.Join(c.Settings.Dir, value)
	}
	return value
}

// police holds a call of st's step to the policies in force, and returns
// the refusal, "" when there is none.
func (f *flow) police(ctx context.Context, st State, info agentturn.ToolCallInfo) string {
	policies := f.policies(st)
	if len(policies) == 0 {
		return ""
	}
	type part struct {
		tool string
		args json.RawMessage
	}
	calls := []part{{info.Call.Name, info.Args}}
	if info.Tool != nil {
		facts, claims, err := agenttool.FactsOf(ctx, info.Tool, info.Args)
		switch {
		case err != nil:
			return fmt.Sprintf("%s cannot say what this call touches: %v", info.Call.Name, err)
		case claims && facts.Calls != nil:
			var claimed []part
			for _, c := range facts.Calls {
				claimed = append(claimed, part{c.Tool, c.Args})
			}
			if facts.Hint {
				claimed = append(claimed, calls...)
			}
			if len(claimed) == 0 {
				return fmt.Sprintf("%s says this call amounts to nothing it can state", info.Call.Name)
			}
			calls = claimed
		case claims:
			// The call is its own one fact.
		}
	}
	for _, c := range calls {
		subjects, known := f.subjects[c.tool]
		if !known {
			return fmt.Sprintf("the step's policy cannot see what %s touches: the host names nothing it reads, writes or runs", c.tool)
		}
		var args map[string]any
		if len(c.args) > 0 && decode(c.args, &args) != nil {
			return fmt.Sprintf("the arguments of %s are not an object the policy can read", c.tool)
		}
		for _, s := range subjects {
			for _, value := range values(args[s.Arg]) {
				v := value
				if s.Access != Executes {
					v = f.inChild(slash.Clean(value))
					if slash.IsAbs(v) || v == ".." || strings.HasPrefix(v, "../") {
						return fmt.Sprintf("%s %q: the policy holds paths below the workspace's root", s.Access, value)
					}
				}
				if why := decide(policies, s.Access, v); why != "" {
					return why
				}
			}
		}
	}
	return ""
}

// values are the strings an argument holds: itself, or an array's.
func values(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// describe says what the policies in force allow, for the step's
// instructions, "" when none is.
func describe(policies []ruled) string {
	if len(policies) == 0 {
		return ""
	}
	var lines []string
	for _, r := range policies {
		p := r.policy
		var said []string
		if p.Write != nil {
			said = append(said, "write only paths matching "+strings.Join(quoted(p.Write), ", "))
		}
		if p.NoWrite != nil {
			said = append(said, "not write "+strings.Join(quoted(p.NoWrite), ", "))
		}
		if p.NoRead != nil {
			said = append(said, "not read or write "+strings.Join(quoted(p.NoRead), ", "))
		}
		if p.Bash != nil {
			said = append(said, "run only commands starting "+strings.Join(quoted(p.Bash), ", "))
		}
		if p.NoBash != nil {
			said = append(said, "not run commands starting "+strings.Join(quoted(p.NoBash), ", "))
		}
		lines = append(lines, "- set by "+r.from+": you may "+strings.Join(said, "; "))
	}
	return "Your tools are limited. Paths are below the workspace's root, * is any part of a name and ** any number of names. A call outside these limits is refused, naming the rule:\n" + strings.Join(lines, "\n")
}
