#!/bin/sh
# Runs in the package's directory.
set -u
log=$(go test . 2>&1)
if [ $? -eq 0 ]; then
  echo '{"outcome": "pass"}'
else
  printf %s "$log" | jq -Rs '{outcome: "fail", data: {log: ., failures: (split("\n") | map(select(startswith("--- FAIL") or test("^\\S+\\.go:\\d+"))))}}'
fi
