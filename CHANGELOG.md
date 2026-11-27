# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

- Initial release. The design is in `docs/design/`.
- `agentflow`: a graph of script, tool, model and person steps that
  drives an `agentturn` loop, built in Go and run with `Apply`, `Invoke`
  and `Resume`. A run's position is read from its record alone.
- Data between steps, calls to other workflows and fresh steps in child
  sessions, questions to people, and judges that may answer for them as
  far as the host delegates.
- Step policy in `agentpolicy`'s grammar, timeouts and bounds, retry,
  cleanup and operator overrides.
- `workflow`: `WORKFLOW.md`, a Mermaid flowchart and a section per node,
  loaded from an Agent Skill.
- `script`: `run_script` over an `agentworkspace`.
- `summary`: how a run went, read back from its record.
- `workflow.SkillBody`: the text a workflow skill's `SKILL.md` opens
  with. It points a model in a host without workflows at the file
  `metadata.agentflow` names and says how to follow it as agentflow
  would. `workflow.HasSkillBody` says whether a skill's body opens with
  it. The example skills carry it.
- A step's `skills` naming a workflow skill is a load error.
