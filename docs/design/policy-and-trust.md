# Policy and trust

What a workflow may limit, what it can never grant itself, and where
trust ends. The settings named here are listed in
[workflow-format.md](workflow-format.md).

## Two keys

A workflow file cannot give itself authority. Anything that widens what
runs takes two keys, the author's in the definition and the host's in its
config:

| What | The author's key | The host's key |
|---|---|---|
| a model answers a person's question | the step's `judge` | `Delegate(mode, steps...)` |
| a model starts a workflow | the workflow is not `hidden` | `workflow.ModelInvokes(names...)` |
| a tool call goes through | the step offers the tool, and no policy of the workflow refuses it | the host's own `BeforeToolCall` |

A workflow's own settings only narrow: a policy refuses calls, an
exclusion removes tools, and neither allows what the host does not.

## Tool policy

````markdown
## build
```yaml
tools: [read, edit, bash]
policy:
  allow: [read, edit(src/*), bash(go test:*)]
  deny:
    - read(.env)
    - {rule: edit(src/gen/*), note: generated; run go generate}
```
````

**The rules are agentpolicy's**, in its grammar: a tool name, and in
parentheses a specifier the tool's matcher reads. agentflow keeps no path
or command matching of its own.

- `deny` refuses the calls its rules match.
- `allow`, if set, refuses every call none of its rules matches. An
  empty `allow` is a load error, since it would allow nothing.
- Deny is read before allow, and a carve-out (`!`) works as it does in
  agentpolicy.
- A rule may carry a `note`, which the refusal and the step's
  instructions give after it.
- There is no `ask`.

**A policy only narrows.** agentflow asks each engine what it would
decide (`Engine.Would`), which remembers nothing and tells no observer,
and acts only on a refusal. A call every policy lets through goes on to
the host's own `BeforeToolCall`, which may still refuse or ask.

**Where it is set:**

- on a model step;
- on a call (`flow:`), where it limits every model step and judge of the
  runs it calls, at any depth;
- on a judge (`judge.policy`).

Every policy in force applies, so the limits only add up.

**What a tool's arguments touch is the host's to say.**
`PolicyMatchers(func(dir string) map[string]agentpolicy.ToolMatcher,
opts...)` gives a matcher, and where one call is several subjects a
splitter, for each tool whose rules take a specifier. The options, such
as `agentpolicy.WithAliases`, go to every engine.

- Paths are below the workspace's root, in a child too. The host is
  asked for the matchers of each working directory the steps run in: `""`
  for the root and a child's `dir`. `InDir(dir, field)` is
  `agentpolicy.GlobMatcher` over a field read as a clean path below
  `dir`, so `edit(secret/*)` set above a child at `secret` refuses its
  edit of `key`.
- A tool with no splitter is one subject per call. A shell's
  substitutions and compounds are the host's splitter's to read;
  one that cannot read a command fails, and agentpolicy refuses the call.
- A tool that claims facts (`agenttool.Factual`) is read by its claims.

**Checked at load:**

- each rule parses, one to an entry;
- a rule on a step or a judge names a tool the step or judge offers. A
  step given `files` may also name `read` without offering it, since the
  program's reads of those files are held to its policy. A step with
  `tools: all` and a rule on a call are not checked this way, since their
  tools are known only later;
- `Apply` builds every policy's engine with the host's matchers, so a
  specifier for a tool with no matcher is refused there
  (`agentpolicy.ErrNoMatcher`), and so is a tool-name glob agentpolicy
  does not honour.

**The model is told its limits before it acts**, in its step's
instructions: each policy's rules as agentpolicy writes them
(`Rule.String()`), each with its note, and which step or call set it.

**A refusal is the gate's block, and names the rule**, after where it was
set:

```
agentflow: step "build" refuses this call: the policy of step "build" of release: denied by edit(src/gen/*): generated; run go generate
```

A call outside a step's `allow` is refused naming that list:
`…: no rule allows write_file: denied by default; it allows only calls
matching write_file(out/ok.txt)`. The model reads the refusal and can go
on.

**A tool's own calls** made while a model step works are held to the
step's policy and its callers', as the model's calls are. The `tools`
list is not asked: it says what the model is offered, and a tool's own
calls are how it is built. A step with an `allow` list therefore allows
what its tools call too. The calls of a program step's tool are not held
to a policy, as the step's own call is not.

**Children** take the policies of the calls and steps they are nested
in, already resolved, and add their own step's. This covers calls, fresh
steps, kept children and judges. A child's engines are built for
its `dir`.

### Rules that take their paths from the record

```yaml
policy:
  allow:
    - {rule: write, path: "${{ steps.plan.outputs.files }}", note: the plan's files}
  deny:
    - {rule: bash, command: "${{ steps.guard.outputs.forbidden }}"}
```

- A rule entry may name a tool alone and take what it matches from the
  record: `path` or `command`, one reference to a string or a list of
  strings. It stands for one rule per value: `write(src/a.go)`,
  `write(src/b.go)`.
- A rule from data with a specifier of its own, with both keys, or with a
  literal in place of the reference is a load error. A fixed path is
  written `write(src/a.go)`.
