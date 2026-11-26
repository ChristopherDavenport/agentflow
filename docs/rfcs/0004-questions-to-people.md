# RFC 0004: questions to people

Status: **spiked**. Every section is built and tested in this
repository, offline with scripted models and scripted answers. No front
draws any of it yet. It builds on RFCs 0001 to 0003, and where they
disagree this RFC is the newer. Syntax is proposed with each part and is
not settled.

It covers three things a person step can ask for:

- **24.** choices computed from an earlier result, picking one or several;
- **25.** an answer in the person's own words that takes a named outcome;
- **26.** a timeout, after which the question takes a named outcome.

Every rule of the earlier RFCs holds. The question is a `flow_ask` call,
and what it offers is in its arguments, so the record holds exactly what
the person was shown. An answer is held to that record, not to whatever
the definition would resolve now. The clock matters only through a
`flow_clock` result the record holds. Nothing in agentturn, agentskill or
the session format changed.

Presentation hints (item 23) and review comments that persist (item 27)
wait for a front, and are not here.

## 24. Choices from an earlier result

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
  with `select: many` a list of them. Every other outcome of the
  question is answered as before. A question with one outcome may leave
  `outcome` out.
- **The list comes from a reference.** `from` is a `${{ }}` reference,
  checked at load as any reference is: it must have a value on every
  path into the question, and where its schema is known it must be an
  array. `latest` and `optional` are allowed; an optional reference with
  no value offers an empty list.
- **The list is resolved into the question.** The question's `choices`
  records `{outcome, as, many, items}`, the items as the reference gave
  them. Its `form` for the pick's outcome is the outcome's schema with
  the member `as` held to an `enum` of the items, or to an array of them,
  so a front draws it without knowing about choices.
- **An answer is held to the recorded list.** A pick outside it, a value
  picked twice, an empty `many` pick, or a list where one is wanted is
  refused by `Answer`, and the question stays pending. The rules
  (`Reply`) are held to it the same way: a rule that picks from outside
  the list leaves the question to the person. Replay holds the output to
  the list in the call too, so a record cannot claim a pick it was never
  offered.
- **What follows reads the pick.** The outcome's schema has the member,
  required, with the schema of an item of `from` where that is known,
  so `${{ steps.pick.outputs.files }}` is certain after `chosen`.
- **Load errors:** choices on a node that is not a person's; `outcome`
  not declared, or missing where there are several; `from` not a
  reference, or not a list; the outcome's own `outputs` declaring the
  member `as`; `unattended` or `on_timeout` taking the pick's outcome,
  since no default written beforehand can know the list.
- **Helpers:** `Pick(q, items...)` builds the answer, held to the list.

## 25. An answer in a person's own words

```yaml
outcomes: [go, change]
text: change            # or {outcome: change, as: note}; as defaults to text
```

- **Words are data under an outcome.** An answer in the person's own
  words takes `outcome`, with the words as the string member `as` of its
  data. The question records `text: {outcome, as}`, so a front can offer
  a box beside the outcomes, and its form for the outcome has the
  member, a string.
- **Something must be said.** Blank words are refused, as is the outcome
  without its member.
- **Load errors:** text on a node that is not a person's; `outcome` not
  declared, or the choices' outcome; the outcome's own `outputs`
  declaring the member, or requiring members the words alone do not
  give.
- **Helpers:** `Words(q, text)` builds the answer.

## Choices, words and the judge

A judge may not choose the pick's outcome or the words' outcome. Its
`judge.outcomes` naming either is a load error. A pick from a list and
an answer in someone's words are a person's to give: a verdict that
wrote either would be a model speaking in a person's place, and the
verdict's data is held only to the node's declared schema, never to the
list the question recorded. A judge still chooses among the question's
other outcomes, and hands back as before; then the person picks or
writes. The host's rules may pick or write, held as a person is, and are
recorded `by: rules`.

## 26. A question's timeout

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
  must be declared and its data is held to its schema, as `unattended`
  data is; it may be the words' outcome, with the words written in, but
  not the pick's.
- **The deadline is recorded before the question is asked.** The
  program calls `flow_clock` (ID `…_v<n>[-a<m>]-clock`), as for a model
  step, and its result is the deadline. The question carries it as
  `deadline`, with `on_timeout`, so a front shows it and a host knows
  when to come back. Replay reads it from that result, never from the
  clock.
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
- **Who answers, in order:**
  1. the host's rules (`Reply`);
  2. a judge the host let decide;
  3. unattended, the node's `unattended` outcome, at once: an unattended
     run with one reads no clock and waits for no timeout;
  4. the timeout, once its deadline has passed;
  5. the person, until then.
  An unattended run whose question has no `unattended` outcome waits as
  before, and its timeout answers it when the host drives after the
  deadline.
- **Under a judge:** under `advise` and `decide` the judge looks first,
  and the clock is read after its verdict, so the person has the whole
  timeout. An answer by the timeout is not a person's, so under `shadow`
  it is not judged, as answers by rules and the unattended default are
  not.
- **Nothing wakes a sleeping host.** The deadline is on the question. A
  host that waits on it, a timer or a scheduler, drives the run then;
  agentflow has no scheduler (RFC 0001, non-goals).

## Open questions

- A front that shows the question needs to say how long is left; the
  deadline is absolute, in UTC, and the front's clock may differ.
- Whether a pick may also carry a person's words, as a reason beside it,
  is left to `Result.Reason`, which every answer may carry.
