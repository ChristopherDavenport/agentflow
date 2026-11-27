#!/bin/sh
# Runs in the package's directory: does it still call the old function?
set -eu
old=$(jq -r .old)
name=${old##*.}
path=${old%.*}
pkg=${path##*/}
if ls *.go 2>/dev/null | grep -v _test.go | xargs grep -q -E "\\b$pkg\\.$name\\(" 2>/dev/null; then
  echo '{"outcome": "todo"}'
else
  jq -n '{outcome: "clean", data: {why: "the package no longer calls it"}}'
fi
