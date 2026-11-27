#!/bin/sh
# The order must be the packages chosen, each once, each a directory.
set -eu
in=$(cat)
errors=$(printf %s "$in" | jq -r '
  (.order | map(select(. as $p | $in_pkgs | index($p) | not)) | map(. + " is not one of the packages chosen")[]),
  ((.packages - .order) | map(. + " is missing from the order")[]),
  (.order | group_by(.) | map(select(length > 1) | .[0] + " is in the order twice")[])
' --argjson in_pkgs "$(printf %s "$in" | jq -c .packages)")
for p in $(printf %s "$in" | jq -r '.order[]'); do
  [ -d "$p" ] || errors="$errors
$p is not a directory"
done
errors=$(printf '%s\n' "$errors" | sed '/^$/d')
if [ -z "$errors" ]; then
  echo '{"outcome": "sound"}'
else
  printf '%s\n' "$errors" | jq -R . | jq -s '{outcome: "flawed", data: {errors: .}}'
fi