- The reference is checked at load as a `with` value is: it must have a
  value on every path into the step, and where its schema is known it
  must be a string or a list of strings. The tool is checked as any
  rule's is, and at `Apply` against the host's matchers with a stand-in
  specifier.
- It is read from the record when the step works: the step's own from its
  run's record, a call's from the calling run's. It is one more policy
  every call must pass. A path must be clean and below the workspace's
  root, and is one path, not a pattern: one that holds `*` or begins `!`
  fails the step. A command must be one the grammar can hold, and may be
  a pattern.
- The step's instructions list the rules as they resolved, so the request
  and its record hold the effective limit. A refusal names the resolved
  rule.
- A fresh step's child is given its step's own policy and its callers'
  already resolved, as the call's record made them, so the child's record
  says what it was held to.
- A judge's rules are fixed: one that takes a value from the record is a
  load error.

**A value that does not resolve fails the step**, which asks to be
retried, and no model works under it:

| Where | What happens |
|---|---|
| a model step working in its run's session | the program calls `flow_limits` before the model's first turn; its error is the step's failure |
| a fresh step or a call | the gate refuses the call into the child, so no child is opened |

`flow_limits` is the program's call under the step's key
(`…_v<n>-limits`), with the run, the step and the reason. Its result is
the error, and the retry question follows it. It holds no definition, so
a model that works later in the same context is sent it, as any program
call.

### What a policy does not reach

- **Documents.** A step's documents are read and written through the
  program's own tools, outside policy; see
  [steps-and-data.md](steps-and-data.md).
- **The program's file reads are reached.** The files a step is given
  are read by the program through the host's read tool (`FileReader`,
  `read` and `path` unless named), so the host's hooks and the step's
  policy decide each read.
- **A sandbox.** A policy decides on arguments and claims. A tool that
  does more than it says is not stopped.

## Tools a step is not offered

- `tools: all` offers a model step every tool the host's config offers,
  except the program's own (`flow_*`, `run_script`).
- `exclude_tools`, on a model step or in the frontmatter for every model
  step of the workflow, removes tools from the request. The two add up,
  and a step cannot take back what its workflow excludes.
- An excluded tool is never offered, and a call to it anyway is refused
  like any tool the step does not allow. A policy's `deny` is different:
  a denied tool is still offered.
- At `Apply`, every excluded name must be one the config offers, with a
  suggestion for a misspelling. A config with a `ToolProvider` names its
  tools only as it runs, so it is not checked there.

A skill a step names grants nothing: its `allowed-tools` are not read.

## What a model may start

- **The host chooses**, with `workflow.ModelInvokes(names...)`. A
  workflow from a repository is code that runs at fixed points, so being
  listed in a catalog does not make it the model's to start. With no
  names, a model starts none. A workflow not named is refused with a
  reason saying a person can start it.
- **A hidden workflow** (`hidden: true`, `Graph.Hidden`) is a part, run
  only by workflows that call it. `Graph.Invocation` refuses it, `Apply`
  refuses it as a root, `workflow.SkillTool` refuses a model's request
  for it whatever the host names, and `workflow.StartPrompt` leaves it
  out. A host's catalog should leave hidden workflows out too.
- **Only in conversation.** A request while a run is under way is
  refused (`agentflow.UnderWay`). A run is under way from its start until
  its end is written.
- **A workflow skill is never read by a model.** Its files hold the
  definition, a judge's criteria among it, which the working model must
  not read. Asking for one with a `path` is refused.
- **The model asks; the program invokes.** The record shows the model's
  call and the program's invocation, sent under a trigger of kind
  `workflow`.

The mechanism is in [host-integration.md](host-integration.md).

## The digest is the unit of approval

- `flow_start` records a digest of the graph, which for a skill covers
  every file in it. The digest recorded for a root covers its whole
  closure: the sorted names of every workflow it can reach, each with its
  own digest. Approving a root approves everything it can call.
- A change to any workflow in the closure makes resuming a run fail with
  `ErrChanged`, naming the workflow that changed, whether or not the run
  has reached it.
- Branches use only recorded outcomes. Time, randomness and anything
  clever belong in a script whose result is recorded.

## Where trust ends

- **Only the program runs scripts, which does not stop a model's code
  from running.** A model that may write files can change what a later
  script step runs. A workflow whose model steps write and whose script
  steps then build or test wants a contained workspace, and a policy that
  holds each step's writes to what was approved.
- **A judge's inputs can be written by someone else:** an issue's text, a
  diff. A judge is told that inputs are material and not instructions,
  and to hand back when an input tries to direct its answer or claims an
  approval. That is not a defence. Delegating a step whose answer
  publishes or deletes means trusting that text that far.
- **Secrets are never data.** Every input, output, question and document
  is in the transcript and can reach a model.
- **Sessions hold whatever the inputs held**, and are only as durable as
  the host's store. Sharing them needs redaction.
- **A model's stated reason is not checked.** A schema makes a model
  fill in a field, not look; a script can branch on what is reported and
  cannot know it is true.
