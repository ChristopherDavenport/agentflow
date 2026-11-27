# The record, position and resuming

A workflow keeps nothing on the side. Everything it does at run time is a
call with arguments and a result, written into the session it works in.
This document says what those calls are, how position is read back from
them, what a model is and is not sent of them, and what resuming needs.

The catalogue of calls and the rules they hold to are in
[overview.md](overview.md).

## Execution on the loop

`Apply(graph, config)` returns an `agentturn.Config`. There is no second
engine: `Prompt`, `Resume`, hooks, recording and the fronts work as they
do for any config.

- `BeforeModelCall` reads where the workflow stands and shapes the
  request. A program or person turn is sent to the model name
  `agentflow/<workflow>`. A model turn gets the node's prompt and only
  the node's tools.
- A router `Model` answers `agentflow/` requests itself, by emitting the
  node's function call, and passes every other request to the real model.
- `BeforeToolCall` refuses calls the node does not allow and defers a
  person's question. The config's own hook still runs for allowed calls,
  so the host's policy applies to program steps.

The model name is set before the turn is recorded. The record therefore
shows which turns the program took, and request hashes reproduce.

## A run

A run is one invocation of a workflow. Its first call is `flow_start`
and its last is `flow_end`.

### How a run begins

- A transcript with no run starts one.
- Another begins only at an **invocation**: a developer message reading
  `agentflow: start <name>` with the inputs, built by `Graph.Invocation`.
  A model cannot write a developer message. An invocation also abandons
  an unfinished run ([limits-failure-and-intervention.md](limits-failure-and-intervention.md)).
- A call from another run starts one ([calls-and-children.md](calls-and-children.md)).
- A model may ask for one in conversation, if the host allows it
  ([host-integration.md](host-integration.md)).

After a run has finished and said so, a plain prompt is a conversation
with the model, with its tools. It is the same after an operator abandons
the run nobody called. A run that failed stays failed until an invocation.

Replay begins at the start, so whatever the transcript held before it is
ignored.

### What the start records

`flow_start`'s arguments:

| Member | What it holds |
|---|---|
| `workflow` | the workflow's name |
| `digest` | for a run nobody called, the digest of the whole closure; for a call, the callee's own |
| `inputs` | the inputs, with every default the run was not given filled in |
| `format` | the record format, `RecordFormat` |
| `definition`, `definitions` | the workflow's definition and, by name, every other workflow it can reach, each with its digest |
| `run` | the run's key |
| `parent`, `caller`, `depth` | for a called run: the calling run's key, the ID of the calling call, and how deeply it is nested |
| `max_depth` | the bound on nesting |
| `max_steps` | for a run nobody called, the bound on the tree's steps; for a child, what its caller had left |
| `context`, `path` | for a run in a child session: `session` or `fresh`, which is what the child opened with, and the way to its call from the run nobody called |
| `files`, `documents`, `question` | for a run in a child: what its call passed in |

The start's result holds the time the start was recorded, and the end's
the time the end was. Nothing is decided by either; `Runs` reads them
back as `Started` and `Ended`.

```json
{"workflow": "pr-review", "digest": "sha256:…", "format": 4,
 "run": "3fa9c2d1e0", "parent": "77b01c4a9e",
 "caller": "flow_77b01c4a9e_review_v1", "depth": 1, "context": "session",
 "inputs": {"pr": 812}}
```

### Keys and call IDs

Every run has a key: 10 hex digits, drawn when the run starts and
recorded in its start. The program's call IDs are built from the key, the
node, the visit and the attempt:

```
flow_<key>_start
flow_<key>_<node>_v<visit>[-a<attempt>][<suffix>]
flow_<key>_override<n>
flow_<key>_end
```

`-a<attempt>` is present from the second attempt on. The ID with no
suffix is the step's own call: its tool or `run_script`, its `flow_call`,
or a person step's `flow_ask`. The suffixes:

| Suffix | Call | What it records |
|---|---|---|
| `-clock` | `flow_clock` | the deadline of a model step's attempt, a call or a question |
| `-timeout` | `flow_clock` with `expired` | that the attempt or call ran past its deadline |
| `-files` | `flow_files` | the files a model step is given as it opens |
| `-limits` | `flow_limits` | that the step's policy could not be read from the record |
| `-judge` | `flow_judge` | the program's call that holds while a judge looks |
| `-shown` | `flow_shown` | the part of the step's result a model is sent |
| `-retry`, `-retry<k>` | `flow_ask` | the question whether to retry a failed attempt; `<k>` from 2 when it is put again |
| `-limit<k>` | `flow_ask` | the `k`th question whether to grant more at a bound |

