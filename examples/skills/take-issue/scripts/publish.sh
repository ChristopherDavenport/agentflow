#!/bin/sh
# Commit the listed files onto HEAD through a scratch index and push that
# commit. The checkout, its branch and its index are not touched.
set -eu
in=$(cat)
repo=$(printf %s "$in" | jq -r .repo)
issue=$(printf %s "$in" | jq -r .issue)
title=$(printf %s "$in" | jq -r .plan.title)
summary=$(printf %s "$in" | jq -r .plan.summary)
branch="issue-$issue"

index=$(mktemp)
trap 'rm -f "$index"' EXIT
export GIT_INDEX_FILE="$index"
git read-tree HEAD
printf %s "$in" | jq -r '.changed[]' | while IFS= read -r f; do
  git add --all -- "$f"
done
commit=$(git commit-tree "$(git write-tree)" -p HEAD -m "$title" -m "Closes #$issue")
# No force: a branch someone else made stops this.
git push --quiet origin "$commit:refs/heads/$branch" >&2
url=$(gh pr create --repo "$repo" --head "$branch" --title "$title" --body "$summary

Closes #$issue")
jq -n --arg u "$url" '{url: $u}'
