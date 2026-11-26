# RFC 0008: contracts and step settings

Status: spiked where a section says so. It covers a step's policy taken
from data, a question whose text is computed, input defaults, timeouts
on calls and for a whole workflow, what a callee may declare of itself,
and tools excluded from a step. Each is held to the rules of RFC 0001
and RFC 0002: a call with arguments and a result in its context;
position a pure function of the definitions and the record; resuming
needs only the store and the definitions; control flow depends only on
the definition and recorded results; unknown keys are load errors.

## 1. Values from the record in a step's text and its policy

### 1a. A policy that takes its paths from the record (spiked)

```yaml
policy:
  allow:
    - {rule: write, path: "${{ steps.plan.outputs.files }}", note: the plan's files}
  deny:
    - {rule: bash, command: "${{ steps.guard.outputs.forbidden }}"}
```

- A rule entry may name a tool alone and take what it matches from the
  record: `path`, or `command`, one reference to a string or a list of
  them. It stands for one rule per value, `write(src/a.go)`,
  `write(src/b.go)`, in agentpolicy's grammar. A rule from data with a
  specifier of its own, with both keys, or with a literal in place of the
  reference, is a load error; a fixed path is written `write(src/a.go)`.
- The reference is checked at load as a `with` value is: it must have a
  value on every path into the step, and where its schema is known it
  must be a string or a list of strings. The tool is checked as any
  rule's is, against the step's tools and, at `Apply`, against the host's
  matchers, with a stand-in specifier.
- It is read from the record when the step works, the step's own from its
  run's record and a call's from the calling run's, and it still only
  narrows: it is one more policy every call must pass, before the host's
  own hook. A path must be clean and below the workspace's root, as rules
  are written there in a child too; a command must be one the grammar can
  hold.
- The step's instructions list the rules as they resolved, so the request,
  and its record, holds the effective limit. A refusal names the resolved
  rule.
- A value that does not resolve fails the step, which asks to be retried,
  and no model works under it:
  - a model step in the current context: the program calls `flow_limits`
    before the model's first turn, whose error is the step's failure;
  - a fresh step or a fresh call: the gate refuses the call into the
    child, as it refuses a `dir` that is no path, so no child is opened;
  - a call in the current context: the gate refuses the callee's start.
- A fresh step's child gets its caller's policies already resolved, as
  its call's record made them, so the child's own record says what it was
  held to. A judge's rules are fixed: one that takes a value from the
  record is a load error.
- `flow_limits` is the program's call, under the step's key
  (`…_v<n>-limits`), with the run, the step and the reason; its result is
  the error, and the step's retry question follows it. It holds no
  definition, so it is not left out of what a model reads: a model that
  works later in the same context sees that the program could not hold
  the step to its policy, as it sees any program call.

Tried, with scripted models: a value that is a number, an unclean path
and a path out of the root each record exactly one `flow_limits` and the
retry question after it, with no model request; a jump back to the plan
resolves the policy and the step goes on; and a run killed with the
failure pending resumes from a jsonl store with it still pending.

### 1b. A question whose text is computed (spiked)

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
- Each expression is checked at load as a `with` value is: a plain
  reference must have a value on every path into the step, `latest` must
  have one of its references, and every name must be one some outcome or
  input declares, with a suggestion for a misspelt step.
- When the question is put, each is filled in from the record: a string
  as it is, an `optional` with no value as nothing, and any other value as
  JSON. Values are data, under the rules `with` has: secrets are never
  data.
- The question's arguments record both: `question`, the text as the
  person is asked it, and `template`, the text as the step writes it.
  `version` is computed from the step's definition, the template among it,
  never from the rendered text, so answers to one question join across
  sessions whatever values it was filled in with. A question that names no
  values carries no `template`.
- A judge reads the rendered text. Its call carries it as `question`, which
  the gate holds to what the record renders, its child's start records it,
  and the judge's instructions give it beside the question as written. A
  judge's own prompt, which quotes the step's text, is not a template.
- A resumed run reads the question from the pending call, so the rendered
  text is what was asked, however the values would come out now.

## 2. Input defaults (spiked)

```yaml
---
inputs:
  pr:   {type: integer, required: true}
  base: {type: string, default: main}
---
```

- A `default` is a literal, held to the input's schema at load. Only a
  workflow's inputs take one: a step's outputs, or a member nested in an
  input, saying `default` is a load error.
- An input with a default is not required: `required` with `default` is a
  load error. An invocation, or a caller's `with`, may leave it out.
- A run's start records its inputs with every default it was not given
  filled in (`Graph.InputDefaults`, applied by the program as it makes
  the start), so the record says what the run was given and a resume
  reads it from there, whatever the definition says later. A caller's
  `flow_call` still records what the caller passed, and is held to the
  callee's start by applying the same defaults.
- A plain `${{ inputs.base }}` is certain on every path, as a required
  input's is, so load accepts it without `optional`.
- A caller is held at load to the callee's required inputs only: a
  defaulted one may be left out of `with`.

## 3. Timeouts on calls, and a workflow's default (spiked)

````markdown
---
timeout: 10m       # each script, tool and model step that sets none
---

## review
```yaml
flow: pr-review
timeout: 1h        # the run it calls, and all it calls, within an hour
```
````

