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
	// Graph is named for the skill, and its digest covers every file.
	Graph *agentflow.Graph
	// Files is the skill's tree as it was when loaded, by slash path.
	// Scripts run from these, so a later change on disk changes nothing.
	Files map[string][]byte
}

// Load reads a skill marked as a workflow.
func Load(s *agentskill.Skill) (*Workflow, error) {
	file, ok := Marker(s)
	if !ok {
		return nil, fmt.Errorf("workflow: skill %q is not a workflow: no metadata.%s", s.Name, MetadataKey)
	}
	if s.FS == nil {
		return nil, fmt.Errorf("workflow: skill %q has no files", s.Name)
	}
	files, digest, err := snapshot(s.FS)
	if err != nil {
		return nil, fmt.Errorf("workflow: skill %q: %w", s.Name, err)
	}
	src, ok := files[file]
	if !ok {
		return nil, fmt.Errorf("workflow: skill %q: metadata.%s names %q, which is not in it", s.Name, MetadataKey, file)
	}
	g, err := Parse(s.Name, src)
	if err != nil {
		return nil, fmt.Errorf("workflow: skill %q: %s: %w", s.Name, file, err)
	}
	for _, n := range g.Nodes {
		if n.Script != "" && files[n.Script] == nil {
			return nil, fmt.Errorf("workflow: skill %q: node %q runs %q, which is not in it", s.Name, n.ID, n.Script)
		}
	}
	g.Sources = digest
	return &Workflow{Graph: g, Files: files}, nil
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
