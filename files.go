package agentflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	slashpath "path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// MaxFile bounds one file a step is given, and MaxFiles all of them
// together. A file over either fails the step; nothing is cut.
const (
	MaxFile  = 256 << 10
	MaxFiles = 1 << 20
)

const filesTool = "flow_files"

// skillPrefix starts a path that names a file of the step's skill.
const skillPrefix = "./"

// fileRef is one file a step is given: a path in the workspace, below the
// step's working directory, or a file of the skill.
type fileRef struct {
	Path  string `json:"path"`
	Skill bool   `json:"skill,omitempty"`
}

// filesArgs is flow_files's arguments: the step, and its files in order.
type filesArgs struct {
	Run   string    `json:"run"`
	Step  string    `json:"step"`
	Files []fileRef `json:"files"`
}

// fileText is one file as flow_files's result holds it.
type fileText struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// FileReader names the host's tool that reads a workspace file for a
// model step's files, and the argument that names the path: read and path
// unless given. The program calls it through agentturn.Invoke, so the
// host's hooks and the step's policy decide each read.
func FileReader(tool, field string) Option {
	return func(f *flow) { f.fileTool, f.fileField = tool, field }
}

// cleanPath says p is a relative, clean path that stays inside the
// directory it is read from.
func cleanPath(p string) bool {
	return p != "" && !slashpath.IsAbs(p) && slashpath.Clean(p) == p && p != ".." && !strings.HasPrefix(p, "../") && !strings.Contains(p, "\\")
}

// checkFiles holds a step's files to what can be: a model step's, not a
// judge's, each a path or a reference; a workspace path relative and
// clean, a skill's path clean within the skill.
func (n *Node) checkFiles() []error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("agentflow: node %q: "+format, append([]any{n.ID}, args...)...))
	}
	if len(n.Files) > 0 && (n.Kind != Agent || n.Verdict) {
		bad("only a model step is given files")
		return errs
	}
	for _, v := range n.Files {
		if v.From != "" {
			continue
		}
		p, ok := v.Literal.(string)
		switch {
		case !ok:
			bad("a file is a path or a reference to one, not %v", v.Literal)
		case strings.HasPrefix(p, skillPrefix):
			if rel := strings.TrimPrefix(p, skillPrefix); !cleanPath(rel) {
				bad("file %q: want a clean path within the skill", p)
			}
		case !cleanPath(p):
			bad("file %q: want a relative, clean path inside the workspace", p)
		}
	}
	for rel, text := range n.SkillFiles {
		switch {
		case len(text) > MaxFile:
			bad("file ./%s of the skill is over %d bytes", rel, MaxFile)
		case !utf8.ValidString(text):
			bad("file ./%s of the skill is not text", rel)
		}
	}
	return errs
}

// fileList is what st's step is given, in order: its files resolved from
// the record, or for the step of a child, what its call passed in.
func (st State) fileList() []fileRef {
	if st.passedFiles != nil {
		return st.passedFiles
	}
	n := st.Node
	var out []fileRef
	add := func(p string) {
		if rel, ok := strings.CutPrefix(p, skillPrefix); ok {
			out = append(out, fileRef{Path: rel, Skill: true})
			return
		}
		out = append(out, fileRef{Path: p})
	}
	for _, v := range n.Files {
		got, ok, err := st.resolve(v)
		if err != nil || !ok {
			// Checked at load: only an optional reference has no value.
			continue
		}
		switch x := got.(type) {
		case string:
			add(x)
		case []any:
			for _, item := range x {
				s, _ := item.(string)
				// A value that is not a path is the tool's to refuse.
				out = append(out, fileRef{Path: s})
			}
		default:
			out = append(out, fileRef{Path: fmt.Sprint(got)})
		}
	}
	return out
}

// hasFiles says st's step is given files.
func (st State) hasFiles() bool {
	return st.Node != nil && (len(st.Node.Files) > 0 || len(st.passedFiles) > 0)
}

func (st State) filesID() string { return st.callID() + "-files" }

// filesDue says the program gives st's model step its files before the
// model works.
func (f *flow) filesDue(st State) bool {
	return modelTurn(st) && st.Node != nil && !st.Done && st.hasFiles() && !st.filed
}

