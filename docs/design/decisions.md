# Decisions

What was chosen over what, and why. Each entry is a choice that looks
arbitrary without its reason. The design itself is in the other
documents; start at [overview.md](overview.md).

## The format

**No expression language and no `if:`.** A value is a literal or exactly
one `${{ }}` expression: a reference, `latest(...)` or `optional(...)`.
There are no operators and no interpolation. A decision is a step with a
named outcome, so every branch is in the diagram.

**The marker is in the skill's `metadata`, and nothing falls back to the
body.** `metadata` is the Agent Skills extension point, so the skill
stays valid; a new top-level key would not be. The marker is explicit, so
a file that happens to be named `WORKFLOW.md` never makes a skill
execute, and a catalog can tell workflows from skills without reading
files. A marker naming a missing or invalid file is an error: a host that
knows workflows must not follow the prose instead on an explicit
invocation.

**Values go into text in one place only: a person's question.** A model
step's prompt that holds an expression is a load error. A model is given
values through `with`, which its instructions show as inputs, so a prompt
is the same text on every visit.

**An output's schema is not read path by path.** It is the schema of
what it refers to where every outcome that declares the member agrees,
and untyped otherwise. Whether a member is present is still read path by
path. The simpler rule was enough for every workflow written.

**A skill grants no tools.** A skill's `allowed-tools` are not read. A
step's tools are its `tools`, under the host's policy and its own, so a
workflow cannot widen what the host allows by naming a skill.

**A judge takes no skills.** What a judge weighs is written where its
reader sees it, in the step's `criteria`. A skill would be a second,
unreviewed place for criteria, in a file that may change apart from the
workflow.

**A tool step takes no `dir`.** Its tool is the host's, built on the
host's workspace, and a step cannot move it. A script step can.

## The record

**Every call runs in a child session, not as a run nested in its
caller's transcript.** A nested run needed a second way to do everything
a child does: to start and return, to time out, to be abandoned, to count
steps, to sum up and to carry its callers' policies. It also left all of
the callee's work in the caller's conversation for good. A child keeps
one way of each, and the caller gets back only what the call returns, so
what a callee puts into its caller's context is the workflow's to choose.

**A child that shares its caller's context is a fork of the caller's
session, not a copy handed to its agent.** A request that carries items
its session never wrote cannot be hashed, and a copy has to be made again
after every restart. A fork holds what it opened with, so its requests
verify and it is rebuilt from its own session.

**What a child opened with is not its record.** Reading it as the
child's would make the caller's runs the child's, and the child's
invocation would abandon them. The host says where the inherited items
end, and position is read only past them.

**A transform needs `History`.** What a transform returns is a view, and
position read from a view depends on what the transform kept. `Apply`
refuses the pair rather than check afterwards that position survived.

**Call IDs carry a run's key, not its workflow's name or its ancestry.**
A name would need cutting and hashing to fit an ID. The start records
the workflow, the parent's key and the calling call, so ancestry is read
from there.

**A fresh step and a judge are child sessions, not narrowed views of the
caller's transcript.** A narrowed view hid the judge's look from later
steps only by omissions, made its request after a compaction
unverifiable, let a summarizer read its look at files as the assistant's,
and showed its turn to a front as it happened. A child's session reads
like any run and every request keeps its hash.

**Old record formats are refused, not carried.** No session needs
keeping, so a start of another format fails replay with `ErrChanged`, and
`Runs` reports it in `Problem`.

**A changed workflow refuses a resume; the pinned copy is not run.** The
record holds definitions but not scripts, so the pinned definition
cannot be run faithfully. Reading a run back uses the pins and is
unaffected.

**An override is a request followed by the program's call.** A request
alone changes nothing. Replay applies an override only from the result
of `flow_override`, so the request, the decision and its reason are on
the record, and a model cannot ask for one.

**A run's summary lists the runs below it flat, each naming its parent.**
A nested list would make the summary's type recursive. A reader rebuilds
the tree by `parent`.

**A run's summary is read from the record, not written into it.** A
summary in `flow_end` said again what the record already holds, and a
child's had to ride back in its call's result so the caller could write
its own. The record keeps only what a summary cannot be made without:
the time of each start and end. The rest is read from `Runs` and each
session's responses, by a package the core does not import, so asking
twice gives the same answer and a restart changes nothing. The price is
that summing a tree means reading every session of it.

