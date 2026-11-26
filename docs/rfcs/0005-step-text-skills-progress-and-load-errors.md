# RFC 0005: step text, skills, progress and load errors

Status: **spiked**. Each section is built and tested in this repository,
offline with scripted models. It builds on RFCs 0001 to 0004, and where
they disagree this RFC is the newer. Syntax is proposed with each part
and is not settled.

It covers four things:

- **15.** a description of a step, or a workflow, for people and fronts;
- **16.** skills a model step names, added to its instructions;
- **28.** progress from a running script;
- **33.** load errors that suggest the name that was meant.

Every rule of the earlier RFCs holds. Nothing in agentturn, agentskill or
the session format changed.

## 15. Descriptions

```yaml
---
description: Review a pull request against what was asked for.
---
```

````markdown
## gather
```yaml
description: Fetches the diff and the issue it claims to close.
run: scripts/gather.sh
```
````

A `description` may open the frontmatter and any section's settings. It
says what the workflow or the step is for, to the people who read a run
and to the fronts that show it. No model is sent it.

- **Where it lives.** In the definition: `Graph.Description` and
  `Node.Description`. The definition is pinned in `flow_start`, which no
  model is sent (RFC 0001), so the description is pinned with it and
  changes the digest like any other part of the definition.
- **Where it is not.** In no instruction, opening message, question,
  call or note. A question's arguments are read by a model working in the
  same context later, so the description stays out of them as well. A
  fresh step's and a judge's child pin their own definitions, which carry
  the step's description, and no request of theirs holds it. Neither does
  what `SummaryFilter` hands a summarizer.
- **Where a front reads it.**
  - `Graph.Question(pending)` reads a pending question with its step's
    description, from the definition its run pinned, at any depth.
    `Question.Description` is never written into the question's
    arguments.
  - `State.Node.Description` is the current step's.
  - `Graph.Describe(step)`, on an instance's pinned `Graph` from `Runs`,
    gives any step's, and `""` the workflow's.
- **A skill's own.** A skill's marked workflow that says nothing of itself
  takes the skill's `description`, so a catalog and a front say the same
  thing. A workflow file of the skill has only its own.
- **Versions.** A description is part of its step's definition, so a
  question's `version` changes with it: what the person was shown
  changed.

## 16. Skills a step names

````markdown
## fix
```yaml
tools: [read, edit]
skills: [go-style, review-checklist]
```
Fix each finding.
````

A model step may name skills. Each one's instructions are added to the
step's, after its prompt:

```
Skills for this step, to follow as you do it:
<skill name="go-style">
…the skill's instructions…
</skill>
```

- **Found at load.** `workflow.LoadWith` finds each named skill with the
  `Find` it is given, as it finds the workflow skills a step calls;
  `workflow.FromCatalog(catalog)` makes one from an `agentskill.Catalog`.
  A skill it cannot find, one whose instructions cannot be read, and one
  that says nothing are load errors. So is naming a skill with `Load`,
  which is given nothing to find skills with.
- **Pinned.** The text is `Skill.Instructions()`, its body and its list of
  files, read into the definition as `Node.Skills`. The digest therefore
  pins exactly what the model is told, and a skill that changes on disk
  changes nothing already loaded. Loading again reads the new text under
  a new digest, which a run in progress refuses, as for any change to the
  definition. `Apply` refuses a step whose skill was never read, such as
  a graph built in Go that names one and gives no text.
- **In context and in children.** The step's instructions carry the
  skills wherever it runs: in the current context, and in a fresh step's
  child, whose one-step workflow keeps the step's skills. A kept child's
  steps may name different skills, since each visit's request is its
  step's own.
- **What a skill grants: nothing.** A skill's `allowed-tools` are not
  read. The step's tools are its `tools`, under the host's policy and the
  step's own, and a workflow cannot widen what the host allows (RFC 0001,
  two keys; RFC 0003, section 17). A skill's files are listed in its
  instructions, and the step reads them only with a tool the host offers
  it, such as the host's `skill` tool.
