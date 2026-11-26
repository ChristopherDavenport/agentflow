#!/bin/sh
# A person may have taken a while. Look again before anything is edited.
set -eu
base=$(jq -r .base)
blocked() { jq -n --arg r "$1" '{outcome: "blocked", data: {reason: $r}}'; exit 0; }
[ "$(git rev-parse HEAD)" = "$base" ] || blocked "HEAD moved since the plan was made"
dirty=$(git status --porcelain -- . ':(exclude).agentflow*')
[ -z "$dirty" ] || blocked "the working tree has uncommitted changes"
echo '{"outcome": "clear"}'
