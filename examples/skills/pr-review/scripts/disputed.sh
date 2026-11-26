#!/bin/sh
exec jq '{report: ("# \(.title)\n\n\(.summary)\n\nRaised and then dismissed; check these yourself.\n"
  + ([.findings[] | "\n- **\(.file)**: \(.problem)"] | join(""))
  + "\n\nWhy they were dismissed:\n"
  + ([.reasons[] | "\n- **\(.file)**: \(.why)\n  Evidence: \(.evidence)"] | join("")))}'
