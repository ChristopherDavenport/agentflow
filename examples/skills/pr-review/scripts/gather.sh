#!/bin/sh
set -eu
in=$(cat)
pr=$(printf %s "$in" | jq -r .pr)
repo=$(printf %s "$in" | jq -r .repo)
meta=$(gh pr view "$pr" --repo "$repo" --json title,body)
diff=$(gh pr diff "$pr" --repo "$repo")
# Never cut a diff: a review of half of one reads like a review of all.
if [ "${#diff}" -gt 40000 ]; then
  echo "the diff is ${#diff} bytes, over the 40000 this review takes" >&2
  exit 1
fi
printf %s "$meta" | jq --arg diff "$diff" '{title, body, diff: $diff}'
