# Open questions

What is not settled, not built, known to be weak, or not yet tried with a
live model. Everything in the other documents is built and tested with
scripted models unless it is listed here.

## Not settled

1. **Compaction without a record.** A host with no `History` cannot
   compact a running workflow: the transform is refused with
   `ErrPositionLost`. Requiring `History` whenever a config has a
   `Transform` would remove the second mode. A front's `Replay` must be
   given the record too.
2. **Fan-out.** Running a step once per item of a list, and joining.
3. **Reminders.** One is the rule. A host may want more for a small
   model.
4. **A failed run** answers every later prompt with its error until an
   invocation, since the transcript does not say the error was seen.
5. **Run directories** show in a repository's status while a script
   runs. Hosts exclude them by hand.
6. **Gates.** A `check` script that rejects a model's result before the
   step finishes.
7. **A model step's deadline cuts only its stream and its tools' calls.**
   A turn whose stream has just finished runs its tool calls under the
   deadline, and the program records the timeout on the next turn. A tool
   that ignores its context is not stopped at all.
8. **A grant in a kept child.** A kept child's later runs are each given
   what the tree has left. A grant made in one counts for the caller, and
   reaches the child's next run only through that count.
9. **A front's clock.** A question's deadline is absolute, in UTC; a
   front that shows how long is left may have a different clock.
10. **A pick with words.** Whether a pick may also carry the person's
    words is left to `Result.Reason`, which every answer may carry.
11. **An attended judge has no deadline.** A judge takes no `timeout`,
    and its workflow's default does not apply to it. Unattended it takes
    `UnattendedTimeout`; attended, nothing bounds its look but a call's
    timeout above it.
12. **Whether the syntax is settled.** Every key in
    [workflow-format.md](workflow-format.md) is built; none has been
    declared stable.
13. **The limit of blindness under `Shadow`.** A later visit's person can
    read an earlier verdict in the record, so only a step's first answer
    in a run is surely blind.

## Known weaknesses

These are in the code as it stands.

- **Without `History`, the gate reads a quarantined transcript.** A host
  whose guards can quarantine must give `History`; nothing checks that it
  does.
- **A model can read a run's key** in its instructions. The gate tells
  the program's calls by their `flow_<key>_…` IDs, which is sound with
  providers that mint call IDs. Replay ignores an end or a shown result
  the gate refused, which covers a model that forges one; other calls
  rest on the IDs.
- **A step can deny a nested tool only if it offers it.** A rule must
  name a tool the step offers, so a step cannot deny a tool that only
  another tool calls, except under `tools: all` or from a calling step.

- **The cost of a seeded child's first request.** It repeats what its
  caller's model had been sent, and a seeded child that calls repeats
  that again. Whether a provider's cache takes most of it is not
  measured.
- **A seeded child with no store.** A host that keeps its agents in
  memory copies the caller's items into the child's agent. Its requests
  have no session to verify against, and it cannot compact.
- **A call in a child's inherited part without its output** is pending in
  the child. A fork before the calling call leaves that call out, but an
  earlier unanswered call of the same response would still be there.

## Proposed, not built

- **Approval by digest before a first run.** A workflow from a
  repository is repository code that runs at fixed points. The digest is
  recorded; nothing yet asks for approval of it.
- **`context` as a list of step IDs** whose results a model step is
  given, between `session` and `fresh`.
- **Reading decisions back.** No extractor is built until there are
  decisions to read. The record already holds what one needs; see
  [record-and-replay.md](record-and-replay.md).
- **Presentation hints** for a question, and **review comments that
  persist** across visits. Both wait for a front.
- **`child:` on a fresh call to a workflow.** A workflow run is a session
  of its own; a kept one can be opened up if a workflow needs it.
- **Checking a child's `tools` names at the caller's `Apply`.** A child's
  tools are the opener's, built for its `dir`, so the caller's config
  cannot say what they will be. The child's own `Apply` checks them.
- **A contained workspace.** Scripts run inside the repository they work
  on, and a model that may write files can change what a later script
  runs. See [policy-and-trust.md](policy-and-trust.md).
- **The `pr-review` example** still draws three report steps; it has not
  been rewritten with `latest` and `optional`.

## Not verified

- That Mermaid itself draws every diagram the parser accepts.
- That a judge justifies `Decide`. Trials show it follows its wording
  without refusing good plans, on cases the wording was written against;
  none shows it catching something new.

## Not tried with a live model

Each of these is tested with scripted models only.

- A question's timeout.
- Cleanup on abort.
- The token counts in a run's summary.
- The tree's step bound, and the repeat rule.
- Script progress, live and kept.
- Input defaults.
- A call's timeout.
- `exclude_tools`.
- A model correcting its inputs when it starts a workflow from a skill.
- A judge reaching `max_returns`.
- The quoting of reasons in what a summarizer is given.
- A model step that uses tools over many turns, and a prompt that
  competes with the instruction to finish.
- A call whose child opens with its caller's view, beyond one fact. A
  callee's model reports a word its caller's step read from a file and
  no output carries, and a fresh callee cannot. What a callee makes of a
  long inherited conversation, of many earlier steps, of a caller's
  folded context, or of what a callee two calls deep inherits, is not
  tried.

Forked child sessions are tested on agentturn/session's memory and jsonl
stores only, not on sqlite or cas, and not through its retry path.

One run each of most other features says the machinery holds with a real
model, not how often a model does the right thing.