A node's ID is at most 20 characters, because providers limit call IDs.
A first attempt's call is at most 39 characters with a one-digit visit,
and the longest suffix on a retry brings it to about 50. A workflow's
name is not in an ID, so a name may hold `/` and has no length cut.
Ancestry is never encoded in an ID: the start records it.

### Every item belongs to one key

- The program's calls name the key in their IDs.
- `flow_finish`, `flow_stuck`, the document calls and a judge's
  `flow_judge` take a required `run` and `step`, each allowed one value.
  A finish that names another run is refused, and the model is told why.
  A finish is matched by its run and its step, never by the step's name
  alone.
- Each step message (the opening message, the reminder and "no verdict")
  ends `(run <key>, visit <n>)`, with `, attempt <m>` on a retry, and is
  matched by key and node.
- Any other call a model makes, such as `read` or `grep`, and anything it
  says, belongs to the run whose model step was current at that item.
  A session has one run under way at any item, so replay knows which.

### One session, one record

A session holds the runs somebody started there, one under way at a
time. A run a workflow calls is in a child session of its own, so no run
is nested in another's transcript.

- **A run reads only its own record.** Position, visit counts,
  references, documents and outputs are each read within one key, from
  one session. A caller learns what its callee did from its call's
  result, as it would from any step's.
- **A start is read only for the run the session is waiting to start:**
  the workflow the session's config follows, not yet started. Any other
  `flow_start` is not a run of this session's, and replay does not guess
  past it.
- **What a child's session opened with is not its record.** A child that
  opens with its caller's view holds the caller's items ahead of its own.
  Replay reads none of them: the caller's runs, invocation and calls are
  not the child's, and the child's own start is the first thing its
  record holds. The host says which items those are with `Inherited`, and
  gives a `History` without them
  ([calls-and-children.md](calls-and-children.md)).

How a call and its child are recorded is in
[calls-and-children.md](calls-and-children.md).

## Results

Every step ends in one result:

```json
{"outcome": "findings", "data": {}, "by": "", "reason": ""}
```

`by` says who answered when it was not a person or the step itself:
`rules`, `ai`, `timeout` or `unattended default`. `reason` is why, in
the words of whoever chose: a person may give one with any answer
(`Result.Reason`), and a judge that decides must. It is for whoever reads
the record; `with` cannot refer to it.

A transcript carries no failure flag, so success has to be said. A
program or person step whose output is anything else, an error, a refusal
or an unknown outcome included, has failed. A model finishes with
`flow_finish`; a finish naming an undeclared outcome is an error the
model sees and can correct.

## Position

Position is a pure function of the definitions and the record, meaning
every item that happened, in order.

- Program steps are matched by call ID.
- Model steps are matched by a successful `flow_finish` naming the run
  and the node, made after the node became current. One model turn
  cannot finish a step it has not yet been given.
- A deadline is read from a `flow_clock` result, never from the clock.
- A result still to be shown is followed by its `flow_shown` before any
  other call, a run's end and a step's question included. `flow_shown`
  moves nothing on.

Nothing else is stored. A child's position is a function of its own
session; a parent's is a function of its own session and its calls'
results. Control flow depends on the outcomes recorded in results and on
nothing else: the host's options decide who answers a question, never
what an answer means once recorded.

## The record and what a model is sent

These are different things. A compaction shortens the model's view, and
so does an omission read back by a resumed session. The record is every
item that happened.

### Left out of every working model's request

Each is left out as an omission that keeps the request hash:

| Left out | Why |
|---|---|
| every `flow_start` and its result | it holds the definition, a judge's criteria among it, and inputs the step's `with` did not name |
| every `flow_clock` and its result | it holds only the program's deadline, and a model that reads one takes it for a call of its own |
| a `shadow` judge's `flow_judge` call and verdict | the verdict is blind |
| a result a step shows in part, with its call | its `flow_shown` stands in its place ([steps-and-data.md](steps-and-data.md)) |

Everything else the program writes is in the conversation and a model
working there later reads it: questions and answers, `flow_call` and its
result, `flow_limits`, `flow_files`, `flow_end`.

