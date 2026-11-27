#!/bin/sh
# The first package of the order not yet handed out.
set -eu
jq '(.done // []) as $done
  | ([.order[] | select(. as $p | $done | index($p) | not)] | first) as $p
  | if $p == null then {outcome: "finished", data: {done: $done}}
    else {outcome: "package", data: {package: $p, done: ($done + [$p])}} end'
