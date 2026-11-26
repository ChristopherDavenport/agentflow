---
name: take-issue
description: Take a GitHub issue to a pull request. Plans, waits for a person to approve the plan, makes the change, runs the repository's checks and publishes only when told to.
metadata:
  agentflow: WORKFLOW.md
---
Work from the issue: read it with `gh issue view`, plan the change, and
ask for approval before editing anything. Run the repository's checks.
Ask again before pushing or opening a pull request.
