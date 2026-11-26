package agentflow

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// startedPrefix starts what a skill tool answers a model whose request
// to start a workflow will be met.
const startedPrefix = "agentflow: the workflow "

// StartText is the result a skill tool gives a model that asked for a
// workflow's skill when the workflow will start: the program, not the
// model, then invokes it. [Requested] reads a request by it.
func StartText(workflow string) string {
	return startedPrefix + strconv.Quote(workflow) + " starts now, run by the workflow; your turn is over."
}

// ErrStartRequested is the error [Drive] returns, as a [*StartRequest],
// when a model asked to start a workflow other than the one the agent
// follows: the host applies that workflow to the agent and calls
// [Invoke] with the request's inputs.
var ErrStartRequested = errors.New("agentflow: a model asked to start a workflow")

// StartRequest is a model's request to start a workflow, read from the
// record: the call it made, the workflow it named and the inputs it gave,
// which the skill tool held to the workflow's schema.
type StartRequest struct {
	Workflow string
	Inputs   any
	// Call is the ID of the model's call that asked.
	Call string
}

func (r *StartRequest) Error() string {
	return fmt.Sprintf("%v: %s, in call %s", ErrStartRequested, r.Workflow, r.Call)
}

// Is reports whether target is [ErrStartRequested].
func (r *StartRequest) Is(target error) bool { return target == ErrStartRequested }

// startArgs is what a request is read from: a skill tool's arguments.
type startArgs struct {
	Name   string `json:"name"`
	Inputs any    `json:"inputs,omitempty"`
}

// Requested reads the request a model made to start a workflow that
// nothing has met yet: a call whose result is [StartText] for the
// workflow it names, with no invocation, start of a run or word from the
// person after it. t is every item of the record, or the transcript
// while nothing has dropped from it.
func Requested(t agentturn.Transcript) (*StartRequest, bool) {
	calls := map[string]*openresponses.FunctionCall{}
	var req *StartRequest
	for _, item := range t {
		switch it := item.(type) {
		case *openresponses.FunctionCall:
			calls[it.CallID] = it
			if it.Name == startTool && strings.HasPrefix(it.CallID, reserved) {
				req = nil
			}
		case *openresponses.FunctionCallOutput:
			c := calls[it.CallID]
			if c == nil || strings.HasPrefix(c.CallID, reserved) {
				continue
			}
			var a startArgs
			if decode([]byte(c.Arguments), &a) != nil || a.Name == "" || it.Output.String() != StartText(a.Name) {
				continue
			}
			req = &StartRequest{Workflow: a.Name, Inputs: a.Inputs, Call: c.CallID}
		case *openresponses.Message:
			if it.Role == openresponses.RoleUser || it.Role == openresponses.RoleDeveloper && strings.HasPrefix(it.Text(), invocationPrefix) {
				req = nil
			}
		}
	}
	return req, req != nil
}

// UnderWay says why a model may not start a workflow now, nil when it
// may: a run is under way from its start until its end is written, and a
// model inside one is working on it. t is every item of the record, or
// the transcript while nothing has dropped from it.
func UnderWay(t agentturn.Transcript) error {
	runs, err := Runs(t, nil)
	if err != nil {
		return fmt.Errorf("agentflow: cannot tell whether a workflow is running: %w", err)
	}
	for _, r := range runs {
		if r.Status == Running {
			return fmt.Errorf("the workflow %q is running, and a model starts a workflow only in conversation, between runs", r.Workflow)
		}
	}
	return nil
}

// startRequested says a run that a skill tool ended was a model asking
// to start a workflow, and which.
func startRequested(end *agentturn.RunEnd, record agentturn.Transcript) (*StartRequest, bool) {
	if end == nil || end.Reason != agentturn.ReasonStopped || end.Cause != agentturn.StopTerminate && end.Cause != agentturn.StopPartialTerminate {
		return nil, false
	}
	return Requested(record)
}
