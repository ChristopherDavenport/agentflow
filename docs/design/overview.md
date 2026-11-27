# agentflow: overview

A workflow is a graph that decides who takes each turn: the **program**,
the **model** or a **person**. All three write to one transcript and one
session. Handing a turn to the model or a person and continuing when they
finish is the same operation as any other step.

Workflows are authored as Agent Skills that carry a `WORKFLOW.md`: a
Mermaid diagram for control flow and a section per node. A Go builder
constructs the same graph.

## Goals

- The same workflow runs interactively or unattended.
- Control flow is deterministic: it depends only on the definition and on
  recorded results.
- A person can read, write and review a workflow. A program or a model can
  generate one.
- Resuming in another process needs the session and the definition, and
  nothing else.
- agentflow builds on the public surface of agentturn, agentskill and the
  session format. None of them knows about workflows.

## Non-goals

- General BPMN. Timers, message correlation and parallel gateways are left
  out until something can schedule and join them.
- An expression language.
- A scheduler or run service.

## The rules

Every part of the design holds these.

- **Position is a pure function of the definitions and the record.** The
  record is every item that happened. A child's position is a function of
  its own session, and a parent's of its own session plus its calls'
  results. What a model is sent is only a view of the record.
- **Control flow depends on recorded outcomes and nothing else.** Time,
  randomness and anything clever belong in a script whose result is
  recorded. A decision is a step with a named outcome, so every branch is
  in the diagram.
- **Resuming needs the store and the definitions.** Nothing is kept on the
  side.
- **The record never passes a program's turn off as a model's.** A program
  or person turn is recorded under the model name `agentflow/<workflow>`,
  and the program's calls carry IDs only the program writes.
- **Only the program runs scripts.** A model's call to `run_script`, and a
  tool's nested one, are refused.
- **The working model never sees a definition, a judge's criteria or a
  blind judge.** Every `flow_start` stays out of its requests.
- **The loop learns nothing about workflows.** `Apply` returns an ordinary
  `agentturn.Config`; children are agents a host makes, and a held call is
  an ordinary deferral.
- **Unknown keys are load errors.** Nothing in a workflow file is ignored,
  and everything that can be checked at load is.
- **A workflow can only narrow what its host permits.** A step's tools and
  policy never widen the host's.
- **Secrets are never data.** Everything a workflow handles is in the
  transcript and can reach a model.
- **Approving a workflow is approving a digest.** A run's start records a
  digest of the graph, which for a skill covers every file in it.

## Everything is a call

Everything a workflow does at run time is a call with arguments and a
result, made in the context it operates in. The same hooks, policy,
recording, replay and fronts handle it as handle any tool call.

| What happens | The call | Who makes it |
|---|---|---|
| a run starts | `flow_start` | program |
| a script step | `run_script` | program |
| a tool step | the step's tool | program |
| a call to a workflow, a fresh model step, a kept child's visit | `flow_call` | program; a child answers a fresh one |
| a model step finishes | `flow_finish` | the step's model |
| a model step reports it is stuck | `flow_stuck` | the step's model |
| a model step reads or writes a document | `flow_doc_read`, `flow_doc_write` | the step's model |
| a model step is given its files | `flow_files` | program |
| a model is shown part of a result | `flow_shown` | program |
| a question is asked | `flow_ask` | program; a person, the host or a judge answers |
| a judge is asked to look, and gives its verdict | `flow_judge` | program asks; the judge's model answers in its own child |
| a deadline is set or passes | `flow_clock` | program |
| a policy rule cannot be resolved from the record | `flow_limits` | program |
| an operator's override is applied | `flow_override` | program |
| a run ends | `flow_end` | program |

A call being a tool call does not make it a model's to make. The gate
refuses `run_script` and every `flow_` call from a model, except the ones
the model's current step offers it: `flow_finish` and `flow_stuck` for
that step and run, the document calls for the documents the step lists,
and, in a judge's child, the verdict. A tool's nested call to `run_script`
or any `flow_` tool is always refused.

Each of the program's calls carries the run's key in its call ID, and each
of the model's names its run and step in its arguments; see
[record-and-replay.md](record-and-replay.md).

## Terms

| Term | Meaning |
|---|---|
| definition, graph | a workflow as loaded: nodes, edges, settings and every text read at load |
| digest | a hash of a definition, covering every file of its skill |
| closure | a workflow and every workflow it can reach by calls |
| step, node | one place in the graph; a program, model or person step |
| outcome | the name a step ends with; edges route on it |
| result | what a step ends in: an outcome, data, who chose and why |
| run, instance | one invocation of a workflow, from `flow_start` to `flow_end` |
| key | the random name of a run, carried by its calls |
| frame | a run on the stack of one session; a call in context pushes one |
| visit | one entry of a step in a run |
| attempt | one try at a visit; a retry is a new attempt |
| record | every item of a session's path, whatever a model was later sent |
| view | what a request sends a model of the record |
| child | a session of its own, opened for a fresh step, a fresh call or a judge |
| front | a program that shows a run to a person and takes their answers |
| host | the program that builds the config, opens children and drives runs |

## The documents

| Document | Holds |
|---|---|
| [workflow-format.md](workflow-format.md) | the skill layout, `WORKFLOW.md`, every key, expressions, schemas and load checks |
| [steps-and-data.md](steps-and-data.md) | what each kind of step does and what flows between steps |
| [record-and-replay.md](record-and-replay.md) | runs, keys, call IDs, position, the record and the view, ending and resuming |
| [calls-and-children.md](calls-and-children.md) | calling a workflow, contracts, child sessions and their settings |
| [questions-and-judges.md](questions-and-judges.md) | questions to people, who answers them, and models that may answer |
| [limits-failure-and-intervention.md](limits-failure-and-intervention.md) | failure and retry, timeouts, bounds, abandoning, cleanup and overrides |
| [policy-and-trust.md](policy-and-trust.md) | tool policy, what a model may start, and what is not protected |
| [host-integration.md](host-integration.md) | what a host supplies and calls |
| [decisions.md](decisions.md) | why the design is this way and not another |
| [open-questions.md](open-questions.md) | what is unsettled, unbuilt or untried |
