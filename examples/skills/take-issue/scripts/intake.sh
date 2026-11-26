#!/bin/sh
# The runner's own files sit in the workspace, so they never count.
set -eu
in=$(cat)
repo=$(printf %s "$in" | jq -r .repo)
issue=$(printf %s "$in" | jq -r .issue)
blocked() { jq -n --arg r "$1" '{outcome: "blocked", data: {reason: $r}}'; exit 0; }

dirty=$(git status --porcelain -- . ':(exclude).agentflow*')
[ -z "$dirty" ] || blocked "the working tree has uncommitted changes"
meta=$(gh issue view "$issue" --repo "$repo" --json title,body,state)
[ "$(printf %s "$meta" | jq -r .state)" = OPEN ] || blocked "issue $issue is not open"

outcome=ready
[ ! -f ".agentflow-plans/$issue.json" ] || outcome=resume
printf %s "$meta" | jq --arg o "$outcome" --arg base "$(git rev-parse HEAD)" \
  '{outcome: $o, data: {title, body: (.body // ""), base: $base}}'