**A workflow's default.** The frontmatter's `timeout` holds every
script, tool and model step of the workflow that sets no timeout of its
own, as if it had said it; a step's own wins. A person step keeps its
own `timeout` and `on_timeout`, which are the question's, a call keeps
its own, and a judge keeps the timeout of an unattended model step. The
order for a model step is its own, then its workflow's, then, with
nobody there, `UnattendedTimeout`. A fresh step's child is held to the
same: the workflow made for the step carries its workflow's default.

**A call's timeout.**

- Before a call with a timeout starts the run it calls, the program calls
  `flow_clock` under the calling run's key (`…_v<n>-clock`), and its
  result is the call's deadline, recorded as a model step's is. Replay
  reads the deadline from it, never from the clock.
- **In the current context**, every run the call waits on, at any depth,
  is held to it: a model step's stream and tools are cut off at the
  earlier of its own deadline and its callers', and a script's or tool's
  run at the earlier of its own timeout and theirs. When the time has
  run out, the program's next turn, whoever's turn it would have been,
  calls `flow_clock` with `expired` under the calling run's key
  (`…-timeout`). Replay reads that call as the word that the call ran
  out of time: every run the call waits on is abandoned, innermost first,
  each after its cleanup path, and each one's end records the reason
  `timeout: call "<step>" of <workflow> ran past its timeout of <d>`.
  The call is then made as any call is once its callee has ended, and its
  result reads the abandoned ending as a failure.
- **A question waiting** in a run the call waits on cannot stop it: when
  `Drive` finds the run waiting and the call's time run out, it sets the
  pending calls aside with `agentflow: timed out`, which replay does not
  read as an answer, and the program's turn follows as above.
- **In a child**, `Drive` holds the child's runs to the deadline: they run
  with a context that ends there, and a child that has not ended by then,
  or that is waiting when `Drive` comes back to it later, is abandoned,
  each run after its cleanup, in its own session. The held call is then
  approved with the failure, and with what the child spent, as any held
  call is.
- **The failure is `failed`**, as any callee's is: a call that draws an edge
  for `failed` goes on there with `{reason, workflow}` as data, and one that
  does not has failed and asks to be retried, which reads the clock again
  for a new deadline.
- The clock decides when the program takes the turn that says the time ran
  out, but what happened is the call and its result, so a resume from the
  store reads the deadline and the timeout from the record. A run resumed
  after its call's deadline has passed is timed out on its first turn.

Tried, with scripted models: a model step with no timeout of its own, and
one with a longer one, is cut off at the call's recorded deadline, in the
current context and in a fresh child; its run is abandoned after its
cleanup, and the call routes on `failed` or asks to be retried. A process
killed while that model worked, resumed past the deadline, reads the
clock once only, from the record, and asks the model nothing.

## 4. What a callee says of itself (spiked)

```yaml
---
context: fresh     # every call runs this workflow in a child of its own
hidden: true       # only a workflow that calls it runs it
---
```

- **`context: fresh`** in a workflow's frontmatter makes every call to it
  run in a child, as if the call said `context: fresh`: a workflow that
  must not read, or write into, its caller's conversation says so once,
  where it is defined. A call that says nothing runs it in a child, and
  may then take a child's settings (`dir`, `model`, `instructions`). A
  call that says `context: session` is a load error, raised by `Link`:
  the callee's word is the one that holds, and it is not overridden in
  silence. `fresh` is the only value a workflow may give; leaving the key
  out leaves it to each call.
- Which calls run fresh is known only once the workflows are linked, so a
  child's settings on a call are checked again then, as a call's outcomes
  already are. The record is unchanged: the call is an ordinary fresh
  `flow_call`, held while its child runs.
- **`hidden: true`** says the workflow is a part, run only by workflows
  that call it. `Graph.Invocation` refuses it, `Apply` refuses it as a
  root, and `workflow.SkillTool` refuses a model's request for its skill
  (RFC 0009, section 8), all reading `Graph.Hidden`. A call starts it, in its caller's
  context or in a child. A catalog that lists workflows, or offers them to
  a model, should leave hidden ones out; agentflow has no catalog of its
  own, so that is the host's.

## 5. Tools a step is not offered (spiked)

````markdown
---
exclude_tools: [bash]          # no model step of the workflow is offered it
---

## look
```yaml
tools: all                     # every tool the host offers
exclude_tools: [fetch]
```
````

- `tools: all` offers a model step every tool the host's config offers,
  except the program's own (`flow_*`, `run_script`). It replaces a list;
  both is a load error.
- `exclude_tools` on a model step, and in the frontmatter for every model
  step of the workflow, takes tools out of what the step is offered. The
  two add up: exclusion only narrows, so a step cannot take back what its
  workflow excludes.
- An excluded tool is removed from the request, so the model is never
  offered it, and a call to it anyway is refused like any tool the step
  does not allow. A policy's `deny` cannot do this: a denied tool is still
  offered, and a model spends turns calling it.
- At load, a step that both names a tool and excludes it, directly or by
  its workflow, is an error, as is a judge naming a tool its workflow
  excludes, and excluding the program's own tools.
- At `Apply`, where the config's tools are known, every excluded name must
  be one the config offers, with a suggestion for a misspelling. A config
  with a `ToolProvider` names its tools only as it runs, so it is not
  checked there.
- A fresh step's child applies the same rule: the workflow made for the
  step carries `tools: all` and the step's exclusions with its
  workflow's, and `ChildSettings` gives the opener `AllTools` and
  `ExcludeTools` to build the child's tools from.
