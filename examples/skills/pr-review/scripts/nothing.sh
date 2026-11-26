#!/bin/sh
exec jq '{report: "# \(.title)\n\n\(.summary)\n\nNo problems to report."}'
