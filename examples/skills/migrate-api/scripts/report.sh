#!/bin/sh
set -eu
in=$(cat)
base=$(printf %s "$in" | jq -r .base)
stat=$(git diff --stat "$base")
printf %s "$in" | jq --arg stat "$stat" '{summary: ("Migrated " + (.migrated | join(", ")) + ".\n\n" + $stat)}'
