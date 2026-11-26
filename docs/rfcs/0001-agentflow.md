# RFC 0001: agentflow, program-driven workflows over the agent loop

Status: draft. Sections marked **spiked** are implemented and tested in
this repository. Everything else is proposed and untested.

## Summary

A workflow is a graph that decides who takes each turn: the **program**,
the **model** or a **person**. All three write to one transcript and one
session. Handing a turn to the model or a person and continuing when
they finish is the same operation as any other step.

Workflows are authored as Agent Skills that carry a `WORKFLOW.md`: a
Mermaid diagram for control flow and a section per node.

## Goals

- The same workflow runs interactively or unattended.
- Control flow is deterministic: it depends only on the definition and
  on recorded results.
- A person can read, write and review a workflow. A program or a model
  can generate one.
- Resume in another process needs the session and the definition, and
  nothing else.
- Nothing in agentturn, agentskill or the session format has to change.

## Non-goals

- General BPMN. Timers, message correlation and parallel gateways are
  left out until something can schedule and join them.
- An expression language.
- A scheduler or run service.

## Packaging: a workflow is a skill

A workflow reuses everything about Agent Skills: the directory layout,
the places skills are found, precedence, the `SKILL.md` frontmatter,
bundled files, `allowed-tools` and the host's trust rules. Only the
interpretation differs.

A skill is a workflow when its frontmatter says so:

```yaml
---
name: pr-review
description: Review a pull request against what was asked for.
metadata:
  agentflow: WORKFLOW.md
---
Prose telling a reader, or a model, how to do this by hand.
```

```
pr-review/
  SKILL.md
  WORKFLOW.md
  scripts/gather-pr.sh
```

- `metadata` is the specification's extension point, so the skill stays
  valid. A new top-level frontmatter key would be a validation error.
- The marker is explicit. A file that happens to be named `WORKFLOW.md`
  never turns a skill into something that executes.
- The marker is visible in the frontmatter, so a catalog can list
  workflows apart from traditional skills without reading any files.
- A host with no workflow support loads an ordinary skill and follows
  the body. Authors SHOULD write the body so that works.
- A marker naming a missing or invalid file is an error. An aware host
  MUST NOT fall back to the body on an explicit invocation.

An aware host offers two ways in. **Explicit**: a command such as
`/pr-review`, or a call from code. **Dynamic**: the model asks for a
workflow skill, which ends its turn and starts the workflow instead of
returning the body.

## WORKFLOW.md

The file holds exactly one `mermaid` block and one `##` section per
node, headed by the node ID.

````markdown
# PR review

```mermaid
flowchart TD
  gather[[gather]] --> review[review]
  review -->|clean| report[[report]]
  review -->|findings| triage([triage])
  triage -->|fix| fix[fix]
  triage -->|report| report
  fix --> gather
```

## gather
```yaml
run: scripts/gather-pr.sh
max_visits: 3
```

## review
```yaml
context: session
tools: [read, grep]
outcomes: [clean, findings]
```
Review the diff against what the session set out to do.

## triage
```yaml
outcomes: [fix, report]
unattended: report
```
These are the findings. Fix them now, or just report them?
````

### Accepted Mermaid

Only this subset. Anything else is a load error and is never ignored,
so the picture cannot disagree with what runs.

| Syntax | Meaning |
|---|---|
| `flowchart TD` or `flowchart LR` | first line |
| `id[[label]]` | the program runs a tool or script |
| `id[label]` | the model works |
| `id([label])` | a person decides |
| `a --> b` | `a` ends with `ok`, then `b` |
| `a -->\|outcome\| b` | `a` ends with `outcome`, then `b` |

The first node mentioned is the start. An outcome with no edge ends the
workflow. `end` is reserved by Mermaid and cannot be an ID.

### Node settings

| Key | Nodes | Meaning |
|---|---|---|
| `run` | program | script path within the skill, or a tool name and args |
| `tools` | model | tools the model may call; none if absent |
| `context` | model | `session`, `fresh`, or a list of node IDs |
| `outcomes` | all | ways the node can end; `[ok]` if absent |
| `unattended` | person | outcome taken when nobody can be asked |
| `max_visits` | all | bound on loops; 1 if absent |

The text under a model or person node is its prompt.

A Go builder constructs the same graph, and the graph can be written
back out as `WORKFLOW.md`.

## Execution (spiked)

`Apply(graph, config)` returns an `agentturn.Config`. There is no second
engine: `Prompt`, `Resume`, hooks, recording and the fronts work as they
do for any config.

- `BeforeModelCall` reads where the workflow stands and shapes the
  request. A program or person turn is sent to the model name
  `agentflow/<workflow>`; a model turn gets the node's prompt and only
  the node's tools.
- A router `Model` answers `agentflow/` requests itself by emitting the
  node's function call, and passes every other request to the real
  model.
- `BeforeToolCall` refuses calls the node does not allow and defers a
  person's question. The config's own hook still runs for allowed calls,
  so policy applies to program steps.

The model name is set before the turn is recorded. The record therefore
shows which turns the program took, and request hashes reproduce.

### An instance

The first call of an instance is `flow_start`, which carries the
workflow's name and the digest of its definition. Replay begins there,
so whatever the transcript held before is ignored. Resuming with a
definition whose digest differs is refused.

A transcript with no instance starts one. Another begins only at an
invocation, a developer message reading `agentflow: start <name>`
(`Graph.Invocation`), which a model cannot write. It also abandons an
unfinished instance. After an instance has finished and said so, a plain
prompt is a conversation with the model, with its tools. An instance
that failed stays failed until an invocation.

### Results

Every step ends in one result:

