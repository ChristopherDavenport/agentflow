#!/bin/sh
echo "scoring..."
jq -c '{outcome: (if .added > 100 then "big" else "small" end), data: {summary: .summary}}' > "$AGENTFLOW_OUTPUT"
