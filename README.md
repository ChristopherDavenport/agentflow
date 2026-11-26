# agentflow

Program-driven workflows over [agentturn](https://github.com/ChristopherDavenport/agentturn):
a graph decides whether the program, the model or a person takes each
turn.

**This is a spike.** The design is
[RFC 0001](docs/rfcs/0001-agentflow.md), which says what is implemented
and what is only proposed. Graphs are built in Go; the `WORKFLOW.md`
format is not parsed yet.

```go
cfg, err := agentflow.Apply(graph, agentturn.Config{Model: model, ModelName: name, Tools: tools})
a := agentturn.New(cfg)
end, err := a.Prompt(ctx, openresponses.UserText("review this PR"))

// A model that stops without finishing is reminded once.
end, state, err := agentflow.Drive(ctx, graph, a, end)

// At a person step the run ends input_required.
answer, err := agentflow.Answer(end.Pending[0], agentflow.Result{Outcome: "fix"})
end, err = a.Resume(ctx, answer)

// Run it again in the same conversation.
end, err = a.Prompt(ctx, graph.Invocation())
```