The same is left out of what a child's session opened with, by the
calls' keys, so a callee's model is sent what its caller's was. A fold in
the child that covers inherited items does not bring them back.

The record itself holds all of it. An agent that continues the session
without agentflow would be sent it.

### `History`

`History(read)` gives agentflow the record: a function returning every
item, for a session the item entries of its path. With it:

- Position, `with` values, step instructions and the gate are read from
  the record, whatever the transcript has lost. A step's inputs reach a
  model as its inputs, and nothing else is restored to the conversation
  to find them.
- The host's `Transform` is given only what a model may read. What the
  table above leaves out is gone before the transform runs, so no summary
  carries it.
- A record that does not hold the calls of the transcript it is read
  beside is refused with `ErrRecordBehind`. A recorder that has stopped,
  or the wrong session, does not rewind a run. `Drive` takes the same
  option.

Without `History` the transcript is the record, so nothing may drop from
it: `Apply` refuses a config with a `Transform`, a compaction among them,
that gives no `History`.

A fresh step and a judge work in child sessions, each with the transform
and the `History` its host's opener gives it.

### Compaction

- With `agentturn/compact`, every request keeps its hash, a child's
  included, because each child is sent the whole of its own session. The
  recorder must be told of folds: `compact.WithOnFold(rec.Fold)`.
- `SummaryFilter`, given to the compactor with `compact.WithFilter`,
  shapes only what the summarizer reads. The request keeps its items and
  its hash. It:
  - keeps the invocation, the opening messages and the reminders out of
    what is summarised, so no summary speaks of a step as under way or
    takes the workflow's words for the person's;
  - hands the summarizer each call the working model did not make, the
    program's and `flow_judge`, with its result, as a developer message
    saying whose it was, with reasons quoted;
  - leaves out any call a `flow_shown` shows, with its result, and notes
    the shown part as the workflow's.
- A host that builds its compactor without the filter gets none of that:
  agentflow is handed only the compactor's `Transform` and cannot add it.
- A judge's own look at files and the workflow's closing report read to a
  summarizer as the assistant's.
- In the one program turn between a partly shown result and its
  `flow_shown`, the host's transform is given the view with the whole
  result left out. The folds already in force still apply and the request
  rebuilds from the record, while a fold made then cannot take the result
  in.
- No description reaches a summarizer, and compaction loses no document
  text or file: both are read from the record.

## How a run ends

A run's end is written, not inferred. Only the program may call
`flow_end`. Its arguments:

| Member | What it holds |
|---|---|
| `instance` | the run's key |
| `status` | `done`, `failed` or `abandoned` |
| `outcome` | the end a done run reached |
| `reason` | why a run failed or was abandoned |
| `path` | each step taken: `step`, `visit`, `outcome`, `by` |
| `outputs` | a done run's outputs |
| `step` | the step a failed run was at |
| `limit` | the bound across the tree a run failed on, so every run it is nested in fails with it |
| `cleanup` | how an abandoned run's cleanup went: `done`, `failed at <step>: <why>`, or `cut short: abandoned again`; empty with no cleanup path |

### Statuses

| Status | When |
|---|---|
| `running` | no `flow_end`: the run is waiting, stalled, interrupted, or left by a host that never prompted again. A run stopped by a model error or a turn limit has not left the workflow |
| `done` | the last step finished; the report follows |
| `failed` | a step failed with nobody to retry it; a person chose `stop`; a bound was reached with nobody to grant more (`limit` names a tree-wide one); or the outputs could not be made. The run then ends in that error |
| `abandoned` | the run was set aside before it finished |

A run is abandoned by any of these, each after its cleanup path if it has
one:

| Cause | `reason` |
|---|---|
| a person's invocation over an unfinished run | empty |
| the run that called a child telling it `agentflow: abandon` | what the caller said: empty, or one of the two below |
| an operator's override | `overridden: retry`, `overridden: jump` or `overridden: abandon` |
| the timeout of a call the run was working under | `timeout: call "<step>" of <workflow> ran past its timeout of <d>` |

The rules for each are in
[limits-failure-and-intervention.md](limits-failure-and-intervention.md).
A cleanup's calls are under the abandoned run's key, after the
invocation, so a run's span reaches past an invocation to its end.

### The summary

