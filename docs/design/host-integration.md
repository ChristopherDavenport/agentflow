# Host integration

What a host supplies to run a workflow, and what it calls. A host is the
program that builds the `agentturn.Config`, owns the session store and
talks to the person. `examples/live_test.go` holds a whole one,
`recordedHost`: a jsonl store, child sessions, a record, sub-workspaces
and compaction.

## The least a host does

```go
cfg, err := agentflow.Apply(graph, agentturn.Config{Model: model, ModelName: name, Tools: tools})
a := agentturn.New(cfg)

// Start a run, and see it through what a config alone cannot.
end, st, err := agentflow.Invoke(ctx, graph, a, inputs)

// At a person step the run ends input_required.
q, err := graph.Question(end.Pending[0])
answer, err := graph.Answer(end.Pending[0], agentflow.Result{Outcome: "fix"})
end, st, err = agentflow.Resume(ctx, graph, a, []agentturn.Answer{answer})
```

`Apply` returns an `agentturn.Config`. There is no second engine:
`Prompt`, `Resume`, hooks, recording and fronts work as they do for any
config. What `Apply` adds, and how the program takes its turns, is in
[record-and-replay.md](record-and-replay.md).

A workflow that calls another, or has a fresh step or a judge, works in
child sessions, and only agentflow's own entry points reach them:

- `Prompt` alone ends `input_required` at every such call. `Invoke` and
  `Drive` open the child and see it through.
- The agent's own `Resume` answers a call its own session holds.
  `agentflow.Resume` takes an answer down to the child that asked.
- An invocation prompted by hand abandons the run it is prompted over,
  not the children that run waits on. `Invoke` abandons those first.

A host with script steps offers the script tool,
`script.New(workspace, timeout, script.Files(w.Files))`, where `w` is
what `workflow.Load` returned. Only the program calls it. `timeout` holds
a script whose step has no timeout of its own or its workflow's; a step
that has one is held to that instead, which a tool reads with
`agentflow.StepDeadline`.

## Options

Options go to `Apply`. `Drive`, `Resume`, `Invoke` and `OverrideRun` use
the options the agent's config was given by `Apply`, with any passed to
them added, provided they are given the same `*Graph` value `Apply` was.

| Option | What it gives |
|---|---|
| `History(read)` | the record: every item that has happened, oldest first, such as the item entries of a session's path. Position, step inputs and the gate are read from it. It must be current when called, and is called several times a turn |
| `Inherited(read)` | for a child that opens with its caller's view, given to `Child.Apply`: every item its session opened with, oldest first, the same each time. No position is read from them, and its `History` holds none of them |
| `Children(open)` | the opener of child sessions. Needed by any graph with a call, a fresh step, or a step the host lets a judge answer; `Apply` refuses such a graph without it |
| `Models(names...)` | the models the host serves to children. `Apply` refuses a step, or a workflow default, that asks for another |
| `JudgeModel(name)` | the model a judge's child is asked for, instead of the one that works |
| `Delegate(mode, steps...)` | how far a judge may answer a judged step: `Shadow`, `Advise` or `Decide`. With no steps, every judged step. A step of a called workflow is named `<workflow>:<step>`. A later `Delegate` overrides an earlier one for the steps both name |
| `Reply(fn)` | the host's own rules, asked before anyone else. `fn(ctx, Question)` returns a `Result` and true, or false to leave the question |
| `Unattended()` | nobody is there to ask: a person step takes its `unattended` outcome, and a failed step or a reached bound ends the run |
| `UnattendedTimeout(d)` | the timeout of an unattended model step that sets none. `DefaultUnattendedTimeout` (30 minutes) if not given; 0 for none |
| `MaxDepth(n)`, `MaxSteps(n)` | bounds on the tree, under the workflow's own; the smaller wins. The root's start records them |
| `PolicyMatchers(matchers, opts...)` | what agentpolicy needs to read a step's policy: for each working directory, each tool's matcher. See [policy-and-trust.md](policy-and-trust.md) |
| `FileReader(tool, field)` | the host's tool that reads a workspace file for a step's `files`, and its path argument: `read` and `path` unless given. `Apply` refuses a step given workspace files when the config does not offer it |
| `Responses(read)` | the session's responses with what each used, so a run's summary is whole after a restart |
| `Prices(price)` | `price(model, usage)` returns a cost and whether the model is priced, for a run's summary |

