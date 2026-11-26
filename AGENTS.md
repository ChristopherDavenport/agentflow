# agentflow

Program-driven workflows over the agent loop. The design is in
`docs/rfcs/0001-agentflow.md`; read it before writing code, and change it
in the same commit as any behaviour it describes.

## Module

- Module path: `github.com/ChristopherDavenport/agentflow`.
- Go 1.26 is the floor, which `agentworkspace` sets.
- The root package builds from `openresponses`, `agenttool`, `agentturn`
  and the standard library; `script` adds `agentworkspace`.
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