A run's summary is not in the record. It is read from it afterwards,
with the `summary` package, so it comes out the same whenever it is
asked for, a restart or not:

```go
runs, err := agentflow.Runs(record, open)
s := summary.Of(runs[0], summary.Options{Responses: responses, Price: price})
```

```json
{"started": "2026-11-26T10:00:00Z", "ended": "2026-11-26T10:04:12Z",
 "steps": 9,
 "usage": {"requests": 6, "input_tokens": 52000, "output_tokens": 4100, "cost": 0.21},
 "calls": [{"workflow": "pr-review", "run": "77b01c4a9e", "parent": "3fa9c2d1e0",
            "status": "done", "outcome": "clean", "read": true, "steps": 5,
            "usage": {"requests": 4, "input_tokens": 30000, "output_tokens": 2600, "cost": 0.12}}]}
```

- **Time** is the run's `Started` and `Ended`, as the results of its
  start and its end hold them. A run under way has no end.
- **Steps** are the steps the run entered itself (`State.Entered`),
  attempts, jumps and person-paced visits included, and those of every
  run it called.
- **Usage** is what the run's model requests used, and the requests of
  every run it called. The program's own turns, sent to
  `agentflow/<workflow>`, use nothing and are not counted
  (`IsProgramModel`).
- **Calls** lists every run below it, at any depth, each before the runs
  it called and with the key of the run that called it (`parent`). The
  list is flat, and a reader rebuilds the tree by `parent`. Each entry's
  totals take in those below it. A child set aside is listed as
  `abandoned`, with its steps.
