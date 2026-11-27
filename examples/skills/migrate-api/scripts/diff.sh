#!/bin/sh
# Runs in the package's directory: what the migration changed here.
set -eu
git diff --relative -- . | jq -Rs '{diff: .}'
