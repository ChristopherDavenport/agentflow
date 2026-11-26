# RFC 0008: contracts and step settings

Status: spiked where a section says so. It covers a step's policy taken
from data, a question whose text is computed, input defaults, timeouts
on calls and for a whole workflow, what a callee may declare of itself,
and tools excluded from a step. Each is held to the rules of RFC 0001
and RFC 0002: a call with arguments and a result in its context;
position a pure function of the definitions and the record; resuming
needs only the store and the definitions; control flow depends only on
the definition and recorded results; unknown keys are load errors.

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
