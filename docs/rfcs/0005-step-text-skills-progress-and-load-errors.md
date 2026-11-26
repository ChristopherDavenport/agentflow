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