## Entry points

| Call | What it does |
|---|---|
| `Apply(g, cfg, opts...)` | makes `cfg` follow `g`. It validates the graph, checks the config's tools, models and matchers against it, and builds every policy's engine |
| `Invoke(ctx, g, a, inputs, opts...)` | a person's invocation: abandons whatever is under way, children included, starts a new run and drives it |
| `Drive(ctx, g, a, end, opts...)` | takes a run that ended and sees it on: reminds a model that stopped, works on held children, lets a question whose deadline has passed answer itself, times out a call, and meets a model's request to start a workflow. A nil `end` drives the agent as it stands |
| `Resume(ctx, g, a, answers, opts...)` | takes answers down to the agent that holds the calls, at any depth, and drives back up |
| `OverrideRun(ctx, g, a, o, opts...)` | an operator's `retry`, `jump` or `abandon` of a run by its key |
| `g.Invocation(inputs)` | the invocation as an item, for a host that prompts with it itself |
| `g.Question(pending)` | a pending call read as a `Question`, with the step's description |
| `g.Answer(pending, result)` | an answer held to the question as it was asked: its outcomes, form, choices, words and deadline |
| `Pick(q, items...)`, `Words(q, text)` | a `Result` that picks from a question's choices, or answers in the person's own words |
| `g.Replay(t)` | the `State` a transcript or record gives |
| `Runs(record, open)` | every run a record holds, as a tree, whatever the workflows have become since. `open` reads a child session's items by spawn, or is nil |
| `Requested(t)`, `UnderWay(t)` | a model's unmet request to start a workflow; whether a run is under way |
| `SummaryFilter` | what a compactor is given to summarise |

`Drive`, `Resume`, `Invoke` and `OverrideRun` return the end and `State`
of the run that waits or stopped: the agent's own, or a child's that
waits on a question. A front sends the person's words to the child that
is waiting on them: while a child waits, its caller takes no prompt.

## A session and its record

With a session, give the workflow the record, so a compaction or a
resumed session loses it nothing:

```go
history := agentflow.History(func(ctx context.Context) (agentturn.Transcript, error) {
	s, err := store.Open(ctx, id)
	if errors.Is(err, agentsession.ErrNoSession) {
		return nil, nil
	}
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
```

- A record that does not hold the calls of the transcript it is read
  beside is refused with `ErrRecordBehind`.
- Without `History` the transcript is the record. A transform that drops
  what position is read from is then refused with `ErrPositionLost`, a
  compaction among them, so a host that compacts gives `History`.
- A host whose guards can quarantine a transcript gives `History` too.

## Children

`Children` gives the opener. It is called each time the program works on
a child, so it builds an agent seeded from the child's session, or keeps
one in memory. It returns the context the child's runs take, because a
recorder finds a child's session from the context, and the agent.

```go
func open(ctx context.Context, c agentflow.Child) (context.Context, *agentturn.Agent, error) {
	cctx, err := rec.ContinueChild(ctx, c.Spawn)
	switch {
	case errors.Is(err, session.ErrNoSpawn) && c.Seeded:
		cctx, err = rec.ForkChildBefore(ctx, c.Spawn, c.Caller)
	case errors.Is(err, session.ErrNoSpawn):
		cctx, err = rec.StartChild(ctx, c.Spawn)
	}
	// ... build the child's workspace from c.Settings.Dir with script.Sub,
	// its tools on that workspace, its model from c.Settings.Model ...
	id := session.SessionIDFromContext(cctx)
	cfg, err := c.Apply(base, ownItems(id), inheritedItems(id))
	// ... make the agent from the child's session with session.AgentOptions,
	// and record it ...
	return cctx, a, nil
}
```