- **Cost** is the caller's to price, with `Options.Price(model, usage)
  (cost, ok)`. Requests of a model with no price are counted, left out of
  `cost`, and their models listed in `unpriced`. With no `Price`, every
  model is unpriced.

Usage is not in the record's items: it is on each session's response
entries, which agentflow does not read on its own.

- **`Options.Responses(spawn)`** returns a session's responses: the model,
  what each used, and `At`, how many items of that session's record come
  before the response's own. The empty spawn is the session the run was
  read from, and any other names a child's, as the `open` given to `Runs`
  names it. The record is the transcript `Runs` was given for that
  session, so a forked child's is counted from its own first item.
- **A response is a run's** when it falls inside the run's span: after
  its `Start` and before its `End`, or before its `Stop` while it has no
  end. A kept child's session holds a run for each visit, and each
  counts its own.
- **Without `Responses`** no usage is counted, and `usage.partial` says
  so. Times, steps and calls are whole. A summary is never padded or
  guessed.
- **A run is read from its own session** when `Runs` was given an `open`
  (`read` is true). One that was not is known only from its call's
  result: its steps are what the result says its tree spent of the
  tree's steps, which leaves out person-paced visits; the runs below it
  are not listed; and no usage is counted for it.
- A run is partial if a run it called is: `partial` keeps the first
  reason it meets.

## Reading a session back

`Runs(transcript, open)`, given every item of a session's path, returns
the runs nobody called there, in the order they started, as
`[]*Instance`. Each has:

- `Workflow`, `ID` (the key), `Digest`, `Inputs`, and `Graph`, the
  definition it ran;
- `Parent`, `Caller` and `Depth`, empty for a run nobody called;
- `Spawn`, for a run in a child session, and `Fresh`, for one whose
  session began with nothing of its caller's;
- `Start` and `End`, the items that called its `flow_start` and its
  `flow_end`, `End` being -1 while the run has none, and `Stop`, the
  first item past its span: the session's next run's start, or the
  record's length. They index the transcript the run was read from. A
  run known only from its call's result has `Start` and `End` -1;
- `Started` and `Ended`, when its start and its end were written, zero
  where the record has none;
- `Status`, `Outcome`, `Reason` and `State`;
- `Children`, the runs it called;
- `Problem`.

`Runs` needs neither the current workflow nor a graph. It reads the
recorded definition, checks it against the digest, and replays. A replay
that disagrees with the recorded ending, an unknown format, or a
definition that does not match its digest is reported in `Problem`, not
guessed past.

`open` reads a child's session by spawn ID. With it, a run called into a
child is read from the child's session. Without it, the child is listed
with what its call's result gives: its status, with a child set aside
read as `abandoned`, and in `State.Entries` the steps its tree spent. A child's session read on its
own lists its runs as runs nobody called there, each naming its caller.

`Instance.At(transcript, i)` gives the run's state after any item.
`Graph.Describe(step)` on an instance's `Graph` gives a step's
description, and with `""` the workflow's.

A run nobody called reads, once its end is written, as its tree stood
then, whatever starts after it.

## Resuming

Resuming needs the store and the definitions. Nothing else was kept.

- **A session reloaded by agentturn/session.** Leaving an item out of a
  request is recorded as an omission, and `agentturn/session` reads an
  omission in force as a trim: `Transcript` and `AgentOptions` rebuild
  the conversation without those items. With `History`, a session loaded
  that way resumes where it stood, because the record still holds
  everything. Without `History`, a session stopped inside a model step
  cannot be resumed and is refused.
- **The model's view is not restored.** After a resume, a `session` step
  reads the conversation as the session rebuilt it, without what was
  omitted. Nothing is put back into it: the record cannot say whether an
  omission was a one-time one or a block that must stand. `Attach` also
  quarantines by content, so a kept item that reads like an omitted one
  is held back too. The workflow's own messages carry the run and visit
  for that reason.
- **No script runs twice.** A step whose result is in the record is not
  run again, and a model step's inputs are taken from the record.
- **A process killed during a model turn** is continued, and the workflow
  finishes.
- **A process killed during a script** leaves its call pending, and the
  script may have run. A tool that says replay is safe is approved and
  the workflow goes on. One that does not cannot be approved; answering
  that the outcome is unknown fails the step, which asks whether to
  retry.
- **A process killed between a result and its `flow_shown`** resumes with
  the result still kept from the model. The program shows it first, and
  nothing runs again.
- **A deadline that passed while the process was down** is read from the
  record: the run is timed out on its first turn.
- **A held call** names its child's key, so `Drive` reopens the child
  from the store, continues it, and answers the parent's call when it
  ends ([calls-and-children.md](calls-and-children.md)).

### Pinned definitions

- Loading resolves the closure: the root and every workflow it can reach.
  The root's start carries each one's definition and digest by name. The
  digest recorded for the root covers the whole closure: the sorted
  names, each with its own digest. Approving a root approves everything
  it can call.
- A child's start carries its own definition and those it can reach, as
  pinned at the root, so the child's session reads on its own.
- **A change to any workflow in the closure makes resuming fail with
  `ErrChanged`,** naming the workflow that changed, whether or not the
  run has reached that workflow yet. The pinned copy cannot be run
  instead, because the record holds definitions but not scripts. Reading
  a run back is unaffected, since it uses the pins.

### Record format

`RecordFormat` is 4. It versions what a workflow writes into a transcript
and how replay reads it: replay's meaning is part of the format, and a
change to it changes the number. A start of any other format is refused
with `ErrChanged`, and `Runs` reports it in `Problem`.

## What the record holds for later analysis

No extractor of decisions is built
([open-questions.md](open-questions.md)). What one would read is held in
the record now, and tests keep it there. One row for each `flow_ask` that
is not a retry:

| Column | Where in the session |
|---|---|
| workflow, definition | the run's `flow_start`: `workflow`, `digest` |
| run, step, visit | the call ID |
| step's version | the question: `version` (this node's definition alone) |
| mode | the question: `mode`, absent for no judge |
| question, inputs shown | the question: `question`, `template`, `with` |
| answer, reason, who | the output: `outcome`, `data`, `reason`, `by` (absent for a person) |
| asked, answered | timestamps of the `hold` and `proceed` decisions |
| verdict, its reason | the output of the program's `flow_judge` call; under `advise`, also the question's `advice` |
| judge's model, criteria, looks, cost | the judge's child session: its start, responses and item entries |
| blind | mode `shadow` |
| unanswered | a question with no output: kept, not dropped |

Which rows measure a judge is in
[questions-and-judges.md](questions-and-judges.md).

Not in the record, and for the workflow or host to supply:

- **The repository's state**, which judging the same case again needs. A
  step must put the commit, and any diff, in what it records.
- **What happened next**: merged, reverted, reopened. A workflow records
  what to join on, such as the pull request's URL.
- **Which person answered**: the record says `human`.
- **The skill's files.** The start holds the definition, criteria among
  it, and a digest of the scripts, not the scripts themselves.
- **Durability and privacy.** Sessions are only as durable as the host's
  store, and hold whatever the inputs held, so sharing them needs
  redaction.
