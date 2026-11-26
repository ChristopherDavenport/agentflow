#!/bin/sh
set -eu
in=$(cat)
file=".agentflow-plans/$(printf %s "$in" | jq -r .issue).json"
base=$(printf %s "$in" | jq -r .base)
jq --arg base "$base" 'if .base == $base
  then {outcome: "current", data: {plan}}
  else {outcome: "outdated", data: {plan, since: .base}} end' "$file"
