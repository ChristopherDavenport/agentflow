#!/bin/sh
set -eu
in=$(cat)
file=".agentflow-plans/$(printf %s "$in" | jq -r .issue).json"
mkdir -p .agentflow-plans
printf %s "$in" | jq '{base, plan}' > "$file"
jq -n --arg p "$file" '{path: $p}'
