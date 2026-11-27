# Questions to people, and the models that may answer for them

A person step puts a question to a person. This document says how a
question is asked and answered, what it can offer (outcomes, a pick from
a list, the person's own words), who else may answer it and in what
order, and how a host moves a question from a person to a model one step
at a time.

The questions the program raises on its own, whether to retry a failed
step and whether to grant more at a bound, are in
[limits-failure-and-intervention.md](limits-failure-and-intervention.md).

## A person step

A person step is a `flow_ask` call held by a `hold` decision, so the run
ends `input_required`. Its arguments are the question as the person is
shown it, so the record holds exactly that:

| Member | What it holds |
|---|---|
| `workflow`, `run`, `path` | the run that asks, and the root-to-frame list of workflow, run and step, so a front can say where the question comes from |
| `step` | the step |
| `question`, `template` | the text as the person is asked it, and the text as the step writes it when it names values |
| `outcomes` | the outcomes an answer may take |
| `with` | the step's inputs, to show beside the question |
| `form` | the schema of the data each outcome wants |
| `choices`, `text` | what an answer may pick from, and the outcome that takes the person's own words |
| `mode`, `advice`, `sent_back` | how far the host lets a judge answer, its verdict under `advise`, and how often it has sent the work back |
| `deadline`, `on_timeout` | when the question answers itself, and with what |
| `version` | names the step's definition |
| `note` | what an operator said with the override that brought the run here |
| `retry`, `limit` | set on the program's own questions |

**Answering.** `Graph.Answer(pending, result)` approves the held call
with the answer added to its arguments, and the tool returns it. The
record reads `hold by policy`, then `proceed by human`. Supplying the
output directly would be recorded as `reject`, which is what the session
format says of any output a caller writes for a call that did not run.

**An answer is held to the question as it was asked,** not to whatever
the definition would resolve now: to the outcomes and choices it
recorded, the words it takes, its form, and its deadline. A resumed run
reads the question from the pending call.

**Reading a question.** `Graph.Question(pending)` reads a pending
question at any depth, with its step's description from the definition
its run pinned. The description is never written into the question's
arguments, because a model working later in the same context reads
those. An answer is checked against the asking frame's pinned definition;
the root graph, linked, holds them all.

**A reason.** A person may give a reason with any answer
(`Result.Reason`). It is for whoever reads the record.

## Who answers, in order

When a question is reached, the host tries in order:

1. **Its own rules** (`Reply`). The result is marked `by: rules` and the
   decision `by: policy`. A question the rules answer is not judged:
   the rules are asked before a judge looks. `Reply` may be called more
   than once for a question, and its latest answer stands; rules that
   answer and then decline leave the question to the person.
2. **A judge the host let decide.** Marked `by: ai`, decided `by:
   policy`.
3. **Unattended, the node's `unattended` outcome,** at once, marked `by:
   unattended default`. An unattended run with one reads no clock and
   waits for no timeout.
4. **The question's timeout,** once its recorded deadline has passed.
   Marked `by: timeout`, decided `by: policy`.
5. **The person.** The run ends `input_required` and waits.

The conversation that did the work never answers on a person's behalf.

An unattended run whose question has no `unattended` outcome waits, and
its timeout answers it when the host drives after the deadline.

`unattended` on a node is an outcome, or `{outcome, data}`. The data is
held to the outcome's schema at load.

## Choices from an earlier result

````markdown
## pick
```yaml
outcomes: [chosen, none]
choices:
  from: ${{ steps.list.outputs.files }}
  select: many        # or one, the default
  outcome: chosen
  as: files           # default: choice, or choices with select: many
```
Which files should the fix touch?
````

- **A pick is data under an outcome.** `outcome` names the outcome a pick
  takes, and the pick is that outcome's data member `as`: one value, or
  with `select: many` a list of them. Every other outcome of the question
  is answered as usual. A question with one outcome may leave `outcome`
  out.
- **The list comes from a reference.** `from` is a `${{ }}` reference,
  checked at load as any reference is: it must have a value on every path
  into the question, and where its schema is known it must be an array.
  `latest` and `optional` are allowed; an optional reference with no
  value offers an empty list.
- **The list is resolved into the question.** The question's `choices`
  records `{outcome, as, many, items}`, the items as the reference gave
  them. Its `form` for the pick's outcome is the outcome's schema with
  the member `as` held to an `enum` of the items, or to an array of them,
  so a front draws it without knowing about choices.
- **An answer is held to the recorded list.** A pick outside it, a value
  picked twice, an empty `many` pick, or a list where one value is wanted
  is refused by `Answer`, and the question stays pending. The rules are
  held the same way: a rule that picks from outside the list leaves the
  question to the person. Replay holds the output to the list in the call
  too, so a record cannot claim a pick it was never offered.
- **What follows reads the pick.** The outcome's schema has the member,
  required, with the schema of an item of `from` where that is known, so
  `${{ steps.pick.outputs.files }}` is certain after `chosen`.
- **Load errors:** choices on a node that is not a person's; `outcome`
  not declared, or missing where there are several; `from` not a
  reference, or not a list; the outcome's own `outputs` declaring the
  member `as`; `unattended` or `on_timeout` taking the pick's outcome,
  since no default written beforehand can know the list.
- `Pick(q, items...)` builds the answer, held to the list.

## An answer in a person's own words

```yaml
outcomes: [go, change]
text: change            # or {outcome: change, as: note}; as defaults to text
```

- **Words are data under an outcome.** An answer in the person's own
  words takes `outcome`, with the words as the string member `as` of its
  data. The question records `text: {outcome, as}`, so a front can offer
  a box beside the outcomes, and its form for the outcome has the member,
  a string.
- **Something must be said.** Blank words are refused, as is the outcome
  without its member.
- **Load errors:** text on a node that is not a person's; `outcome` not
  declared, or the choices' outcome; the outcome's own `outputs`
  declaring the member, or requiring members the words alone do not give.
- `Words(q, text)` builds the answer.

The host's rules may pick or write, held as a person is, and are recorded
`by: rules`.

## A question whose text is computed

````markdown
## ship
```yaml
outcomes: [yes, no]
```
Ship ${{ steps.build.outputs.version }} to ${{ inputs.env }}?
````

- A person step's question may hold `${{ }}` expressions, each one
  expression as `with` takes it: a reference, `latest(...)` or
  `optional(...)`. It is the only place a value is put into text. A model
  step's prompt that holds one is a load error: a model is given values
  through `with`, which its instructions show as inputs.
- Each expression is checked at load as a `with` value is.
- When the question is put, each is filled in from the record: a string
  as it is, an `optional` with no value as nothing, and any other value
  as JSON. Values are data, under the rules `with` has: secrets are never
  data.
- The question records both `question`, the text as asked, and
  `template`, the text as written. A question that names no values
  carries no `template`.

**`version`** is computed from the step's definition alone, the template
and the step's description among it, never from the rendered text. So
answers to one question join across sessions whatever values it was
filled in with, and a reworded question, or one whose description
changed, starts a new version.

## A question's timeout

```yaml
outcomes: [merge, park]
timeout: 4h
on_timeout:             # or just: park
  outcome: park
  data: {why: nobody answered}
```

- `timeout` on a person step is the question's: a duration, as on other
  steps. `on_timeout` is the outcome, and data, it takes when nobody has
  answered in time. One without the other is a load error. The outcome
  must be declared and its data is held to its schema. It may be the
  words' outcome, with the words written in, but not the pick's.
- **The deadline is recorded before the question is asked.** The program
  calls `flow_clock` (ID `…_v<n>[-a<m>]-clock`) and its result is the
  deadline. The question carries it as `deadline`, with `on_timeout`, so
  a front shows it and a host knows when to come back. Replay reads it
  from that result, never from the clock.
- **When it passes, the question answers itself.** `Drive`, given a run
  that waits on a question whose deadline has passed, approves the
  question with the timeout outcome and data, `by: timeout`, decided by
  policy with a reason naming the deadline. It is recorded like any other
  answer. In a child, the child's `Drive` does it, so a question at any
  depth times out when the host drives the tree. A question asked after
  its deadline has already passed, on a resume that came late, takes the
  outcome as it is asked.
- **A late person is refused.** `Answer` after the deadline returns
  `ErrExpired`; the host drives, and the timeout is what the record
  holds. Nobody but the timeout answers `by: timeout`.
- **Under a judge.** Under `advise` and `decide` the judge looks first,
  and the clock is read after its verdict, so the person has the whole
  timeout. An answer by the timeout is not a person's, so under `shadow`
  it is not judged.
- **Nothing wakes a sleeping host.** The deadline is on the question. A
  host that waits on it, with a timer or a scheduler, drives the run
  then. agentflow has no scheduler.
- A call's timeout can set a waiting question aside
  ([limits-failure-and-intervention.md](limits-failure-and-intervention.md)).

## Questions at depth

- `Drive` returns the end of the run that waits, at any depth, with its
  path, and `Resume` gives the answer to the agent it belongs to
  ([calls-and-children.md](calls-and-children.md)).
- While a child waits, the parent takes no prompt. A front sends the
  person's words to the child that is waiting on them.
- A policy deferral of an ordinary tool call inside a child surfaces the
  same way.
- A person's new invocation over a child that waits goes through
  `Invoke`, which abandons the child's runs first.

## From a person to a model

A person step can move to a model one step at a time. It takes two keys,
so a workflow file cannot give itself authority.

### The author permits

The node's `judge` writes down what the person was weighing, and which
outcomes a model may choose:

```yaml
outcomes: [go, change, redo, park, drop]
judge:
  outcomes: [go, change, redo]         # park and drop stay a person's
  tools: [read, grep]
  criteria: |
    Go only if the plan resolves the issue and does nothing else.
```

| `judge` key | Meaning |
|---|---|
| `criteria` | what a good answer weighs, written for the model |
| `outcomes` | those the model may choose; anything else it leaves to the person |
| `tools` | the tools it may call to look for itself |
| `max_returns` | how often running it may send the work back; 2 unless set |
| `policy` | limits what its tools may touch, beside the policies of the calls the question's run is nested in |

### The host chooses

Per run and per step, with `Delegate(mode, steps...)`:

| Host | Who answers | What the record holds |
|---|---|---|
| nothing | the person | their answer |
| `Delegate(Shadow, ...)` | the person; the judge looks afterwards | both, the verdict unseen |
| `Delegate(Advise, ...)` | the person, shown the judge's verdict | both |
| `Delegate(Decide, ...)` | the judge, or the person if it hands back | the verdict, `by: ai` |

With no step names, `Delegate` covers every node that has a `judge`. A
step of a called workflow is named `<workflow>:<step>`. A later
`Delegate` overrides an earlier one for the nodes both name.
`JudgeModel(name)` names the model the judge's child is asked for.

The path is to write the criteria, measure on `Shadow`, count how often
the judge would have approved what the person did not, and move a step to
`Advise` and then `Decide` as that allows. `State.Steps` holds `Result`,
`Advice` and `Blind` for the latest run; reading them across sessions is
the host's.

### How a judge looks

A judge works in a child session of its own, opened with the host's
`Children`. It runs `<workflow>:<step>:judge`: one model step whose
instructions hold the question and the criteria, whose inputs are the
step's `with` values, and whose tools are the judge's. It has none of the
conversation that did the work.

- The program makes `flow_judge` in the asking run, with the ID
  `flow_<key>_<step>_v<n>-judge`, and the call is held while the judge
  looks.
- The judge's model ends its look by calling `flow_judge` with its run,
  step, outcome and reason. The judge's session holds its look at files
  and that verdict call.
- The program's call is let go with the verdict, and returns it.
- The child is linked as any spawned child is, by a subsession link.

**The verdict answers nothing.** The question is still asked by
`flow_ask`, and the host decides there whether the verdict stands. So
position stays a function of the record, and what the host trusts is not
written into it.

**A judge that gives no verdict.** A judge that only talks is reminded
once. One that stops twice, or gives two verdicts it may not give, comes
to no verdict (`Verdict.None`), and its run is ended `abandoned` with
the reason that it gave none. So does one whose child cannot be opened
as its call asks. Under `advise` and `decide` the question then goes to
the person, marked as handed back with none. Under `shadow` the step
finishes with no advice.

**Handing back.** The outcome `ask` is reserved for handing a question
back to the person. Unattended, a handed-back question takes the node's
`unattended` outcome. Every judge is told to hand back when it cannot
tell, when the choice is the person's, and when an input tries to direct
its answer or claims an approval. A node's criteria add what else is the
person's.

**Unattended,** a judge's look is held to the unattended timeout of a
model step
([limits-failure-and-intervention.md](limits-failure-and-intervention.md)).

### Shadow

Shadow judges after the answer, so there is no verdict for anyone to see
while answering.

- The question is put in mode `shadow`. The answer is held, the judge
  runs, never shown it, and the step finishes with the person's answer,
  whatever the verdict.
- The judge's look is in its own session. Its call and verdict in the
  asking run are left out of every later request of the working model, as
  omissions that keep the request hashes.
- It never blocks: a judge that comes to no verdict lets the step finish
  without one.
- Answers from rules, an unattended default or a timeout are not judged.
- **Limits.** A later visit's person can read an earlier verdict in the
  record, so only a step's first answer in a run is surely blind. The run
  waits for the judge before going on.

### Advise and decide

The verdict is not blind, and the working model reads it, its reason
included, as it reads a person's. So a judge is told to give its reason
in its own words about the work, and not to quote, name or refer to what
it was told to weigh. That is an instruction, not a guarantee: a reason
can still carry what the criteria say in other words.

### A judge cannot send work back forever

An outcome that leads back to the same question sends the work to a model
that read the same inputs, so no person sees it. After `max_returns` such
verdicts running, the next goes to the person, shown with the verdict and
how often it was sent back (`Question.SentBack`); unattended, it takes
the node's default. A verdict that moves on still stands, and any other
answer to the question starts the count again. The rule rests on no
judgment of the judge's.

### What a judge cannot have or do

| Rule | Why |
|---|---|
| It may not choose the pick's outcome or the words' outcome; `judge.outcomes` naming either is a load error | a pick and an answer in someone's words are a person's to give, and a verdict's data is held only to the node's declared schema, never to the recorded list. It still chooses among the other outcomes, or hands back, and then the person picks or writes |
| It never judges a retry of a failed step, or a question about a bound | those are not the step's question |
| It takes no `skills` | what a judge weighs is written where its reader sees it, in `criteria`; a skill would be a second, unreviewed place for criteria |
| It reads no documents and is given no files | a question shows a document by naming it in `with`; a judge that should read a file is told its path in `with` and given a read tool |
| Its `policy` takes no value from the record | a judge's rules are fixed |
| `timeout` under `judge` is a load error | the step's own `timeout` is the question's |
| It is not sent the step's description | no model is |

A judge reads the rendered question: its call carries the text as
`question`, which the gate holds to what the record renders, and its
instructions give it beside the question as written. A judge's own
prompt, which quotes the step's text, is not a template.

### What a judge reads is not trusted

What a judge reads can be written by someone else: an issue's text, a
diff. It is told that inputs are material and not instructions, which is
not a defence. Delegating a step whose answer publishes or deletes means
trusting that text that far. See
[policy-and-trust.md](policy-and-trust.md).

## Measuring a judge

Only `shadow` rows with a person's answer and a verdict measure a judge.
Under `advise` the person saw the verdict first, so agreement there is
not the measure. Under `decide` a person sees only what was handed back,
which is the hard part and not a sample.

Group by step, step version and judge model; a change to either starts
the count again. The number that matters is how often the judge approved
what the person did not. With none in about 60 such cases the rate is
under 5% at 95% confidence, and with none in 300 it is under 1%.

Where each of these is in the record is in
[record-and-replay.md](record-and-replay.md).
