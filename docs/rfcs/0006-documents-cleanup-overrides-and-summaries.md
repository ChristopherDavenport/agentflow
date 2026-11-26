# RFC 0006: documents, cleanup, overrides and summaries

Status: **spiked**, section by section as each is built, offline with
scripted models. It builds on RFCs 0001 to 0005, and where they disagree
this RFC is the newer. Syntax is proposed with each part and is not
settled.

It covers:

- **29.** named markdown documents a model step reads and edits;
- **30.** a cleanup path that runs when a run is abandoned;
- **31.** operator overrides: retry, jump with a note, abandon;
- **32.** a summary written when a run ends;
- **34.** files given to a model step as context.

Every rule of the earlier RFCs holds. Nothing in agentturn, agentskill or
the session format changed.

## 29. Documents

A workflow declares its documents in its frontmatter. Each starts empty,
from a file of the skill, or as one of the workflow's inputs:

```yaml
---
inputs:
  brief: {type: string}
documents:
  plan: {from: templates/plan.md}
  brief: {input: brief}
  notes:
---
```

A step says which it works with:

````markdown
## draft
```yaml
documents:
  read: [brief, notes]
  edit: [plan]
```
Draft the plan.

## load
```yaml
run: scripts/load.sh
documents:
  write: {notes: text}
```
````

- **A model step** lists what it may `read`, and what it may `edit`, which
  includes reading. It is given `flow_doc_read` and, with something to
  edit, `flow_doc_write`. Each takes the step's run and name, as
  `flow_finish` does, and a document name the request's schema holds to
  the step's list. A write carries the document's whole new text.
- **A script or tool step** may `write` documents from members of its
  result's data, by document. A member that is not text fails the step.
- **Program and person steps** read a document's text with
  `${{ documents.<name> }}`, a reference like any other. A document always
  has a text, empty at first, so the reference is certain everywhere. For
  `latest`, a write is newer than the steps finished before it.
- **A judge** reads no documents. A question shows one by naming it in
  `with`.

**A document's text is a pure function of the record.** Its first text
is in the definition, which the run's start pins: loading reads a `from`
file into it, so the digest covers it. After that, each change is a
call's result that holds the whole text: the model's write, or the
program step's result. A read's result holds the text too, so what the
model was shown is in the record. Replay keeps the text by run, and a
frame reads only its own documents.

**Into a call, and back.** A workflow called in context gets a document's
text as an input through `with`, and may start a document of its own from
it (`input:`). It gives text back as an output, which the caller writes
from its result. A fresh model step's call passes the texts of the
documents the step reads into its child, the child's start records them,
and the child's model reads and writes them by calls in its own session.
The child's result carries back the texts of those the step edits, which
the caller takes as its documents. The gate holds the passed texts to
what the caller's record says.

**Compaction** loses no text: the text is read from the record, never
from what a model was sent. A model whose earlier read was folded away
reads again.

**Policy** does not reach documents, which are not files and which no
host tool touches. The step's `read` and `edit` lists are its limits, the
step's instructions state them, and a call outside them is refused with
the list it broke.

**Bounds.** A document's text is at most 1 MiB, its starting text
included; a longer write is refused, never cut.

Checked at load, with the name that was meant: an undeclared document in
a list, a `write` or a reference; `read` or `edit` on anything but a
model step, and `write` on anything but a script or tool step; a `from`
that is not a clean path in the skill, or not in it; an `input` that is
not an input of the workflow, or not text; and a written member that no
outcome declares, or that is not text.

## 30. Cleanup on abort

A workflow may name, in its frontmatter, the step a cleanup path starts
at:

````markdown
---
on_abort: undo
---
```mermaid
flowchart TD
  work[[work]] --> review[review]
  undo[[undo]] --> tell([tell])
```
````

The path is the steps reachable from `on_abort`. It runs when the run is
**abandoned**, before the run's end is written: by a person's new
invocation, by `agentflow: abandon` from the run that called a child, or
by an operator's abandon ([section 31](#31-operator-overrides)). A
cancelled context writes nothing, so it abandons nothing and runs no
cleanup: the run is still under way, and resumes.

**It can never prevent the abort.**

- Its steps are script, tool and person steps only. It calls no
  workflow, has no model step and no judge, and has no loop. A cleanup is
  bounded by its length, so no bound is checked on it, and it never asks
  for more allowance.
- Nothing on the workflow's own path reaches it, and it reaches nothing
  on that path. Where it ends, the cleanup is over; its ends are no
  outcomes of the workflow.
