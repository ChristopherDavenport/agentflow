# RFC 0007: showing part of a result

Status: spiked. A script or tool step can hand code a large structured
result and show a model only part of it. Builds on RFC 0001's Data and
Position sections.

## The problem

A script's whole result lands in the transcript a `session` step reads.
A step that gathers hundreds of rows for later program steps puts all
of them in front of every later model turn, and into every summary of
the conversation. `context: fresh` keeps a step from reading it, but
also from reading anything else of the conversation.

## `show`

````markdown
## gather
```yaml
run: scripts/gather.sh
outputs:
  ok:
    summary: {type: string, required: true}
    rows: {type: array, required: true}
show: [summary]          # or: show: none
```
````

`show`, on a script or tool step, names the members of its result's
data that a working model sees. `show: none` shows the outcome alone.
Without `show`, a model sees the whole result, as before.

- **The record keeps everything.** The step's call and its whole result
  are recorded as any step's are. Every step's `with` reads the whole
  data, so a later program step, and a model step that names the rows in
  its `with`, are handed all of them.
- **A model is sent a view.** Right after the result, before any turn a
  model could take, the program calls `flow_shown` with the run, the
  step, the visit and `of`, the call it shows. Its result is the outcome
  and only the members named, those the data has. A working model's
  requests leave out the call it shows, and that call's result, as
  omissions that keep the request hash, as `flow_start` is left out
  (RFC 0001, Leaving, and reading it back). They keep `flow_shown`.
- **No summary takes in the whole result.** The host's `Transform` is
  given what a model may read, so a compactor never sees it.
  `SummaryFilter` also leaves out any call a `flow_shown` shows, with its
  result, and notes the shown part as the workflow's, so a host without
  `History` summarises none of it either. In the one program turn
  between a result and its `flow_shown`, the host's transform is given
  the view with the whole result left out, so the folds already in force
  still apply and the request rebuilds from the record, while a fold
  made then cannot take the result in. Without `History` it is not run
  then. A live trial found that turn's request unhashed when the
  transform was skipped after a fold.
- **A fresh child is unaffected.** It is sent what its call passes in,
  and its own steps' `show` holds in its own session.

What `with` hands a model step is in its instructions, and that is not
narrowed: naming the rows in `with` is asking for them.

## Checked at load

- `show` is a script or tool step's. On a model step, a person step or
  a call it is an error: a model step's result is its own, and a call's
  is its callee's outputs.
- Each member must be declared by some outcome of the step's `outputs`,
  and a misspelt one suggests the intended name. Naming a member twice is
  an error.
- A step that names members must declare its outputs: without a schema
  nothing holds what is shown to what the step makes. `show: none` needs
  no schema.
- `show: []`, or a value that is neither `none` nor a list of names, is a
  parse error.

## Position and resume

Position is still read from the record. A step finishes on its result,
as before; `flow_shown` moves nothing on and is a view of a result the
record already holds. Replay keeps the results still to be shown, and
while one is, the program's next turn is its `flow_shown`, before any
other call, a run's end and a step's question included. A process
killed between the result and its `flow_shown` resumes with the result
still kept from the model: the program shows it first, and nothing runs
again.

## What a front shows

The record holds both. A front can show the whole result where it shows
a program step's call, and mark `flow_shown` as what the model was sent
of it, so a person sees what the model did not.

## Limits

- A result that fails its step, one that breaks its schema say, is not
  a step's result and is not shown in part: the retry question quotes it,
  and a model reads that as it does now.
- A session rebuilt from its omissions after a resume has lost the
  whole result from its transcript, as it has lost `flow_start`. With
  `History` that costs nothing: position and every `with` are read from
  the record. Without it, the transcript is the record, and a session
  rebuilt that way is refused as RFC 0001 says of any omission.

## Tried live (2026-11-26)

Tried live with claude-haiku-5.5 on 2026-11-26: over 300 rows the model answered from the shown summary in 2 of 2 and no request held a row; with compaction the showing turn's request was unhashed until the fix above, then every response hashed in 2 runs of 13 and 14 folds.