// filesTool reads a step's files: a skill's from the definition, which
// the digest pins, and a workspace's through the host's read tool, as
// nested calls its hooks and the step's policy decide. Its result holds
// every file's text, so what the model was given is in the record and no
// replay reads a disk.
func (f *flow) filesTool() agenttool.Tool {
	return agenttool.New(filesTool, "Give a workflow step its files.", func(ctx context.Context, a filesArgs) (Result, error) {
		if err := exact(ctx, &a); err != nil {
			return Result{}, err
		}
		st, err := f.current(ctx)
		if err != nil {
			return Result{}, err
		}
		n := st.Node
		if n == nil || n.ID != a.Step || a.Run != st.Instance || !sameJSON(a.Files, st.fileList()) {
			return Result{}, errors.New("agentflow: these are not the files of the step under way")
		}
		var out []fileText
		total := 0
		for _, ref := range a.Files {
			var text string
			switch {
			case !cleanPath(ref.Path):
				return Result{}, fmt.Errorf("agentflow: file %q: want a relative, clean path inside the workspace", ref.Path)
			case ref.Skill:
				t, ok := n.SkillFiles[ref.Path]
				if !ok {
					return Result{}, fmt.Errorf("agentflow: file ./%s of the skill was never read", ref.Path)
				}
				text = t
			default:
				if text, err = f.readFile(ctx, ref.Path); err != nil {
					return Result{}, fmt.Errorf("agentflow: file %s: %w", ref.Path, err)
				}
			}
			switch total += len(text); {
			case len(text) > MaxFile:
				return Result{}, fmt.Errorf("agentflow: file %s is over %d bytes; give the step a smaller one, or its path to read", ref.Path, MaxFile)
			case total > MaxFiles:
				return Result{}, fmt.Errorf("agentflow: the step's files are over %d bytes together", MaxFiles)
			}
			name := ref.Path
			if ref.Skill {
				name = skillPrefix + ref.Path
			}
			out = append(out, fileText{Path: name, Text: text})
		}
		return Result{Outcome: OK, Data: map[string]any{"files": out}}, nil
	}, agenttool.WithReplay(safeReplay))
}

// readFile reads a workspace file through the host's read tool.
func (f *flow) readFile(ctx context.Context, p string) (string, error) {
	args, err := json.Marshal(map[string]string{f.fileArg(): p})
	if err != nil {
		return "", err
	}
	res, err := agentturn.Invoke(ctx, f.readTool(), args)
	if err != nil {
		return "", err
	}
	text := res.Output.String()
	if strings.HasPrefix(text, "Error: ") {
		return "", errors.New(strings.TrimPrefix(text, "Error: "))
	}
	return text, nil
}

func (f *flow) readTool() string {
	if f.fileTool == "" {
		return "read"
	}
	return f.fileTool
}

func (f *flow) fileArg() string {
	if f.fileField == "" {
		return "path"
	}
	return f.fileField
}

// readsFiles says info is a read the program makes for a step's files.
func readsFiles(info agentturn.ToolCallInfo) bool {
	return strings.HasPrefix(info.Parent, reserved) && strings.HasSuffix(info.Parent, "-files")
}

// keepFiles puts the step's files back into what its model is sent, after
// its opening message, where a compaction folded them away: the model was
// given them, and the record holds them.
func keepFiles(input openresponses.Items, record agentturn.Transcript, st State) openresponses.Items {
	if !st.filed {
		return input
	}
	id := st.filesID()
	for _, item := range input {
		if c, ok := item.(*openresponses.FunctionCall); ok && c.CallID == id {
			return input
		}
	}
	var kept openresponses.Items
	for _, item := range record {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			if it.CallID == id {
				kept = append(kept, it)
			}
		case *openresponses.FunctionCallOutput:
			if it.CallID == id {
				kept = append(kept, it)
			}
		}
	}
	if len(kept) != 2 {
		return input
	}
	at := 0
	for i, item := range input {
		if m, ok := item.(*openresponses.Message); ok && opens(m, &st) {
			at = i + 1
		}
	}
	return slices.Concat(input[:at], kept, input[at:])
}

// filesFor is what a fresh step's call passes into its child: the files
// the step is given, resolved here.
func (st State) filesFor(n *Node) []fileRef {
	if len(n.Files) == 0 {
		return nil
	}
	at := st
	at.Node = n
	return at.fileList()
}

// checkFileReader holds a config that names its tools to offer the read
// tool a step's workspace files need, where a step in this context is
// given any. A child's tools are its opener's.
func (f *flow) checkFileReader(local map[string]*Graph, cfg agentturn.Config) error {
	if cfg.ToolProvider != nil {
		return nil
	}
	for _, x := range local {
		for i := range x.Nodes {
			n := &x.Nodes[i]
			if n.Kind != Agent || n.fresh() {
				continue
			}
			workspace := slices.ContainsFunc(n.Files, func(v Value) bool {
				s, ok := v.Literal.(string)
				return v.From != "" || ok && !strings.HasPrefix(s, skillPrefix)
			})
			if workspace && !slices.ContainsFunc(cfg.Tools, func(t agenttool.Tool) bool { return t.Name() == f.readTool() }) {
				return fmt.Errorf("agentflow: %s: node %q is given workspace files, read with the tool %s, which the config does not offer; name another with FileReader", x.Name, n.ID, f.readTool())
			}
		}
	}
	return nil
}
