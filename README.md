# agentflow

Program-driven workflows over [agentturn](https://github.com/ChristopherDavenport/agentturn):
a graph decides whether the program, the model or a person takes each
turn.

A workflow is a `WORKFLOW.md` in an Agent Skill, or a graph built in Go.
The loop never learns workflows exist: `Apply` returns an ordinary
`agentturn.Config`. Where a run stands is read from its record alone, so
resuming needs the session and the definition and nothing else.

The design is in [docs/design](docs/design/overview.md); what is not built
or not settled is in [open questions](docs/design/open-questions.md).
Before v1.0.0 the API and the `WORKFLOW.md` keys may change between minor
versions.

```sh
go get github.com/ChristopherDavenport/agentflow
```

Needs Go 1.26.

| Package | What it holds |
| --- | --- |
| `agentflow` | the graph, the Go builder and everything that drives a run |
| `agentflow/workflow` | `WORKFLOW.md`: reading, writing and loading it from a skill |
| `agentflow/script` | `run_script`, which runs script steps in an `agentworkspace` |
| `agentflow/summary` | how a run went, read back from its record |
| `agentflow/examples` | whole workflow skills and the tests that run them |

## Using it


```go
cfg, err := agentflow.Apply(graph, agentturn.Config{Model: model, ModelName: name, Tools: tools})
a := agentturn.New(cfg)

// Start a run and see it on: a model that stops without finishing is
// reminded once, and a workflow it calls is worked in a child session.
end, state, err := agentflow.Invoke(ctx, graph, a, nil) // or its inputs

// At a person step the run ends input_required.
answer, err := graph.Answer(end.Pending[0], agentflow.Result{Outcome: "fix"})
end, state, err = agentflow.Resume(ctx, graph, a, []agentturn.Answer{answer})

// Run it again in the same conversation.
end, state, err = agentflow.Invoke(ctx, graph, a, nil)
```

A graph that calls another workflow, or has a fresh step or a judge,
needs `agentflow.Children`: see
[host integration](docs/design/host-integration.md).

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

## Development

```sh
make check
```

runs gofmt, `go mod tidy -diff`, vet, the dependency check, staticcheck,
govulncheck and the race tests. Tests run offline against scripted models;
the live trials skip unless `AGENTFLOW_LIVE_URL` and
`AGENTFLOW_LIVE_MODEL` are set. The tests run shell scripts that need
`jq` and `git`. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

MIT; see [LICENSE](LICENSE).
