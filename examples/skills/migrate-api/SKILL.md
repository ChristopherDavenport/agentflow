---
name: migrate-api
description: Replace a deprecated Go function across a repository, one package at a time. A person picks the packages and reviews the result; nothing is pushed.
metadata:
  agentflow: WORKFLOW.md
---
This skill is an agentflow workflow. The file that `metadata.agentflow`
names in the frontmatter above defines it: that file says what to do,
and this text says how to read it.

If a tool you have runs agentflow workflows, start this one with it,
giving its inputs, and stop: the program takes every step from there.
Otherwise follow the file by hand, as below. Do what it defines, in the
order it defines, and nothing it does not.

## The file

- **Frontmatter:** `inputs` the workflow takes, its `outputs` at the
  end, the `documents` it keeps, and defaults for its steps.
- **One `mermaid` flowchart:** the steps, and which follows which.
- **One `##` section per step,** headed by the step's ID: a `yaml`
  block of settings, then prose.

## Following it

1. Take each input from the request. Ask the person for a required one
   you cannot find; never guess it. An input left out takes its
   `default`.
2. Start at the first node the diagram names.
3. Do the step, as its kind says below.
4. End it with one of its `outcomes`, `ok` when it lists none, and data
   that fits the schema its `outputs` gives for that outcome. Say so in
   one line: the step, its outcome and its data.
5. Take the edge for that outcome: `a --> b` on `ok`, `a -->|name| b` on
   `name`. An edge into an end, `((name))`, ends the workflow with
   `name` as its outcome; an outcome with no edge ends it with that
   outcome.
6. At the end, report the outcome and the frontmatter's `outputs`, or
   its `outputs_by` for that outcome.

A branch is an outcome and nothing else. Skip no step, add none, and
take no edge the diagram does not draw. Take one step at a time: start
a step only once the one before it has ended and you have its outcome,
never in the same command. Between steps do nothing: when a step ends,
take its edge at once.

## Kinds of step

The node's shape gives its kind.

**`id[[label]]`, a program step.** Do exactly what its settings say,
and decide nothing.

- `run: <path>` runs that script, a path within this skill's
  directory. Run the script itself, not commands of your own in its
  place. Run it in the working directory, or in `dir` below it, with
  `with` as one JSON object on stdin, `AGENTFLOW_SKILL_DIR` set to this
  skill's directory, `AGENTFLOW_OUTPUT` to a new empty file and
  `AGENTFLOW_PROGRESS` to a file it may append to. Its result is what it
  writes to that file, or its standard output if it writes nothing
  there: the step's data when the step lists no outcomes, and
  `{"outcome": ..., "data": ...}` when it does. A non-zero exit, or a
  result that is not one JSON value, fails the step.
- `tool: <name>` calls that tool with `args`, and `with` over them.
- `flow: <name>` follows the workflow of the skill of that name, or of
  the file `./<path>.md` of this skill, in the same way, with `with` as
  its inputs. Its outcome is the step's, `failed` if it fails, and its
  outputs are the step's data.

**`id[label]`, your step.** Its prose is your instructions and `with`
is what you are handed. First read the `files`, the `instructions` file
and the skills under `skills` it names.

- **Tools.** Use only the tools in `tools`, none when it lists none, and
  never one in `exclude_tools`, the frontmatter's included. Where your
  tools have other names, use the one that does what the name says and
  no more: `read` reads files, `grep` searches them, `edit` changes
  them. A shell is none of these; use it only where the step offers one,
  and then for nothing it forbids.
- **Policy.** Keep to its `policy`: `deny` forbids what it matches, and
  `allow`, if given, forbids everything else.
- **What you read.** Read nothing of this skill but what the step
  names. Its scripts and the other steps' sections are what judges your
  work, not part of it.
- **`context: fresh`.** The step works from its `with`, its files and
  its prompt alone, as if it had seen nothing before. If you can hand it
  to a separate agent given only those, do. Steps with the same `child`
  go to the same agent.
- **Stuck.** If you cannot do the step, do not make up a way round:
  end it with the outcome `stuck` and data `{"reason": ...}`. Take its
  `stuck` edge if the diagram draws one; if it draws none, the step has
  failed.

**`id([label])`, a question for the person.** Ask them its prose with
`with` beside it, and offer its outcomes, each with what it needs: the
data its `outputs` require, or the words `text` takes. `choices` are
what they pick from; `text` names the outcome that takes their own
words. Wait for the answer, and never give it yourself. `unattended` is
the answer only when nobody can be asked. A `judge` changes nothing
here: the person answers.

## Values

A setting's value is a literal or one `${{ }}`:

- `inputs.<name>`: an input.
- `steps.<id>.outputs.<member>`: that member of the data of the step's
  latest result.
- `documents.<name>`: a document's text now.
- `latest(a, b, ...)`: whichever of them was made last.
- `optional(a, ...)`: the same, and the setting is left out when none
  has a value yet.

## Documents

Each document in the frontmatter is a text kept through the run. Keep
each one as a file of its own, `<name>.md`, in a new directory outside
the working directory, and say where. It starts empty, from a file of
this skill (`from`) or from an input (`input`).

- A step reads the ones under its `documents.read`, and changes the
  ones under `documents.edit` by writing their files. A step asked to
  write into a document has not ended until it has.
- A program step's `documents.write` sets a document's file from a
  member of its result's data.
- `${{ documents.<name> }}` is the file's text: a question that names
  one shows it to the person.

## Limits

- A step runs at most `max_visits` times: if it does not say, 5 for a
  step on a loop and 1 for any other. Before passing that, or when a
  step fails, stop and ask the person whether to go on.
- A program step or your step that runs past its `timeout`, or the
  frontmatter's, has failed.
- If the person stops the workflow and the frontmatter names
  `on_abort`, follow the steps from that one to an end first.

A `description` is for people. The other settings, `model`,
`thinking`, `show`, `hidden`, `on_timeout`, `max_depth` and
`max_steps`, tell an agentflow host how to run the workflow. By hand,
leave them be.
