#!/bin/sh
set -eu
changed=$(git ls-files --modified --deleted --others --exclude-standard -- . ':(exclude).agentflow*' | sort -u | jq -R . | jq -s .)
if [ "$changed" = "[]" ]; then
  echo '{"outcome": "fail", "data": {"log": "nothing was changed"}}'
  exit 0
fi
# The diff a reviewer reads, new files included, through a scratch index.
index=$(mktemp)
trap 'rm -f "$index"' EXIT
diff=$(
  export GIT_INDEX_FILE="$index"
  git read-tree HEAD
  printf %s "$changed" | jq -r '.[]' | while IFS= read -r f; do git add --all -- "$f"; done
  git diff --cached HEAD
)
# Never part of one: a judge of half a diff reads like a judge of all.
[ "${#diff}" -le 40000 ] || diff="not shown: ${#diff} bytes"
report() { jq -n --arg o "$1" --arg k "$2" --arg d "$diff" --argjson c "$changed" '{outcome: $o, data: {check: $k, changed: $c, diff: $d}}'; }
if grep -qs '^check:' Makefile; then check="make check"
elif grep -qs '^test:' Makefile; then check="make test"
elif [ -f go.mod ]; then check="go test ./..."
else
  report none "no check found"
  exit 0
fi
if log=$($check 2>&1); then
  report pass "$check passed"
else
  # The end of a log is where the failure is told.
  printf '%s' "$log" | tail -c 8000 | jq -Rs '{outcome: "fail", data: {log: .}}'
fi