What a host does for a child that opens with its caller's view
(`c.Seeded`):

- Forks the child's session from the caller's, before the call:
  `ForkChildBefore(ctx, c.Spawn, c.Caller)`.
- Makes the agent from the child's session and seeds nothing by hand; the
  session holds what it opened with.
- Gives `c.Apply` a `History` of the item entries of `session.OwnPath`
  and an `Inherited` of the item entries of `session.PrefixPath`.
- Reads the child for `Runs` from `OwnPath` too, or the caller's runs
  appear as the child's.
- With no store, seeds the child's agent with `c.Seed`, gives the same
  items back as `Inherited`, and gives a `History` of what the agent
  holds past them. Such a host cannot compact a seeded child.

- `Child` names the run (`Run`), its session (`Spawn`), the call that
  holds it (`Caller`, `Parent`), what runs in it (`Workflow`, `Step`,
  `Judge`), a kept child's name (`Kept`), `Depth` and `Path`, and whether
  it opens with its caller's view (`Seeded`, `Seed`).
- `Child.Settings` is what the step says of its child: `Dir`, `Model`,
  `Thinking`, `Instructions`, `Tools`, `AllTools` and `ExcludeTools`.
- `c.Apply(cfg, opts...)` makes the child's config follow its workflow,
  with the caller's options and `opts` added. The opener gives the
  child's own `History`, and its own `Responses` if the host gives one.
  It makes the same checks of the child's config that `Apply` makes of
  the root's.
- An opener that cannot open the child where its call asks returns an
  error wrapping `ErrChildRefused` (`script.Sub` does): the calling step
  fails and may be retried.
- A child that compacts is given its own transform by the opener.

More on children is in [calls-and-children.md](calls-and-children.md).

## Compaction

```go
compact.NewLocal(summarizer, append(rec.CompactOptions(),
	compact.WithOnFold(rec.Fold),
	compact.WithFilter(agentflow.SummaryFilter))...)
```

- The recorder must be told of folds (`compact.WithOnFold(rec.Fold)`), or
  requests after a fold cannot be verified.
- `SummaryFilter` keeps the invocation, the opening messages and the
  reminders out of what is summarised, and hands the summarizer each call
  the working model did not make, with its result, as a developer message
  saying whose it was. Without it summaries tell later steps that earlier
  ones are still under way, and credit the model with what the program
  did. agentflow is handed only the compactor's `Transform` and cannot
  add the filter itself.

## Starting from a skill

A person's invocation is one way in. The other is a model asking for a
workflow's skill.

```go
tool, err := workflow.SkillTool(catalog, flows, workflow.ModelInvokes("greet"))
base.Tools = append(base.Tools, tool)
base.Instructions += catalog.Prompt() + "\n\n" + workflow.StartPrompt(flows, workflow.ModelInvokes("greet"))
```

- **`workflow.SkillTool(catalog, flows, opts...)`** stands in for
  agentskill's `skill` tool, under the same name. Its arguments are that
  tool's, `name` and `path`, and one more, `inputs`.
  - An ordinary skill is served by agentskill's own tool, unchanged.
    `inputs` on one is an error the model reads.
  - A workflow skill is never read through it.
  - Asked for by name, a workflow starts when the host lets a model start
    it and nothing is under way. `inputs` is held to the workflow's input
    schema; a bad one is an error the model reads and can correct. The
    result is `agentflow.StartText(name)`, with `Terminate` set, so the
    model's turn ends there.
  - A name given to `ModelInvokes` that is not one of `flows` is an error
    from `SkillTool`, with a suggestion.
- **Refusals, in order:** a file of a workflow; a workflow the host did
  not name; a hidden one; a run under way; inputs that do not fit.
- **Options:** `workflow.ModelInvokes(names...)`;
  `workflow.SkillHistory(read)`, the record, which the tool needs when
  the transcript can be compacted; `workflow.SkillToolOptions(opts...)`,
  passed to the catalog's own tool. A file claim is made for an ordinary
  skill's read and none for a workflow's, which reads nothing.
