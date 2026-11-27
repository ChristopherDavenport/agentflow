# agentflow

Program-driven workflows over the agent loop. The design is in
`docs/design/`, one document per topic, starting at `overview.md`. Read
the documents your change touches before writing code.

## Design documents

- They describe the design as it is: present tense, no history. A commit
  that changes behaviour changes the document that describes it.
- One home per fact. Other documents link to it; none restates it.
- Why something was chosen over an alternative goes in `decisions.md`.
  What is unbuilt, unsettled or untried goes in `open-questions.md`, and
  leaves it when it is done. Neither goes in the topic documents.
- Do not add a dated or numbered proposal file. Propose a change as a
  pull request that edits these documents.

## Module

- Module path: `github.com/ChristopherDavenport/agentflow`.
- Go 1.26 is the floor, which `agentworkspace` sets.
- The root package builds from `openresponses`, `agenttool`, `agentturn`,
  `agentpolicy` and the standard library; `script` adds `agentworkspace`;
  `workflow` adds `agentskill` and `go.yaml.in/yaml/v3`.
  A step's policy is agentpolicy's grammar and engine; agentflow keeps no
  matching of its own.
  `agentsession` and `agentturn/session` are test dependencies only.
  `make deps` enforces the module's list.

## Rules

- The loop learns nothing about workflows. Build on its public surface.
- Position is a pure function of the graph and the record: every item
  that happened. What a model is sent is only a view of it.
- The record never passes a program's turn off as a model's.
- Only the program runs scripts.

## Checks

`GOWORK=off GOFLAGS=-mod=readonly make check`
