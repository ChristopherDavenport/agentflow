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
