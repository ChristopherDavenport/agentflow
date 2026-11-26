---
name: pr-review
description: Review a GitHub pull request against what it says it does, check each finding, and report. Posts nothing.
metadata:
  agentflow: WORKFLOW.md
---
Read the pull request's description and diff with `gh pr view` and
`gh pr diff`. Look for what it claims and does not do, and for bugs in
what it changes. Check each finding against the code before reporting it.
Post nothing.
