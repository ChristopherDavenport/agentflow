#!/bin/sh
# Every call of the old function, outside its own package.
set -eu
in=$(cat)
old=$(printf %s "$in" | jq -r .old)
name=${old##*.}
path=${old%.*}
pkg=${path##*/}
sites=$(git grep -n -E "\\b$pkg\\.$name\\(" -- '*.go' ':(exclude)*_test.go' ":(exclude)$path/*" | cut -d: -f1,2 || true)
if [ -z "$sites" ]; then
  echo '{"outcome": "none"}'
  exit 0
fi
printf '%s\n' "$sites" | jq -R 'split(":") | {file: .[0], line: (.[1] | tonumber)}' | jq -s '{
  outcome: "found",
  data: {
    sites: .,
    count: length,
    packages: (map(.file | split("/") | .[:-1] | join("/") | if . == "" then "." else . end) | unique),
    report: ("# Scan\n\n" + (map("- " + .file + ":" + (.line | tostring)) | join("\n")) + "\n")
  }
}'
