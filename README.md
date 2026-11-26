# agentflow

Program-driven workflows over [agentturn](https://github.com/ChristopherDavenport/agentturn):
a graph decides whether the program, the model or a person takes each
turn.

**This is a spike.** The design is
[RFC 0001](docs/rfcs/0001-agentflow.md), which says what is implemented
and what is only proposed. A workflow is a `WORKFLOW.md` in an Agent
Skill, or a graph built in Go. Needs Go 1.26.

```go
cfg, err := agentflow.Apply(graph, agentturn.Config{Model: model, ModelName: name, Tools: tools})
a := agentturn.New(cfg)
end, err := a.Prompt(ctx, openresponses.UserText("review this PR"))

// A model that stops without finishing is reminded once.
end, state, err := agentflow.Drive(ctx, graph, a, end)

// At a person step the run ends input_required.
answer, err := graph.Answer(end.Pending[0], agentflow.Result{Outcome: "fix"})
end, err = a.Resume(ctx, answer)

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

A session says which runs it holds and how each ended, whatever the
workflow has become since:

```go
runs, err := agentflow.Instances(transcript)
state, err := runs[0].At(transcript, i) // where it stood after item i
```
