package workflow

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentskill"
)

// MetadataKey is the frontmatter metadata entry that makes a skill a
// workflow; its value names the file.
const MetadataKey = "agentflow"

// maxSkill bounds what is read of a skill's tree.
const maxSkill = 8 << 20

// Marker says whether a skill is a workflow, and names its file. It
// reads the frontmatter alone, so a catalog can tell workflows from
// other skills without opening anything.
func Marker(s *agentskill.Skill) (file string, ok bool) {
	file, ok = s.Metadata[MetadataKey]
	return file, ok
}

// Workflow is a skill read as a workflow.
type Workflow struct {
	// Graph is named for the skill, and its digest covers every file,
	// and every workflow it can call. It is linked to them.
	Graph *agentflow.Graph
	// Files is the skill's tree as it was when loaded, by slash path.
	// Scripts run from these, so a later change on disk changes nothing.
	Files map[string][]byte
	// Flows are the other workflows Graph can reach: files of its own
	// skill and other workflow skills.
	Flows []*agentflow.Graph
	// Skills holds the files of each skill a reachable workflow is of,
	// by the skill's name, Graph's own among them, for script.Skills.
	Skills map[string]map[string][]byte
}

// Find returns the workflow skill a name calls, for a workflow that
// calls another skill.
type Find func(name string) (*agentskill.Skill, error)

// Load reads a skill marked as a workflow. It may call workflow files of
// its own skill; one that calls another skill needs [LoadWith].
func Load(s *agentskill.Skill) (*Workflow, error) { return LoadWith(s, nil) }

// LoadWith reads a skill marked as a workflow, with every workflow it
// can reach: a file of the same skill, named ./<path>.md, or another
// workflow skill, found by find. It links them and checks every call
// against what it calls.
func LoadWith(s *agentskill.Skill, find Find) (*Workflow, error) {
	root, files, err := loadSkill(s)
	if err != nil {
		return nil, err
	}
	w := &Workflow{Graph: root, Files: files, Skills: map[string]map[string][]byte{s.Name: files}}
	sources := map[string]string{s.Name: root.Sources}
	loaded := map[string]*agentflow.Graph{root.Name: root}
	queue := []*agentflow.Graph{root}
	for len(queue) > 0 {
		g := queue[0]
		queue = queue[1:]
		skill, _, _ := strings.Cut(g.Name, "/")
		for _, n := range g.Nodes {
			if n.Flow == "" {
				continue
			}
			var c *agentflow.Graph
			rel, inSkill := strings.CutPrefix(n.Flow, "./")
			name := n.Flow
			if inSkill {
				name = skill + "/" + strings.TrimSuffix(rel, ".md")
			}
			if loaded[name] != nil {
				continue
			}
			switch {
			case inSkill:
				src, ok := w.Skills[skill][rel]
				if !ok {
					return nil, fmt.Errorf("workflow: %s: node %q calls %s, which is not in skill %q", g.Name, n.ID, n.Flow, skill)
				}
				if c, err = Parse(name, src); err != nil {
					return nil, fmt.Errorf("workflow: skill %q: %s: %w", skill, rel, err)
				}
				if err := scriptsIn(c, skill, w.Skills[skill]); err != nil {
					return nil, err
				}
				c.Sources = sources[skill]
			case find == nil:
				return nil, fmt.Errorf("workflow: %s: node %q calls the skill %s, and nothing was given to find skills", g.Name, n.ID, name)
			default:
				other, err := find(name)
				if err != nil {
					return nil, fmt.Errorf("workflow: %s: node %q calls %s: %w", g.Name, n.ID, name, err)
				}
				if other == nil || other.Name != name {
					return nil, fmt.Errorf("workflow: %s: node %q calls %s: no such skill", g.Name, n.ID, name)
				}
				var files map[string][]byte
				if c, files, err = loadSkill(other); err != nil {
					return nil, err
				}
				w.Skills[name], sources[name] = files, c.Sources
			}
			loaded[name] = c
			w.Flows = append(w.Flows, c)
			queue = append(queue, c)
		}
	}
	if err := root.Link(w.Flows...); err != nil {
		return nil, fmt.Errorf("workflow: skill %q: %w", s.Name, err)
	}
	return w, nil
}

// loadSkill reads the workflow a skill marks, from a snapshot of its
// files.
func loadSkill(s *agentskill.Skill) (*agentflow.Graph, map[string][]byte, error) {
	file, ok := Marker(s)
	if !ok {
		return nil, nil, fmt.Errorf("workflow: skill %q is not a workflow: no metadata.%s", s.Name, MetadataKey)
	}
	if s.FS == nil {
		return nil, nil, fmt.Errorf("workflow: skill %q has no files", s.Name)
	}
	files, digest, err := snapshot(s.FS)
	if err != nil {
		return nil, nil, fmt.Errorf("workflow: skill %q: %w", s.Name, err)
	}
	src, ok := files[file]
	if !ok {
		return nil, nil, fmt.Errorf("workflow: skill %q: metadata.%s names %q, which is not in it", s.Name, MetadataKey, file)
	}
	g, err := Parse(s.Name, src)
	if err != nil {
		return nil, nil, fmt.Errorf("workflow: skill %q: %s: %w", s.Name, file, err)
	}
	if err := scriptsIn(g, s.Name, files); err != nil {
		return nil, nil, err
	}
	g.Sources = digest
	return g, files, nil
}

// scriptsIn holds a workflow's scripts and instruction files to its
// skill's files, and reads each step's instructions from the snapshot,
// so the definition, and its digest, hold them.
func scriptsIn(g *agentflow.Graph, skill string, files map[string][]byte) error {
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if n.Script != "" && files[n.Script] == nil {
			return fmt.Errorf("workflow: skill %q: node %q runs %q, which is not in it", skill, n.ID, n.Script)
		}
		if n.InstructionsFile != "" {
			text, ok := files[n.InstructionsFile]
			if !ok {
				return fmt.Errorf("workflow: skill %q: node %q takes instructions from %q, which is not in it", skill, n.ID, n.InstructionsFile)
			}
			n.Instructions = strings.TrimSpace(string(text))
		}
	}
	return g.Validate()
}

func snapshot(fsys fs.FS) (map[string][]byte, string, error) {
	files := map[string][]byte{}
	size := 0
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir():
			return nil
		case !d.Type().IsRegular():
			return fmt.Errorf("%s: not a regular file", p)
		}
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(f, int64(maxSkill-size)+1))
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
		if size += len(b); size > maxSkill {
			return fmt.Errorf("over %d bytes", maxSkill)
		}
		files[p] = b
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	h := sha256.New()
	for _, p := range slices.Sorted(maps.Keys(files)) {
		sum := sha256.Sum256(files[p])
		fmt.Fprintf(h, "%q %x\n", p, sum)
	}
	return files, "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}
