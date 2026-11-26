#!/bin/sh
exec jq -c -f "$AGENTFLOW_SKILL_DIR/scripts/filter.jq"
