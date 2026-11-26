# RFC 0002: nested workflows and child sessions

Status: sections 1 to 8 are **spiked** for calls in the current
context; children (fresh calls, sections 9 to 12) are proposed and not
built. It covers nested flows and children: what
a nested call records, how invocations are keyed, how failures and
resumes cross levels, pinning, bounds, the record format, fresh steps as
child sessions, child settings, kept children and the judge. Where it
differs from RFC 0001, this RFC is the proposal. Syntax is proposed
with each part and is not settled.

## The principle, applied

Everything a workflow does at run time is a call with arguments and a
result, made in the context it operates in. The same hooks, policy,
recording, replay and fronts handle it as handle any tool call. Nothing
is kept on the side.

| What happens | The call that records it | Whose it is |
|---|---|---|
| an invocation starts | `flow_start` | its key, in the call ID |
| a program step | the step's tool, or `run_script` | the frame's key, in the call ID |
| a call to a workflow | `flow_call` | the calling frame's key, in the call ID |
| a model step in context | `flow_finish`, made by the model | its `run` argument |
| a model step in a child | `flow_call`, which the child answers | the calling frame's key |
| a question | `flow_ask` | the key, in the call ID |
| a verdict | `flow_judge` | its `run` argument |
| an invocation ends | `flow_end` | the key, in the call ID |

A call being a tool call does not make it a model's to make. The program
decides control flow. The gate refuses every `flow_` call and
`run_script` from a model, as it does now.

Nothing in agentturn, agentskill or the session format changes. What
this design uses already exists:

- agentturn: deferral and `ApproveWith`. A fresh child's call is held
  the way `flow_ask` is held now.
- agentturn/session: `Recorder.StartChild` and `ContinueChild`, which
  write a subsession link carrying a host-chosen spawn ID.
- agentsession: `SpawnedSessionID(parent, spawnID)`, which derives the
  child session's ID from the parent's ID and the spawn ID alone.

## 1. Calling a workflow (spiked)

In Go a node sets `Flow`, and `Graph.Link(flows...)` resolves the calls
of a graph and of what it calls, and checks each contract. `Apply`
refuses a graph that calls and is not linked. `workflow.LoadWith(skill,
find)` loads a skill with every workflow it can reach and links them;
`workflow.Load` is that with no other skills to find. A script of a
workflow of another skill than the run's root names its skill in
`ScriptCall.Skill`, and `script.Skills` gives each skill's files.

A program node that names `flow` calls that workflow:

````markdown
```mermaid
flowchart TD
  gather[[gather]] --> review[[review]]
  review -->|clean| report[[report]]
  review -->|findings| fix[fix]
  fix --> review
```

## review
```yaml
flow: pr-review
with:
  pr: ${{ inputs.pr }}
  diff: ${{ steps.gather.outputs.diff }}
```
````

- `flow` takes the place of `run` and of `tool`/`args`. A node with
  `flow` has no `outputs`: its data is whatever the callee outputs.
- The shape is `[[ ]]`, because the program makes the call. Mermaid's
  `[[ ]]` is the subroutine shape.
- `flow` names one of two things:
  - **another workflow skill**, by its name (`flow: pr-review`). The host
    finds it the way it finds any skill (`agentskill.Catalog`).
  - **a workflow file in the same skill**, by a path starting `./`
    (`flow: ./flows/triage.md`). The file has the format of a
    `WORKFLOW.md` and is taken from the snapshot the skill was loaded
    from, so the skill's digest already covers it. Its workflow is named
    `<skill>/<path without .md>`, here `pr-review/flows/triage`.
    - Its scripts are paths within the skill, as in the skill's own
      `WORKFLOW.md`.
    - It can be called only from within its skill. A skill's surface is
      its marked workflow.
- Loading a workflow also loads every workflow it can reach. A missing
  or invalid one is a load error that names the chain of calls:
  `take-issue → review → pr-reviw: no such workflow`.
