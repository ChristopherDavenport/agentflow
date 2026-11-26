#!/bin/sh
exec jq '{report: ("# \(.title)\n\n\(.summary)\n" + ([.findings[] | "\n- **\(.file)**: \(.problem)\n  Evidence: \(.evidence)"] | join("")))}'
