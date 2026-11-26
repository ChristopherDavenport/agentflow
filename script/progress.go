package script

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agenttool"
)

// progressEvery is how often a running script's progress file is read.
var progressEvery = 100 * time.Millisecond

const (
	// maxProgress bounds what is read of a progress file; past it, a
	// script's later lines are not reported.
	maxProgress = 1 << 20
	// maxProgressLine bounds one update. A longer line is not reported.
	maxProgressLine = 4 << 10
	// MaxKeptProgress bounds the updates of one call that are kept in the
	// session. Later ones are reported as they come and not kept.
	MaxKeptProgress = 256
)

// ProgressNS is the namespace of the session entry each kept update is
// written under, beside the call that reported it.
const ProgressNS = "agentflow:progress"

// ProgressRecord is a kept update: what a front reads back of a script's
// progress after the process that ran it has gone. Seq counts the call's
// kept updates from 1.
type ProgressRecord struct {
	Seq      int     `json:"seq"`
	Progress float64 `json:"progress,omitempty"`
	Total    float64 `json:"total,omitempty"`
	Message  string  `json:"message,omitempty"`
}

// RecordNS returns [ProgressNS], so a recorder writes the update as a
// custom entry of that namespace.
func (ProgressRecord) RecordNS() string { return ProgressNS }

// progressLine is one line of a progress file.
type progressLine struct {
	Progress float64 `json:"progress"`
	Total    float64 `json:"total"`
	Message  string  `json:"message"`
}

// watchProgress reports each line a script appends to the progress file
// name, until the stop it returns is called, which reports what is left
// and waits. The updates go to the call on ctx, as a long-running tool's
// do, and the first [MaxKeptProgress] are written to the record on ctx
// as [ProgressRecord]s; none is part of the result, which they do not
// change, and none is an item of the transcript, which a model reads.
func watchProgress(ctx context.Context, fsys fs.FS, name string) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	var read, kept int
	var partial []byte
	update := func(ctx context.Context, line []byte) {
		if p, ok := progressOf(line); ok {
			report(ctx, p)
			if kept < MaxKeptProgress {
				kept++
				// A record that cannot be written costs the front a line
				// of progress, and never the script its run.
				_ = agenttool.WriteRecord(ctx, ProgressRecord{Seq: kept, Progress: p.Progress, Total: p.Total, Message: p.Message})
			}
		}
	}
	scan := func(final bool) {
		if read >= maxProgress {
			return
		}
		b, err := readFrom(fsys, name, read)
		if err != nil {
			return
		}
		read += len(b)
		partial = append(partial, b...)
		for {
			i := bytes.IndexByte(partial, '\n')
			if i < 0 {
				break
			}
			update(ctx, partial[:i])
			partial = partial[i+1:]
		}
		if final && len(partial) > 0 {
			update(ctx, partial)
			partial = nil
		}
	}
	go func() {
		defer close(finished)
		tick := time.NewTicker(progressEvery)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				scan(false)
			}
		}
	}()
	return func() {
		close(done)
		<-finished
		scan(true)
	}
}

// readFrom reads what name holds past offset, up to the bound.
func readFrom(fsys fs.FS, name string, offset int) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxProgress))
	if err != nil || len(b) <= offset {
		return nil, err
	}
	return b[offset:], nil
}

// progressOf reads one line: an update of numbers and a message, or of
// a message alone when the line is not one. A blank or long line is
// none.
func progressOf(line []byte) (progressLine, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || len(line) > maxProgressLine {
		return progressLine{}, false
	}
	var p progressLine
	if line[0] != '{' || json.Unmarshal(line, &p) != nil {
		p = progressLine{Message: string(line)}
	}
	return p, true
}

// report sends one update to the call on ctx.
func report(ctx context.Context, p progressLine) {
	text := p.Message
	if p.Total > 0 {
		text = strings.TrimSpace(fmt.Sprintf("%g/%g %s", p.Progress, p.Total, p.Message))
	}
	agenttool.Progress(ctx, agenttool.Result{
		Output:  agenttool.Text(text).Output,
		Details: agenttool.ProgressInfo{Progress: p.Progress, Total: p.Total, Message: p.Message},
	})
}
