#!/bin/sh
# Sets the migration's changes aside on a branch of their own and
# returns the tree to the base. Nothing is lost and nothing is pushed.
set -eu
base=$(jq -r .base)
[ -n "$(git status --porcelain)" ] || { echo '{}'; exit 0; }
branch="migrate-api/abandoned-$(date +%Y%m%d%H%M%S)"
git switch -q -c "$branch"
git commit -q -am "migrate-api: abandoned"
git switch -q "$base"
jq -n --arg b "$branch" '{branch: $b}'
