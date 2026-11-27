# RFC 0009: kept progress, and a workflow a model starts by its skill

Status: spiked. Two items:

- **7.** a script's progress kept in the session, where a front reads it
  back after a restart, and no model reads it;
- **8.** a model starting a workflow by asking for its skill, the way in
  RFC 0001 calls dynamic.

## 7. Kept progress

RFC 0005 made a script's progress a live update of its call, which the
loop raises as a `ToolUpdate` and agentturn/session writes nothing for.
A front that restarts, or opens a session another process ran, saw no
progress at all.

Each update is now also written to the session, as a long-running tool
writes a handle it must not lose:

- `run_script` calls `agenttool.WriteRecord` with a
  `script.ProgressRecord` (`seq`, `progress`, `total`, `message`) for each
  line it reports. The recorder a host installs as
  `agentturn.Config.ToolRecorder`, agentturn/session's
  `Recorder.RecordFunc`, writes it at once and durably as a **custom
  entry** of namespace `agentflow:progress` (`script.ProgressNS`), with
  `call_id` naming the script step's call.
- **What a front reads.** The custom entries of that namespace on the
  session's path, in order, each naming its call: `seq` counts the
  call's kept updates from 1, and the numbers and message are the line's.
  The entry's own time is when it was written.
- **What it is not.** Custom entries are out of context: agentturn/session
  rebuilds no item from them, so no request carries progress and the
  transcript holds none. Position, `with` values and the result never
  see one. The session format already has custom entries; nothing in it
  changed.
- **Bounds.** The first 256 updates of a call are kept
  (`script.MaxKeptProgress`); later ones are reported live and not kept.
  The bounds of RFC 0005 still hold before that: a line over 4 KiB is not
  reported, nor anything past the file's first 1 MiB. So a call keeps at
  most 1 MiB of progress.
- **A record that cannot be written** costs a line of progress, never the
  script its run. Without a recorder on the call's context, nothing is
  written, as before.

## 8. A workflow a model starts by its skill

RFC 0001 offers two ways in. **Explicit**: a person's invocation, or a
call from code. **Dynamic**: the model asks for a workflow skill, which
ends its turn and starts the workflow instead of returning the body.
Only the first was built.

### The tool

`workflow.SkillTool(catalog, flows, opts...)` stands in for agentskill's
`skill` tool, under the same name, in package `workflow`, the one that
imports agentskill. Its arguments are the skill tool's, `name` and
`path`, and one more, `inputs`. It returns an error for a name given to
`ModelInvokes` that is not one of `flows`, with a suggestion.
`SkillToolOptions` passes options, such as a file claim, to the catalog's
own tool; a claim is made for an ordinary skill's read and none for a
workflow's, which reads nothing.

- **An ordinary skill** is served by agentskill's own tool, as it would
  be without agentflow: the same body, file list, files, errors and
  `agentskill.Read` record. `inputs` on one is an error the model reads.
- **A workflow skill**, one of `flows`, is never read through it. Its body
  is the prose for a host without workflows, and its files hold the
  definition, a judge's criteria among it, which the working model must
  not read. Asking for one with a `path` is refused.
- **Asking for a workflow skill by name** starts it, when the host lets a
  model start it and nothing is under way:
  - `inputs` is held to the workflow's input schema, as an invocation's
    is. A bad one is an error the model reads and can correct; nothing
    starts.
  - The result says the workflow starts now (`agentflow.StartText`), and
    sets `Terminate`, so the model's turn ends there.
  - **The program invokes it.** `Drive` reads the request from the record
    (`agentflow.Requested`): the model's call, its arguments and the
    tool's result. It then sends the invocation, the developer message no
    model can write, under a trigger of kind `workflow`. So the record
    shows the model asking and the program invoking, and never passes one
    off as the other.

### What the model may start

- **The host chooses**, with `workflow.ModelInvokes(names...)`. A workflow
  from a repository is code that runs at fixed points, so being listed in
  a catalog does not make it the model's to start. With no names, a model
  starts none. A workflow not named is refused with a reason saying a
  person can start it.
- **A hidden workflow** (`Graph.Hidden`, frontmatter `hidden: true`) is
  refused whatever the host names, and `workflow.StartPrompt` leaves it
  out. It is only for other workflows to call (RFC 0008, section 4, which
  defines the key and the field).
- **Only in conversation.** A request while a run is under way is
  refused (`agentflow.UnderWay`): inside a running step the model is
  working on that step, and a new invocation would abandon the run it is
  part of. A run is under way from its start until its end is written.
  One that is done and reported, or that an operator abandoned, leaves
  the conversation to the model; after a failed run the conversation
  waits for an invocation, so no model is asked anything to ask with.
- **The order of refusals:** a file of a workflow, then a workflow the
  host did not name, then a hidden one, then a run under way, then
  inputs that do not fit. The tool reads this
  from the record: the transcript on the call's context, or the record a
  host gives with `workflow.SkillHistory`, which it needs when the
  transcript can be compacted.

### Starting a workflow other than the agent's own

An agent follows the one workflow `Apply` made its config for. When the
model starts that one, `Drive` invokes it and drives the new run. When it
starts another, `Drive` returns the run's end with `ErrStartRequested`; a
`*StartRequest` (`errors.As`) names the workflow and its inputs. The host
then applies that workflow to the agent's config (`Agent.SetConfig` with
`Apply`'s result) and calls `Invoke` with the request's inputs. A host
with no workflow applied reads the same request with `Requested`.

### How a model knows which skills are workflows

`workflow.StartPrompt(flows, opts...)` renders a block for the
instructions, beside the catalog's own `Catalog.Prompt()`:

```
<workflows>
These skills are workflows. Calling the skill tool with one's name starts it
with the inputs given, instead of returning instructions, and ends your turn.
<workflow name="pr-review" inputs='{"type":"object",...}'>Review a pull request.</workflow>
</workflows>
```

It lists only the workflows the model may start, with each one's input
schema and description, and is empty when there are none. The tool's own
description also says that a skill listed as a workflow starts rather
than being read. The others stay in the catalog as skills; asking
for one is refused with its reason.

### A request read from the record

`agentflow.Requested(t)` reads the last request nothing has met: a call
whose result is `StartText` for the workflow its `name` argument names,
with no invocation, start of a run or word from the person after it. A
host's own skill tool, built some other way, starts workflows the same
way by answering with `StartText` and ending the turn.

### Not built

- A model that starts a workflow inside a running step, as a call of the
  current run. That is a nested call, which a definition makes; a model
  does not.
- Starting from inside a child session: a child's model works on its
  step, so the request is refused there as in any running step.

## Tried live (2026-11-26)

Starting from a skill was tried live with claude-haiku-5.5 on 2026-11-26: asked to greet Ada twice, the model started `greet` with `{who: Ada, times: 2}` in 2 of 2, and asked for the hidden `ops` workflow and was refused in 2 of 2. It never needed to correct its inputs, so that path is tested only scripted. Kept progress was not tried live.
