# agentflow

Program-driven workflows over [agentturn](https://github.com/ChristopherDavenport/agentturn):
a graph decides whether the program, the model or a person takes each
turn.

A workflow is a `WORKFLOW.md` in an Agent Skill, or a graph built in Go.
The design is in [docs/design](docs/design/overview.md); what is not built
or not settled is in [open questions](docs/design/open-questions.md).
Needs Go 1.26.

```go
cfg, err := agentflow.Apply(graph, agentturn.Config{Model: model, ModelName: name, Tools: tools})
a := agentturn.New(cfg)
end, err := a.Prompt(ctx, openresponses.UserText("review this PR"))

// A model that stops without finishing is reminded once.
end, state, err := agentflow.Drive(ctx, graph, a, end)

// At a person step the run ends input_required.
answer, err := graph.Answer(end.Pending[0], agentflow.Result{Outcome: "fix"})
end, state, err = agentflow.Resume(ctx, graph, a, []agentturn.Answer{answer})

// Run it again in the same conversation.
again, err := graph.Invocation(nil) // or its inputs
end, err = a.Prompt(ctx, again)
```

Steps pass JSON. A script reads its `with` values on stdin and prints its
result, so a program that emits JSON works as a step:

```go
{ID: "filter", Kind: agentflow.Run, Script: "filter.sh",
	With: map[string]agentflow.Value{"files": agentflow.From("steps.gather.outputs.files")}}

cfg, err := agentflow.Apply(graph, agentturn.Config{ /* ... */
	Tools: []agenttool.Tool{script.New(workspace, time.Minute)}})
```

A skill marked `metadata: {agentflow: WORKFLOW.md}` loads as a workflow.
`workflow/testdata/release` is a whole one.

```go
w, err := workflow.Load(skill) // skill from agentskill
cfg, err := agentflow.Apply(w.Graph, agentturn.Config{ /* ... */
	Tools: []agenttool.Tool{script.New(ws, time.Minute, script.Files(w.Files))}})
```

A person step that declares a `judge` can be handed to a model, as far as
the host says:

```go
// Why you chose is kept with your answer.
answer, err := graph.Answer(pending, agentflow.Result{Outcome: "go", Reason: "smallest fix"})

agentflow.Apply(graph, cfg, agentflow.Delegate(agentflow.Shadow))           // you decide; it judges afterwards, unseen
agentflow.Apply(graph, cfg, agentflow.Delegate(agentflow.Advise))           // you decide, with its advice
agentflow.Apply(graph, cfg, agentflow.Delegate(agentflow.Decide, "approve")) // it decides that step
```

With a session, give the workflow that record, so a compaction or a
resumed session loses it nothing. The model still sees only what the
transcript holds, and each step is handed its inputs:

```go
history := agentflow.History(func(ctx context.Context) (agentturn.Transcript, error) {
	s, err := store.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	var items agentturn.Transcript
	for _, e := range s.Path(s.Leaf()) {
		if item, ok := e.(*agentsession.ItemEntry); ok {
			items = append(items, item.Item)
		}
	}
	return items, nil
})
cfg, err := agentflow.Apply(graph, base, history) // and agentflow.Drive(..., history)
```

A compactor needs one thing from the workflow, or its summaries tell
later steps that earlier ones are still running, and credit the model with
what the program did:

```go
compact.NewLocal(summarizer, compact.WithOnFold(rec.Fold),
	compact.WithFilter(agentflow.SummaryFilter))
```

A session says which runs it holds and how each ended, whatever the
workflow has become since. Give it every item of the session's path; the
context `agentturn/session` rebuilds leaves out what the last request did.

```go
runs, err := agentflow.Runs(record, open) // open reads a child session's items, or nil
state, err := runs[0].At(record, i)       // where it stood after item i
```

## Documents

| | |
| --- | --- |
| [Overview](docs/design/overview.md) | what a workflow is, the rules it holds, every `flow_` call |
| [Workflow format](docs/design/workflow-format.md) | `WORKFLOW.md`: the diagram, every key, what is checked at load |
| [Steps and data](docs/design/steps-and-data.md) | what each kind of step does and what passes between them |
| [Record and replay](docs/design/record-and-replay.md) | the record, position, resuming, reading runs back |
| [Calls and children](docs/design/calls-and-children.md) | calling a workflow, child sessions |
| [Questions and judges](docs/design/questions-and-judges.md) | people, and the models that may answer for them |
| [Limits, failure and intervention](docs/design/limits-failure-and-intervention.md) | timeouts, bounds, abandoning, overrides |
| [Policy and trust](docs/design/policy-and-trust.md) | what a step may do, and what that does not promise |
| [Host integration](docs/design/host-integration.md) | what a host supplies and calls |
| [Decisions](docs/design/decisions.md) | what was chosen over what, and why |
| [Open questions](docs/design/open-questions.md) | unbuilt, unsettled, and not yet tried with a model |
