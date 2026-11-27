#!/bin/sh
set -eu
in=$(cat)
base=$(printf %s "$in" | jq -r .base)
set -- $(printf %s "$in" | jq -r '.migrated[]')
stat=$( [ $# -eq 0 ] || git diff --stat "$base" -- "$@")
printf %s "$in" | jq --arg stat "$stat" '{summary: ("Migrated " + (.migrated | join(", ")) + ".\n\n" + $stat)}'