- Workflow names leave call IDs ([section 4](#4-invocations-and-keys)),
  so a name may hold `/`.
- Flows nest to any depth up to the bound in
  [section 7](#7-bounds-across-a-tree). A workflow may call itself,
  directly or through others. When the callee ends, the caller goes on
  from the calling step, by the callee's outcome.

## 2. The contract, checked at load (spiked)

**A workflow's outcomes are its ends.** An edge into an end, written
`a -->|clean| clean((clean))`, ends the workflow with the end's ID as its
outcome. An outcome with no edge ends it with that outcome's own name.
Today an end's ID means nothing, so this gives an existing meaning to
end IDs, and `Format` draws one end per outcome where it now draws a
single `done`. Ends are how a reader already sees the ways out, so they
are where the contract is written.

**A workflow's outputs are by outcome,** as a step's are. Each outcome
declares what its end gives back:

```yaml
---
inputs:
  pr: {type: integer, required: true}
outputs:
  clean:
    summary: ${{ steps.score.outputs.summary }}
  findings:
    findings: ${{ steps.review.outputs.findings }}
    summary: ${{ steps.score.outputs.summary }}
---
```

- Each outcome's outputs are checked on every path into its end, as a
  step's `with` is now. A plain reference must have a value on each such
  path.
- An output's schema is the schema of what it refers to, on each path
  into the end, as the outcome taken there declares it. Paths that
  disagree, or a member that declares no schema, leave the output untyped
  but present.
- The flat form of RFC 0001 is still read: it gives the same outputs for
  every outcome. It is the only form when the workflow has a single
  outcome. A map whose keys are all outcomes of the workflow is read by
  outcome.

**`failed` is an outcome of every call.** A callee that fails gives the
caller the outcome `failed`, with the data `{reason, workflow, step}`.
The caller routes on it like any other outcome. With no edge for
`failed`, the calling step fails instead, and raises its retry question
([section 5](#5-failure-at-each-level)). An end may be named `failed`:
reaching it ends the invocation `failed` with that outcome, and it
declares no outputs.

**The contract:**

| Part | The caller | Checked at load |
|---|---|---|
| inputs | `with` | every input the callee requires is given on every path into the call, and no name is unknown. A literal holds to its schema. A reference holds to the callee's schema on each path, using the schema the referenced outcome declares there |
| outcomes | the node's `outcomes`, which default to the callee's plus `failed` | the caller's list is the callee's set, with or without `failed`. An edge for any other outcome is an error |
| data | `steps.review.outputs.x` | `x` is an output of the outcome taken on each path that reaches the reference, the same rule as for a step's members. A loop that can call again with another outcome loses the first outcome's outputs |

An output's schema is spiked more simply than the paragraph above says:
it is the schema of what it refers to where every outcome of the
referenced step that declares the member agrees, and untyped otherwise,
not read path by path. Whether a member is present is read path by
path, as for any step.

A value whose schema is not known until it is made, such as the untyped
output of a script, is checked again when the call is made. A value that
breaks the schema then fails the step, as a script that breaks its
schema does now.

## 3. Context (spiked for the current context)

- **The default is the current context.** The callee's steps write to the
  caller's transcript. Its `session` model steps read that conversation
  as any step does.
- **`context: fresh`** on a call or on a model step runs the work in a
  child session.
- **Both kinds work at any depth.** Inside a child, the child's own
  context is the current one, so a nested call there runs in the child
  unless it is marked fresh.
- **A child sees what it is given.** That is the inputs its call passes
  in, the instructions its settings give it
  ([section 10](#10-a-childs-own-settings)) and its own tools. It never
  sees the parent's transcript.

## 4. Invocations and keys (spiked)

**Every invocation has a key.** The key is 10 hex digits, drawn when the
invocation starts and recorded in its `flow_start`. Call IDs are built
from the key, the node and the visit:

```
flow_<key>_start
flow_<key>_<node>_v<visit>[-a<attempt>][-retry]
flow_<key>_end
```

The longest is 49 characters. The workflow's name leaves the ID, so the
16-character cut on workflow names and its hash both go away. Ancestry
is never encoded in an ID: the start records it.

**The start names the key, the parent's key and the calling call:**

```json
{"workflow": "pr-review", "digest": "sha256:…", "format": 2,
 "run": "3fa9c2d1e0", "parent": "77b01c4a9e",
 "caller": "flow_77b01c4a9e_review_v1", "depth": 1,
 "inputs": {"pr": 812}}
```

The end records the key as `instance`, as format 1 did, with the
`outcome` reached, the `step` a failed run was at and, for a bound
across the tree, the `limit` it hit.

**Every call, result, model finish, verdict and step message belongs to
exactly one key:**

- The program's calls name the key in their IDs.
- `flow_finish` and `flow_judge` take a required `run`, allowed one
  value, as `step` is now. A finish that names another run is refused,
  and the model is told why. A finish is matched by its run and its
  step, never by the step's name alone.
- Each step message (the opening message, the reminder and "no verdict")
  ends `(run <key>, visit <n>)`, the tag format 1 already wrote, and is
  matched by key and node.
- Any other call a model makes, such as `read` or `grep`, and anything
  it says, belongs to the frame whose model step or judge was current at
  that item. Exactly one frame is current at any item, so replay knows
  which.

**The running frames form one chain,** from the root to the current
frame:

- A start whose parent is the current frame, and whose caller is that
  frame's current call, pushes a frame onto the chain.
- An end pops the frame off.
- A start of any other shape is refused by the gate. Replay reports it,
  and does not guess past it.
- An inner start does not abandon the frame that called it.

**A frame reads only its own record.** Position, visit counts,
references and outputs are each read within one key. A caller learns
what its callee did from its call's result, as it would from any step's.

**An explicit new invocation by a person abandons the whole unfinished
stack.** `flow_end` with status `abandoned` is written for each frame,
innermost first, before the new start. For a fresh child, the child's
end is written in the child's session, and the parent's held call is
refused with the reason `abandoned`.

### The record, in context

A callee's work cannot sit between a call and that call's output:

- agentturn will not go on while a call is unanswered.
- Providers refuse a request that holds an unanswered call.
- A deferral ends the run, so the callee could not run in the same
  transcript meanwhile.

So in a shared context the callee's start names the calling call in
advance, and the call and its result are written when the callee ends:

```
flow_start  flow_3fa9c2d1e0_start   {run: 3fa9…, parent: 77b0…, caller: flow_77b01c4a9e_review_v1, inputs}
  … pr-review's steps, all under 3fa9… …
flow_end    flow_3fa9c2d1e0_end     {run, status: done, outcome: clean, outputs, path}
flow_call   flow_77b01c4a9e_review_v1  {workflow: pr-review, digest, run: 3fa9…, with}
  → {"outcome": "clean", "data": {…}}
```

- The call's ID is known before the call is made, because it is a
  function of the caller's position.
- The call's arguments are resolved from the caller's record, which does
  not move while the callee runs. The gate refuses a `flow_call` whose
  `with` differs from what the callee's start recorded.
- The `flow_call` tool returns the callee's ending, read from the record.
  A callee that failed gives an error, and the calling step fails
  ([section 5](#5-failure-at-each-level)).

### The record, through a child

A fresh call goes the way `flow_ask` goes now:

```
parent  flow_call  flow_77b01c4a9e_review_v1  {workflow, digest, run: 3fa9…, with, context: fresh}
        decision   hold by policy             (the run ends input_required)
        link       subsession, spawn_id 3fa9c2d1e0
child   flow_start … steps … flow_end         (session SpawnedSessionID(parent, "3fa9c2d1e0"))
parent  decision   proceed by policy, with the result in the arguments
        output     {"outcome": "clean", "data": {…}, "usage": {…}}
```

The parent holds the call, the result and the link. The spawn ID is the
child's key, so the child's session can be found from the parent's
session alone.

### Reading it back as a tree

`Runs(record, open)` replaces `Instances`, and returns `[]*Instance`.
It returns each invocation with:

- its key, parent, caller, workflow and digest;
- the session, for a child;
- the span of items it covers, its path, status and state;
- its children.

`open` reads a child's session by ID. Without `open`, a child appears as
a link with the status its call's result gives. Children are not built,
so `open` is unused.

## 5. Failure at each level (spiked for the current context)

| Event | In the frame where it happens | In each frame above |
|---|---|---|
| a step fails | the retry question, in its own key. `retry` runs it again; `stop`, or nobody to ask, ends the frame `failed` | the call ends with the outcome `failed`. With an edge for it, the caller goes on there. Without one, the calling step has failed and raises its own retry question; `retry` calls the callee again under a new key. Nobody to ask: that frame fails too, with the reason chained (`review: pr-review failed at gather: …`) |
| a visit bound | the frame ends `failed` | as above |
| a bound across the tree (depth, step entries) | the frame that would cross it ends `failed` | every frame fails without a retry question, since a retry would cross the bound again. Item 21 turns this into a request for more allowance |
| abort (context cancelled, `Agent.Abort`) | nothing is written; the frame is still running | the same. A child's run is aborted, and its parent's call stays held |
| a person's new invocation | `abandoned`, innermost first | `abandoned` |
| a pending question | the run ends `input_required` on that frame's `flow_ask` | in context, nothing more. Through a child, the parent is already `input_required` on its held `flow_call` |
| resume | replay gives the chain, and the current frame continues | a held `flow_call` names its child's key. `Drive` reopens the child from the store, continues it, and when it ends answers the parent's call. This recurses through each child |

A failed inner flow never ends the outer one silently. Each level writes
its own end with its own reason. Only a bound across the tree skips the
`failed` edge, as it skips the retry question.

**Questions at depth:**

- `Question` gains `run`, `workflow` and `path`, the root-to-frame list
  of workflow, run and step, so a front can say where the question comes
  from.
- An answer is checked against that frame's pinned definition. The root
  graph, linked, holds them all, so `Graph.Answer` stays.
- A host names a judged step of a called workflow to `Delegate` as
  `<workflow>:<step>`.
- `Drive` returns pending calls at any depth, each with its path, and
  resumes the agent the answer belongs to.
- A policy deferral of an ordinary tool call inside a child surfaces the
  same way.
- While a child waits, the parent takes no prompt, since agentturn
  refuses one while a call is pending. A front sends the person's words
  to the child that is waiting on them.

## 6. Pinning definitions (spiked)

- Loading resolves the closure: the root and every workflow it can reach.
  The root's `flow_start` carries `definitions`, each workflow's
  definition and digest by name. The digest recorded for the root covers
  the whole closure: the sorted names, each with its own digest. So
  approving a root approves everything it can call.
- An in-context start carries the callee's name and digest. Its
  definition is read from the root's pins, and a start naming a digest
  that is not pinned is refused.
- A fresh child's start carries the closure as pinned at the root, so
  the child's session reads on its own and cannot drift from its parent.
- Naming ends gives existing end IDs a meaning, so a workflow with an end
  named other than its outcome, such as `take-issue`'s `done`, has a new
  digest: a run of it in progress under format 1 does not resume.
- **A change to a run in progress:** a change to any workflow in the
  closure makes resuming fail with `ErrChanged`, naming the workflow that
  changed. This holds whether or not the run has reached that workflow
  yet. Running the pinned copy instead is not possible, because the
  record holds definitions but not scripts. Reading back a run is
  unaffected, since it uses the pins.

## 7. Bounds across a tree (spiked: depth, and counting entries)

- **Depth.** The root has depth 0, and each call adds 1, in context or
  fresh. The default bound is 8. It can be set with `max_depth` in the
  root's frontmatter or with the host option `MaxDepth`; the smaller
  wins. A call that would go deeper fails the calling step, as a
  tree-wide limit ([section 5](#5-failure-at-each-level)).
- **Steps.** Every step entry, which is a node made current, counting
  attempts, counts once toward the tree's total. A calling step's own
  entry counts as well.
  - In context, replay counts every frame's entries.
  - A fresh call passes the remaining `allowance` in its arguments. The
    child counts against it, its end records the entries it used, and
    the caller adds those.
  - The default bound and its syntax belong to item 18. The count is
    `State.Entries`.

## 8. Record format 2 (spiked)

`RecordFormat` becomes 2. The changes:

- call IDs;
- the `run` argument of `flow_finish` and `flow_judge`;
- the wording of step messages;
- the start's `run`, `parent`, `caller`, `depth` and `definitions`;
- the end's `run` and `outcome`;
- `flow_call`.

**Older sessions:**

- A format-1 run is read, and resumed, as a root frame with no children:
  - its key is its instance;
  - its IDs are `flow_<workflow>_<instance>_…`;
  - its finishes carry no `run` and belong to it;
  - its messages end `(run X, visit N)`.
- A frame carries its format, and only ID parsing and the matching of
  finishes and messages depend on it.
- A transcript holding runs of both formats reads both. Every new run is
  format 2.
- A format-1 reader that meets format 2 says so in `Problem`, as it does
  for any unknown format now.

## 9. A fresh model step is a child

`context: fresh` on a model step becomes a fresh call of a one-step
workflow made from that step, with the step's prompt, tools, outcomes
and schemas. It is pinned in the closure. Its session reads like any
run: start, opening message, the model's work, finish, end. The narrowed
view of the parent's transcript goes, and with it `since`, `ownInput`
and their special cases.

| Concern | As a child |
|---|---|
| inputs | the step's `with`, resolved in the caller and carried by the child's start. The child's instructions show them, as now |
| results | the child's `flow_finish` gives the outcome and data, the child's end records them, and the parent's `flow_call` returns them. The parent routes on them as on any step |
| request hashes | each request of the child's is the child's whole transcript, with no narrowing, so every request keeps a verifiable hash, fold or no fold. Today a fresh step's request after a fold is unverifiable |
| compaction | the child has its own `Transform`, from the host's opener. The parent's compaction never sees the child's turns. `SummaryFilter` serves both |
| cost | recorded on the child's responses. agentsession's `DelegatedWork` already adds child sessions up by link. The parent's result carries `usage` (requests, input and output tokens) for the child's subtree, so a summary needs no other session |
| cancellation | cancelling the parent's context aborts the child's run and writes nothing. A resume continues the child |
| questions inside | a model step asks nothing, but a policy may hold one of its tool calls. The child's run then ends `input_required` and `Drive` surfaces the hold with its path ([section 5](#5-failure-at-each-level)) |
| resuming | from the store: the held call carries the child's key, and the key is the spawn ID |

**The host's opener:**

```go
// Child is what a fresh call needs made.
type Child struct {
	Run, Caller      string // the child's key, which is its spawn ID, and the calling call
	Workflow, Step   string // Step is set for a fresh model step
	Kept             string // the name of a kept child (section 11), or empty
	Settings         ChildSettings
}

type ChildSettings struct {
	Dir, Model, Instructions string
	Tools                    []string
}

// Children makes and reopens the agents that fresh calls run in. open
// starts the child with Recorder.StartChild(ctx, c.Run), or continues
// it with ContinueChild, builds its tools for c.Settings.Dir and passes
// its config through apply, which makes it follow the workflow.
func Children(open func(ctx context.Context, c Child,
	apply func(agentturn.Config) (agentturn.Config, error),
) (*agentturn.Agent, error)) Option
```

`Apply` refuses a graph with a fresh step or a fresh call when the host
gave no `Children`. agentflow cannot record a child itself, because the
root package does not import agentturn/session.

## 10. A child's own settings

These keys are allowed only with `context: fresh`. Item 14 opens `model`
to every model step.

```yaml
context: fresh
dir: services/api              # relative to the workspace root
model: claude-haiku-5-5
tools: [read, grep, edit]
instructions: prompts/api.md   # a file in the skill
```

- **`dir`** is the child's working directory.
  - It must be relative and clean, with no `..`; this is checked at load.
    Whether it exists is checked when the call is made, and a missing
    one fails the step.
  - agentworkspace offers no sub-workspace, so agentflow adds
    `script.Sub(ws, dir)`, a workspace whose root, file system and
    commands are confined to `dir`.
  - The host's opener builds the child's tools on it. The AGENTS.md chain
    for `dir` is the host's to add (`agentsmd.Chain`), as it would be
    for any agent started there.
- **`model`** names the child's model. The opener maps the name to a
  model. A host option `Models(names...)` lets `Apply` check the names at
  load.
- **`tools`** keeps its present meaning. `Apply` checks the names
  against the tools the host offers.
- **`instructions`** is a file in the skill, taken into the snapshot and
  the digest, and added to the child's instructions.

## 11. Keeping one child

```yaml
context: fresh
child: reviewer
```

- Model steps of one invocation that name the same `child` share one
  child session, across visits and across steps. The name is scoped to
  the invocation, so a recursive invocation gets its own.
- The first call starts the child. Its key is the spawn ID, and the
  call's arguments carry `child: reviewer` and `spawn`. Later calls name
  the same spawn and continue the session at its leaf.
- Each visit is a frame of its own in the child: its own key, start, end
  and opening message. So the child reads as a sequence of runs, and its
  model sees its own earlier visits.
- At load, steps that share a child must agree on `dir`, `model` and
  `instructions`. Their `tools` may differ, since each request offers
  its own step's tools.
- A fresh call to a workflow does not take `child`, because a workflow
  run is a session of its own. That can be opened up if a workflow needs
  it.

## 12. The judge becomes a child

Approved.

Today a judge's request is the parent's transcript narrowed to the
node's opening message. This causes four problems:

- what it looks at is hidden from later steps by spans of omissions
  (`unseen`);
- its request after a fold is unverifiable;
- a summarizer reads its look at files as the assistant's;
- a front shows its turn as it happens.

As a child:

- The program makes `flow_judge` in the parent, under the frame's key,
  and the call is held while the judge runs.
- The judge's session holds its look at files and its verdict call.
- The parent's call is approved with the verdict.
- agentsession has a `judged_by` link relation for this.

Under `shadow`, only that one call and its result are left out of the
working model's requests, not a span of the transcript, and `unseen` goes
away. The judge's input is unchanged: the question, the `with` values,
the criteria and its tools.

The cost is a second rewrite of the judge's record and tests in the same
format change, and the trials recorded in RFC 0001 were made with the
judge inline.

## The rules, held

- **Position** is a pure function of the definitions and the record. A
  child's position is a function of its own session, and a parent's of
  its own session plus its calls' results.
- **Resuming** needs the store and the definitions. A child is found by
  `SpawnedSessionID(parent, key)`.
- **Control flow** depends on outcomes recorded in results and on
  nothing else.
- **Unknown keys** stay load errors. Each new key is checked at load
  where it can be (contract, closure, `dir`, kept-child agreement,
  models and tools where the host names them).
- **The working model** never sees a definition, criteria or a blind
  judge. Every `flow_start` stays out of its requests, a child holds no
  definition the child's model is sent, and a judge child is the judge's
  own.
- **The loop** learns nothing: children are agents a host makes, and the
  hold is an ordinary deferral.
