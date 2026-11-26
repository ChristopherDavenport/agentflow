// Package script runs a workflow's script steps in a workspace.
//
// A script reads its arguments as one JSON object on standard input. Its
// result is what it writes to the file named by $AGENTFLOW_OUTPUT, and
// its standard output when it writes none. For a step with no declared
// outcomes the result is the data itself, so a program that prints JSON
// works as it is. A step with outcomes prints
// {"outcome": ..., "data": ...}.
package script

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentflow"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentworkspace"
)

// OutputEnv names the variable holding the result file's path.
const OutputEnv = "AGENTFLOW_OUTPUT"

// maxResult bounds a result. A larger one fails rather than being cut.
const maxResult = 1 << 20

// New returns the tool a graph's script steps call, running them in ws.
// A script is a path within the workspace.
func New(ws agentworkspace.Workspace, timeout time.Duration) agenttool.Tool {
	return agenttool.New(agentflow.ScriptTool, "Run a workflow script.", func(ctx context.Context, a agentflow.ScriptCall) (agentflow.Result, error) {
		// Read again keeping numbers as written, which the typed
		// decoding does not.
		if call, ok := agenttool.CallFrom(ctx); ok {
			var exact agentflow.ScriptCall
			d := json.NewDecoder(bytes.NewReader(call.Args))
			d.UseNumber()
			if err := d.Decode(&exact); err != nil {
				return agentflow.Result{}, fmt.Errorf("script: arguments: %w", err)
			}
			a = exact
		}
		return run(ctx, ws, timeout, a)
	})
}

func run(ctx context.Context, ws agentworkspace.Workspace, timeout time.Duration, a agentflow.ScriptCall) (res agentflow.Result, err error) {
	if !filepath.IsLocal(a.Run) {
		return res, fmt.Errorf("script %q: not a path within the workspace", a.Run)
	}
	with := a.With
	if with == nil {
		with = map[string]any{}
	}
	stdin, err := json.Marshal(with)
	if err != nil {
		return res, err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return res, err
	}
	name := ".agentflow-output-" + hex.EncodeToString(id[:])
	if err := ws.WriteFile(ctx, name, nil, 0o600); err != nil {
		return res, fmt.Errorf("script %q: result file: %w", a.Run, err)
	}
	defer func() {
		// The result is read by now, so a leftover file is only litter.
		_ = ws.Remove(context.WithoutCancel(ctx), name)
	}()
	file := name
	if root := ws.Root(); root != "" {
		file = path.Join(root, name)
	}
	out, err := ws.Exec(ctx, agentworkspace.Command{
		Args:    []string{"./" + filepath.ToSlash(filepath.Clean(a.Run))},
		Env:     []string{OutputEnv + "=" + file},
		Stdin:   stdin,
		Timeout: timeout,
	})
	if err != nil {
		return res, fmt.Errorf("script %q: %w", a.Run, err)
	}
	if out.ExitCode != 0 || out.TimedOut {
		return res, fmt.Errorf("script %q: exit %d, timed out %v: %s", a.Run, out.ExitCode, out.TimedOut, tail(out.Stderr))
	}
	body, err := readResult(ws.FS(), name)
	if err != nil {
		return res, fmt.Errorf("script %q: result file: %w", a.Run, err)
	}
	if len(body) == 0 {
		body = bytes.TrimSpace(out.Stdout)
	}
	if len(body) > maxResult {
		return res, fmt.Errorf("script %q: result is over %d bytes; write large data to a file and return its path", a.Run, maxResult)
	}
	res, err = parse(body, len(a.Outcomes) > 0)
	if err != nil {
		return res, fmt.Errorf("script %q: %w", a.Run, err)
	}
	return res, nil
}

// readResult reads at most one byte over the limit.
func readResult(fsys fs.FS, name string) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxResult+1))
	return bytes.TrimSpace(b), errors.Join(err, f.Close())
}

// parse reads one JSON value: the result itself when the step branches,
// and its data otherwise.
func parse(body []byte, branches bool) (agentflow.Result, error) {
	var v any
	if len(body) > 0 {
		d := json.NewDecoder(bytes.NewReader(body))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			return agentflow.Result{}, fmt.Errorf("result is not JSON: %w", err)
		}
		if _, err := d.Token(); err != io.EOF {
			return agentflow.Result{}, errors.New("result is more than one JSON value")
		}
	}
	if !branches {
		return agentflow.Result{Outcome: agentflow.OK, Data: v}, nil
	}
	m, ok := v.(map[string]any)
	outcome, _ := m["outcome"].(string)
	if !ok || outcome == "" {
		return agentflow.Result{}, errors.New(`a step with outcomes prints {"outcome": ..., "data": ...}`)
	}
	return agentflow.Result{Outcome: outcome, Data: m["data"]}, nil
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 2000 {
		s = "..." + s[len(s)-2000:]
	}
	return s
}