```json
{"outcome": "findings", "data": {}, "by": ""}
```

A transcript carries no failure flag, so success has to be said. A
program or person step whose output is anything else, an error, a
refusal or an unknown outcome included, has failed. A model finishes
with `flow_finish`; a finish naming an undeclared outcome is an error
the model sees and can correct.

### Failure

A failed step raises a `flow_ask` with the outcomes `retry` and `stop`.
`retry` runs the step again under a new call ID. `stop` fails the
workflow. The host's rules may answer; unattended, with no rule that
does, the workflow fails.

### Position

Position is a pure function of the graph and the transcript. Program
steps are matched by call ID
(`flow_<workflow>_<instance>_<node>_v<visit>`, with `-a<attempt>` on a
retry), model steps by a successful `flow_finish` naming the node, made
after the node became current, so one model turn cannot finish a step it
has not yet been given. Nothing else is stored. Names are bounded, 16
characters for a workflow and 20 for a node, because providers limit
call IDs.

Position needs every earlier step, so a transform that drops any of them,
a compaction among them, is refused. Long workflows cannot be compacted
until position is read from the session path instead (proposed). A
transform that records its fold before returning may still have written
it.

### Answering a question

A person step is a `flow_ask` call held by a `hold` decision, so the run
ends `input_required`. `Answer` approves it with the answer added to the
arguments, and the tool returns it. The record reads `hold by policy`,
then `proceed by human`. Supplying the output directly would be recorded
as `reject`, which is what the format says of any output a caller writes
for a call that did not run.

### A model that stops (spiked)

A config cannot keep a run going, so `Drive` takes a run that ended and,
if a model step is still current, reminds the model once. The reminder is
a user message sent under a trigger of kind `workflow`: the record names
its source, and neither the loop nor a reader takes it for the person's.
A model that stops again is `ErrStalled`. An interactive host shows the
person what the model said and prompts with their reply; an unattended
one fails.

## Interactive and unattended

When a question is reached, the host tries in order:

1. its own rules (`Reply`), marked `by: rules` in the result and decided
   `by: policy` in the record (spiked)
2. unattended, the node's `unattended` outcome, marked `by` (spiked)
3. waiting: the run ends `input_required` (spiked)

The working model never answers on a person's behalf.

## Context (proposed)

A workflow records into its own child session, linked to the session it
was invoked from, which it leaves untouched. A model node chooses what
it sees: `session` (the invoking conversation), `fresh` (only its
inputs) or named node results. A reviewer that inherits a session also
inherits its blind spots, so both have uses.

## Determinism and trust (proposed)

- `flow_start` records a digest of the graph (spiked). Loaded from a
  skill, it must cover the whole tree, scripts included.
- Branches use only recorded outcomes. Time, randomness and anything
  clever belong in a script whose result is recorded.
- A workflow from a repository is repository code that runs at fixed
  points. It needs approval by tree digest before its first run.
- Skill files must be materialised in the workspace where scripts run.

## Spike findings

Offline, with a scripted model:

- A session recorded with `agentturn/session` survives SIGKILL while a
  question is pending. A new process resumes from the store, completes
  the workflow and runs only the remaining steps. All request hashes
  reproduce.
- SIGKILL during a model turn: `Continue` finishes the workflow.
- SIGKILL during a script: the call is pending and may have run. A tool
  that says replay is safe is approved and the workflow goes on. One
  that does not cannot be approved; answering that the outcome is
  unknown fails the step, which asks whether to retry.
- Loops, visit limits, failed scripts, policy deferral of a program step
  and the unattended default behave as described.
- Fixed: one model turn could finish two steps; a transform could send
  the workflow back to its start.
- No change to agentturn or the session format was needed.

With real models, one model step with two outcomes after a program step,
8 trials each (6 for the local model), 2026-11-25:

| Model | Plain | Forced tool choice | One nudge |
|---|---|---|---|
| z-ai/glm-5.3 | 8/8 | 8/8 | |
| anthropic/claude-haiku-5.5 | 8/8 | 8/8 | |
| deepseek/deepseek-v4.1-flash | 8/8 | 8/8 | |
| qwen3:1.7b (Ollama) | 1/6 | 4/6 | 6/6 |

The figures count steps finished; every hosted model also chose the right
outcome each time. The small model usually wrote the result as JSON text
instead of calling `flow_finish`. The providers accepted a history
holding a call to a tool the request no longer offered. The samples are
small and the task easy. The hosted figures predate `flow_start`.

The role of the reminder, qwen3:1.7b, one reminder, 40 trials a row:

| Reminder role | Stalls recovered |
|---|---|
| developer | 5 of 21, and 3 of 20 reworded |
| user | 14 of 21 |

Not yet tested: a model step that uses tools over many turns, a prompt
that competes with the finish instruction, the real `compact` transform,
and the `context` modes.

## Open questions

1. **Compaction.** Read position from the session path, or record
   checkpoints that a fold keeps.
2. **Data between steps.** A model step sees earlier results in the
   transcript. A script gets none: nothing says how one step's data
   becomes another's arguments.
3. **Reminders.** One is the rule. A host may want more for a small
   model.
4. **A failed instance** answers every later prompt with its error until
   an invocation, since the transcript does not say the error was seen.
5. **Policy and the flow tools.** The config's `BeforeToolCall` sees
   `flow_start`, `flow_ask` and `flow_finish`, so a policy that asks by
   default asks about them. A host must allow them.
6. **Cost and eval tools** see `agentflow/` responses with no usage.
   They should skip them, not report an unpriced model.
7. **Gates.** A `check` script that rejects a model's result before the
   step finishes.