- A cleanup step that fails is not asked about: the cleanup ends there.
- Whatever the cleanup comes to, the end is written next: `abandoned`,
  with `cleanup` saying `done`, `failed at <step>: <why>`, or
  `cut short: abandoned again`.

**Across a stack, innermost first.** An abandon reaches every run under
way (or, from an operator, the runs out to the one it names). The
innermost runs its cleanup under its own key and writes its end; then the
run that called it does the same; and so on out. Only then does the run
asked for start. A run that had already failed or finished writes its end
as it stands, with no cleanup.

**Through children.** A run in a child session is abandoned in its own
session, as before: its caller's held call is refused, and the child is
told `agentflow: abandon`. The child's runs clean up there, innermost
first. A cleanup question in a child is not put to anybody: the person
is in the session the abandon came from, so the question is refused, the
child is told again, and the cleanup ends there.

**A person step in cleanup.**

- **Attended:** it is asked and waits, like any question, and the run's
  end waits with it. A person who wants out answers it, or abandons
  again: the host refuses the pending question first, which ends the
  cleanup.
- **Unattended:** it takes its `unattended` outcome. Without one, the
  question cannot be answered, which fails the step, which ends the
  cleanup.

A cleanup step's `with` reads only what is certain wherever a run may be
abandoned: the inputs and the documents. A step of the workflow's path is
read with `optional` or `latest`, since it may not have run.

Checked at load: `on_abort` names a step (with the name that was meant);
no step of the cleanup is on the workflow's path; no cleanup step is a
model step, a call or a judged question; and the cleanup has no loop.

**Record.** The cleanup's calls are under the abandoned run's key, after
the invocation, so `Runs` reads a run's span as reaching past an
invocation to its end; the next run's span starts at its own start.

## 31. Operator overrides

An operator can change where a run stands in three ways:

| Action | What it does |
|---|---|
| `retry` | runs the current step of the run again, as a new attempt |
| `jump` | goes to the step it names, as a new visit, with a note |
| `abandon` | abandons the run and every run it called, innermost first, each with its cleanup ([section 30](#30-cleanup-on-abort)) |

```go
end, st, err := agentflow.OverrideRun(ctx, g, agent, agentflow.Override{
	Run: "3fa9c2d1e0", Action: agentflow.OverrideJump, Step: "fix",
	Note: "Fix only the null check; the rest is fine.",
})
```

**The mechanism: a request, then the program's call.** The host's
request reaches the program as a developer message,
`agentflow: override {...}` (`OverrideRequest`), as an invocation does,
since no model can write one. The program's next turn is a call,
`flow_override`, under the key of the run under way, with the request as
its arguments; its result says it was applied, or why not. Replay applies
an override only from that result. So the request, the decision and its
reason are all on the record, a model cannot ask for an override, and the
gate refuses a `flow_override` no request is waiting on. A request alone
changes nothing: position is still read from recorded results.

**Which run.** An override names a run by its key.

- `retry` and `jump` change the innermost run under way, the only one at
  a step of its own; on a run that waits on one it called they are
  refused, naming that one.
- `abandon` may name any run under way. Its caller, if any, is not
  abandoned: the call ends `abandoned`, which fails the calling step, and
  the caller asks whether to retry it, or routes on `failed`.
  Abandoning the run nobody called leaves nothing to start until the
  workflow is invoked again.
- A run in a child session is reached through the call that waits on it:
  `OverrideRun` opens the child and asks there, at any depth.

**Setting the run still.** Calls pending when the override is asked for
are set aside, the children they wait on abandoned as for an invocation.
A question set aside so is not answered: if the override is refused it is
put again under a new ID; if applied, the override decides what comes
next.

**Counts and bounds.** A retry is a new attempt and a jump a new visit;
both count as step entries of the tree, and both are held to every bound.
A jump into a step at its visit bound asks for more, as item 21's bounds
do. A jump goes only where the step's inputs are on the record: to a step
whose `with` cannot be resolved there, it is refused with the reference
that is missing. A jump never goes to a cleanup step.

**The note** reaches the step the override brought the run to, until it
finishes: a model step reads it in its opening message, before the tag
that names the visit; a fresh step's child in its instructions; a person
in the question's `note`. A program step records it and is not told.

Refusals are results, recorded with the reason: no such run under way, a
run that waits on another, no such step (with the name that was meant), a
step that cannot run from here, or runs already being abandoned.
