# agentflow

Program-driven workflows over the agent loop. The design is in
`docs/rfcs/0001-agentflow.md`; read it before writing code, and change it
in the same commit as any behaviour it describes.

## Module

- Module path: `github.com/ChristopherDavenport/agentflow`.
- Go 1.25 is the floor.
- The root package builds from `openresponses`, `agenttool`, `agentturn`
  and the standard library. `agentsession` and `agentturn/session` are
  test dependencies only. `make deps` enforces it.

## Rules

- The loop learns nothing about workflows. Build on its public surface.
- Position is a pure function of the graph and the transcript.
- The record never passes a program's turn off as a model's.

## Checks

`GOWORK=off GOFLAGS=-mod=readonly make check`