**Usage is counted from the session's responses, not as requests are
served.** A count kept in the process is lost at a restart, and a second
count beside the record's can disagree with it. The responses are the
record of what was used, so a host that wants usage in a summary gives
them.

## Models

**Each visit of a model step opens with a message naming the step.**
Without it a model reads its previous `flow_finish` under a tool that now
accepts only this step's, takes that call for a mistake, and begins by
apologising for it.

**The reminder to a model that stopped is a user message, under a
`workflow` trigger.** Models heed a user message far more often than a
developer one. The trigger names its source, so neither the loop nor a
reader takes it for the person's.

**One reminder.** A model that stops again is `ErrStalled`; a person, if
there is one, is better placed to say what is wrong.

**An inherited session keeps its model.** A step or a call on another
model is fresh, and saying `context: session` with it is a load error,
not a choice. Reasoning is signed for the model that wrote it, so
another model cannot be sent it, and a conversation one model wrote is a
poor opening for another: it reads the earlier turns as its own, without
what was thought behind them. The loop can leave another model's
reasoning out item by item, but that keeps the worse half. It is decided
from the definitions alone, never from the host's config, so position
stays a function of the definitions and the record; the cost is that a
model named against a workflow with none is always another.

**`SummaryFilter` keeps the workflow's own messages out of summaries and
says whose each call was.** A summarizer cannot tell a call the program
made from one the model made. Summaries said a step was "currently
executing", that the user had asked for it, and that the assistant had
chosen a step the program ran.

**`flow_clock` and `flow_start` are left out of what a working model is
sent.** A model sent a program's `flow_clock` took it for a call of its
own.

**`exclude_tools` exists beside `deny`.** A denied tool is still
offered, and a model spends turns calling it. An excluded tool is removed
from the request.

**A model does not start a workflow inside a running step.** That would
be a nested call, which a definition makes and a model does not. A
request while a run is under way, a child's included, is refused.

## Judges

**A judge may not pick from a list or write a person's words.** A pick
and an answer in someone's words are a person's to give. A verdict's data
is held only to the step's declared schema, never to the list the
question recorded, so a verdict that wrote either would be a model
speaking in a person's place.

**`max_returns` rests on no judgment of the judge's.** A judge that
decides and sends work back sends it to a model that read the same
inputs, so no person sees it. In trials a judge never chose to hand a
question back on its own. After `max_returns` such verdicts running, the
next goes to the person whatever the judge says.

**A judge is told not to quote what it weighs in its reason.** Its
reason reaches the working model, and its criteria must not.

**A bound is not a judge's to raise, and neither is a retry.** Both ask
whether to spend more, which is the person's or the host's rules'.

## Limits

**A bound asks before it fails.** A bound is a guess made before the
run, and the person watching knows whether this loop is getting
somewhere. Failing would throw the run away to learn what one answer
could say. Unattended nothing changes, and a host that wants a bound to
fail at once answers `stop` by rule.

**A result with no data is not counted as a repeat.** It says nothing to
compare: a poll that answers only "not yet", a check that prints only
`fail`. Only the visit bound holds such a loop.

**A bound across the tree skips the `failed` edge and the retry
question.** A retry, or a route that calls again, would cross the same
bound.

**A paced visit is held to no bound.** A loop that waits on a person at
every pass cannot run away, and asking that person at the sixth pass
whether to allow more is noise. Only an answer a person gave paces, so
rules, judges and defaults cannot loop without a bound.

**A policy has no `ask`.** A workflow's question about a tool call would
be a second way to ask a person, with no step to hold it. A policy only
refuses; the host's own hook may still ask.

**A cleanup can never prevent the abort.** It has no model step, no
call, no judge and no loop, a failed cleanup step is not asked about, and
the end is written whatever the cleanup came to. An abandon that could be
held up by its own cleanup would not be one.

**An abandoned or overridden child still counts.** What it spent is
added to the caller's tree however the call was answered, so a bound
cannot be dodged by overriding and the count is the same after a resume.

**A workflow's outputs by outcome have a key of their own.** `outputs`
is the same for every outcome and `outputs_by` is for each. One key read
either way by the shape of its values took an output named for an
outcome, holding a mapping, for a table of outcomes, and loaded it
without a word.