- **Not a judge's.** A judge's instructions are the question, its inputs
  and the criteria, and nothing else, so what a judge weighs is written
  where its reader sees it, in the step's `criteria`. A skill named for a
  judge would be a second, unreviewed place for criteria, read from
  another skill that may change apart from the workflow. `judge` takes no
  `skills`, and a person step names none.
- **Only model steps.** A program step or a person step that names skills
  is a load error.

## 28. Progress from a running script

```sh
printf '{"progress": 2, "total": 5, "message": "testing"}\n' >> "$AGENTFLOW_PROGRESS"
echo 'linking' >> "$AGENTFLOW_PROGRESS"
```

A script is told, in `$AGENTFLOW_PROGRESS`, the path of a file it may
append progress to, one line for each update:

- a JSON object, `{"progress": …, "total": …, "message": …}`, any member
  left out, the numbers read as `agenttool.ProgressInfo` reads them;
- or a line of plain text, which is a message alone.

While the script runs, `run_script` reads the file every 100ms and
reports each complete line as a progress update of its call,
`agenttool.Progress` with `ProgressInfo` as the details and the message
as the text (`2/5 testing`). The loop raises each as a `ToolUpdate`, as it
does for any long-running tool, so a front shows it beside the step. A
last line with no newline is reported when the script exits.

- **It changes nothing.** The result is what the script writes to
  `$AGENTFLOW_OUTPUT` or its standard output, as before. No update reaches
  the record's items, so position, `with` values and control flow never
  see one, and replaying a run reads none.
- **The recorder keeps none.** agentturn/session writes no entry for a
  `ToolUpdate`, and `ProgressInfo` is not an `agenttool.Recordable`, so
  progress is live only: for subscribers while the call runs. Nothing in
  the session format was needed or changed. A host that wants a history
  of progress subscribes and keeps it itself.
- **Bounds.** A line over 4 KiB is not reported, nor anything past the
  file's first 1 MiB, and a malformed object is read as text. None of it
  fails the step.
- **Where it lives.** The file is `.agentflow-progress-<id>` at the
  workspace's root, beside the result file, and is removed when the call
  ends.

## 33. Load errors that name what was meant

```
workflow: node "review": line 3: unknown key "outcome"; did you mean "outcomes"?
agentflow: node "fix": edge to unknown node "gathr"; did you mean "gather"?
agentflow: node "fix": steps.reveiw.outputs.findings: no step "reveiw"; did you mean "review"?
```

Every load error that names something unknown ends, when one name is
close enough to be what was meant, with `; did you mean "x"?`. One helper
says it, `agentflow.Suggest(name, among)`, in both packages: the closest
name by edit distance, at most 2 edits and at most a third of the name's
length (1 for a short one), the first in order on a tie, and nothing when
none is that close, since a guess at something unrelated misleads.

It covers:

- **Keys.** Every key of the frontmatter and of a section's settings, at
  any depth (`judge`, `policy`, a schema field), is checked against what
  that place takes before the settings are decoded, with the line.
  `unattended`, `choices`, `text`, `on_timeout`, a policy rule and a
  schema field, which are read by hand, say the same. The new keys of
  this RFC, `description` and `skills`, are among them.
- **Nodes.** A section for no node, and an edge to an unknown node.
- **Outcomes.** An edge or end for an undeclared outcome, a judge's
  outcomes, `unattended`, `on_timeout`, `choices` and `text`, the outcomes
  of `outputs`, and a step's data.
- **References.** `steps.<id>` to no step, `inputs.<name>` to no input, and
  a member no outcome declares.
- **Workflows, files and skills.** A call to no workflow, a `./<path>.md`
  that is not in the skill, a misspelt skill name (`FromCatalog` suggests
  among the catalog's names), and a step `Delegate` names.
- **Tools and models.** A rule of a policy naming a tool its step does not
  offer, a model the host does not serve, and, new here, a tool a step
  names that the config does not offer: a program step's `tool`, and the
  `tools` of a model step working in the current context. A child's tools
  are its opener's and a config with a `ToolProvider` names its tools
  only as it runs, so neither is checked.

One check is new rather than reworded. A tool a step names that the
config does not offer was, when the step ran, left out of the model's
request without a word, or failed the program's call. It is now a load
error, since what the config offers is known at `Apply`.
