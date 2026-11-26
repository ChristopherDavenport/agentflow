#!/bin/sh
# What a schema cannot say: the plan has to be about this tree.
set -eu
plan=$(jq -c .plan)
errors=$(
  [ "$(printf %s "$plan" | jq '.steps | length')" -gt 0 ] || echo "the plan has no steps"
  [ "$(printf %s "$plan" | jq '(.files + (.new_files // [])) | length')" -gt 0 ] || echo "the plan names no files"
  printf %s "$plan" | jq -r '.files[]' | while IFS= read -r f; do
    git ls-files --error-unmatch -- "$f" >/dev/null 2>&1 || echo "$f is not a file of the repository"
  done
  printf %s "$plan" | jq -r '(.new_files // [])[]' | while IFS= read -r f; do
    case "$f" in /*|../*|*/../*|.git/*|.agentflow*) echo "$f is not a path a change may add" ;; esac
    [ ! -e "$f" ] || echo "$f is listed as new and exists"
  done
)
if [ -z "$errors" ]; then
  echo '{"outcome": "sound"}'
else
  printf '%s\n' "$errors" | jq -R . | jq -s '{outcome: "flawed", data: {errors: .}}'
fi