- **`workflow.StartPrompt(flows, opts...)`** renders a `<workflows>`
  block for the instructions, beside `Catalog.Prompt()`. It lists only
  the workflows the model may start, each with its input schema and
  description, and is empty when there are none. It is given the same
  `ModelInvokes` as the tool.
- **The program invokes.** `Drive` reads the request from the record
  (`agentflow.Requested`): a call whose result is `StartText` for the
  workflow its `name` names, with no invocation, start of a run or word
  from the person after it. It then sends the invocation under a trigger
  of kind `workflow`.
- **Another workflow than the agent's own.** An agent follows the one
  workflow its config was made for. When the model starts another,
  `Drive` returns the run's end with `ErrStartRequested`; a
  `*StartRequest` (`errors.As`) names the workflow, its inputs and the
  call. The host applies that workflow (`Agent.SetConfig` with `Apply`'s
  result) and calls `Invoke` with the request's inputs. A host with no
  workflow applied reads the same request with `Requested`.
- A host's own skill tool starts workflows the same way by answering with
  `StartText` and ending the turn.

## What a front reads

- **A question:** `g.Question(pending)`. `Workflow`, `Run` and `Path` say
  where it comes from; `Description` is the step's; `With` is the step's
  inputs; `Form`, `Choices` and `Text` say what an answer may hold;
  `Advice` and `Mode` what a judge said and how far it may answer;
  `Retry` and `Limit` mark a retry question and a bound's.
  `Deadline` is absolute, in UTC. See
  [questions-and-judges.md](questions-and-judges.md).
- **A partial result:** a step with `show` records a `flow_shown` beside
  its whole result. A front shows the person the whole result; the model
  is sent the part.
- **Progress:** a script's progress entries, live and kept. See
  [steps-and-data.md](steps-and-data.md).
- **Runs:** `Runs(record, open)` gives each run's status, outcome, path,
  summary and children. `Instance.At(record, i)` is where a run stood
  after item `i`. `Instance.Problem` says a record cannot be read as it
  claims to be.

## Operational notes

- **The host's policy must allow the program's tools.** The config's own
  `BeforeToolCall` sees every `flow_` call and `run_script`, so a policy
  that asks by default asks about them.
- **`MaxTurns` counts the program's turns,** in each session: a start,
  an end and each program step are turns of the session they are in.
- **The program's turns use nothing.** They are sent to the model name
  `agentflow/<workflow>`, answered by agentflow itself, and carry no
  usage. Cost and eval tools should skip them, not report an unpriced
  model.
- **Nothing wakes a sleeping host.** A question's deadline and a call's
  are on the record; the host drives the run when one comes.
- **A killed script's call is pending** and may have run. See
  [limits-failure-and-intervention.md](limits-failure-and-intervention.md).
- **Scripts run inside the workspace**, so a script's own run directory
  shows in a repository's status while it runs.

## Errors a host sees

| Error | Meaning |
|---|---|
| `ErrStalled` | a model stopped, was reminded once, and stopped again. The step is still current: an interactive host shows the person what the model said and prompts with their reply; an unattended one fails |
| `ErrChanged` | a workflow in the run's closure changed since the run started, or the record is of another format. The run cannot be resumed; it can still be read with `Runs` |
| `ErrRecordBehind` | the record given by `History` does not hold the transcript's calls |
| `ErrPositionLost` | without `History`, a transform dropped what position is read from |
| `ErrExpired` | `Answer` was given after the question's deadline. Drive the run; the timeout is what the record holds |
| `ErrStepTimeout` | a model's stream was cut off at its step's deadline. `Drive` continues the run |
| `ErrStepFailed` | a step's output was not a result with a declared outcome, and the step is not to be retried |
| `ErrVisits`, `ErrDepth`, `ErrSteps`, `ErrRepeat` | a bound was reached and nobody granted more |
| `ErrChildRefused` | a child cannot be opened as its call asks |
| `ErrStartRequested` | a model asked to start a workflow other than the agent's own |
