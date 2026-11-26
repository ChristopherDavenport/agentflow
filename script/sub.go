package script

import (
	"context"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agentworkspace"
)

// Sub is ws confined to dir, a slash path below its root, as a step's
// dir names a child's working directory: its root, its files and where
// its commands start are dir's, and a name or a command's directory that
// leaves dir is refused with [agentworkspace.ErrOutside]. A link inside
// dir is followed as ws follows it, so it may reach elsewhere in ws.
// Closing it leaves ws open.
//
// A dir it cannot work in, one that is missing, is not a directory or is
// not below the root, is refused with an error that wraps
// [agentflow.ErrChildRefused] as well as the cause, so an opener that
// returns it fails the step that asked, which the step may retry.
func Sub(ws agentworkspace.Workspace, dir string) (agentworkspace.Workspace, error) {
	if !fs.ValidPath(dir) || dir == "." {
		return nil, fmt.Errorf("script: dir %q: %w: %w", dir, agentflow.ErrChildRefused, agentworkspace.ErrOutside)
	}
	info, err := fs.Stat(ws.FS(), dir)
	if err != nil {
		return nil, fmt.Errorf("script: dir %q: %w: %w", dir, agentflow.ErrChildRefused, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("script: dir %q is not a directory: %w", dir, agentflow.ErrChildRefused)
	}
	return &sub{ws: ws, dir: dir}, nil
}

type sub struct {
	ws  agentworkspace.Workspace
	dir string
}

func (s *sub) inside(name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", fmt.Errorf("%q: %w", name, agentworkspace.ErrOutside)
	}
	return path.Join(s.dir, name), nil
}

func (s *sub) Root() string {
	if r := s.ws.Root(); r != "" {
		return filepath.Join(r, filepath.FromSlash(s.dir))
	}
	return ""
}

func (s *sub) FS() fs.FS {
	f, err := fs.Sub(s.ws.FS(), s.dir)
	if err != nil {
		// Sub was given a valid path.
		panic(err)
	}
	return f
}

func (s *sub) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	p, err := s.inside(name)
	if err != nil {
		return err
	}
	return s.ws.WriteFile(ctx, p, data, perm)
}

func (s *sub) Remove(ctx context.Context, name string) error {
	p, err := s.inside(name)
	if err != nil {
		return err
	}
	return s.ws.Remove(ctx, p)
}

func (s *sub) Env() []string { return s.ws.Env() }

func (s *sub) Exec(ctx context.Context, cmd agentworkspace.Command) (*agentworkspace.Output, error) {
	dir := cmd.Dir
	if dir == "" {
		dir = "."
	}
	p, err := s.inside(dir)
	if err != nil {
		return nil, err
	}
	cmd.Dir = p
	return s.ws.Exec(ctx, cmd)
}

func (s *sub) Descriptor() agentworkspace.Descriptor {
	d := s.ws.Descriptor()
	if d.Root != "" {
		d.Root = s.Root()
	}
	return d
}

func (s *sub) Close() error { return nil }
