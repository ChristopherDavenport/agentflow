#!/bin/sh
pr=$(jq .pr)
printf '{"pr": %s, "files": [{"path":"a.go","additions":900},{"path":"b.md","additions":3}]}' "$pr"
